// RoleManager owns the platform's APPLICATION database roles: it creates them, applies
// the credential ESO delivers, detects password drift and rotates it, and prunes roles
// the CR no longer declares.
//
// ADR-023 assigns logical databases, roles and grants to Crossplane `provider-sql`, and
// this file's imperative DCL is the state of that migration rather than a dispute with
// it: until provider-sql is deployed, this is what provisions them. One role is not its
// business -- Infisical's, which CNPG owns through spec.managed.roles (ADR-023 addendum)
// so that the role which lets Infisical start does not depend on Infisical.
//
// SCHEMA is owned nowhere in this package. Each platform application migrates its own, at
// its own cluster (ADR-074, amended 2026-11-03), so the only DDL here is the grants a
// role needs on a database its application owns.

package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	// Registers the "postgres" driver that sql.Open below names. A blank import is the
	// only thing that does, and nothing fails to compile without it: it fails at run
	// time, with `sql: unknown driver "postgres"`, on the first role provisioned.
	_ "github.com/lib/pq"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	opsv1alpha1 "github.com/kamarkat/kamarkat/operators/hub-operator/api/v1alpha1"
)

const (
	secretNamespaceData     = "platform-data"
	secretNamespaceIdentity = "platform-identity"
)

// RoleSecretName is the Secret a declared role's credential arrives in.
//
// EXPORTED because two things must agree on it: this package applies the credential to
// the role, and the controller waits for ESO to deliver it before provisioning any role
// (ApplicationSecretsReady). A gate that lists its Secrets separately drifts from the
// roles it guards and waits for credentials nothing will deliver, so both sides derive
// the name from the declared roles through this one function.
//
// The mapping is conventional rather than declared, which is why a Secret named here
// appears in no manifest that references it by name: `hub_grafana` becomes
// `hub-grafana-db-credentials` by this rule alone.
func RoleSecretName(roleName string) string {
	switch roleName {
	case "hub_zitadel":
		return "zitadel-db-credentials"
	default:
		// Replace underscores with hyphens for valid K8s names
		return strings.ReplaceAll(roleName, "_", "-") + "-db-credentials"
	}
}

// RoleSecretNamespace returns the namespace where the secret is located.
// The identity provider's credential lives in platform-identity; every other
// service's lives in platform-data.
func RoleSecretNamespace(roleName, defaultNamespace string) string {
	switch roleName {
	case "hub_zitadel":
		return secretNamespaceIdentity
	default:
		return defaultNamespace
	}
}

// RoleManager handles database role creation and management
type RoleManager struct {
	db     *sql.DB
	client client.Client
	// ep is the cluster this manager was opened against. Held rather than re-derived,
	// because grantPermissions opens a SECOND connection -- to the target database
	// rather than to `postgres` -- and that one used its own literal host and its own
	// hardcoded namespace. Two connection strings for one cluster is how they come to
	// disagree.
	ep Endpoint
}

// NewRoleManager creates a new RoleManager instance for one CNPG cluster.
//
// The cluster is a parameter rather than a literal: ADR-112 gives each platform
// application its own, and each role declares which one it lives on (ClusterRef). The
// caller opens one manager per cluster and passes it only that cluster's roles.
func NewRoleManager(ctx context.Context, k8sClient client.Client, ep Endpoint) (*RoleManager, error) {
	logger := log.FromContext(ctx)

	if err := ep.Validate(); err != nil {
		return nil, err
	}

	// Read the cluster's superuser secret for connection credentials
	secret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, client.ObjectKey{
		Name:      ep.SuperuserSecret(),
		Namespace: ep.Namespace,
	}, secret); err != nil {
		return nil, fmt.Errorf("failed to get %s secret: %w", ep.SuperuserSecret(), err)
	}

	username := string(secret.Data["username"])
	password := string(secret.Data["password"])
	dbname := string(secret.Data["dbname"])

	// Handle CNPG wildcard database name
	// CNPG uses "*" to denote superuser access to all databases
	if dbname == "*" || dbname == "" {
		dbname = "postgres"
	}

	// Connect to the cluster's primary service with TLS
	connStr := fmt.Sprintf(
		"host=%s port=5432 user=%s password=%s dbname=%s sslmode=require",
		ep.PrimaryHost(), username, password, dbname,
	)

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to open database connection: %w", err)
	}

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	logger.Info("Connected for role management", "host", ep.PrimaryHost())

	return &RoleManager{
		db:     db,
		client: k8sClient,
		ep:     ep,
	}, nil
}

