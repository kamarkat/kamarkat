package tenant

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/browser"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/opstore"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// Onboarding asks for credentials that have to be CREATED somewhere else first,
// and for years it asked for them by name alone: `HCLOUD_TOKEN: `. That is the
// part of a name an operator already has; what they do not have is the page that
// issues it and the boxes to tick on it, so every prompt was a search.
//
// So each one carries the page and the steps, the page is opened, and the prompt
// waits while the operator does the work -- the shape
// scripts/package/flip-public.sh already uses for package visibility: check
// first, skip what is done, open, wait, and print [n/total] so the end is in
// sight.
//
// A value is still only ever read, never echoed, and never written anywhere the
// platform keeps it (ADR-065). The one exception is the platform's OWN GitHub
// credential, which is the operator's across every tenant and belongs in their
// store.
type guidance struct {
	// URL is the page that issues this credential. Printed whether or not a
	// browser opens, because a printed address survives a machine with no
	// desktop session and a browser that silently fails to appear.
	URL string
	// Steps are what to do on that page -- the scopes, the menu path. Written as
	// what is clicked, not as prose about the credential.
	Steps []string
}

// credentialGuidance is where each credential comes from.
//
// Keyed by the one name the credential has everywhere (platformCredential.Secret:
// the repository key, the Day-0 variable, the prompt). An entry with no URL is a
// value nothing can issue -- an address, a hostname, a connection string the
// operator composes -- and those get a plain prompt rather than an invented link.
func credentialGuidance(secret string) guidance {
	switch secret {
	case PlatformTokenEnv:
		return guidance{
			// BOTH SCOPES, pre-selected. `workflow` is not implied by `repo`,
			// and a box's repository carries the workflow that bootstraps it
			// (ADR-072) -- so a token with `repo` alone creates the repository
			// and is then refused by the push, naming a path rather than a
			// scope.
			URL: "https://github.com/settings/tokens/new?scopes=repo,workflow&description=kamarkat%20box",
			Steps: []string{
				"`repo` and `workflow` are both pre-selected; leave them checked",
				"  `workflow` is required: a box's repository carries its own",
				"  bootstrap workflow, and GitHub refuses a token without it",
				"Generate the token, then copy it",
				"An App installation token for this organisation works here too, and is",
				"  preferred -- paste it instead and nothing else changes",
			},
		}
	case "GITOPS_REPO_WRITE_TOKEN":
		return guidance{
			URL: "https://github.com/settings/personal-access-tokens/new",
			Steps: []string{
				"Resource owner: the organisation this box's repository lives in",
				"Repository access: all repositories",
				"Repository permissions: Contents and Secrets, both Read and write",
				"Organisation permissions: Secrets, Read and write -- this is what lets",
				"  application builds inherit it; without it Day-0 cannot publish them",
			},
		}
	case "HCLOUD_TOKEN":
		return guidance{
			URL: "https://console.hetzner.cloud/",
			Steps: []string{
				"Open the project this box's servers belong in",
				"Security > API tokens > Generate API token",
				"Permissions: Read & Write -- it creates and destroys servers",
			},
		}
	case "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY":
		return guidance{
			URL: "https://console.hetzner.cloud/",
			Steps: []string{
				"Open your project, then Security > S3 credentials > Generate credentials",
				"Both halves are shown once; the secret cannot be read back",
				"Any S3-compatible provider works -- use its own console instead if you",
				"  keep backups elsewhere",
			},
		}
	case "GHCR_TOKEN":
		return guidance{
			URL: "https://github.com/settings/tokens/new?scopes=read:packages&description=kamarkat%20image%20pull",
			Steps: []string{
				"`read:packages` is pre-selected, and read is all this needs",
			},
		}
	case "TS_AUTHKEY":
		return guidance{
			URL: "https://login.tailscale.com/admin/settings/keys",
			Steps: []string{
				"Generate auth key",
				"Reusable, so every node this box adds can use it",
				"Ephemeral off: these nodes are meant to stay in the tailnet",
			},
		}
	case "INFISICAL_ESCROW_CLIENT_SECRET":
		return guidance{
			URL: "https://app.infisical.com/organization/identities",
			Steps: []string{
				"Open the `kamarkat-automation` identity",
				"Universal Auth > Client Secrets > Add Client Secret",
				"It is shown once. The escrow issues many per identity, so adding one",
				"  does not revoke what your other boxes are using",
			},
		}
	// Both come from ONE integration, and the walkthrough asks for them as two
	// values, so this page is opened twice. Accepted: the second visit is the
	// heartbeat tab of the integration the first visit created.
	//
	// ACCOUNT CREATION, which is the only page that works for everyone. The two
	// addresses this replaced each assumed something: `grafana.com/orgs` needs a
	// live session and refuses an operator without one with "that login doesn't
	// work", which reads as a broken credential rather than as a missing session,
	// and the sign-in page assumes an account exists -- where this value is
	// needed, the usual case is an operator who has no stack yet. The page offers
	// sign-in to anyone who does. Not the integrations page either: that lives
	// under the stack's own hostname (`<stack>.grafana.net`), which this box's
	// declaration does not name, and an address that is right for one customer
	// and wrong for the next is worse than one more click.
	case "GRAFANA_IRM_ALERTMANAGER_URL", "GRAFANA_IRM_HEARTBEAT_URL":
		return guidance{
			URL: "https://grafana.com/auth/sign-up/create-user",
			Steps: []string{
				"Create an account, or sign in if you already have one",
				"Launch the stack this box reports to",
				"Alerts & IRM > IRM > Integrations > Add integration > Alertmanager",
				"GRAFANA_IRM_ALERTMANAGER_URL is that integration's URL",
				"GRAFANA_IRM_HEARTBEAT_URL is on the same integration, under Heartbeat",
			},
		}
	}
	return guidance{}
}

