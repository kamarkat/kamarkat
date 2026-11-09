package tenant

import (
	"fmt"
	"os"
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
	// Management names the management clusters this repository declares, by
	// directory. More than one is the planned-succession case (ADR-111 ss4),
	// where a predecessor and its successor coexist.
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
func DiscoverBox(gitopsDir string) (Box, error) {
	clusters := filepath.Join(gitopsDir, RegistryDir, "clusters")
	entries, err := os.ReadDir(clusters)
	if err != nil {
		return Box{}, fmt.Errorf("%s is not a box: it has no %s/clusters/ to read a box from.\n"+
			"Run this inside a tenant's gitops repository, or pass --gitops-dir.",
			gitopsDir, RegistryDir)
	}

	var mgmt []string
	ids := map[string][]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		values := filepath.Join(clusters, e.Name(), "values.yaml")
		if _, err := os.Stat(filepath.Join(clusters, e.Name(), managementMarker)); err != nil {
			continue
		}
		mgmt = append(mgmt, e.Name())
		raw, err := os.ReadFile(values)
		if err != nil {
			return Box{}, fmt.Errorf("%s declares a management cluster with no values: %w",
				e.Name(), err)
		}
		m := boxIDLine.FindSubmatch(raw)
		if m == nil {
			return Box{}, fmt.Errorf("%s is not a box: %s declares no boxId.\n\n"+
				"boxId is the box's permanent identity -- it keys the escrow path holding\n"+
				"this box's master keys, the DNS records it owns and its database's archive\n"+
				"prefix. A repository scaffolded by this platform always carries one.",
				gitopsDir, filepath.Join(RegistryDir, "clusters", e.Name(), "values.yaml"))
		}
		id := string(m[1])
		ids[id] = append(ids[id], e.Name())
	}

	if len(mgmt) == 0 {
		return Box{}, fmt.Errorf("%s is not a box: nothing under %s/clusters/ declares a\n"+
			"management cluster (a directory holding %s).\n\n"+
			"A box is created by `kakt tenant scaffold`, which writes its first one.",
			gitopsDir, RegistryDir, managementMarker)
	}
	if len(ids) > 1 {
		var lines []string
		for id, in := range ids {
			lines = append(lines, fmt.Sprintf("  %s: %s", id, strings.Join(in, ", ")))
		}
		sort.Strings(lines)
		return Box{}, fmt.Errorf("this repository declares %d boxes, and a repository is one box:\n%s\n\n"+
			"Every management cluster here is an incarnation of ONE box and must carry the\n"+
			"same boxId. Two values mean two boxes sharing one escrow scope and one DNS\n"+
			"owner id, which is the collision boxId exists to prevent.",
			len(ids), strings.Join(lines, "\n"))
	}

	sort.Strings(mgmt)
	for id := range ids {
		return Box{ID: id, Management: mgmt}, nil
	}
	return Box{}, fmt.Errorf("%s is not a box", gitopsDir)
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
	// box declares more than one.
	From string
}

// Add writes the new incarnation, and returns the paths a tenant commits.
//
// Rendered from the incarnation that is already there rather than from
// templates/mgmt: that directory IS the template with this box's own values in
// it, so the successor inherits the domain, the provider, the escrow coordinates
// and every later edit without a second set of tokens to keep in step. The
// template is consumed at onboarding and does not survive it.
func (m ManagementCluster) Add() ([]string, error) {
	if strings.TrimSpace(m.Name) == "" {
		return nil, fmt.Errorf("--name is required: the new management cluster's name")
	}
	box, err := DiscoverBox(m.GitopsDir)
	if err != nil {
		return nil, err
	}

	from := m.From
	switch {
	case from == "" && len(box.Management) == 1:
		from = box.Management[0]
	case from == "":
		return nil, fmt.Errorf("box %s declares %d management clusters (%s), so --from must\n"+
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
			return nil, fmt.Errorf("--from %s is not a management cluster of box %s (%s)",
				from, box.ID, strings.Join(box.Management, ", "))
		}
	}

	if m.Name == from {
		return nil, fmt.Errorf("%s is the incarnation this would be rendered from.\n"+
			"A new one needs a name the box has not used: its WAL archive prefix is keyed\n"+
			"on it, and barman refuses a prefix already holding another database's segments.",
			m.Name)
	}
	if m.Name == box.ID {
		return nil, fmt.Errorf("%s is this box's boxId, which the first incarnation already\n"+
			"took. Name the cluster, not the box -- the box is %s and stays %s.",
			m.Name, box.ID, box.ID)
	}

	src := filepath.Join(m.GitopsDir, RegistryDir, "clusters", from)
	dst := filepath.Join(m.GitopsDir, RegistryDir, "clusters", m.Name)
	if _, err := os.Stat(dst); err == nil {
		return nil, fmt.Errorf("%s is already declared, so this would replace a declaration a\n"+
			"cluster may be reconciling. Pick a name the box has not used.",
			filepath.Join(RegistryDir, "clusters", m.Name))
	}

	written, err := renderIncarnation(src, dst, from, m.Name)
	if err != nil {
		return nil, err
	}
	sort.Strings(written)
	rel := make([]string, 0, len(written))
	for _, p := range written {
		r, err := filepath.Rel(m.GitopsDir, p)
		if err != nil {
			return nil, err
		}
		rel = append(rel, r)
	}
	return rel, nil
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