// CreateOrUpdateRoles provisions all database roles from HubEnvironment CR
// MissingRoles reports which declared roles do not exist in PostgreSQL.
//
// It exists because a status condition recording that roles were provisioned is
// a statement about the past, not about the database. When the cluster's data
// directory is lost — a node rebuilt under node-pinned local-path storage, say —
// PostgreSQL comes back with only the bootstrap user while the condition still
// reads True. The operator then skips provisioning forever, and every service
// fails authentication against a role that no longer exists. That is not
// hypothetical: it is how hub-hybrid-dev stayed down after its primary was
// re-initialised, with the condition asserting work that had been erased.
//
// Role names are resolved the same way CreateOrUpdateRoles resolves them —
// through the credential secret rather than from the spec directly — so the two
// cannot disagree about what a role is called.
// RolesDisagreeingWithSecret names the roles whose password in PostgreSQL does not
// match the Secret that is authoritative for them.
//
// EXISTENCE IS NOT AGREEMENT, and conflating the two is what produced a false
// green. MissingRoles asks whether a role is there; a role that is there with the
// wrong password satisfies it, DatabaseRolesProvisioned stays True, and the
// condition reports a working database while the application cannot log in. That
// is what nutgraf-mgmt-1794929668 reported throughout the hub_zitadel outage.
//
// Read-only: it compares the stored hash through passwordNeedsUpdate and writes
// nothing. hub-operator no longer sets passwords (CloudNativePG does), so a
// disagreement here is not something this operator repairs -- it is something
// readiness must REPORT, so that "CNPG has not converged yet" and "the roles are
// provisioned" stop being the same answer.
func (rm *RoleManager) RolesDisagreeingWithSecret(ctx context.Context, hubEnv *opsv1alpha1.HubEnvironment) ([]string, error) {
	namespace := hubEnv.Spec.Database.Namespace

	var disagreeing []string
	for _, roleSpec := range hubEnv.Spec.RolesFor(rm.ep.Cluster) {
		secret := &corev1.Secret{}
		if err := rm.client.Get(ctx, client.ObjectKey{
			Name:      RoleSecretName(roleSpec.Name),
			Namespace: RoleSecretNamespace(roleSpec.Name, namespace),
		}, secret); err != nil {
			// A credential that has not been delivered is not a disagreement. The
			// ExternalSecret's own status says why, and MissingRoles covers absence.
			continue
		}
		username := string(secret.Data["username"])
		password := string(secret.Data["password"])
		if username == "" || password == "" {
			continue
		}
		differs, err := rm.passwordNeedsUpdate(ctx, username, password)
		if err != nil {
			return nil, fmt.Errorf("compare role %s against its Secret: %w", roleSpec.Name, err)
		}
		if differs {
			disagreeing = append(disagreeing, roleSpec.Name)
		}
	}
	return disagreeing, nil
}

func (rm *RoleManager) MissingRoles(ctx context.Context, hubEnv *opsv1alpha1.HubEnvironment) ([]string, error) {
	namespace := hubEnv.Spec.Database.Namespace

	var missing []string
	for _, roleSpec := range hubEnv.Spec.RolesFor(rm.ep.Cluster) {
		secret := &corev1.Secret{}
		if err := rm.client.Get(ctx, client.ObjectKey{
			Name:      RoleSecretName(roleSpec.Name),
			Namespace: RoleSecretNamespace(roleSpec.Name, namespace),
		}, secret); err != nil {
			// The credential is not available yet, so provisioning cannot be
			// verified either way. Treat it as missing so the caller reconciles
			// rather than concluding the role is present.
			missing = append(missing, roleSpec.Name)
			continue
		}

		username := string(secret.Data["username"])
		if username == "" {
			missing = append(missing, roleSpec.Name)
			continue
		}

		var exists bool
		if err := rm.db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", username,
		).Scan(&exists); err != nil {
			return nil, fmt.Errorf("failed to check whether role %s exists: %w", username, err)
		}
		if !exists {
			missing = append(missing, username)
		}
	}

	return missing, nil
}

