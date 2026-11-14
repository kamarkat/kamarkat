package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/browser"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/opstore"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/session"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// newBoxCmd groups what is done to a box from outside it.
func newBoxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "box",
		Short: "Prepare and inspect a box from outside it",
	}
	cmd.AddCommand(newBoxInitCmd())
	return cmd
}

// newBoxInitCmd creates a box: the tenant's repository, with no clusters.
//
// # WHAT A BOX IS, AND WHY IT IS CREATED BEFORE ANY CLUSTER
//
// A box outlives every cluster in it (ADR-111 §1), so the box is the thing that
// is created and a cluster is a declaration added to it afterwards --
// `kakt tenant add-management` for the management cluster, `kakt tenant
// add-cluster` for a workload one.
//
// This used to be one command, `kakt tenant scaffold`, which created the
// repository AND declared the first management cluster in the same pass. That
// made the box's own identity a field inside that cluster's values file: a
// repository could not exist without a cluster, and the one fact that must
// outlive every cluster was stored in the first one. registry/box.yaml holds it
// now, and this command writes it.
//
// # WHY THE CREDENTIALS ARE COLLECTED HERE, BEFORE ANYTHING IS CREATED
//
// They come from four different consoles, and obtaining them is not the same
// activity as creating a repository. Collected afterwards, a run that failed at a
// credential had already created a repository; collected in the middle, a run
// interrupted while a browser was open left the operator guessing what had
// happened. So this command stages everything first, reports what is still
// outstanding, and creates nothing until the set is complete.
func newBoxInitCmd() *cobra.Command {
	var b boxInit

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a box: the tenant's repository, with no clusters",
		Long: `Create <box>/<tenant>-gitops from the platform template, with no clusters.

Onboarding, as ADR-062 defines it. The repository is created in the TENANT's
organisation and is theirs; the platform holds a repository-scoped credential
that can propose changes to it and nothing else. No cloud credential is involved:
the tenant runs Day-0 against its own account.

Two phases, in this order. First the credentials this box is built with are
staged under ~/.kamarkat/: each one that is missing gets a numbered step saying
what it is for, the page that issues it opens in a browser, and the prompt waits.
Nothing is echoed, and anything already present is skipped. Then, once the set is
complete, the repository is created, pushed, and given those credentials as its
own Actions secrets.

What this box IS -- its boxId, its domain, its escrow, and the defaults its
clusters are declared from -- is written to registry/box.yaml. Its first
management cluster is a separate step:

  kakt tenant add-management --name <cluster>

Two credentials are never asked for. The administrator of the box's own Infisical
takes its address from your session, and its password is generated and escrowed
when the management cluster is declared -- there is nothing to invent or
remember.

--dry-run renders to a local directory and creates nothing, which is how to see
what a tenant would receive before any repository exists.`,
		RunE: b.run,
		// Every refusal here is a paragraph that says what is missing and how to
		// obtain it. Cobra's usage text after one buries it under thirty flag
		// descriptions, and the operator reads the flags rather than the reason.
		SilenceUsage: true,
	}
	b.bind(cmd)
	return cmd
}

// boxInit is everything `kakt box init` takes.
type boxInit struct {
	spec    tenant.Spec
	secrets tenant.Secrets

	workers   int
	resume    string
	noBrowser bool
	noPrompt  bool
	dryRun    bool
	out       string
	template  string
}

