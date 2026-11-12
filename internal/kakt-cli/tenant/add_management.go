package tenant

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// A box is discovered from the repository, never named on the command line.
//
// Every one of these commands runs inside a box's own gitops repository, so the
// box's identity is already written down there -- and asking for it again invites
// the one mistake that cannot be undone. `boxId` keys the escrow path holding this
// box's master keys, the DNS records it owns and its database's archive prefix
// (ADR-111 ss1). A mistyped one does not fail: it silently addresses a different
// box's escrow, or claims another box's DNS ownership.
//
// So the value is read, and a directory that carries no box says so rather than
// defaulting to something plausible.
type Box struct {
	// ID is `boxId`: the logical hub, stable for as long as the box exists.
	ID string
	// Decl is the box's own declaration, which ID is read from and which a
	// cluster declared here is rendered from.
	Decl BoxDecl
	// Management names the management clusters this repository declares, by
	// directory. EMPTY is a valid box -- one created and not yet given a
	// management cluster. More than one is the planned-succession case
	// (ADR-111 ss4, narrowed by Revision 8: several may be DECLARED, one holds
	// infrastructure), where a predecessor and its successor coexist.
	Management []string
}

// managementMarker is the file that makes a cluster directory a management
// cluster.
//
// The root Application is what a management cluster has and a workload cluster
// does not: it is applied to its own control plane and reconciles everything
// else on the box. The tenant repository's own removal check discriminates the
// same way, so the two agree by construction rather than by a list of names kept
// in step.
const managementMarker = "root.yaml"

// boxIDLine matches the declaration rather than the value, so a comment
// mentioning boxId is not mistaken for it.
var boxIDLine = regexp.MustCompile(`(?m)^boxId:\s*"?([A-Za-z0-9][A-Za-z0-9._-]*)"?\s*$`)

// DiscoverBox reads the box a gitops repository declares.
//
// The IDENTITY comes from the box's own declaration (registry/box.yaml) and the
// INCARNATIONS from the directories under registry/clusters/. It used to take
// both from the clusters: `boxId` was regexed out of the first management
// cluster's values.yaml, which made a repository with no clusters unreadable as
// a box -- and stored the one fact that must outlive every cluster (ADR-111 §1)
// inside the first one.
//
// A box with no clusters is therefore a box. `kakt box init` creates one, and
// `kakt tenant add-management` declares its first management cluster.
func DiscoverBox(gitopsDir string) (Box, error) {
	decl, err := ReadBoxDecl(gitopsDir)
	if err != nil {
		return Box{}, err
	}
	mgmt, err := managementClusters(gitopsDir, decl.BoxID)
	if err != nil {
		return Box{}, err
	}
	return Box{ID: decl.BoxID, Decl: decl, Management: mgmt}, nil
}

// managementClusters lists this box's management clusters, and checks that each
// one claims the box it is in.
//
// The check is the one DiscoverBox always made, pointed at the declaration
// instead of at a sibling: two different `boxId` values in one repository are
// two boxes sharing one escrow scope and one DNS owner id, and that is the
// collision boxId exists to prevent. Now that the box declares its own id, a
// mismatch names which of the two is wrong instead of reporting a tie.
func managementClusters(gitopsDir, boxID string) ([]string, error) {
	clusters := filepath.Join(gitopsDir, RegistryDir, "clusters")
	entries, err := os.ReadDir(clusters)
	if err != nil {
		if os.IsNotExist(err) {
			// A box before its first cluster. Not an error: the directory is
			// created with the box and holds only .gitkeep until a cluster is
			// declared.
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", clusters, err)
	}

	var mgmt []string
	var foreign []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(clusters, e.Name(), managementMarker)); err != nil {
			continue
		}
		mgmt = append(mgmt, e.Name())

		values := filepath.Join(clusters, e.Name(), "values.yaml")
		raw, err := os.ReadFile(values)
		if err != nil {
			return nil, fmt.Errorf("%s declares a management cluster with no values: %w",
				e.Name(), err)
		}
		m := boxIDLine.FindSubmatch(raw)
		if m == nil {
			return nil, fmt.Errorf("%s declares no boxId.\n\n"+
				"A management cluster carries the boxId of the box it belongs to, and the\n"+
				"chart reads it from there. This box is %s.", values, boxID)
		}
		if id := string(m[1]); id != boxID {
			foreign = append(foreign, fmt.Sprintf("  %s declares boxId %s", e.Name(), id))
		}
	}

	if len(foreign) > 0 {
		sort.Strings(foreign)
		return nil, fmt.Errorf("this repository is box %s, and %d of its management clusters\n"+
			"claim another box:\n%s\n\n"+
			"Every management cluster here is an incarnation of ONE box and must carry the\n"+
			"same boxId. Two values mean two boxes sharing one escrow scope and one DNS\n"+
			"owner id, which is the collision boxId exists to prevent.",
			boxID, len(foreign), strings.Join(foreign, "\n"))
	}

	sort.Strings(mgmt)
	return mgmt, nil
}

