package secrets

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"
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
var FleetCredentialKeys = []string{
	"S3_ACCESS_KEY_ID",
	"S3_SECRET_ACCESS_KEY",
	// ADR-078 add.5 §5: each spoke's Alertmanager delivers mail and IRM alerts
	// with the box's credentials. Its heartbeat URL is per cell, seeded per cell.
	"ALERT_SMTP_PASSWORD",
	"GRAFANA_IRM_ALERTMANAGER_URL",
}

// ExternalPerCellCredentialKeys are per-cell credentials the platform does not and
// must not generate: each cell has its OWN value, issued by an external system, and
// copying the hub's into every cell would point every cell's alerts at one
// integration (ADR-078 add.5 §5, 2026-10-30).
//
// Declared here so the producer check can tell "externally provisioned per cell"
// apart from "nothing produces this", and so that a key added to a consumer without
// a seeding contract still fails. Declaration is NOT evidence the value exists: that
// is verified live, per cell, by scripts/validate/cluster/078-alert-credentials.sh,
// because a credential that is merely declared is a credential that silently is not
// there.
//
//	key                         source                                   seeded by
//	GRAFANA_IRM_HEARTBEAT_URL   one heartbeat integration per cell,       the box owner,
//	                            created in the incident-response tool;    into
//	                            each cell needs its own, so this is       /spoke-pool/<cell>/shared/
//	                            never derived from the hub's
var ExternalPerCellCredentialKeys = []string{
	"GRAFANA_IRM_HEARTBEAT_URL",
}

// MaterialiseFleetCredentialsResult reports per-key outcomes so a partially seeded
// fleet is visible rather than reduced to a single pass/fail.
type MaterialiseFleetCredentialsResult struct {
	Created  []string
	Existing []string
	// MissingAtRoot are keys absent from the Infisical root, i.e. never seeded
	// from k8-secrets/. They are reported, not fabricated: inventing a value for
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
func (c *InfisicalClient) EnsureFleetCredentialsMaterialised(ctx context.Context, clusterName string) (*MaterialiseFleetCredentialsResult, error) {
	logger := log.FromContext(ctx).WithValues("cluster", clusterName)
	sharedPath := fmt.Sprintf(InfisicalSharedPathFormat, clusterName)
	out := &MaterialiseFleetCredentialsResult{}

	if err := c.EnsureFolder(ctx, sharedPath); err != nil {
		return nil, fmt.Errorf("failed to ensure shared folder %s: %w", sharedPath, err)
	}

	for _, key := range FleetCredentialKeys {
		exists, err := c.SecretExists(ctx, sharedPath, key)
		if err != nil {
			return nil, fmt.Errorf("failed to check %s at %s: %w", key, sharedPath, err)
		}
		if exists {
			out.Existing = append(out.Existing, key)
			continue
		}

		value, err := c.GetSecret(ctx, "/", key)
		if err != nil || value == "" {
			out.MissingAtRoot = append(out.MissingAtRoot, key)
			continue
		}

		if err := c.CreateSecret(ctx, sharedPath, key, value); err != nil {
			return nil, fmt.Errorf("failed to materialise %s into %s: %w", key, sharedPath, err)
		}
		out.Created = append(out.Created, key)
	}

	logger.Info("Fleet credentials materialised into cell path",
		"path", sharedPath,
		"created", len(out.Created),
		"existing", len(out.Existing),
		"missingAtRoot", out.MissingAtRoot)
	return out, nil
}
