package openbao

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Hub application login roles (ADR-117 §7.3). CloudNativePG owns each role's
// existence and membership (DatabaseRole, no passwordSecret); OpenBao owns its
// password through a static role that authenticates AS that role, by certificate,
// to the postgres maintenance database only, and rotates its own password.
const (
	// StaticRotationPeriod is the initial policy, not an invariant (§7.5).
	StaticRotationPeriod = "720h"
	// certRoot is where values.yaml mounts each database's client cert and CA.
	certRoot = "/openbao/db"
)

// StaticRoleName is the static role delivering hub_<app>'s password; ESO reads
// database/static-creds/<name>.
func StaticRoleName(username string) string {
	return strings.ReplaceAll(username, "_", "-")
}

// ConnectionURL is the self-rotation connection for one login role.
func ConnectionURL(cluster, namespace, username string) string {
	dir := certRoot + "/" + cluster
	return fmt.Sprintf("postgres://%s@%s-rw.%s.svc:5432/postgres?sslmode=verify-full"+
		"&sslrootcert=%s/ca/ca.crt&sslcert=%s/client/tls.crt&sslkey=%s/client/tls.key",
		username, cluster, namespace, dir, dir, dir)
}

// EnsureStaticRole converges database/config/<db> and the static role for username.
// OpenBao verifies the connection when the config is written, so an error here
// usually means the role, its certificate or pg_hba is not in place yet.
func (c *Client) EnsureStaticRole(ctx context.Context, db, cluster, namespace, username string) error {
	role := StaticRoleName(username)
	if err := c.Write(ctx, "database/config/"+db, map[string]any{
		"plugin_name":       "postgresql-database-plugin",
		"connection_url":    ConnectionURL(cluster, namespace, username),
		"allowed_roles":     []string{role},
		"verify_connection": true,
		// Passwords are hashed before they are sent in ALTER ROLE, so the
		// plaintext never appears in PostgreSQL's statement logs.
		"password_authentication": "scram-sha-256",
	}, nil); err != nil {
		return fmt.Errorf("configure database/config/%s: %w", db, err)
	}
	// Creating the role rotates it once; rewriting an existing one only
	// reconciles its period, never forces a rotation.
	return c.Write(ctx, "database/static-roles/"+role, map[string]any{
		"db_name": db, "username": username, "rotation_period": StaticRotationPeriod,
	}, nil)
}

// StaticRoleRotated reports whether the role has been rotated at least once since
// `since`, i.e. OpenBao holds a password it set itself.
func (c *Client) StaticRoleRotated(ctx context.Context, username string, since time.Time) (bool, error) {
	r, err := c.ReadStaticRole(ctx, StaticRoleName(username))
	if err != nil {
		return false, err
	}
	if r.LastVaultRotation == "" {
		return false, nil
	}
	t, err := time.Parse(time.RFC3339Nano, r.LastVaultRotation)
	if err != nil {
		return false, fmt.Errorf("unreadable last_vault_rotation %q: %w", r.LastVaultRotation, err)
	}
	return !t.Before(since), nil
}
