package components

import (
	"context"
	"fmt"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/infisical"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// infisicalAdministrator returns the administrator of this box's own Infisical,
// read from the escrow that holds it.
//
// One source, and it can only be this one. The credential is generated once when
// the box is initialized and written to the tenant's escrow under
// infisical-admin-credential; every bootstrap, retry and reconstruction reads
// back that exact value (ADR-076). It used to be delivered as two repository
// secrets as well, which could not work as a source of truth: a GitHub secret
// cannot be read back, so a copy there that drifted from the escrowed one was
// undetectable, and the drift surfaced as a Day-0 unable to log in to an
// Infisical this box had already bootstrapped.
//
// Fails closed, in every direction. An unreachable escrow, an absent artefact or
// a half-populated one is a refusal -- never a generated password, never a read
// from the environment, never a default administrator. Regenerating would be the
// worst of those: the running instance was bootstrapped with a password that
// already exists, a new one would not log in to it, and writing it would destroy
// the one a successor needs.
func (i *Installer) infisicalAdministrator(ctx context.Context) (infisical.Administrator, error) {
	var none infisical.Administrator
	if strings.TrimSpace(i.GitopsDir) == "" {
		return none, fmt.Errorf("no tenant checkout to read this box's escrow coordinates from.\n\n" +
			"The Infisical administrator is held in the box's escrow, and the box declares\n" +
			"where that escrow is in registry/box.yaml. Set the installer's GitopsDir.")
	}
	// Declaration carries the escrow's coordinates as the BOX declares them, and
	// Open verifies the project is bound to this box before reading from it -- so
	// a repository pointed at another box's escrow is refused rather than read.
	coords, err := hubaccess.Declaration(i.GitopsDir, i.ClusterName)
	if err != nil {
		return none, err
	}
	access, err := hubaccess.Open(ctx, coords)
	if err != nil {
		return none, err
	}

	// The box on both halves: this administrator belongs to the box's Infisical,
	// which outlives any single hub incarnation, so a successor hub must find the
	// same one (ADR-111 Section 1).
	id := hubaccess.Identity{BoxID: coords.BoxID, ClusterName: coords.BoxID}
	payload, err := access.Artifact(ctx, id, escrow.ArtifactInfisicalAdmin)
	if err != nil {
		// Unreadable is not absent, and neither is recoverable here.
		return none, fmt.Errorf("reading the escrowed Infisical administrator for box %s: %w",
			coords.BoxID, err)
	}
	cred, err := escrow.RequireAdminCredential(coords.BoxID, payload)
	if err != nil {
		return none, err
	}

	admin := infisical.Administrator{Email: cred.Email, Password: cred.Password}
	if err := admin.Validate(); err != nil {
		return none, err
	}
	// The address, never the password: which account is being used is the useful
	// fact in a bootstrap log, and the other half is not.
	fmt.Printf("[bootstrap-secrets] Infisical administrator read from the escrow of box %s (%s)\n",
		coords.BoxID, admin.Email)
	return admin, nil
}