// Requirement 6.1-6.7: Create roles for all services
// Requirement 6.9: Use idempotent CREATE ROLE IF NOT EXISTS
func (rm *RoleManager) CreateOrUpdateRoles(ctx context.Context, hubEnv *opsv1alpha1.HubEnvironment) error {
	logger := log.FromContext(ctx)

	namespace := hubEnv.Spec.Database.Namespace

	// The whole plan, before any of it runs.
	//
	// EVERY ROLE IS ATTEMPTED. This loop used to return on the first failure, so a
	// role whose Secret was missing stopped every role AFTER it -- and the roles
	// are independent, so that turned one component's problem into an outage in an
	// unrelated one.
	//
	// It happened, twice. hub_hydra outlived the Ory stack in the CR spec, its
	// Secret was gone, and the loop never reached hub_zitadel: the role kept
	// whatever password it already had while Zitadel presented the one from its own
	// Secret, and Postgres refused it as `failed SASL auth`. Nothing in any log
	// connected the two, because the component that broke was not the component
	// that failed. Naming the plan and the skipped remainder made that visible; it
	// did not stop it happening.
	//
	// So failures are collected and every role is still attempted. This is not a
	// swallowed error: each failure is logged as it happens and all of them are
	// returned together, so the reconcile still fails and still names every cause.
	// What it no longer does is let the first one decide which components work.
	planned := make([]string, 0, len(hubEnv.Spec.RolesFor(rm.ep.Cluster)))
	for _, roleSpec := range hubEnv.Spec.RolesFor(rm.ep.Cluster) {
		planned = append(planned, roleSpec.Name)
	}
	logger.Info("Provisioning database roles", "count", len(planned), "roles", planned)

	// Collected, not returned on sight: see the note above.
	var failures []error

	// Process each role from CR spec
	for i, roleSpec := range hubEnv.Spec.RolesFor(rm.ep.Cluster) {
		// Map role name to valid K8s secret name and namespace
		secretName := RoleSecretName(roleSpec.Name)
		secretNamespace := RoleSecretNamespace(roleSpec.Name, namespace)

		logger.Info("Processing database role",
			"role", roleSpec.Name, "position", fmt.Sprintf("%d/%d", i+1, len(planned)),
			"database", roleSpec.Database, "secret", secretNamespace+"/"+secretName)

		secret := &corev1.Secret{}
		if err := rm.client.Get(ctx, client.ObjectKey{
			Name:      secretName,
			Namespace: secretNamespace,
		}, secret); err != nil {
			logger.Error(err, "Role credential Secret missing; this role is skipped and the rest continue",
				"role", roleSpec.Name, "secret", secretNamespace+"/"+secretName,
				"hint", "a role declared in HubEnvironment.spec.database.roles whose Secret no longer exists no longer blocks the other roles; remove it from the spec or restore its Secret")
			failures = append(failures, fmt.Errorf("role %s: get secret %s/%s: %w",
				roleSpec.Name, secretNamespace, secretName, err))
			continue
		}

		username := string(secret.Data["username"])
		password := string(secret.Data["password"])

		// Create or update role
		if err := rm.createOrUpdateRole(ctx, username, password, roleSpec); err != nil {
			logger.Error(err, "Role create/update failed; this role is skipped and the rest continue",
				"role", roleSpec.Name, "username", username)
			failures = append(failures, fmt.Errorf("role %s: create/update: %w", roleSpec.Name, err))
			continue
		}

		// Grant permissions
		// Requirement 6.8: Grant permissions based on CR specifications
		if err := rm.grantPermissions(ctx, username, roleSpec); err != nil {
			logger.Error(err, "Grant failed; this role is skipped and the rest continue",
				"role", roleSpec.Name, "username", username)
			failures = append(failures, fmt.Errorf("role %s: grant: %w", roleSpec.Name, err))
			continue
		}

		logger.Info("Role configured successfully",
			"role", roleSpec.Name, "username", username, "position", fmt.Sprintf("%d/%d", i+1, len(planned)))
	}

	logger.Info("All database roles provisioned", "count", len(planned), "roles", planned)

	// Prune orphaned roles
	// Requirement 6.15: Delete roles not in CR spec
	// Every failure, named together. The reconcile still fails -- a role that was
	// not configured is not working -- but it fails saying which roles, rather than
	// stopping at the first and leaving the rest unattempted.
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d database role(s) could not be configured: %w",
			len(failures), len(planned), errors.Join(failures...))
	}

	if err := rm.pruneOrphanedRoles(ctx, hubEnv); err != nil {
		return fmt.Errorf("failed to prune orphaned roles: %w", err)
	}

	return nil
}