// ManagementCluster is a new incarnation of the box's management cluster.
//
// ADR-111 ss1: the box outlives the cluster. `boxId` stays and `clusterName`
// changes, so a replacement is a new directory beside the old one rather than an
// edit of it -- during a planned succession both exist at once, and the
// predecessor stays demotable until the switchover.
type ManagementCluster struct {
	GitopsDir string
	// Name is the new incarnation's clusterName. It keys this cluster's own
	// object and node names and its database's WAL archive prefix, so it must
	// never be a name the box has used before.
	Name string
	// From is the incarnation this one is rendered from, needed only when the
	// box declares more than one. Meaningless for a box's FIRST management
	// cluster, which is rendered from the template and succeeds nothing.
	From string
	// What this cluster differs from its box in. Empty means "as the box
	// declares", which is the usual answer: registry/box.yaml carries the
	// defaults every cluster of this box starts from.
	Overrides ClusterOverrides
}

// Add declares a management cluster for this box, and returns the paths a tenant
// commits.
//
// TWO SOURCES, BY WHETHER THE BOX HAS ONE YET.
//
//   - The FIRST is rendered from templates/mgmt, using the facts the box
//     recorded in registry/box.yaml. It succeeds nothing, so there is nothing to
//     inherit from.
//   - A SUCCESSOR is rendered from the incarnation already there rather than
//     from the template: that directory is the template with this box's own
//     values in it, so the successor inherits the domain, the provider, the
//     escrow coordinates and every later edit without a second set of tokens to
//     keep in step.
//
// Declaring is not building. ADR-111 Revision 8 allows several management
// clusters to be DECLARED and at most one to hold infrastructure, and the
// bootstrap's own admission check is what enforces that -- so a successor
// declared here cannot be stood up beside a predecessor that still owns servers.
func (m ManagementCluster) Add() (string, []string, error) {
	box, err := DiscoverBox(m.GitopsDir)
	if err != nil {
		return "", nil, err
	}
	if len(box.Management) == 0 {
		// The FIRST cluster's name is the one the box minted with its own
		// identity, so it is not asked for: every name this box owns carries
		// the same suffix, and typing one of them by hand is how they stop
		// agreeing. --name still overrides, for a box being given a first
		// cluster under a name of the operator's choosing.
		if strings.TrimSpace(m.Name) == "" {
			m.Name = box.Decl.ManagementCluster
		}
		if strings.TrimSpace(m.Name) == "" {
			return "", nil, fmt.Errorf("%s declares no managementClusterName, so there is no name "+
				"to give this box's first management cluster. Pass --name.",
				filepath.Join(RegistryDir, BoxDeclFile))
		}
		return m.addFirst(box)
	}
	if strings.TrimSpace(m.Name) == "" {
		return "", nil, fmt.Errorf("--name is required: box %s already declares %s, so a new\n"+
			"management cluster needs a name of its own -- and never one this box has used.",
			box.ID, strings.Join(box.Management, ", "))
	}

	from := m.From
	switch {
	case from == "" && len(box.Management) == 1:
		from = box.Management[0]
	case from == "":
		return "", nil, fmt.Errorf("box %s declares %d management clusters (%s), so --from must\n"+
			"name the one this incarnation succeeds.",
			box.ID, len(box.Management), strings.Join(box.Management, ", "))
	default:
		found := false
		for _, c := range box.Management {
			if c == from {
				found = true
			}
		}
		if !found {
			return "", nil, fmt.Errorf("--from %s is not a management cluster of box %s (%s)",
				from, box.ID, strings.Join(box.Management, ", "))
		}
	}

	if m.Name == from {
		return "", nil, fmt.Errorf("%s is the incarnation this would be rendered from.\n"+
			"A new one needs a name the box has not used: its WAL archive prefix is keyed\n"+
			"on it, and barman refuses a prefix already holding another database's segments.",
			m.Name)
	}
	if m.Name == box.ID {
		return "", nil, fmt.Errorf("%s is this box's boxId, which the first incarnation already\n"+
			"took. Name the cluster, not the box -- the box is %s and stays %s.",
			m.Name, box.ID, box.ID)
	}

	src := filepath.Join(m.GitopsDir, RegistryDir, "clusters", from)
	dst := filepath.Join(m.GitopsDir, RegistryDir, "clusters", m.Name)
	if _, err := os.Stat(dst); err == nil {
		return "", nil, fmt.Errorf("%s is already declared, so this would replace a declaration a\n"+
			"cluster may be reconciling. Pick a name the box has not used.",
			filepath.Join(RegistryDir, "clusters", m.Name))
	}

	written, err := renderIncarnation(src, dst, from, m.Name)
	if err != nil {
		return "", nil, err
	}
	sort.Strings(written)
	rel := make([]string, 0, len(written))
	for _, p := range written {
		r, err := filepath.Rel(m.GitopsDir, p)
		if err != nil {
			return "", nil, err
		}
		rel = append(rel, r)
	}
	return m.Name, rel, nil
}