func (b *boxInit) bind(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&b.spec.TenantID, "tenant", "", "tenant identifier (required)")
	f.StringVar(&b.spec.GitOrg, "box", "", "the box this repository belongs to: the tenant's git organisation, where it is created (required)")
	f.StringVar(&b.spec.Domain, "domain", "", "the tenant's base domain (required)")
	// The DNS label this box sits under, declared rather than derived from the
	// environment (ADR-051 amendment 2026-09-18). Empty publishes on the apex.
	f.StringVar(&b.spec.Subdomain, "subdomain", "",
		"DNS label every hostname on this box sits under; empty publishes on the apex")
	// No --box-id. A new box MINTS its identity, once, and a continuation reads
	// the identity the box recorded -- so there is no flag that could give two
	// boxes the same one, and none that could rename a box that exists.
	f.StringVar(&b.resume, "resume", "",
		"continue initializing the box in this checkout, reusing the identity it declares")
	f.StringVar(&b.spec.PlatformAdmin, "platform-admin", "",
		"the one human this box accepts for escrow-backed operations, by Infisical's "+
			"account id. Defaults to the signed-in session; pass it only to name somebody else")

	// What a cluster declared in this box starts as. Recorded in box.yaml, and
	// read from there by `kakt tenant add-management`.
	f.StringVar(&b.spec.Provider, "provider", "hetzner",
		"cloud provider, and which credentials this box needs: hetzner, or hybrid for nodes on your own machines")
	f.StringVar(&b.spec.Region, "region", "hel1", "cloud region")
	f.IntVar(&b.workers, "workers", -1,
		"cloud worker nodes a cluster of this box starts with (default: 2). "+
			"Pass 0 to run on nodes on your own premises alone, which then have to "+
			"be there (ADR-075)")

	f.StringVar(&b.spec.BundleVersion, "bundle-version", defaultBundleVersion(),
		"platform bundle version this box starts on (default: the version this CLI carries)")
	f.StringVar(&b.spec.PlatformRepoURL, "platform-repo", "https://github.com/kamarkat/kamarkat", "where the bundle is sourced from")
	f.StringVar(&b.spec.PlatformRevision, "platform-revision", "",
		"the platform branch a development box reads its chart from (default: the branch you are on). Ignored by a released box")
	f.StringVar(&b.spec.BundleRegistry, "bundle-registry", "ghcr.io/kamarkat/charts",
		"registry published bundles are pulled from (no scheme)")
	f.BoolVar(&b.spec.Private, "private", true, "create the repository private")
	f.BoolVar(&b.spec.PrivateRegistry, "private-registry", false,
		"this tenant runs private images; a registry credential becomes required (ADR-066)")

	// The tenant's own credentials. Flags so automation can supply them;
	// prompted when a terminal is present and they are absent.
	//
	// Never defaulted from the platform's environment. A token this machine
	// happens to hold is the platform's, and writing it into a tenant's
	// repository would make the platform's access the tenant's access.
	f.StringVar(&b.secrets.ProviderToken, "provider-token", "",
		"the tenant's cloud API token; their clusters are created with it")
	f.StringVar(&b.secrets.GitopsToken, "gitops-token", "",
		"write access to the tenant's repository, so Day-0 can commit what it generates")
	f.StringVar(&b.secrets.TailscaleAuthkey, "tailscale-authkey", "",
		"a Tailscale auth key; the hybrid provider's control plane and home workers join the tenant's tailnet with it (ADR-046)")
	f.StringVar(&b.secrets.EscrowURL, "escrow-url", "",
		"an Infisical the TENANT controls and this box does not host, holding its master keys (ADR-076). Usually https://app.infisical.com")
	f.StringVar(&b.secrets.EscrowProjectID, "escrow-project-id", "", "the project in that Infisical the backup is written to")
	f.StringVar(&b.secrets.EscrowClientID, "escrow-client-id", "", "a machine identity with write access to that project")
	f.StringVar(&b.secrets.EscrowClientSecret, "escrow-client-secret", "", "the matching machine identity client secret")

	f.BoolVar(&b.noPrompt, "no-prompt", false,
		"never prompt; read every credential from the environment and this operator's store")
	f.BoolVar(&b.noBrowser, "no-browser", false,
		"do not open the page that issues each credential; print the address instead")
	f.StringVar(&b.out, "out", "",
		"the checkout to leave behind. Defaults to ./<repository>, which is the name "+
			"the box mints for itself. Must be empty or absent")
	f.BoolVar(&b.dryRun, "dry-run", false, "render only; create and push nothing")
	f.StringVar(&b.template, "template", "manifests/tenants/gitops-template", "template to render")
	for _, hidden := range []string{"platform-repo", "bundle-registry", "template"} {
		_ = f.MarkHidden(hidden)
	}
}