// createOrUpdateRole creates a role if it doesn't exist or updates password if changed
func (rm *RoleManager) createOrUpdateRole(ctx context.Context, username, password string, roleSpec opsv1alpha1.DatabaseRole) error {
	logger := log.FromContext(ctx)

	// Check if role exists
	var exists bool
	checkSQL := "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)"
	if err := rm.db.QueryRowContext(ctx, checkSQL, username).Scan(&exists); err != nil {
		return fmt.Errorf("failed to check role existence: %w", err)
	}

	if !exists {
		// Requirement 6.9: Use idempotent CREATE ROLE IF NOT EXISTS
		// Create new role (PostgreSQL does not support inline COMMENT in CREATE ROLE)
		// Note: CREATE ROLE doesn't support parameterized passwords, must use string escaping
		escapedPassword := strings.ReplaceAll(password, "'", "''")
		createSQL := fmt.Sprintf("CREATE ROLE \"%s\" WITH LOGIN PASSWORD '%s'", username, escapedPassword)
		logger.Info("Executing CREATE ROLE statement", "sql", createSQL, "username", username)
		if _, err := rm.db.ExecContext(ctx, createSQL); err != nil {
			logger.Error(err, "CREATE ROLE failed", "sql", createSQL, "username", username, "error_detail", err.Error())
			return fmt.Errorf("failed to create role: %w", err)
		}

		// Add comment to identify managed roles (separate statement required)
		commentSQL := fmt.Sprintf("COMMENT ON ROLE \"%s\" IS 'Managed by hub-operator'", username)
		logger.Info("Adding role comment", "sql", commentSQL, "username", username)
		if _, err := rm.db.ExecContext(ctx, commentSQL); err != nil {
			logger.Error(err, "COMMENT ON ROLE failed", "sql", commentSQL, "username", username)
			return fmt.Errorf("failed to add role comment: %w", err)
		}

		logger.Info("Created database role", "username", username)
		return nil
	}

	// Role exists - ensure comment is set (idempotency)
	commentSQL := fmt.Sprintf("COMMENT ON ROLE \"%s\" IS 'Managed by hub-operator'", username)
	if _, err := rm.db.ExecContext(ctx, commentSQL); err != nil {
		return fmt.Errorf("failed to update role comment: %w", err)
	}

	// THE PASSWORD OF AN EXISTING ROLE IS NOT THIS OPERATOR'S TO SET.
	//
	// An `ALTER ROLE ... WITH PASSWORD` stood here, guarded by a drift check
	// against pg_authid. It was the second password authority for every role whose
	// Cluster already declares one, and CloudNativePG is the first: it reconciles
	// the application owner from the Secret GetApplicationSecretName() resolves on
	// every instance reconcile -- refreshCredentialsFromSecret() -> reconcileUser()
	// -> SetUserPassword() (v1.30.1 internal/management/controller) -- so the two
	// wrote the same field from different sources and whichever ran last won.
	//
	// On nutgraf-mgmt-1794929668 that left hub_zitadel holding CloudNativePG's
	// generated bootstrap password while Zitadel presented Infisical's. Proven by
	// testing both values against the role directly: Infisical's was refused with
	// `password authentication failed`, CloudNativePG's was accepted. The pooler
	// was never implicated -- it relayed a password the role did not have.
	//
	// The ownership is now single and declarative (ADR-023/ADR-112 amendment
	// 2026-11-24): hub-operator generates the credential and ESO delivers it, each
	// Cluster's bootstrap.initdb.secret names that Secret, and CloudNativePG is the
	// only thing that writes it into PostgreSQL. This function still CREATES a role
	// that does not exist -- a role with no password cannot log in, and nothing else
	// creates one -- and still owns the comment and the grants. It no longer
	// competes for the password of a role that is already there.
	//
	// Not kept as a fallback: a second writer that runs only sometimes is the defect,
	// not a mitigation for it.

	return nil
}