// Ask is one credential the walkthrough collects.
type Ask struct {
	Secret string
	// Why is what this box cannot do without it, in a few words. It is the
	// answer to the question a prompt provokes and never answers.
	Why string
	// Store is an opstore path to keep the value at, for a credential that is
	// the OPERATOR's rather than the tenant's. Empty for everything else:
	// keeping a tenant's credential on the platform operator's machine is the
	// thing ADR-065 exists to prevent.
	Store string
	// Optional marks a value that may be skipped with one keystroke.
	Optional bool
	// Target is where the answer goes. A pointer rather than a setter because
	// every caller is filling a field of its own struct, and the walkthrough has
	// no business knowing which struct that is.
	Target *string
}

// Collect runs the walkthrough over the asks it is given, in order, and reports
// which of them were left empty.
//
// requireAll is the difference between the two callers. `kakt box init` is
// about to push these onto a repository and start a bootstrap, so an empty
// required answer is an error there. `kakt box init` exists to be run more than
// once -- gather what you have, come back for the rest -- so it records the
// skipped names and says what is still outstanding.
//
// Terminal-only by contract: the caller checks for one. A pipeline reaching this
// would block on a read that never completes, which is the failure mode of every
// interactive installer that has ever hung a CI job.
func Collect(asks []Ask, requireAll bool) ([]string, error) {
	var skipped []string
	if len(asks) == 0 {
		return nil, nil
	}
	fmt.Printf("\n%d credential(s) to obtain. Each one opens the page that issues it.\n",
		len(asks))
	fmt.Println("What is already set -- in the environment, or from a previous run -- is skipped.")

	for i, a := range asks {
		g := credentialGuidance(a.Secret)
		fmt.Printf("\n[%d/%d] %s", i+1, len(asks), a.Secret)
		if a.Why != "" {
			fmt.Printf(" -- %s", a.Why)
		}
		fmt.Println()
		if g.URL != "" {
			fmt.Printf("        %s\n", g.URL)
			for _, step := range g.Steps {
				fmt.Printf("        %s\n", step)
			}
			if err := browser.Open(g.URL); err != nil {
				fmt.Printf("        (could not open a browser: %v -- open that address yourself)\n", err)
			}
		}
		if a.Optional {
			fmt.Println("        Optional: press enter to skip.")
		}

		v, err := readSecret(fmt.Sprintf("        %s: ", a.Secret))
		if err != nil {
			return skipped, err
		}
		if strings.TrimSpace(v) == "" {
			if a.Optional {
				continue
			}
			if !requireAll {
				skipped = append(skipped, a.Secret)
				fmt.Println("        skipped; this box cannot be bootstrapped until it is set")
				continue
			}
			// Required, and empty. Said here rather than at the end, so the
			// operator fixes it while they are still on the page that issues it.
			return skipped, fmt.Errorf("%s is required: %s", a.Secret, a.Why)
		}
		*a.Target = strings.TrimSpace(v)

		if a.Store != "" {
			p, err := opstore.Write(a.Store, v)
			if err != nil {
				return skipped, err
			}
			// The path, never the value. This confirms where to look next time
			// without putting a credential in a scrollback buffer.
			fmt.Printf("        kept in %s (mode 0600), so this is asked once per machine\n", p)
		}
	}
	fmt.Println()
	return skipped, nil
}

