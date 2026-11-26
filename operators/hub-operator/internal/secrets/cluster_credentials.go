package secrets

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/openbao"
)

// Cluster-scoped credential materialisation (ADR-031 Cell-Based Identity
// Topology, whose "cell" is one spoke).
//
// Named for the cluster, not the cell: the value scoping these paths has always
// been one spoke's own name. A cell is a policy group -- `cells` in a cluster's
// values.yaml, several per cluster, each contributing policy (ADR-064) -- and
// never scoped a credential.
//
// A spoke's SecretStore is authorised for /spoke-pool/<clusterName>/ ONLY, so every
// spoke ExternalSecret must resolve inside that prefix. Two classes of credential
// were consumed there with nothing producing them, which is not a seeding gap —
// the material exists at the root path and the hub reads it fine — but a missing
// producer for the per-cluster path:
//
//   - S3_* and the alert-delivery keys are single fleet credentials that must be
//     materialised into each cluster's own path. The alternative — granting each
//     cluster a cross-cluster read path to the root — would weaken the ADR-031
//     invariant that a cell can read only its own prefix. Duplication is the
//     deliberate trade, so it needs a producer that performs it.
//
// Without these, platform-db archived zero WAL since creation: Barman had no
// credentials at all.
//
// The other cell-scoped credential, AGENTGATEWAY_OIDC_COOKIE_SECRET, is NOT
// produced here. It is generated, not copied, so it belongs with the other
// generated application secrets: see ClusterScopedKey in
// internal/infisical/secret_mappings.go. Producing it from this reconciler raced
// the folder this same reconciler creates, which is why it never appeared in the
// cell path.

// FleetCredentialKeys are credentials issued OUTSIDE the platform, seeded once at
// the Infisical root, and copied verbatim into each cell. They are never generated
// here: a regenerated value would not match the external system it authenticates to.
//
// Keep in step with the EXTERNAL set in
// scripts/validate/preflight/95-infisical-key-producers.sh.
// Lifecycle says what happens to a cell's copy when the authority changes.
//
// The distinction is the whole reason this is a table rather than a list. A
// credential the box owns centrally must CONVERGE when it is rotated, or every
// cluster keeps authenticating with a value that has been replaced. A credential
// a cell may legitimately hold its own version of must NOT be overwritten, or the
// operator fights a deliberate per-cell override on every reconcile.
type Lifecycle int

const (
	// CopiedOnce is written when absent and never touched again.
	CopiedOnce Lifecycle = iota
	// CentrallyRotated is converged on the authority whenever the two differ.
	CentrallyRotated
)

// FleetCredential is one credential a cell receives, and its lifecycle.
type FleetCredential struct {
	Key       string
	Lifecycle Lifecycle
	Why       string

	// SourceKey is where the value lives at the authority when that is not Key.
	// One %s receives the cluster's name.
	//
	// Needed because a per-cluster value can be named per cluster at the authority
	// and must not be in the destination: the project IS the cluster now, so
	// repeating its name in the key says nothing and makes the consumer's path
	// depend on it twice.
	SourceKey string
}

// FleetCredentials are the credentials a workload cluster's own secrets project
// receives from the box's authority.
//
// EXPLICITLY CLASSIFIED, not the whole of CLISecretMappings. That registry also
// carries the Infisical database credentials, the GitHub repository tokens and
// the identity service's -- values whose blast radius is the box, not a cluster.
// Only what a cluster's own workloads must read crosses into a cluster's project.
var FleetCredentials = []FleetCredential{
	{Key: "S3_ACCESS_KEY_ID", Lifecycle: CopiedOnce,
		Why: "the object store the cell's database archives to; a cell may be pointed at its own bucket"},
	{Key: "S3_SECRET_ACCESS_KEY", Lifecycle: CopiedOnce,
		Why: "the other half of the same credential, and rotated with it rather than independently"},
	// ADR-078 add.10 §4: mail is NOT self-hosted, so every cluster delivers its own
	// alerts by email with the box's credential. SMTP is the transport alerts leave
	// by, not monitoring infrastructure, so add.10 §1's rule does not reach it.
	{Key: "ALERT_SMTP_PASSWORD", Lifecycle: CopiedOnce,
		Why: "outbound mail for this cluster's alerts, which every cluster sends for itself"},
	// GRAFANA_IRM_ALERTMANAGER_URL AND GRAFANA_IRM_HEARTBEAT_URL ARE THE HUB'S.
	//
	// Both were here. Copying the box owner's IRM integration into every cluster is
	// what add.7 warned about in one direction -- one integration receiving every
	// cluster's heartbeat, so a dead cluster looks alive -- and what add.10 §1
	// forbids outright in the other: a spoke uses no third-party cloud stack, for
	// the same reason the box self-hosts Infisical and rents only the escrow.
	//
	// A spoke pages nobody through Grafana IRM. It delivers its own alerts by email
	// (above), and its silence is observed by the hub over its VMAuth read route
	// (add.10 §5), not by a cloud endpoint it would have to be given.
	// The registry credential is NOT copied to every cluster. It goes into the
	// OpenBao scope of each application that declares private images, and only
	// there (EnsureTenantFolderAndCredentials).
	{Key: "hcloud-token", Lifecycle: CentrallyRotated,
		Why: "provider-owned DNS, for the cell's cert-manager DNS01 solver and external-dns. " +
			"One account credential for the box, so a rotation converges"},
	// Generated once per cluster by EnsureCrossplaneAdminPassword, which keeps
	// writing it to the authority's root as <cluster>-crossplane-admin-password.
	// That stays the authority; this copies it into the cluster's own project,
	// which is the only project the cluster's identity can now read.
	//
	// COPIEDONCE IS THE WHOLE POINT. This password is the spoke's CNPG
	// crossplane-admin role: regenerating or converging it desyncs a live database
	// role, and EnsureCrossplaneAdminPassword treats absence as first-time and
	// GENERATES. Copying the existing value is what keeps that from happening.
}