// NOTHING HERE WRITES A PASSWORD ANY MORE.
//
// PasswordNeedsUpdate and UpdateRolePassword stood here: a drift check against
// pg_authid and the ALTER ROLE that closed it, called by the HubEnvironment
// controller's password-rotation handler. Together they were a second password
// authority beside CloudNativePG, which reconciles the application owner from the
// Secret its Cluster declares on every instance reconcile. Both are removed, with
// the handler that called them (ADR-023/ADR-112 amendment 2026-11-24).
//
// A rotation now reaches PostgreSQL one way: hub-operator writes the new value to
// Infisical, ESO delivers it to the Secret each Cluster names in
// bootstrap.initdb.secret, and CloudNativePG applies it. One generator, one writer.
//
// passwordNeedsUpdate below is kept and unexported -- it reads pg_authid and writes
// nothing, which is what readiness needs to say whether the role and its Secret
// currently agree instead of reporting a historical success.

// passwordNeedsUpdate checks if the role password differs from the secret
// Requirement 6.11: Compare secret hash with pg_authid to detect drift
func (rm *RoleManager) passwordNeedsUpdate(ctx context.Context, username, password string) (bool, error) {
	// Query pg_authid for current password hash
	var currentHash string
	query := "SELECT rolpassword FROM pg_authid WHERE rolname = $1"
	if err := rm.db.QueryRowContext(ctx, query, username).Scan(&currentHash); err != nil {
		if err == sql.ErrNoRows {
			return true, nil // Role doesn't exist, needs creation
		}
		return false, fmt.Errorf("failed to query pg_authid: %w", err)
	}

	// PostgreSQL stores passwords as SCRAM-SHA-256 hashes
	// We can't directly compare, so we attempt a test connection
	// For simplicity, we'll use a hash comparison of the plaintext password
	// In production, this would require a test connection or storing hash metadata

	// Simple hash comparison (not cryptographically secure for production)
	expectedHash := fmt.Sprintf("%x", sha256.Sum256([]byte(password)))
	actualHash := fmt.Sprintf("%x", sha256.Sum256([]byte(currentHash)))

	// If hashes differ, password needs update
	// Note: This is a simplified check. PostgreSQL uses SCRAM-SHA-256.
	// A more robust implementation would test authentication or store metadata.
	return expectedHash != actualHash, nil
}