// run is the two phases: stage every credential, then create the box.
//
// The order is the whole point. Nothing is created until the set is complete, so
// a run that stops leaves a store to re-run against rather than a half-made
// repository in somebody else's organisation.
func (b *boxInit) run(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if b.noBrowser {
		browser.Suppressed = true
	}
	if err := refuseUndeclaredSourceOverride(cmd); err != nil {
		return err
	}

	// NEW OR CONTINUATION, and the operator says which.
	//
	// Not inferred. Credentials staged under ~/.kamarkat/ cannot answer it:
	// they are legitimately shared across several boxes, so their presence says
	// nothing about whether this run means to create one or finish one. And the
	// existence of other `<tenant>-gitops-*` repositories cannot answer it
	// either -- a tenant may hold two boxes on purpose, which is the whole of
	// ADR-111 Revision 9's replacement journey.
	if b.resume != "" {
		return b.continueInit(ctx)
	}
	// Only when actually passed. The sentinel keeps "zero workers" -- a box whose
	// capacity is the tenant's own hardware (ADR-075) -- distinguishable from "no
	// opinion", so the default survives.
	if b.workers >= 0 {
		w := b.workers
		b.spec.Workers = &w
	}
	// A development box reads the platform chart from the branch under test, so
	// the change being tested is the change the cluster reconciles.
	//
	// Taken from the checkout that supplies the TEMPLATE, not from the working
	// directory. A box is initialized in an empty directory, which is never a
	// checkout -- read from there the branch was always undetectable, and the
	// old default ("main") made that silent.
	if b.spec.PlatformRevision == "" {
		if rev, ok := tenant.TemplateRevision(b.template); ok {
			b.spec.PlatformRevision = rev
		}
	}

	// The administrator defaults to WHOEVER IS SIGNED IN. You have already proved
	// who you are with `kakt login`, and the box accepts one human by Infisical's
	// id for that account -- so asking for it again is asking you to copy a value
	// the CLI is holding. The flag stays for the case it is actually for: creating
	// a box somebody else will administer.
	if strings.TrimSpace(b.spec.PlatformAdmin) == "" {
		domain := strings.TrimSpace(b.secrets.EscrowURL)
		if domain == "" {
			domain = defaultEscrowDomain
		}
		if sess, err := session.Current(ctx, domain); err == nil && strings.TrimSpace(sess.UserID) != "" {
			b.spec.PlatformAdmin = sess.UserID
			fmt.Printf("[box] platform administrator: %s (%s)\n", sess.Email, sess.UserID)
		}
	}
	if strings.TrimSpace(b.spec.PlatformAdmin) == "" {
		fmt.Println("! No platform administrator declared, and no session to take one from.")
		fmt.Println("  A box that declares none authorizes no human session: every escrow")
		fmt.Println("  command against it refuses until one is declared.")
		fmt.Printf("  Sign in with `kakt login`, or pass --platform-admin <infisical user id>.\n\n")
	}

	// What this box cannot be created without, asked before anything else
	// happens. Credentials are collected next and the escrow is reached after
	// that, and neither is worth doing for a run that was never going to create
	// a box.
	//
	// Validated against a minted identity rather than the bare flags, because
	// the three names are required and a new box has them by construction. The
	// mint here is provisional: it is replaced below, immediately before the
	// render that first needs it, so a run refused for a missing flag has not
	// claimed a timestamp.
	if err := b.spec.Validate(); err != nil {
		return err
	}

	skipped, err := b.stage()
	if err != nil {
		return err
	}
	if len(skipped) > 0 {
		// Reported, not fatal, and nothing is created: the answers already given
		// are staged, and re-running asks only for what is still missing.
		return boxInitVerdict(ctx, skipped)
	}

	return b.create(ctx)
}

// stage collects every credential this box is built with, and reports what is
// still outstanding.
func (b *boxInit) stage() ([]string, error) {
	b.secrets.Resolve(b.spec)

	// The platform's own credential first, and separately, because it has a
	// different owner: it creates repositories across every tenant organisation
	// it can reach, it is this operator's, and it is the one credential needed
	// before a repository exists to put anything in.
	_, platformErr := tenant.TokenFromEnv()
	asks := tenant.CredentialAsks(b.spec, &b.secrets)

	fmt.Println("\nWhat this box is built with. Present values are left alone.")
	printState(tenant.PlatformTokenEnv, platformErr == nil,
		"yours, creates the repository in the tenant's organisation")
	for _, a := range asks {
		printState(a.Secret, strings.TrimSpace(*a.Target) != "", a.Why)
	}
	printEscrowState()

	if b.noPrompt || !tenant.Interactive() {
		// Automation: every value comes from the environment or this operator's
		// store, and a missing one is reported by the gate below rather than
		// asked for.
		var missing []string
		for _, a := range tenant.Pending(asks) {
			if !a.Optional {
				missing = append(missing, a.Secret)
			}
		}
		return missing, nil
	}

	if platformErr != nil {
		if _, err := tenant.EnsurePlatformCredential(true); err != nil {
			return nil, err
		}
	}
	pending := tenant.Pending(asks)
	if len(pending) == 0 {
		return nil, nil
	}
	// requireAll=false: this command is meant to be re-run, so an answer left
	// empty is recorded and reported rather than ending the session and losing
	// the answers given before it.
	return tenant.Collect(pending, false)
}