// renderIncarnation copies a management cluster's declaration under a new name.
func renderIncarnation(src, dst, from, to string) ([]string, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", src, err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, err
	}

	var written []string
	for _, e := range entries {
		// Day-0's own output is NOT carried over.
		//
		// generated/ holds the predecessor's CAPI Cluster, its PKI values, its
		// load balancer's address and its identity. Copying them hands the new
		// control plane a topology describing machines that do not exist, and a
		// certificate authority that belongs to a cluster it is replacing. Day-0
		// writes this directory for the cluster it creates; the Application over
		// it syncs nothing and reports healthy until then.
		if e.IsDir() && e.Name() == "generated" {
			keep := filepath.Join(dst, "generated", ".gitkeep")
			if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(keep, nil, 0o644); err != nil {
				return nil, err
			}
			written = append(written, keep)
			continue
		}
		if e.IsDir() {
			sub, err := renderIncarnation(filepath.Join(src, e.Name()),
				filepath.Join(dst, e.Name()), from, to)
			if err != nil {
				return nil, err
			}
			written = append(written, sub...)
			continue
		}

		raw, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return nil, err
		}
		out := filepath.Join(dst, e.Name())
		if err := os.WriteFile(out, renameIncarnation(raw, from, to), 0o644); err != nil {
			return nil, err
		}
		written = append(written, out)
	}
	return written, nil
}

// boxScoped are the declarations that key on the BOX and must survive a cluster
// being replaced.
//
// Both hold the box's own name, which for a box's first incarnation is the same
// string as the cluster's -- so a replacement that rewrote every occurrence would
// silently re-key them. ADR-111 ss4 is explicit about the second one: record
// ownership belongs to boxId, so a successor ADOPTS the zone's records rather
// than re-keying them, and two owner ids over one zone under `--policy=sync` is
// the hazard the model exists to avoid.
var boxScoped = regexp.MustCompile(`(?m)^\s*(boxId|ownerId):`)

// renameIncarnation rewrites a declaration from one incarnation to another.
func renameIncarnation(raw []byte, from, to string) []byte {
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if boxScoped.MatchString(line) {
			continue
		}
		lines[i] = strings.ReplaceAll(line, from, to)
	}
	return []byte(strings.Join(lines, "\n"))
}

