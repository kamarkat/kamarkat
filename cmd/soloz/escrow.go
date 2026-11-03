package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/soloz-io/zero-ops/internal/platform/escrow"
	"github.com/spf13/cobra"
)

var (
	escrowURL          string
	escrowClientID     string
	escrowClientSecret string
	escrowProjectName  string
	escrowOut          string
	escrowOwnerEmail   string
)

// newEscrowCmd creates this tenant's escrow project.
//
// The escrow account is the tenant's and the platform holds no credential to it
// beyond what the tenant supplies (ADR-076), so creating the ACCOUNT and its
// first machine identity stays manual. Everything after that does not need to
// be, and was: the project, the identity's access to it, and recording the id.
//
// Two of those three are silent to get wrong. An Infisical project has a TYPE
// fixed at creation, and only a secret-manager project accepts what an escrow
// does -- a kms project authenticates perfectly and refuses every read and
// write. An identity that is not a member of the project fails differently and
// just as quietly. Both were wrong on the box that found this, and neither
// surfaced until a bootstrap stalled forty minutes in on database roles that
// could not be created.
func newEscrowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "escrow",
		Short: "Manage this tenant's escrow",
	}
	cmd.AddCommand(newEscrowInitCmd())
	cmd.AddCommand(newEscrowVerifyCmd())
	cmd.AddCommand(newEscrowMigratePathsCmd())
	cmd.AddCommand(newEscrowDeliverBackupCredentialsCmd())
	cmd.AddCommand(newEscrowEnrollStoreCredentialCmd())
	cmd.AddCommand(newEscrowVerifyRecoveryCmd())
	return cmd
}

func newEscrowInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create the escrow project this tenant's boxes back up to",
		Long: `Create the Secret Manager project a box escrows its master keys to.

You create the Infisical account and one machine identity; this creates the
project, and Infisical adds the creating identity to it as an admin, so the
access it needs comes with it.

The project id is written to the file the rest of the tooling already reads, so
nothing else changes: same directory, same names, one fewer value to supply and
one fewer thing to get wrong.`,
		RunE: runEscrowInit,
	}
	f := cmd.Flags()
	f.StringVar(&escrowURL, "url", "https://app.infisical.com", "the escrow Infisical")
	f.StringVar(&escrowClientID, "client-id", "", "machine identity client id (required)")
	f.StringVar(&escrowClientSecret, "client-secret", "", "machine identity client secret (required)")
	f.StringVar(&escrowProjectName, "name", "", "project name (required)")
	f.StringVar(&escrowOut, "out", "k8-secrets/infisical",
		"directory holding INFISICAL_ESCROW_* files")
	// The person who must be able to open the escrow. Infisical adds only the
	// creating actor to a project, so a project created by a machine identity has
	// no human members and does not appear in anyone's project list -- an escrow
	// only a machine can open is one more credential to lose.
	f.StringVar(&escrowOwnerEmail, "owner-email", "",
		"the Infisical account that should own this project (required)")

	for _, required := range []string{"client-id", "client-secret", "name", "owner-email"} {
		_ = cmd.MarkFlagRequired(required)
	}
	return cmd
}

func runEscrowInit(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	// Refused, not overwritten. A project id already on disk may name a project
	// holding the only copy of a running box's master keys, and replacing it
	// would leave those keys reachable by nothing -- the exact loss the escrow
	// exists to prevent. Removing the file is a deliberate act and stays one.
	idPath := filepath.Join(escrowOut, "INFISICAL_ESCROW_PROJECT_ID")
	if existing, err := os.ReadFile(idPath); err == nil && strings.TrimSpace(string(existing)) != "" {
		return fmt.Errorf("%s already names project %s.\n\n"+
			"Creating another would leave whatever that one holds reachable by nothing.\n"+
			"If it is wrong, verify no box depends on it and remove the file first.",
			idPath, strings.TrimSpace(string(existing)))
	}

	projectID, err := escrow.EnsureProject(ctx, escrowURL, escrowClientID, escrowClientSecret,
		escrowProjectName, escrowOwnerEmail)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(escrowOut, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", escrowOut, err)
	}
	// The three values together, so the directory describes one escrow rather
	// than a project id beside credentials for somewhere else.
	for name, value := range map[string]string{
		"INFISICAL_ESCROW_URL":           escrowURL,
		"INFISICAL_ESCROW_CLIENT_ID":     escrowClientID,
		"INFISICAL_ESCROW_CLIENT_SECRET": escrowClientSecret,
		"INFISICAL_ESCROW_PROJECT_ID":    projectID,
	} {
		p := filepath.Join(escrowOut, name)
		if err := os.WriteFile(p, []byte(value), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
	}

	fmt.Printf("✓ escrow project %q created: %s\n", escrowProjectName, projectID)
	fmt.Printf("  written to %s/\n", escrowOut)
	fmt.Printf("  %s can open it at %s\n", escrowOwnerEmail, escrowURL)
	fmt.Println("  Scaffolding reads these; nothing else to supply.")
	return nil
}
