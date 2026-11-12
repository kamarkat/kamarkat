package tenant

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/opstore"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// ProviderCredential is the secret name a provider's clusters are created with.
//
// The name belongs to the provider, not to this platform: HCLOUD_TOKEN is what
// Hetzner's own tooling reads, and a tenant looking at its repository secrets
// should see a name it recognises from the provider's documentation rather than
// one invented here.
func ProviderCredential(provider string) (string, error) {
	switch provider {
	case "hetzner", "hybrid":
		// A hybrid cell is a Hetzner control plane with home-lab workers
		// (ADR-046), so it is created with the same credential.
		return "HCLOUD_TOKEN", nil
	default:
		return "", fmt.Errorf("no credential is known for provider %q; the "+
			"scaffolded repository will need its cloud secret set by hand", provider)
	}
}

// Secrets are what a tenant's repository needs to bootstrap itself.
//
// Held together because they are set together and because a repository with one
// of them cannot bootstrap: the workflow fails at the step that needs the other,
// after the tenant has been told the setup succeeded.
type Secrets struct {
	// ProviderToken creates clusters. It never leaves the tenant's account.
	ProviderToken string
	// GitopsToken lets Day-0 commit the artifacts it generates (ADR-072).
	GitopsToken string
	// TailscaleAuthkey enrols this box's control plane and its home workers on
	// the tenant's tailnet. Hybrid only: ADR-046 invariant 6 requires a routable
	// tailnet IP on every node that carries pod traffic, and under §21 that
	// includes the hub control plane. Empty for every other provider, where
	// nothing reads it.
	TailscaleAuthkey string

	// EscrowURL, EscrowClientID, EscrowClientSecret and EscrowProjectID reach an
	// Infisical the TENANT controls and this box does not host -- Infisical Cloud,
	// or one they run elsewhere.
	//
	// It holds the box's Infisical master keys: the root secret without which the
	// box's own secret store cannot be decrypted, and every credential the platform
	// manages is lost with the cluster (ADR-076).
	//
	// It cannot be the box's own Infisical, which is the thing those keys unlock and
	// is unreachable exactly when they are needed. And it is the tenant's rather
	// than the platform's, because ADR-065 says the platform holds no secret
	// credential belonging to a tenant -- least of all the one that unlocks the rest.
	EscrowURL          string
	EscrowClientID     string
	EscrowClientSecret string
	EscrowProjectID    string

	// The box's own Infisical administrator. Collected with the rest rather than
	// defaulted: the defaults these replace were a personal email address and the
	// literal "Password@123", identical on every box, on the account that can read
	// every secret the platform manages for this tenant.
	InfisicalAdminEmail    string
	InfisicalAdminPassword string

	// What the hub needs to come up healthy, rather than merely up. Defined as
	// a list in platform_credentials.go so prompting, validation and writing
	// them into the repository cannot disagree about the set.
	S3AccessKeyID     string
	S3SecretAccessKey string

	// ADR-078 add.5 §5: where the hub's Alertmanager pages, and its heartbeat.
	GrafanaIRMAlertmanagerURL string
	GrafanaIRMHeartbeatURL    string

	// The issuer's outbound mail, as an SMTP URL (smtps://user:password@host:port).
	ZitadelSMTPURL string

	GHCRUsername string
	GHCRToken    string
}

// hasEscrow reports whether a complete escrow credential was supplied.
//
// Complete or absent, never partial: hub-operator reads all four from one Secret,
// and three of four produces an operator that attempts a backup on every reconcile
// and fails -- an escrow that appears to exist and does not work.
func (s Secrets) hasEscrow() bool {
	return strings.TrimSpace(s.EscrowURL) != "" &&
		strings.TrimSpace(s.EscrowClientID) != "" &&
		strings.TrimSpace(s.EscrowClientSecret) != "" &&
		strings.TrimSpace(s.EscrowProjectID) != ""
}

// needsTailscale reports whether a provider's clusters join a tailnet.
//
// Only hybrid does. Asking a hetzner tenant for a Tailscale key would be asking
// for a credential nothing consumes, and skipping the question for a hybrid one
// produces a box whose control plane silently never joins the tailnet.
func needsTailscale(provider string) bool {
	return strings.TrimSpace(provider) == "hybrid"
}