// IncarnationsEverDeclared enumerates every management cluster this repository
// has ever declared, including those whose directory is gone.
//
// `Box.Management` reads the working tree, which is the right answer for what the
// box declares NOW and the wrong one for what it has OWNED. A directory removed
// or renamed -- by a mistaken cleanup, a rebase, or someone tidying up after a
// cluster they believed was destroyed -- takes its incarnation out of that list,
// and an incarnation absent from the list is never asked about at the provider.
// Its servers then become invisible to the duplicate check at exactly the moment
// they matter: a second management cluster is admitted because the first was
// forgotten.
//
// So the repository's own history is read as well. It is box-scoped by
// construction (one repository is one box), authoritative about what was
// declared, needs no credential, and cannot be edited by deleting a file --
// which is the failure this closes. CAPH's label identifies a cluster rather than
// a box, so the identities have to come from the box's own records; this is one
// of them, and nothing here guesses a name from a prefix or a resource.
//
// Names are returned, not verdicts. Whether any of these clusters EXISTS is a
// question only the provider answers.
func IncarnationsEverDeclared(ctx context.Context, gitopsDir string) ([]string, error) {
	// --diff-filter=A over the marker path: every commit that ADDED a management
	// cluster's root Application, whatever happened to it afterwards.
	marker := filepath.Join(RegistryDir, "clusters")
	cmd := exec.CommandContext(ctx, "git", "-C", gitopsDir, "log",
		"--all", "--diff-filter=A", "--name-only", "--pretty=format:",
		"--", marker+"/*/"+managementMarker)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("read this box's declaration history (git log in %s): %w: %s",
			gitopsDir, err, strings.TrimSpace(string(out)))
	}

	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "/")
		// registry/clusters/<name>/root.yaml
		if len(parts) != 4 || parts[3] != managementMarker {
			continue
		}
		name := parts[2]
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// addFirst declares the box's first management cluster, from the template.
//
// WHAT IS DIFFERENT FROM A SUCCESSOR, and it is not only the source directory.
// A successor must take a name the box has never used, because its WAL archive
// prefix is keyed on `clusterName` and barman refuses a prefix already holding
// another database's segments. The first incarnation has the opposite rule: its
// name IS the box's id by convention (`kakt box init` pins boxId to the name the
// first cluster will take), so a name equal to the boxId is the normal answer
// here and a refusal would reject every ordinary box.
//
// Rendered strictly: every cluster token must resolve, because this directory is
// what the box's root Application reconciles and an unsubstituted token in it is
// a cluster that reports healthy while reconciling a literal `<CLUSTER_NAME>`.
func (m ManagementCluster) addFirst(box Box) (string, []string, error) {
	if strings.TrimSpace(m.From) != "" {
		return "", nil, fmt.Errorf("--from %s names an incarnation to succeed, and box %s has no\n"+
			"management cluster yet. This is its first; drop --from.", m.From, box.ID)
	}

	src := filepath.Join(m.GitopsDir, "templates", "mgmt")
	if _, err := os.Stat(src); err != nil {
		return "", nil, fmt.Errorf("%s has no templates/mgmt to render a management cluster from.\n\n"+
			"A box created by `kakt box init` carries it. A repository scaffolded before the\n"+
			"box and its clusters were separated does not, and declares its management\n"+
			"cluster already.", m.GitopsDir)
	}
	dst := filepath.Join(m.GitopsDir, RegistryDir, "clusters", m.Name)

	// ALREADY RENDERED IS A CONTINUATION, not a collision.
	//
	// Declaring the first management cluster does two things -- it renders the
	// declaration, then it generates and escrows this box's Infisical
	// administrator. The second can fail on its own, and it leaves the first
	// done: re-running then refused for a directory this command had just
	// written, and the only way forward was to delete it by hand.
	//
	// Continued only when the declaration is THIS box's own: the name the box
	// declares, carrying the box's id. Anything else is a cluster this command
	// did not write, and is refused.
	resuming := false
	if _, err := os.Stat(dst); err == nil {
		if m.Name != box.Decl.ManagementCluster {
			return "", nil, fmt.Errorf("%s is already declared, so this would replace a declaration a\n"+
				"cluster may be reconciling. Pick a name the box has not used.",
				filepath.Join(RegistryDir, "clusters", m.Name))
		}
		declared, err := os.ReadFile(filepath.Join(dst, "values.yaml"))
		if err != nil {
			return "", nil, fmt.Errorf("%s exists and cannot be read, so this run will not write to "+
				"it: %w", filepath.Join(RegistryDir, "clusters", m.Name), err)
		}
		if mm := boxIDLine.FindSubmatch(declared); mm == nil || string(mm[1]) != box.ID {
			return "", nil, fmt.Errorf("%s is already declared and does not belong to box %s.\n"+
				"Nothing was written. Inspect it before reusing either.",
				filepath.Join(RegistryDir, "clusters", m.Name), box.ID)
		}
		resuming = true
	}

	if !resuming {
		if err := copyDir(src, dst); err != nil {
			return "", nil, fmt.Errorf("render %s from the template: %w", m.Name, err)
		}
		spec := box.Decl.specFor(m.Name, m.Overrides)
		if err := substitute(dst, spec.clusterTokens(), true); err != nil {
			return "", nil, err
		}
	}

	var written []string
	if err := filepath.Walk(dst, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(m.GitopsDir, p)
		if relErr != nil {
			return relErr
		}
		written = append(written, rel)
		return nil
	}); err != nil {
		return "", nil, err
	}
	sort.Strings(written)
	return m.Name, written, nil
}