// create makes the repository and gives it the credentials it was built with.
func (b *boxInit) create(ctx context.Context) error {
	// The escrow, before anything exists. ADR-076 makes it mandatory -- a box
	// that would have nowhere to keep its master keys is refused -- and checking
	// it here means the refusal costs nothing, where it used to arrive after a
	// repository had been created.
	if err := b.secrets.RequireEscrow(); err != nil {
		return err
	}
	// Usable, not merely supplied. An Infisical project has a TYPE, and only a
	// secret-manager project accepts what an escrow does; a kms project
	// authenticates perfectly and then refuses every read and write.
	//
	// NOT UNDER --dry-run, which renders and creates nothing. This reaches the
	// escrow over the network and authenticates against it, so requiring it to
	// work made a render impossible anywhere the escrow is not reachable --
	// including the release gate that checks the published binary carries its
	// platform content, which never got as far as rendering. RequireEscrow above
	// still applies: a box with no escrow DECLARED is refused in either mode,
	// because that is a property of the box and not of this machine's network.
	if !b.dryRun {
		if err := b.secrets.VerifyEscrow(); err != nil {
			return err
		}
	}
	if err := b.secrets.RequireSelectedCapabilities(b.spec); err != nil {
		return err
	}
	if !b.secrets.CompleteFor(b.spec.Provider) {
		w := bufio.NewWriter(os.Stdout)
		tenant.HandoverInstructions(b.spec, b.secrets, w)
		w.Flush()
		return fmt.Errorf("this box is not complete, so nothing was created")
	}

	// The box's escrow, as names, for its own declaration. The store is read
	// first and a flag wins over it, which is the precedence every other value
	// here follows.
	b.spec = b.spec.WithEscrowCoordinates()
	if v := strings.TrimSpace(b.secrets.EscrowURL); v != "" {
		b.spec.EscrowURL = v
	}
	if v := strings.TrimSpace(b.secrets.EscrowProjectID); v != "" {
		b.spec.EscrowProjectID = v
	}
	// AND THE ORGANIZATION THEY BELONG TO, which the box declares so that the
	// administrator it names can be checked. Refused here, where the
	// coordinates have just been resolved and nothing has been minted or
	// created yet, rather than inside Validate -- which runs before them.
	if err := b.spec.RequireEscrowOrganization(); err != nil {
		return err
	}

	// WHERE THE CHECKOUT WILL LAND, checked before an identity is minted.
	//
	// A box's repository is cloned out as a working directory here, and that
	// directory must not land inside anything else: run from a repository's own
	// checkout it becomes untracked content in that repository, which is one
	// `git add .` away from a tenant's box being committed into the platform's
	// source -- or into another box's.
	if err := requireEmptyTarget(b.out, b.spec.TenantID); err != nil {
		return err
	}

	// MINTED HERE, immediately before the identity is first required. One
	// timestamp, three names, and no other call to this function anywhere: a
	// second call would be a second box.
	b.spec = tenant.DeriveBoxIdentity(b.spec.TenantID).Apply(b.spec)

	dir := b.out
	if dir == "" {
		dir = b.spec.RepoName()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fmt.Printf("[box] template: %s\n", tenant.TemplateOrigin(b.template))
	if err := tenant.RenderBox(b.template, dir, b.spec); err != nil {
		return err
	}
	fmt.Printf("[box] rendered %s into %s\n", b.spec.RepoName(), dir)

	if b.dryRun {
		fmt.Println("[box] dry run: no repository created, nothing pushed")
		return printTree(dir)
	}

	cred, err := tenant.EnsurePlatformCredential(!b.noPrompt)
	if err != nil {
		return err
	}
	// The KIND of credential, never its source: naming the source printed the
	// filesystem path of a secret file to the terminal and to whatever collects
	// it.
	fmt.Printf("[box] authenticating as the platform (%s)\n", cred.Kind())

	existed, err := tenant.CreateRepo(ctx, b.spec, cred)
	if err != nil {
		return err
	}
	if existed {
		// A COLLISION, not a resume. This name was minted seconds ago from this
		// second's timestamp, so a repository already holding it belongs to
		// another box -- one initialized in the same second, or one whose
		// identity was reused. Adopting it would give two boxes one escrow
		// scope and one DNS owner id, and picking a different identity now
		// would rename a box whose declaration is already on disk.
		return fmt.Errorf("%s/%s already exists, so this box was not created.\n\n"+
			"Its name was minted from this second, so the repository under it is another\n"+
			"box's. Nothing was pushed and no secret was set.\n\n"+
			"  To create a box, run this again: the next second mints a different identity.\n"+
			"  To continue the box in %s, run:\n\n"+
			"      kakt box init --resume %s\n\n"+
			"A declaration was written to %s. It is this run's, and is not the box the\n"+
			"repository declares -- compare them before you reuse either.",
			b.spec.GitOrg, b.spec.RepoName(), dir, dir, dir)
	}
	fmt.Printf("[box] created %s/%s\n", b.spec.GitOrg, b.spec.RepoName())
	if err := tenant.Publish(ctx, dir, b.spec, cred); err != nil {
		return err
	}
	fmt.Printf("[box] pushed to https://github.com/%s/%s\n", b.spec.GitOrg, b.spec.RepoName())

	if err := tenant.SetSecrets(ctx, b.spec, b.secrets); err != nil {
		return err
	}

	return boxCreatedVerdict(b.spec, dir)
}

// requireEmptyTarget refuses to create a box anywhere but an empty directory.
//
// Without --out the box's checkout is created in the working directory, so that
// directory is where it lands and it has to be empty. With --out the operator
// has named the destination, so only that destination is checked: it may be
// absent, and if it exists it must be empty.
//
// .DS_Store is ignored. It is written by the Finder into any directory a person
// has looked at, and treating a folder as occupied because macOS wrote a
// thumbnail cache into it would refuse an empty folder for no reason.
func requireEmptyTarget(out, tenantID string) error {
	dir := out
	what := "the directory named by --out"
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		dir = cwd
		what = "the working directory"
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// A named destination that does not exist yet is the clearest case
			// there is: nothing can be in the way of it.
			return nil
		}
		return err
	}
	var found []string
	for _, e := range entries {
		if e.Name() == ".DS_Store" {
			continue
		}
		found = append(found, e.Name())
	}
	if len(found) == 0 {
		return nil
	}

	shown := found
	if len(shown) > 6 {
		shown = append(shown[:6:6], fmt.Sprintf("… and %d more", len(found)-6))
	}
	return fmt.Errorf(`a box is created in an empty directory, and %s is not empty.

  %s
  holds: %s

The box's repository is checked out here, so anything already in this directory
is beside it -- and inside a repository's own checkout it becomes untracked
content in that repository, which is one `+"`git add .`"+` away from a tenant's box
being committed into somebody else's source.

  mkdir -p ~/boxes/%s && cd ~/boxes/%s
  kakt box init …

Or name the destination yourself with --out <empty or absent directory>.

Nothing was created: no identity was minted, no repository exists, and no
credential was sent anywhere.`,
		what, dir, strings.Join(shown, ", "), tenantID, tenantID)
}