// Complete reports whether both are present. A repository is only dispatchable
// with both, so this is the test for whether scaffolding can go further than
// creating the repository.
func (s Secrets) Complete() bool {
	return strings.TrimSpace(s.ProviderToken) != "" && strings.TrimSpace(s.GitopsToken) != ""
}

// CompleteFor is Complete plus whatever the provider additionally requires.
// Dispatching a hybrid box without a tailnet key produces a control plane that
// never joins the tailnet, which is not a failure the run reports.
func (s Secrets) CompleteFor(provider string) bool {
	if !s.Complete() {
		return false
	}
	return !needsTailscale(provider) || strings.TrimSpace(s.TailscaleAuthkey) != ""
}

// nothingLeftToAsk reports whether a prompt session would ask anything.
//
// Deliberately not folded into CompleteFor. That answers "can this box be
// dispatched", which the handover message and its tests depend on meaning
// exactly that; this answers "is there anything still to collect", which now
// includes the escrow and the credentials the hub needs to come up healthy.
// Conflating them redefined a contract three tests pin, for no gain.
func (s Secrets) nothingLeftToAsk(spec Spec) bool {
	return s.CompleteFor(spec.Provider) &&
		s.hasEscrow() &&
		len(s.missingInTier(spec, tierCapability)) == 0 &&
		len(s.missingInTier(spec, tierDestination)) == 0
}

// Prompt asks for whichever secret is missing, one guided step at a time.
//
// Each step names what the credential is for, opens the page that issues it, and
// waits while the operator creates it (internal/kakt-cli/tenant/guided.go).
// Reads without echo, because a token typed into a terminal that echoes it is a
// token in a scrollback buffer and, on a shared screen, in someone else's memory.
//
// Returns what it has when there is no terminal to ask -- a pipeline gets what it
// passed and no prompt that would hang it.
func (s Secrets) Prompt(spec Spec) (Secrets, error) {
	s.Resolve(spec)
	if s.nothingLeftToAsk(spec) || !interactive() {
		return s, nil
	}

	asks := Pending(CredentialAsks(spec, &s))
	if len(asks) == 0 {
		// Everything was supplied, and what is left is derived. Saying nothing is
		// the right amount: a heading over an empty list reads like a failure.
		return s, nil
	}

	// Said before the first step, because the order otherwise reads backwards:
	// the repository already exists by this point, created with the platform's own
	// credential, and these are a different set entirely. They are stored on that
	// repository for the workflow to use later -- nothing here authenticates with
	// them, and the platform keeps no copy beyond the staging store (ADR-065).
	//
	// `kakt box init` is where this is meant to happen, before anything is
	// created. Reaching it here means the scaffold found something missing, so it
	// asks rather than ending a run that has already made a repository.
	fmt.Println("\nCredentials this box needs, which `kakt box init` collects before scaffolding.")
	if s.escrowNamedButUnauthenticated() {
		fmt.Println("\nThis box's escrow is known, but its client secret is not on this machine.")
		fmt.Println("Infisical shows a client secret once. Issuing another is additive, so your")
		fmt.Println("other boxes keep working with theirs.")
	}

	if _, err := Collect(asks, true); err != nil {
		return s, err
	}
	return s, nil
}

// Resolve supplies whatever was not passed from the environment and from this
// operator's store, asking nothing.
//
// Called before the prompts and also instead of them, because `--no-prompt` and a
// pipeline both need the same resolution and neither can be asked. It used to
// live inside the prompt path, so a run that could not be prompted got no
// resolution either: a CI job with every credential in its own environment was
// refused for having none.
func (s *Secrets) Resolve(spec Spec) {
	s.fillFromEnvironment()
	s.fillFromStore(spec)
	s.fillEscrowFromStore()
}

// fillFromStore takes whatever `kakt box init` staged on this machine.
//
// After the environment, which therefore wins: a pipeline sets variables and has
// no store, and a workstation has a store precisely so that nothing has to be
// exported. Nothing here is a fallback for a value that is required and absent --
// a missing credential still fails with its own name.
func (s *Secrets) fillFromStore(spec Spec) {
	if strings.TrimSpace(s.ProviderToken) == "" {
		s.ProviderToken = opstore.Value(providerTokenPath(spec.Provider))
	}
	if strings.TrimSpace(s.GitopsToken) == "" {
		s.GitopsToken = opstore.Value("github/gitops-token")
	}
	if strings.TrimSpace(s.TailscaleAuthkey) == "" {
		s.TailscaleAuthkey = opstore.Value("tailscale/authkey")
	}
	for _, c := range platformCredentials() {
		if c.Store == "" {
			continue
		}
		f := c.Field(s)
		if strings.TrimSpace(*f) != "" {
			continue
		}
		if v := opstore.Value(c.Store); v != "" {
			*f = v
		}
	}
}