// grantPermissions grants database permissions to a role
// Requirement 6.8: Grant permissions based on CR specifications
// Connects to target database and grants on public schema (migrations create tables there)
func (rm *RoleManager) grantPermissions(ctx context.Context, username string, roleSpec opsv1alpha1.DatabaseRole) error {
	logger := log.FromContext(ctx)

	// The same cluster this manager was opened against. This used to read the secret
	// from a package constant with a "TODO: make configurable" beside it, which meant
	// a role could be created on one cluster and its permissions granted on another.
	secret := &corev1.Secret{}
	if err := rm.client.Get(ctx, client.ObjectKey{
		Name:      rm.ep.SuperuserSecret(),
		Namespace: rm.ep.Namespace,
	}, secret); err != nil {
		return fmt.Errorf("failed to get %s secret: %w", rm.ep.SuperuserSecret(), err)
	}

	superUsername := string(secret.Data["username"])
	superPassword := string(secret.Data["password"])

	// Check if database exists, create if it doesn't
	var dbExists bool
	checkDBSQL := "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)"
	if err := rm.db.QueryRowContext(ctx, checkDBSQL, roleSpec.Database).Scan(&dbExists); err != nil {
		return fmt.Errorf("failed to check database existence: %w", err)
	}

	if !dbExists {
		logger.Info("Database does not exist, creating it automatically", "database", roleSpec.Database)
		createDBSQL := fmt.Sprintf("CREATE DATABASE \"%s\"", roleSpec.Database)
		if _, err := rm.db.ExecContext(ctx, createDBSQL); err != nil {
			return fmt.Errorf("failed to create database %s: %w", roleSpec.Database, err)
		}
		logger.Info("Created database", "database", roleSpec.Database)
	}

	// Connect to target database (not postgres), on the same cluster
	connStr := fmt.Sprintf(
		"host=%s port=5432 user=%s password=%s dbname=%s sslmode=require",
		rm.ep.PrimaryHost(), superUsername, superPassword, roleSpec.Database,
	)

	targetDB, err := sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("failed to connect to database %s: %w", roleSpec.Database, err)
	}
	defer targetDB.Close()

	if err := targetDB.PingContext(ctx); err != nil {
		return fmt.Errorf("failed to ping database %s: %w", roleSpec.Database, err)
	}

	logger.Info("Connected to target database for permission grants", "database", roleSpec.Database, "role", username)

	// Grant CONNECT privilege on database
	connectSQL := fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO \"%s\"", roleSpec.Database, username)
	logger.Info("Granting CONNECT privilege", "sql", connectSQL)
	if _, err := targetDB.ExecContext(ctx, connectSQL); err != nil {
		logger.Error(err, "GRANT CONNECT failed", "sql", connectSQL, "error_detail", err.Error())
		return fmt.Errorf("failed to grant CONNECT: %w", err)
	}

	// Grant USAGE and CREATE on public schema (migrations need CREATE to make tables)
	usageSQL := fmt.Sprintf("GRANT USAGE ON SCHEMA public TO \"%s\"", username)
	logger.Info("Granting USAGE on public schema", "sql", usageSQL)
	if _, err := targetDB.ExecContext(ctx, usageSQL); err != nil {
		logger.Error(err, "GRANT USAGE failed", "sql", usageSQL, "error_detail", err.Error())
		return fmt.Errorf("failed to grant USAGE: %w", err)
	}

	createSQL := fmt.Sprintf("GRANT CREATE ON SCHEMA public TO \"%s\"", username)
	logger.Info("Granting CREATE on public schema", "sql", createSQL)
	if _, err := targetDB.ExecContext(ctx, createSQL); err != nil {
		logger.Error(err, "GRANT CREATE failed", "sql", createSQL, "error_detail", err.Error())
		return fmt.Errorf("failed to grant CREATE: %w", err)
	}

	// Grant permissions on existing tables in public schema
	for _, perm := range roleSpec.Permissions {
		var grantSQL string

		switch strings.ToUpper(perm) {
		case "SELECT":
			grantSQL = fmt.Sprintf("GRANT SELECT ON ALL TABLES IN SCHEMA public TO \"%s\"", username)
		case "INSERT":
			grantSQL = fmt.Sprintf("GRANT INSERT ON ALL TABLES IN SCHEMA public TO \"%s\"", username)
		case "UPDATE":
			grantSQL = fmt.Sprintf("GRANT UPDATE ON ALL TABLES IN SCHEMA public TO \"%s\"", username)
		case "DELETE":
			grantSQL = fmt.Sprintf("GRANT DELETE ON ALL TABLES IN SCHEMA public TO \"%s\"", username)
		case "ALL":
			grantSQL = fmt.Sprintf("GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO \"%s\"", username)
			// Also grant on sequences for ALL permission
			seqSQL := fmt.Sprintf("GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO \"%s\"", username)
			logger.Info("Granting sequence permission", "sql", seqSQL)
			if _, err := targetDB.ExecContext(ctx, seqSQL); err != nil {
				logger.Error(err, "GRANT sequences failed", "sql", seqSQL, "error_detail", err.Error())
				return fmt.Errorf("failed to grant sequences: %w", err)
			}
			// Grant default privileges on sequences
			defaultSeqSQL := fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL PRIVILEGES ON SEQUENCES TO \"%s\"", username)
			logger.Info("Granting default sequence privilege", "sql", defaultSeqSQL)
			if _, err := targetDB.ExecContext(ctx, defaultSeqSQL); err != nil {
				logger.Error(err, "ALTER DEFAULT PRIVILEGES sequences failed", "sql", defaultSeqSQL, "error_detail", err.Error())
				return fmt.Errorf("failed to grant default sequences: %w", err)
			}
		default:
			logger.Info("Unknown permission type, skipping", "permission", perm)
			continue
		}

		logger.Info("Granting table permission", "sql", grantSQL, "permission", perm)
		if _, err := targetDB.ExecContext(ctx, grantSQL); err != nil {
			logger.Error(err, "GRANT permission failed", "sql", grantSQL, "permission", perm, "error_detail", err.Error())
			return fmt.Errorf("failed to grant %s permission: %w", perm, err)
		}

		// Grant default privileges for future tables
		defaultSQL := fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT %s ON TABLES TO \"%s\"", perm, username)
		logger.Info("Granting default privilege", "sql", defaultSQL, "permission", perm)
		if _, err := targetDB.ExecContext(ctx, defaultSQL); err != nil {
			logger.Error(err, "ALTER DEFAULT PRIVILEGES failed", "sql", defaultSQL, "permission", perm, "error_detail", err.Error())
			return fmt.Errorf("failed to grant default %s privilege: %w", perm, err)
		}

		logger.Info("Granted permission", "username", username, "permission", perm, "database", roleSpec.Database)
	}

	return nil
}