// continueInit finishes a box that already has a declaration.
//
// IT RENAMES NOTHING. Every name comes from the checkout's own
// registry/box.yaml, so a continuation cannot mint a second identity however
// many times it runs -- which is the whole reason the declaration is written
// before the first remote call.
//
// What it continues is everything after that write: creating the repository,
// pushing it, and setting the credentials on it. Each of those is checked
// against what is already there rather than assumed undone.
func (b *boxInit) continueInit(ctx context.Context) error {
	decl, err := tenant.ReadBoxDecl(b.resume)
	if err != nil {
		return fmt.Errorf("%w\n\nThere is no box to continue in %s. To create one, run "+
			"`kakt box init` without --resume", err, b.resume)
	}
	b.spec = tenant.SpecFromDecl(decl)
	if err := b.spec.Validate(); err != nil {
		return fmt.Errorf("the box declared in %s is incomplete: %w", b.resume, err)
	}
	// Including the organization its escrow belongs to. A continuation reads it
	// from the declaration rather than from the store, because by this point
	// the box has declared one and that declaration is what every later check
	// reads -- a run that filled the gap from this machine's settings would
	// leave the committed file still saying nothing.
	if err := b.spec.RequireEscrowOrganization(); err != nil {
		return fmt.Errorf("the box declared in %s is incomplete: %w", b.resume, err)
	}
	fmt.Printf("[box] continuing %s, declared in %s\n", b.spec.BoxID, b.resume)
	fmt.Printf("[box]   repository %s/%s, management cluster %s\n",
		b.spec.GitOrg, b.spec.RepoName(), b.spec.ManagementCluster)

	// The credentials, read and asked for exactly as a new box asks for them:
	// what a continuation is usually missing is one of these.
	skipped, err := b.stage()
	if err != nil {
		return err
	}
	if len(skipped) > 0 {
		return boxInitVerdict(ctx, skipped)
	}
	if err := b.secrets.RequireEscrow(); err != nil {
		return err
	}
	if err := b.secrets.VerifyEscrow(); err != nil {
		return err
	}
	if err := b.secrets.RequireSelectedCapabilities(b.spec); err != nil {
		return err
	}

	cred, err := tenant.EnsurePlatformCredential(!b.noPrompt)
	if err != nil {
		return err
	}
	fmt.Printf("[box] authenticating as the platform (%s)\n", cred.Kind())

	existed, err := tenant.CreateRepo(ctx, b.spec, cred)
	if err != nil {
		return err
	}
	if !existed {
		fmt.Printf("[box] created %s/%s\n", b.spec.GitOrg, b.spec.RepoName())
	}

	// WHOSE REPOSITORY IS THIS. A continuation may only reuse a repository that
	// declares the box being continued. A name is not an identity, and a
	// repository under this name that declares something else -- or declares
	// nothing readable -- is one this command must not write to.
	hasCommits, err := tenant.RemoteHasCommits(ctx, b.spec, cred)
	if err != nil {
		return err
	}
	switch {
	case !hasCommits:
		if err := tenant.Publish(ctx, b.resume, b.spec, cred); err != nil {
			return err
		}
		fmt.Printf("[box] pushed to https://github.com/%s/%s\n", b.spec.GitOrg, b.spec.RepoName())
	default:
		remote, err := tenant.RemoteBoxID(ctx, b.spec, cred)
		if err != nil {
			return fmt.Errorf("cannot read what %s/%s declares, so this run will not write to "+
				"it: %w", b.spec.GitOrg, b.spec.RepoName(), err)
		}
		if remote == "" {
			return fmt.Errorf("%s/%s has commits and declares no box, so this run will not\n"+
				"write to it. A repository this platform created declares one in %s/%s.",
				b.spec.GitOrg, b.spec.RepoName(), tenant.RegistryDir, tenant.BoxDeclFile)
		}
		if remote != b.spec.BoxID {
			return fmt.Errorf("%s/%s is box %s, and this run is continuing %s.\n\n"+
				"Two identities under one name: nothing was pushed and no secret was set.\n"+
				"Continue the box that repository declares, or create a new one.",
				b.spec.GitOrg, b.spec.RepoName(), remote, b.spec.BoxID)
		}
		fmt.Printf("[box] %s/%s already holds this box; nothing to push\n",
			b.spec.GitOrg, b.spec.RepoName())
	}

	// Set again, not conditionally. A secret cannot be read back, so "already
	// correct" is not a state this can observe -- and setting the value it
	// already holds changes nothing, where skipping one that failed last time
	// leaves a box that cannot bootstrap.
	if err := tenant.SetSecrets(ctx, b.spec, b.secrets); err != nil {
		return err
	}
	return boxCreatedVerdict(b.spec, b.resume)
}

