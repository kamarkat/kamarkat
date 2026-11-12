package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/session"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
	"github.com/spf13/cobra"
)

var (
	escrowURL          string
	escrowClientID     string
	escrowClientSecret string
	escrowProjectName  string
	escrowGitops       string
	escrowCluster      string
	escrowOut          string
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
	cmd.AddCommand(newEscrowEnrollProviderCredentialCmd())
	cmd.AddCommand(newEscrowBindDestinationCmd())
	cmd.AddCommand(newEscrowVerifyRecoveryCmd())
	return cmd
}

func newEscrowInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create the escrow project this tenant's boxes back up to",
		Long: `Create the Secret Manager project a box escrows its master keys to.

Run it after ` + "`kakt login`" + `, as the administrator. It creates the project as YOU,
so you are its admin, then makes sure the organisation's automation identity
exists, can authenticate, and is a member of the project.

Nothing to do in Infisical's dashboard, and no roles to choose. The identity is
created with the organisation role ` + "`no-access`" + ` -- it performs no organisation
action -- and given the project role ` + "`member`" + `, which is what reading and writing
secrets and creating folders needs. Not admin: that would add membership and
role management an automation credential has no use for.

ONE IDENTITY PER ORGANISATION. A second box reuses the first box's identity and
is added to its own project, because the free plan allows five identities in
total and the escrow paths are already box-scoped. A re-run finds everything and
writes nothing.

It needs an ORGANISATION ADMIN. A member can create an identity and cannot give
it an authentication method, so the identity would exist and be unusable; this
refuses instead of leaving one behind.

Pass --client-id and --client-secret to use an identity you manage yourself. The
project is still created by you, and that identity is added to it.`,
		RunE: runEscrowInit,
	}
	f := cmd.Flags()
	f.StringVar(&escrowURL, "url", "https://app.infisical.com", "the escrow Infisical")
	f.StringVar(&escrowProjectName, "name", "", "project name (required)")
	f.StringVar(&escrowOut, "out", "",
		"write the INFISICAL_ESCROW_* files here instead of this operator's own store "+
			"(~/.kamarkat/infisical, or $KAMARKAT_HOME/infisical)")
	// The escape hatch, and nothing more. An organisation whose policy forbids
	// the CLI creating identities supplies one; everyone else lets this manage
	// the single automation identity.
	f.StringVar(&escrowGitops, "gitops-dir", ".",
		"a box's repository. If it already declares an escrow, that project is prepared "+
			"rather than a second one created")
	f.StringVar(&escrowCluster, "cluster", "",
		"the management cluster whose declaration to read, when the repository holds more than one")
	f.StringVar(&escrowClientID, "client-id", "",
		"use this machine identity instead of the organisation's automation identity")
	f.StringVar(&escrowClientSecret, "client-secret", "",
		"its client secret, recorded for the box's automation to authenticate with")
	cmd.MarkFlagsRequiredTogether("client-id", "client-secret")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func runEscrowInit(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	// Refused, not overwritten. A project id already on disk may name a project
	// holding the only copy of a running box's master keys, and replacing it
	// would leave those keys reachable by nothing -- the exact loss the escrow
	// exists to prevent. Removing the file is a deliberate act and stays one.
	// A BOX THAT ALREADY HAS AN ESCROW IS NOT GIVEN A SECOND ONE.
	//
	// Run inside a box's repository, the declaration is authoritative: it names
	// the project holding that box's master keys, and creating another would
	// leave the real one untouched and unmentioned while this machine's settings
	// pointed at an empty replacement. That happened; this is the guard.
	//
	// Adopted rather than refused, because the work an existing box actually
	// needs is the rest of what this command does: the organisation's automation
	// identity, its membership of that project, and a client secret recorded
	// here.
	declared, declaredFor := declaredEscrow(escrowGitops, escrowCluster)

	guardDir := strings.TrimSpace(escrowOut)
	if guardDir == "" {
		d, err := escrow.StorePath()
		if err != nil {
			return err
		}
		guardDir = d
	}
	idPath := filepath.Join(guardDir, "INFISICAL_ESCROW_PROJECT_ID")
	if declared.EscrowProjectID != "" {
		// The declaration wins over whatever this machine last recorded: a stale
		// local value is corrected rather than treated as a conflict.
		if existing, readErr := os.ReadFile(idPath); readErr == nil {
			if had := strings.TrimSpace(string(existing)); had != "" && had != declared.EscrowProjectID {
				fmt.Printf("! %s named project %s, which box %s does not use.\n",
					idPath, had, declared.BoxID)
				fmt.Printf("  Correcting it to the project that box declares: %s\n\n",
					declared.EscrowProjectID)
			}
		}
	} else if existing, err := os.ReadFile(idPath); err == nil && strings.TrimSpace(string(existing)) != "" {
		return fmt.Errorf("%s already names project %s.\n\n"+
			"Creating another would leave whatever that one holds reachable by nothing.\n"+
			"If it is wrong, verify no box depends on it and remove the file first.",
			idPath, strings.TrimSpace(string(existing)))
	}

	// The administrator's own session. Every call this makes is authorized by
	// them, which is what removes the dashboard steps: there is no longer a
	// chicken-and-egg where a credential had to exist before the CLI could speak.
	s, err := session.Current(ctx, escrowURL)
	if err != nil {
		return fmt.Errorf("%w\n\nEscrow initialisation runs as you. Sign in first:\n\n  kakt login --domain %s", err, escrowURL)
	}
	if strings.TrimSpace(s.OrgID) == "" {
		return fmt.Errorf("this session names no organisation, so there is nowhere to create " +
			"the project. Run `kakt login` again")
	}

	var clientID, clientSecret string
	var p escrow.Provisioned
	if strings.TrimSpace(escrowClientID) != "" {
		// A supplied identity: the project is still created as the human, and
		// the identity is given membership of it.
		p, err = escrow.ProvisionEscrowWithIdentity(ctx, escrowURL, s.Token, s.OrgID,
			escrowProjectName, strings.TrimSpace(escrowClientID))
		clientID, clientSecret = strings.TrimSpace(escrowClientID), strings.TrimSpace(escrowClientSecret)
	} else if declared.EscrowProjectID != "" {
		// The box's own escrow, prepared rather than replaced.
		escrowURL = declared.EscrowURL
		p, err = escrow.ProvisionEscrowInProject(ctx, escrowURL, s.Token, s.OrgID,
			declared.EscrowProjectID, declaredFor)
		clientID, clientSecret = p.ClientID, p.ClientSecret
	} else {
		p, err = escrow.ProvisionEscrow(ctx, escrowURL, s.Token, s.OrgID, escrowProjectName)
		clientID, clientSecret = p.ClientID, p.ClientSecret
	}
	if err != nil {
		return err
	}
	projectID := p.ProjectID

	// The operator's own store unless they asked for somewhere else. Not a
	// checkout: these outlive any clone, and a client secret Infisical issues
	// once must not be deleted with one.
	out := strings.TrimSpace(escrowOut)
	if out == "" {
		d, serr := escrow.StorePath()
		if serr != nil {
			return serr
		}
		out = d
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", out, err)
	}
	// The values together, so the directory describes one escrow rather than a
	// project id beside credentials for somewhere else.
	//
	// The client secret is written only when this run issued one. For a reused
	// identity Infisical cannot return it -- it is readable once -- and the copy
	// a running box authenticates with is already in this directory. Overwriting
	// it with nothing would take that box's credential away.
	values := map[string]string{
		"INFISICAL_ESCROW_URL":        escrowURL,
		"INFISICAL_ESCROW_CLIENT_ID":  clientID,
		"INFISICAL_ESCROW_PROJECT_ID": projectID,
	}
	if strings.TrimSpace(clientSecret) != "" {
		values["INFISICAL_ESCROW_CLIENT_SECRET"] = clientSecret
	}
	for name, value := range values {
		pth := filepath.Join(out, name)
		if err := os.WriteFile(pth, []byte(value), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", pth, err)
		}
	}

	if declared.EscrowProjectID != "" {
		fmt.Printf("✓ box %s's escrow prepared: %s\n", declared.BoxID, projectID)
	} else {
		fmt.Printf("✓ escrow project %q created: %s\n", escrowProjectName, projectID)
	}
	fmt.Printf("  %s is its admin\n", s.Email)
	switch {
	case p.IdentityCreated:
		fmt.Printf("  automation identity %s created, with a client secret\n", escrow.AutomationIdentityName)
	case p.Reused:
		fmt.Printf("  automation identity %s reused, added to this project, and issued\n",
			escrow.AutomationIdentityName)
		fmt.Println("  this box its own client secret -- the identity's existing secrets are")
		fmt.Println("  untouched, so boxes already running keep authenticating with theirs")
	default:
		fmt.Println("  the identity you supplied was added to this project")
	}
	fmt.Printf("  written to %s/\n", out)
	if strings.TrimSpace(clientSecret) == "" {
		// Only reachable when an identity was supplied with --client-id and its
		// secret was not: the operator is managing that identity themselves, so
		// this does not issue one for it.
		secretPath := filepath.Join(out, "INFISICAL_ESCROW_CLIENT_SECRET")
		if _, statErr := os.Stat(secretPath); statErr != nil {
			fmt.Printf("\n! No client secret recorded for the identity you supplied.\n")
			fmt.Printf("  Pass --client-secret with it, or put the value in\n    %s\n", secretPath)
		}
	}
	return nil
}

// declaredEscrow reads the escrow a box in this repository already declares.
//
// Empty coordinates when there is no box here, which is the ordinary case for a
// first initialisation: the repository does not exist yet. The second return is
// the cluster the declaration belongs to, used to label the client secret this
// run issues.
func declaredEscrow(gitopsDir, cluster string) (hubaccess.Coordinates, string) {
	var none hubaccess.Coordinates
	if strings.TrimSpace(gitopsDir) == "" {
		return none, ""
	}
	name := strings.TrimSpace(cluster)
	if name == "" {
		box, err := tenant.DiscoverBox(gitopsDir)
		if err != nil || len(box.Management) != 1 {
			// No box here, or more than one management cluster and no --cluster
			// to choose. Either way there is nothing unambiguous to adopt.
			return none, ""
		}
		name = box.Management[0]
	}
	coords, err := hubaccess.Declaration(gitopsDir, name)
	if err != nil || strings.TrimSpace(coords.EscrowProjectID) == "" {
		return none, ""
	}
	return coords, name
}