// fillEscrowFromStore takes the box's escrow coordinates from this operator's
// own store, where `kakt escrow init` wrote them.
//
// The environment still wins, because StoredValue reads it first. This exists
// because the same four values had two resolution paths with different rules: the
// render read them from the store, while the repository secrets read only flags --
// so an operator with a complete store was still asked to type all four, and the
// refusal for a box with no escrow claimed the opposite ("these settings are
// recorded for you, so scaffolding finds them without any being passed").
func (s *Secrets) fillEscrowFromStore() {
	for _, f := range []struct {
		name  string
		field *string
	}{
		{"INFISICAL_ESCROW_URL", &s.EscrowURL},
		{"INFISICAL_ESCROW_PROJECT_ID", &s.EscrowProjectID},
		{"INFISICAL_ESCROW_CLIENT_ID", &s.EscrowClientID},
		{"INFISICAL_ESCROW_CLIENT_SECRET", &s.EscrowClientSecret},
	} {
		if strings.TrimSpace(*f.field) != "" {
			continue
		}
		if v := escrow.StoredValue(f.name); v != "" {
			*f.field = v
		}
	}
}

// escrowNamedButUnauthenticated reports the one partial escrow worth a prompt:
// the project is known and the credential that reaches it is not.
func (s Secrets) escrowNamedButUnauthenticated() bool {
	return strings.TrimSpace(s.EscrowURL) != "" &&
		strings.TrimSpace(s.EscrowProjectID) != "" &&
		strings.TrimSpace(s.EscrowClientID) != "" &&
		strings.TrimSpace(s.EscrowClientSecret) == ""
}

func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("reading the value: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// SetSecrets writes the secrets into the tenant's repository.
//
// Through `gh secret set` reading stdin rather than an argument, so the value
// never appears in this process's command line -- where anything on the machine
// can read it out of the process table for as long as the call runs.
func SetSecrets(ctx context.Context, spec Spec, s Secrets) error {
	credName, err := ProviderCredential(spec.Provider)
	if err != nil {
		return err
	}
	repo := fmt.Sprintf("%s/%s", spec.GitOrg, spec.RepoName())

	values := map[string]string{
		credName:                  s.ProviderToken,
		"GITOPS_REPO_WRITE_TOKEN": s.GitopsToken,
	}
	if v := strings.TrimSpace(s.TailscaleAuthkey); v != "" {
		values["TS_AUTHKEY"] = v
	}
	// All three or none: a partial set produces a box that believes it has an
	// escrow and cannot reach it, which is worse than having none.
	if s.hasEscrow() {
		values["INFISICAL_ESCROW_URL"] = strings.TrimSpace(s.EscrowURL)
		values["INFISICAL_ESCROW_CLIENT_ID"] = strings.TrimSpace(s.EscrowClientID)
		values["INFISICAL_ESCROW_CLIENT_SECRET"] = strings.TrimSpace(s.EscrowClientSecret)
		values["INFISICAL_ESCROW_PROJECT_ID"] = strings.TrimSpace(s.EscrowProjectID)
	}

	// Whatever this box has. Capability credentials are refused earlier when
	// absent, so anything empty here is a telemetry key the tenant declined --
	// which is a narrower support contract, not a misconfiguration.
	for _, c := range platformCredentials() {
		if v := strings.TrimSpace(*c.Field(&s)); v != "" {
			values[c.Secret] = v
		}
	}

	// Sorted, not map order. Go randomises map iteration, so a failure part-way
	// through left a different subset of secrets set on every run -- which made
	// "which ones landed?" unanswerable from the log and the failure itself
	// irreproducible.
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if err := setSecretWithRetry(ctx, repo, name, values[name]); err != nil {
			return err
		}
		fmt.Printf("[box] ✓ %s set on %s\n", name, repo)
	}

	return setOrgGitopsToken(ctx, spec.GitOrg, s.GitopsToken)
}