// boxCreatedVerdict says what exists and what declares its first cluster.
func boxCreatedVerdict(spec tenant.Spec, dir string) error {
	fmt.Printf("\n%s/%s is a box, with no clusters.\n\n", spec.GitOrg, spec.RepoName())
	// The identity, printed in full. It was minted rather than chosen, so this
	// is where an operator learns it -- and every escrow command takes the box
	// id, which nothing else on the machine can tell them.
	fmt.Printf("  box id             %s\n", spec.BoxID)
	fmt.Printf("  repository         %s/%s\n", spec.GitOrg, spec.RepoName())
	fmt.Printf("  management cluster %s   (declared next, not yet created)\n", spec.ManagementCluster)
	fmt.Printf("  checkout           %s\n", dir)
	fmt.Printf("\nregistry/box.yaml records all three. Next, declare the management cluster:\n\n")
	fmt.Printf("  cd %s\n", dir)
	fmt.Printf("  kakt tenant add-management\n\n")
	if spec.DevelopmentBox() {
		// A development box is not dispatched, and carries no workflow to
		// dispatch. `kakt tenant add-management` prints the local sequence.
		fmt.Printf("This box pins `development`, so it reads its charts from a branch and is\n")
		fmt.Printf("bootstrapped LOCALLY -- it carries no bootstrap workflow. The declare step\n")
		fmt.Printf("above prints the commands.\n\n")
	} else {
		fmt.Printf("Then commit and push it, and dispatch the repository's bootstrap-cluster\n")
		fmt.Printf("workflow with that cluster name. Day-0 builds it from there.\n\n")
	}
	fmt.Printf("If this run stops before it finishes, continue it with:\n\n")
	fmt.Printf("  kakt box init --resume %s\n", dir)
	return nil
}