// EnsurePlatformCredential resolves the credential the PLATFORM creates the
// repository with, asking for it when there is a terminal to ask at.
//
// Separate from the tenant's secrets, and collected before them, because it is a
// different credential with a different owner at a different moment: this one
// creates the repository that those are then stored on. It used to be a dead end
// -- resolved from the environment or one file, and a hard error otherwise, with
// no prompt anywhere in the CLI -- which made it the one credential an operator
// could not supply by being asked.
//
// mayAsk is the caller's --no-prompt: a run told never to prompt is told that
// about every credential, not only the tenant's.
func EnsurePlatformCredential(mayAsk bool) (Credential, error) {
	cred, err := TokenFromEnv()
	if err == nil {
		return cred, nil
	}
	if !mayAsk || !interactive() {
		// Unchanged for automation: the same refusal, naming the store path and
		// the legacy location if the file is still sitting in a checkout.
		return Credential{}, err
	}
	fmt.Println("\nThe credential that creates the repository in the tenant's organisation.")
	fmt.Println("Not the tenant's: it is yours across every box, so it is kept on this machine.")

	var token string
	if _, err := Collect([]Ask{{
		Secret: PlatformTokenEnv,
		Why:    "creates the tenant's GitOps repository in their organisation",
		Store:  platformTokenPath,
		Target: &token,
	}}, true); err != nil {
		return Credential{}, err
	}
	return Credential{Token: token, Source: PlatformTokenEnv}, nil
}

// interactive reports whether there is somebody to ask.
func interactive() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// CredentialAsks is every credential a box of this shape needs, in the order it
// makes sense to obtain them, set or not.
//
// One list, two callers. `kakt box init` shows all of them with their state and
// collects what is missing; `kakt box init` collects only what is still
// missing when it runs. They were two lists once, and the second one had entries
// the first did not -- which is how a credential came to have no prompt anywhere.
//
// Targets point into s, so collecting fills the caller's own struct.
func CredentialAsks(spec Spec, s *Secrets) []Ask {
	credName, err := ProviderCredential(spec.Provider)
	if err != nil {
		credName = "PROVIDER_TOKEN"
	}
	asks := []Ask{
		{
			Secret: credName,
			Why:    "creates this box's clusters, in your own cloud account",
			Store:  providerTokenPath(spec.Provider),
			Target: &s.ProviderToken,
		},
		{
			Secret: "GITOPS_REPO_WRITE_TOKEN",
			Why:    "Day-0 commits what it generates with it, and builds inherit it",
			Store:  "github/gitops-token",
			Target: &s.GitopsToken,
		},
	}
	// Hybrid only. Asked for here rather than left for the tenant to discover,
	// because a box that bootstraps without it reports success and then cannot
	// pass pod traffic between its control plane and its home workers.
	if needsTailscale(spec.Provider) {
		asks = append(asks, Ask{
			Secret: "TS_AUTHKEY",
			Why:    "the control plane and home workers join your tailnet with it",
			Store:  "tailscale/authkey",
			Target: &s.TailscaleAuthkey,
		})
	}
	// The escrow's client secret, and only it. The other three are names rather
	// than credentials and `kakt escrow init` has already recorded all four, so a
	// prompt for them would ask somebody to copy what the CLI is holding -- and a
	// box with no escrow at all needs a project created, not four lines typed.
	//
	// This one is the exception because Infisical shows a client secret once: a
	// store holding the other three and not this one is a real gap, with a page
	// that fixes it.
	if s.escrowNamedButUnauthenticated() {
		asks = append(asks, Ask{
			Secret: "INFISICAL_ESCROW_CLIENT_SECRET",
			Why:    "what this box's automation authenticates to the escrow with",
			Store:  escrowStorePath("INFISICAL_ESCROW_CLIENT_SECRET"),
			Target: &s.EscrowClientSecret,
		})
	}
	// Then whatever this box's own configuration calls for, and only that. The
	// table is the authority for which those are, and for which of them the CLI
	// supplies itself (platformCredential.Derived).
	for _, tr := range []tier{tierCapability, tierDestination} {
		for _, c := range platformCredentials() {
			if c.Tier != tr || c.Derived || !c.Needed(spec, *s) {
				continue
			}
			asks = append(asks, Ask{
				Secret:   c.Secret,
				Why:      c.Capability,
				Store:    c.Store,
				Optional: tr == tierDestination,
				Target:   c.Field(s),
			})
		}
	}
	return asks
}

// Pending is the asks still unanswered.
func Pending(asks []Ask) []Ask {
	var out []Ask
	for _, a := range asks {
		if strings.TrimSpace(*a.Target) == "" {
			out = append(out, a)
		}
	}
	return out
}

// Interactive reports whether there is somebody to ask.
func Interactive() bool { return interactive() }

// providerTokenPath is where this operator stages a provider's token.
//
// Named after the provider rather than after the platform, matching the rest of
// the store (`hetzner/token`), so a machine that operates boxes on two providers
// keeps two tokens rather than one that is sometimes wrong.
func providerTokenPath(provider string) string {
	p := strings.TrimSpace(strings.ToLower(provider))
	if p == "" || p == "hybrid" {
		// Hybrid clusters' cloud half is Hetzner's (ADR-046), so the token is the
		// same one under the same name.
		p = "hetzner"
	}
	return p + "/token"
}

// escrowStorePath places a value in the escrow settings directory the escrow
// package owns, rather than inventing a second home for it.
func escrowStorePath(name string) string {
	return escrow.StoreEscrowSubdir + "/" + name
}