// setOrgGitopsToken publishes GITOPS_REPO_WRITE_TOKEN once at the ORGANISATION
// level, so every application repository the tenant owns can write the one line its
// build owes the GitOps repository -- without the platform ever learning that
// the repository exists.
//
// The alternative was a repository-level secret per application, and it does not
// hold. `kakt box init` knows exactly one repository: <tenant>-gitops.
// Applications are the tenant's to create, rename and retire long after
// scaffolding has run, so a platform that set their secrets would have to track
// a set that changes without it -- and the failure when it falls behind is
// silent. waypoint's bff has published a chart on every commit since September
// and pinned a version NOT ONCE, because its deploy job reads a
// GITOPS_REPO_WRITE_TOKEN that nothing ever set. Three consecutive red runs,
// publish green each time.
//
// One credential, one place, inherited by construction. A repository added
// tomorrow is covered the moment it exists.
//
// REQUIRED PERMISSION, and it is not the classic `admin:org`: a fine-grained
// token needs only the organisation's "Secrets: write" permission. Read the
// refusal carefully -- GitHub names both routes --
//
//	You must be an org admin or have the actions secrets fine-grained permission.
//
// so the narrow one is available and is what the prompt asks for.
//
// A failure here is REPORTED, NOT FATAL. The box it was scaffolding is complete
// and bootstraps without this; what is missing is the credential a future
// application build will need, and aborting the scaffold over it would destroy
// a working repository to fix something no cluster depends on.
// publishOrgSecret performs the write and REPORTS ITS FAILURE.
//
// Separate from setOrgGitopsToken because the two callers want opposite things
// from a failure. Scaffold wants to continue: the box is complete without this
// credential and aborting would destroy a working repository over something no
// cluster depends on. An operator who ran the retry deliberately wants the
// opposite -- a retry that prints a warning and exits 0 has told them it worked
// when it did not, and the next thing they do is wonder why their build still
// cannot pin a version.
func publishOrgSecret(ctx context.Context, org, token string) error {
	// A plausible length, before anything is sent.
	//
	// gh accepts whatever arrives on stdin and exits 0, so a paste that did not
	// register produces a published secret containing nothing and a command that
	// reports success. Downstream that is indistinguishable from a wrong token:
	// the build fails on "Invalid username or token", which sends the operator
	// to re-mint a credential that was never read.
	//
	// GitHub tokens are far longer than this; the bound only has to be tight
	// enough to catch an empty or single-character read.
	if err := checkTokenPlausible(token); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "gh", "secret", "set", "GITOPS_REPO_WRITE_TOKEN",
		"--org", org, "--visibility", "all")
	cmd.Stdin = strings.NewReader(token)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s", firstLine(string(out)))
	}
	return nil
}

func setOrgGitopsToken(ctx context.Context, org, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}

	// Scaffold knows ONE repository -- the GitOps repository it just created --
	// so on a plan where organisation secrets do not reach private repositories
	// that is the one it can cover. It is also the only one that exists at this
	// point: applications are created later, and each will need the credential
	// when it is.
	//
	// Publishing organisation-wide anyway would be worse than doing nothing. It
	// succeeds, the secret is listed against every repository, and every private
	// repository's workflow receives an empty string -- a credential that looks
	// present everywhere except where it is read.
	if reaches, plan, perr := OrgSecretsReachPrivateRepos(ctx, org); perr == nil && !reaches {
		fmt.Printf("[box] ! %s is on the %s plan: an organisation secret reaches only PUBLIC\n", org, plan)
		fmt.Printf("[box]   repositories, so publishing one would arrive empty in every private\n")
		fmt.Printf("[box]   repository's build. Setting it per repository instead.\n")
		fmt.Printf("[box]   Each application repository needs it when it is created:\n")
		fmt.Printf("[box]     kakt tenant set-gitops-token --repo <owner>/<name>\n")
		return nil
	}

	if err := publishOrgSecret(ctx, org, token); err != nil {
		fmt.Printf("[box] ! GITOPS_REPO_WRITE_TOKEN not set on the %s organisation: %s\n",
			org, firstLine(err.Error()))
		fmt.Printf("[box]   Application builds cannot pin a chart version until it is.\n")
		fmt.Printf("[box]   Re-run with a token carrying the organisation's\n")
		fmt.Printf("[box]   \"Secrets: write\" permission, or set it once by hand:\n")
		fmt.Printf("[box]     gh secret set GITOPS_REPO_WRITE_TOKEN --org %s --visibility all\n", org)
		return nil
	}
	fmt.Printf("[box] ✓ GITOPS_REPO_WRITE_TOKEN set on the %s organisation (all repositories)\n", org)
	return nil
}