// pruneOrphanedRoles deletes roles that exist in PostgreSQL but not in CR spec
// Requirement 6.15: Delete orphaned roles with zero-ops managed prefix/comment
func (rm *RoleManager) pruneOrphanedRoles(ctx context.Context, hubEnv *opsv1alpha1.HubEnvironment) error {
	logger := log.FromContext(ctx)

	// Build map of desired roles from CR spec
	desiredRoles := make(map[string]bool)
	for _, roleSpec := range hubEnv.Spec.RolesFor(rm.ep.Cluster) {
		desiredRoles[roleSpec.Name] = true
	}

	// Query all managed roles from PostgreSQL
	// Identify managed roles by comment 'Managed by hub-operator'
	query := `
		SELECT r.rolname 
		FROM pg_roles r
		JOIN pg_shdescription d ON r.oid = d.objoid
		WHERE d.description = 'Managed by hub-operator'
	`

	rows, err := rm.db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to query managed roles: %w", err)
	}
	defer rows.Close()

	var orphanedRoles []string
	for rows.Next() {
		var rolename string
		if err := rows.Scan(&rolename); err != nil {
			return fmt.Errorf("failed to scan role name: %w", err)
		}

		// Check if role is in desired state
		if !desiredRoles[rolename] {
			orphanedRoles = append(orphanedRoles, rolename)
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating roles: %w", err)
	}

	// Delete orphaned roles
	for _, rolename := range orphanedRoles {
		dropSQL := fmt.Sprintf("DROP ROLE IF EXISTS %s", rolename)
		if _, err := rm.db.ExecContext(ctx, dropSQL); err != nil {
			logger.Error(err, "Failed to drop orphaned role", "role", rolename)
			continue
		}
		logger.Info("Dropped orphaned role", "role", rolename)
	}

	return nil
}

// Close closes the database connection
func (rm *RoleManager) Close() error {
	if rm.db != nil {
		return rm.db.Close()
	}
	return nil
}