// printState is one line of the opening inventory, the way
// scripts/package/flip-public.sh prints one per package: state first, so what is
// outstanding can be read without reading the descriptions.
func printState(name string, present bool, why string) {
	mark, state := "·", "to obtain"
	if present {
		mark, state = "✓", "present"
	}
	fmt.Printf("  %s %-30s %-10s %s\n", mark, name, state, why)
}

// printEscrowState reports the escrow without asking for it.
//
// `kakt escrow init` owns creating it, and this says whether that has happened
// because a box cannot be scaffolded without one and the refusal is nicer here
// than four steps later.
func printEscrowState() {
	have := 0
	for _, n := range []string{
		"INFISICAL_ESCROW_URL", "INFISICAL_ESCROW_PROJECT_ID",
		"INFISICAL_ESCROW_CLIENT_ID", "INFISICAL_ESCROW_CLIENT_SECRET",
	} {
		if escrow.StoredValue(n) != "" {
			have++
		}
	}
	printState("INFISICAL_ESCROW_* (4)", have == 4,
		fmt.Sprintf("where this box's master keys are kept (%d/4)", have))
}

// boxInitVerdict says what remains, and what to run next.
func boxInitVerdict(ctx context.Context, skipped []string) error {
	root, err := opstore.Root()
	if err != nil {
		return err
	}

	escrowComplete := true
	for _, n := range []string{
		"INFISICAL_ESCROW_URL", "INFISICAL_ESCROW_PROJECT_ID",
		"INFISICAL_ESCROW_CLIENT_ID", "INFISICAL_ESCROW_CLIENT_SECRET",
	} {
		if escrow.StoredValue(n) == "" {
			escrowComplete = false
		}
	}

	fmt.Printf("Staged in %s, and NOTHING WAS CREATED.\n\n", root)
	fmt.Printf("Still outstanding: %s\n", strings.Join(skipped, ", "))
	if !escrowComplete {
		// The escrow is not something this command can collect: it is a project
		// somebody has to create, and it has its own command. Said here because a
		// box is refused without one (ADR-076) and finding that out later costs
		// another round trip.
		fmt.Printf("\nThis box also has no escrow, and will not be created without one:\n")
		fmt.Printf("hub-operator copies the master keys that decrypt its secret store to an\n")
		fmt.Printf("Infisical you control, because the box cannot hold them itself.\n\n")
		if _, err := session.Current(ctx, defaultEscrowDomain); err != nil {
			fmt.Printf("  kakt login\n")
		}
		fmt.Printf("  kakt escrow init --name <project-name>\n")
	}
	fmt.Printf("\nRe-run `kakt box init` when you have them; what is set is not asked for\n")
	fmt.Printf("again, and the box is created once the set is complete.\n")
	return nil
}