// setSecretWithRetry writes one secret, retrying while GitHub is the one at
// fault.
//
// It is called after the repository has been created and pushed to, and there is
// no way back from here: an error aborts the scaffold with the repository
// already existing, so the next attempt fails on "already exists" before it
// reaches this code, and the operator has to delete the repository by hand. A
// single HTTP 500 -- GitHub's, not ours -- therefore cost a whole run:
//
//	Error: setting S3_ACCESS_KEY_ID on soloz-io/acme-gitops: exit status 1
//	failed to set secret "S3_ACCESS_KEY_ID": HTTP 500: Server Error
//
// Secrets are set by PUT, so writing one twice is the same as writing it once
// and a retry cannot half-apply anything.
func setSecretWithRetry(ctx context.Context, repo, name, value string) error {
	const attempts = 4
	var lastOut []byte
	var lastErr error

	for attempt := 1; attempt <= attempts; attempt++ {
		cmd := exec.CommandContext(ctx, "gh", "secret", "set", name, "--repo", repo)
		// Through stdin rather than an argument, so the value never appears in
		// this process's command line.
		cmd.Stdin = strings.NewReader(value)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		lastOut, lastErr = out, err

		// Retry only what is worth retrying. A rejected token, a repository that
		// does not exist, or a value GitHub will not accept fails the same way
		// four times and turns a clear error into a slow one.
		if permanentSecretFailure(string(out)) {
			break
		}
		if attempt < attempts {
			delay := time.Duration(attempt*attempt) * time.Second
			fmt.Printf("[box] %s: %s -- retrying in %s (attempt %d/%d)\n",
				name, firstLine(string(out)), delay, attempt, attempts)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return fmt.Errorf("setting %s on %s: %w\n%s", name, repo, lastErr, lastOut)
}

// permanentSecretFailure reports whether retrying would only repeat the same
// answer. Anything not named here -- 5xx, a timeout, a reset connection -- is
// treated as GitHub's problem and retried.
func permanentSecretFailure(out string) bool {
	for _, sig := range []string{
		"HTTP 401", "HTTP 403", "HTTP 404", "HTTP 422",
		"Bad credentials", "Resource not accessible",
	} {
		if strings.Contains(out, sig) {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// HandoverInstructions prints what is left for the tenant to do.
//
// Printed rather than performed when a secret is absent. Scaffolding has already
// created a repository at this point, so the run cannot simply fail: what it owes
// is an accurate account of where it stopped and what completes it.
func HandoverInstructions(spec Spec, s Secrets, w *bufio.Writer) {
	defer w.Flush()
	repo := fmt.Sprintf("%s/%s", spec.GitOrg, spec.RepoName())
	credName, err := ProviderCredential(spec.Provider)
	if err != nil {
		credName = "<PROVIDER_TOKEN>"
	}

	fmt.Fprintf(w, "\n%s is scaffolded and pinned to %s.\n", repo, spec.BundleVersion)
	fmt.Fprintf(w, "  https://github.com/%s\n\n", repo)

	if !s.CompleteFor(spec.Provider) {
		fmt.Fprintf(w, "It cannot bootstrap yet: the workflow it carries runs under the\n")
		fmt.Fprintf(w, "tenant's own secrets, and %s.\n\n", missingWord(s, credName, spec.Provider))
		if strings.TrimSpace(s.ProviderToken) == "" {
			fmt.Fprintf(w, "  gh secret set %s --repo %s\n", credName, repo)
		}
		if strings.TrimSpace(s.GitopsToken) == "" {
			fmt.Fprintf(w, "  gh secret set GITOPS_REPO_WRITE_TOKEN --repo %s\n", repo)
		}
		if needsTailscale(spec.Provider) && strings.TrimSpace(s.TailscaleAuthkey) == "" {
			fmt.Fprintf(w, "  gh secret set TS_AUTHKEY --repo %s\n", repo)
		}
		fmt.Fprintln(w)
	}

	// Said whether or not anything else is missing: a box without an escrow
	// bootstraps and runs, so nothing else will ever mention it, and the moment it
	// matters is the moment it cannot be added.
	if !s.hasEscrow() {
		fmt.Fprintf(w, "This box has no escrow.\n\n")
		fmt.Fprintf(w, "hub-operator copies this box's Infisical master keys -- the root secret\n")
		fmt.Fprintf(w, "without which its secret store cannot be decrypted -- to an Infisical you\n")
		fmt.Fprintf(w, "control. The box's own Infisical cannot hold them: they are the keys that\n")
		fmt.Fprintf(w, "decrypt it, and it is unreachable exactly when they are needed.\n\n")
		fmt.Fprintf(w, "Without it, losing this cluster loses every secret the platform manages\n")
		fmt.Fprintf(w, "for it, and the escrow cannot be added after the fact.\n\n")
		for _, name := range []string{
			"INFISICAL_ESCROW_URL",
			"INFISICAL_ESCROW_PROJECT_ID",
			"INFISICAL_ESCROW_CLIENT_ID",
			"INFISICAL_ESCROW_CLIENT_SECRET",
		} {
			fmt.Fprintf(w, "  gh secret set %s --repo %s\n", name, repo)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "Then bootstrap it, in the tenant's repository:\n\n")
	fmt.Fprintf(w, "  gh workflow run bootstrap-cluster.yml --repo %s\n", repo)
	fmt.Fprintf(w, "  https://github.com/%s/actions/workflows/bootstrap-cluster.yml\n", repo)
}

// missingWord names what is absent. Built from the set rather than switched over
// pairs: with a third secret the cases stop being enumerable, and a sentence
// naming one missing secret while two are missing is worse than no sentence.
func missingWord(s Secrets, credName, provider string) string {
	var missing []string
	if strings.TrimSpace(s.ProviderToken) == "" {
		missing = append(missing, credName)
	}
	if strings.TrimSpace(s.GitopsToken) == "" {
		missing = append(missing, "GITOPS_REPO_WRITE_TOKEN")
	}
	// Only where something reads it. Naming TS_AUTHKEY to a hetzner tenant sends
	// them to obtain a credential nothing on their box consumes.
	if needsTailscale(provider) && strings.TrimSpace(s.TailscaleAuthkey) == "" {
		missing = append(missing, "TS_AUTHKEY")
	}
	switch len(missing) {
	case 0:
		return "everything it needs is set"
	case 1:
		return missing[0] + " is not set"
	case 2:
		return missing[0] + " and " + missing[1] + " are not set"
	default:
		return strings.Join(missing[:len(missing)-1], ", ") + " and " +
			missing[len(missing)-1] + " are not set"
	}
}

// shellQuote renders a value safe to paste into the shell lines above.
//
// A client secret is generated material and may hold any byte; unquoted, one
// containing a space or a dollar sign produces a command that runs and exports
// something else, which surfaces much later as an escrow that cannot authenticate.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(strings.TrimSpace(v), "'", `'\''`) + "'"
}

// PublishOrgGitopsToken republishes the organisation's GITOPS_REPO_WRITE_TOKEN
// for a box that already exists.
//
// SetSecrets does this once, during scaffold, and deliberately does not fail the
// scaffold when it cannot: the box is complete without it and aborting would
// destroy a working repository over a credential no cluster depends on. The
// consequence is that a scaffold whose token lacked the organisation's
// "Secrets: write" permission leaves a box permanently short of it, with no way
// back that does not involve scaffolding again -- which is not a thing anyone
// can do to a running box.
//
// So the retry is a command. What it replaces is an operator pasting
// `gh secret set` from a warning printed days earlier, which is the platform
// asking a tenant to do the platform's job (ADR-084 assigns this credential to
// the Day-0 CLI).
func PublishOrgGitopsToken(ctx context.Context, org, token string) error {
	if strings.TrimSpace(org) == "" {
		return fmt.Errorf("no organisation: the credential is published at the organisation " +
			"level so every application repository inherits it, and there is no default")
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("no token: pass one with --token or on stdin")
	}
	if err := publishOrgSecret(ctx, org, token); err != nil {
		// TWO credentials are involved and the refusal names neither. The token
		// read above is the VALUE being stored; the write itself is performed by
		// whatever `gh` is logged in as. A 403 here is almost always the second
		// one, and an error that talks about the first sends the operator to
		// re-mint a token that was never the problem.
		return fmt.Errorf("could not publish GITOPS_REPO_WRITE_TOKEN on the %s organisation: %w\n\n"+
			"This is about the credential `gh` is authenticated with, NOT the token you\n"+
			"just supplied -- that one is the value being stored and is never used to\n"+
			"perform the write. Check it with `gh auth status`.\n\n"+
			"Writing an organisation secret needs one of:\n"+
			"  - admin:org on the gh login:  gh auth refresh -h github.com -s admin:org\n"+
			"  - a fine-grained PAT with the organisation's \"Secrets: write\", supplied\n"+
			"    for this one call:          GH_TOKEN=<pat> kakt tenant set-gitops-token --org %s\n\n"+
			"GitHub names both routes in its refusal, which is why it reads as though a\n"+
			"fine-grained permission were missing from a classic token that cannot carry one.",
			org, err, org)
	}
	return nil
}

// orgSecretsReachPrivateRepos reports whether an ORGANISATION secret is
// delivered to this organisation's private repositories.
//
// On GitHub's free plan it is not. The write succeeds, the API reports the
// secret with visibility "all", it appears in the repository's own list of
// available organisation secrets -- and the workflow receives an empty string.
// Nothing at any layer says the value was withheld.
//
// That cost a full diagnostic cycle here: a token was re-minted twice, the
// publishing command was hardened twice, and the actual answer was the
// organisation's billing plan. So this is checked and SAID, rather than left
// for the next person to rediscover from a blank line in a workflow log.
func OrgSecretsReachPrivateRepos(ctx context.Context, org string) (bool, string, error) {
	out, err := exec.CommandContext(ctx, "gh", "api",
		"/orgs/"+org, "--jq", ".plan.name").CombinedOutput()
	if err != nil {
		// Not fatal to the caller: the plan is advisory, and refusing to publish
		// because it could not be read would be worse than publishing.
		return true, "", fmt.Errorf("could not read the %s organisation's plan: %s", org, firstLine(string(out)))
	}
	plan := strings.TrimSpace(string(out))
	return plan != "free", plan, nil
}

// PublishRepoGitopsToken publishes GITOPS_REPO_WRITE_TOKEN on ONE repository.
//
// The per-repository form exists because organisation secrets do not reach
// private repositories on the free plan, which is the arrangement most boxes
// start on. It is weaker than the organisation form in exactly the way ADR-084
// describes -- a repository created tomorrow is NOT covered, and must be given
// the credential when it is created -- and that is the trade the plan imposes
// rather than one the platform chose.
func PublishRepoGitopsToken(ctx context.Context, repo, token string) error {
	if strings.TrimSpace(repo) == "" {
		return fmt.Errorf("no repository: pass owner/name")
	}
	if !strings.Contains(repo, "/") {
		return fmt.Errorf("repository %q is not owner/name", repo)
	}
	if err := checkTokenPlausible(token); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "gh", "secret", "set", "GITOPS_REPO_WRITE_TOKEN", "--repo", repo)
	cmd.Stdin = strings.NewReader(token)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("publish GITOPS_REPO_WRITE_TOKEN on %s: %s", repo, firstLine(string(out)))
	}
	return nil
}

// ReposNeedingGitopsToken finds the repositories whose workflows actually write
// to this box's GitOps repository.
//
// DERIVED, never enumerated. ADR-084 says the platform is not told which
// application repositories exist -- they are created, renamed and retired by the
// tenant long after scaffolding -- so a list held anywhere falls behind
// silently. The same argument rules out publishing to every repository in the
// organisation: most of them have nothing to do with this box, and a credential
// that can write a tenant's GitOps repository should not sit in a repository
// that never needed it.
//
// A repository needs the credential exactly when one of its workflows reads
// `secrets.GITOPS_REPO_WRITE_TOKEN` AND names this GitOps repository. Both
// halves matter: the first finds the workflows that will fail without it, and
// the second keeps one organisation's second box from receiving the first box's
// credential.
//
// Read from the workflow files rather than GitHub's code search, which returns
// nothing for private repositories on this plan and would silently discover an
// empty set -- indistinguishable from "no repository needs it".
func ReposNeedingGitopsToken(ctx context.Context, org, gitopsRepo string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "gh", "repo", "list", org,
		"--limit", "500", "--json", "nameWithOwner", "--jq", ".[].nameWithOwner").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list repositories in %s: %s", org, firstLine(string(out)))
	}

	var needing []string
	for _, repo := range strings.Split(string(out), "\n") {
		repo = strings.TrimSpace(repo)
		if repo == "" || repo == gitopsRepo {
			// The GitOps repository is the destination, not a writer.
			continue
		}
		if repoWritesTo(ctx, repo, gitopsRepo) {
			needing = append(needing, repo)
		}
	}
	return needing, nil
}

// repoWritesTo reports whether any workflow in repo both reads the credential
// and names the GitOps repository.
//
// A repository that cannot be read is treated as not needing it. The
// alternative -- failing the whole discovery on one unreadable repository --
// would block a credential every other repository does need, over one the
// operator may not even own.
func repoWritesTo(ctx context.Context, repo, gitopsRepo string) bool {
	listing, err := exec.CommandContext(ctx, "gh", "api",
		"/repos/"+repo+"/contents/.github/workflows", "--jq", ".[].name").CombinedOutput()
	if err != nil {
		return false
	}
	for _, wf := range strings.Split(string(listing), "\n") {
		wf = strings.TrimSpace(wf)
		if wf == "" {
			continue
		}
		body, err := exec.CommandContext(ctx, "gh", "api",
			"/repos/"+repo+"/contents/.github/workflows/"+wf, "--jq", ".content").CombinedOutput()
		if err != nil {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(
			strings.ReplaceAll(strings.TrimSpace(string(body)), "\n", ""))
		if err != nil {
			continue
		}
		text := string(decoded)
		if strings.Contains(text, "secrets.GITOPS_REPO_WRITE_TOKEN") && strings.Contains(text, gitopsRepo) {
			return true
		}
	}
	return false
}

// checkTokenPlausible rejects a value too short to be a GitHub token.
func checkTokenPlausible(token string) error {
	if n := len(strings.TrimSpace(token)); n < 20 {
		return fmt.Errorf("the token read was %d characters, which is too short to be a "+
			"GitHub token -- nothing was published.\n\n"+
			"A paste into a no-echo prompt that does not register looks exactly like this: "+
			"the command succeeds, the secret is created empty, and the build that reads it "+
			"fails on \"Invalid username or token\" as though the credential were wrong", n)
	}
	return nil
}

// SetAdminSecrets sets the two secrets that are generated rather than supplied:
// the administrator of this box's OWN Infisical.
//
// Separate from SetSecrets because the two are set at different moments by
// different commands. Everything else is a credential the operator holds and is
// delivered when the box is created; this pair is derived -- the address from the
// session, the password generated and escrowed -- and cannot exist until the box
// has a cluster to key its escrow artefact by (escrow.Identity takes a box AND a
// cluster, even for a box-scoped artefact).
//
// Create-only in effect: the value written is the one the escrow holds, so a
// re-run sets the same two values rather than rotating a password the instance
// is already using.
func SetAdminSecrets(ctx context.Context, gitOrg, tenantID string, s Secrets) error {
	repo := fmt.Sprintf("%s/%s-gitops", gitOrg, tenantID)
	values := map[string]string{
		"INFISICAL_ADMIN_EMAIL":    strings.TrimSpace(s.InfisicalAdminEmail),
		"INFISICAL_ADMIN_PASSWORD": s.InfisicalAdminPassword,
	}
	for _, name := range []string{"INFISICAL_ADMIN_EMAIL", "INFISICAL_ADMIN_PASSWORD"} {
		if values[name] == "" {
			return fmt.Errorf("%s is empty, and Day-0 creates this box's Infisical with it.\n"+
				"The pair is generated and escrowed together, so an empty one means the\n"+
				"escrow write did not happen", name)
		}
		if err := setSecretWithRetry(ctx, repo, name, values[name]); err != nil {
			return err
		}
		// The NAME, never the value: this is the one credential on the box that
		// nobody has a copy of outside the escrow.
		fmt.Printf("[box] ✓ %s set on %s\n", name, repo)
	}
	return nil
}