// NOTHING IS SEEDED PER CLUSTER. There used to be a list here.
//
// ExternalPerCellCredentialKeys declared GRAFANA_IRM_HEARTBEAT_URL as a credential
// the box owner creates per cell in the incident-response tool and seeds by hand
// (ADR-078 add.7, 2026-10-30). Removed by add.10: a spoke calls no third-party
// cloud endpoint, so there is no per-cell heartbeat integration to create, and
// creating a managed cluster asks the box owner for nothing a second time.
//
// A spoke's silence is observed by the HUB, over the read route add.8 gives it: the
// spoke publishes its liveness as a small recording-rule series and a hub-side rule
// pages when that series goes absent (add.10 §5). The observer is still outside the
// cluster it watches, which is the property a dead-man's switch needs; it is simply
// the hub rather than a rented endpoint.
//
// The hub's own heartbeat still goes to Grafana IRM, because nothing inside the box
// can witness the whole box going quiet. That is the same carve-out the escrow has:
// one cloud dependency, at the hub, for the case the box cannot cover itself.

// MaterialiseFleetCredentialsResult reports per-key outcomes so a partially seeded
// fleet is visible rather than reduced to a single pass/fail.
type MaterialiseFleetCredentialsResult struct {
	Created  []string
	Existing []string
	// Converged are centrally-owned keys whose cell copy was brought back onto the
	// authority because the two differed -- a rotation reaching this cluster.
	Converged []string
	// MissingAtRoot are keys absent from the Infisical root, i.e. never seeded at
	// Day-0. They are reported, not fabricated: inventing a value for
	// an externally-issued credential yields a secret that authenticates to
	// nothing and fails later, further from the cause.
	MissingAtRoot []string
}

// EnsureFleetCredentialsMaterialised copies fleet credentials from the Infisical
// root into the cell's shared path so a cell-scoped SecretStore can resolve them
// without a cross-cell read grant.
//
// Values are copied, never generated. A key absent at the root is recorded in
// MissingAtRoot and skipped — the operator cannot invent an external credential,
// and failing the whole reconcile would block a spoke over an optional one
// (IRM alert delivery) as readily as a required one (S3).
//
// Existing cell values are left alone. Overwriting on every reconcile would make
// the operator fight any deliberate per-cell override, and rotation at the root is
// a fleet-wide operation that should propagate explicitly, not as a side effect.
func (c *Store) MaterialiseFleetCredentials(ctx context.Context, clusterName string) (*MaterialiseFleetCredentialsResult, error) {
	dest := c
	logger := log.FromContext(ctx).WithValues("cluster", clusterName)
	sharedPath := openbao.SpokePlatformScope(clusterName)
	out := &MaterialiseFleetCredentialsResult{}

	for _, cred := range FleetCredentials {
		key := cred.Key
		sourceKey := key
		if cred.SourceKey != "" {
			sourceKey = fmt.Sprintf(cred.SourceKey, clusterName)
		}

		// The authority first: a cell's copy is only as good as the value it came
		// from, and a credential absent at the source cannot be converged to.
		value, err := c.GetSecret(ctx, HubScope, sourceKey)
		if err != nil || value == "" {
			out.MissingAtRoot = append(out.MissingAtRoot, sourceKey)
			continue
		}

		exists, err := dest.SecretExists(ctx, sharedPath, key)
		if err != nil {
			return nil, fmt.Errorf("failed to check %s at %s: %w", key, sharedPath, err)
		}

		if !exists {
			if err := dest.CreateSecret(ctx, sharedPath, key, value); err != nil {
				return nil, fmt.Errorf("failed to materialise %s into %s: %w", key, sharedPath, err)
			}
			out.Created = append(out.Created, key)
			continue
		}

		if cred.Lifecycle == CopiedOnce {
			// Left alone deliberately: this cell may hold its own version, and
			// overwriting it every reconcile would fight that.
			out.Existing = append(out.Existing, key)
			continue
		}

		// CentrallyRotated: converge, and only when the values actually differ, so
		// an unchanged authority writes nothing.
		current, err := dest.GetSecret(ctx, sharedPath, key)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s at %s before converging it: %w", key, sharedPath, err)
		}
		if current == value {
			out.Existing = append(out.Existing, key)
			continue
		}
		if err := dest.UpdateSecret(ctx, sharedPath, key, value); err != nil {
			return nil, fmt.Errorf("failed to converge %s at %s onto its authority: %w", key, sharedPath, err)
		}
		out.Converged = append(out.Converged, key)
	}

	logger.Info("Fleet credentials materialised into the cluster's own scope",
		"path", sharedPath,
		"created", out.Created,
		"converged", out.Converged,
		"existing", len(out.Existing),
		"missingAtRoot", out.MissingAtRoot)
	return out, nil
}
