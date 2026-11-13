package tenant

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// BoxDeclFile is the box's own declaration, inside RegistryDir.
//
// A box is created before any of its clusters, so what the box IS cannot be a
// field in a cluster's values file. It was one: `boxId` was read out of the
// first management cluster's declaration, which made a repository without a
// cluster unreadable as a box and stored the one fact that must outlive every
// cluster (ADR-111 §1) inside the first one.
const BoxDeclFile = "box.yaml"

// BoxDecl is what a repository says about the box it is.
//
// Everything here is a name, an address or an identifier. No credential is
// declared in a tenant's repository: its secrets are that repository's own
// Actions secrets, and its master keys are in the escrow this addresses.
type BoxDecl struct {
	BoxID    string `yaml:"boxId"`
	TenantID string `yaml:"tenantId"`
	GitOrg   string `yaml:"gitOrg"`
	// GitopsRepo and ManagementCluster are the other two names minted with the
	// boxId. Recorded rather than recomputed: deriving them from the boxId would
	// mean parsing an identity, and a continuation that parsed wrongly would
	// rename a repository or a cluster that already exists.
	GitopsRepo        string `yaml:"gitopsRepo"`
	ManagementCluster string `yaml:"managementClusterName"`

	Domain    string `yaml:"domain"`
	Subdomain string `yaml:"subdomain"`

	// What a cluster declared in this box starts as, unless its own declaration
	// says otherwise.
	Provider string `yaml:"provider"`
	Region   string `yaml:"region"`
	// Workers is a pointer because zero is a real answer -- a box whose capacity
	// comes from the tenant's own hardware (ADR-075) -- and indistinguishable
	// from "unset" otherwise.
	Workers *int `yaml:"workers"`

	BundleVersion   string `yaml:"bundleVersion"`
	PlatformRepoURL string `yaml:"platformRepoUrl"`
	BundleRegistry  string `yaml:"bundleRegistry"`
	// PlatformRevision is the platform branch a development box reads its charts
	// from, and empty on a released box.
	PlatformRevision string `yaml:"platformRevision"`

	Escrow struct {
		URL            string `yaml:"url"`
		ProjectID      string `yaml:"projectId"`
		OrganizationID string `yaml:"organizationId"`
	} `yaml:"escrow"`

	PlatformAdminIdentity string `yaml:"platformAdminIdentity"`
}

// ReadBoxDecl reads the box a gitops repository declares.
//
// The error says what the directory is rather than what the reader wanted: every
// tenant command runs inside a box's repository, and "no box.yaml" is almost
// always "this is not that repository" rather than a damaged box.
func ReadBoxDecl(gitopsDir string) (BoxDecl, error) {
	path := filepath.Join(gitopsDir, RegistryDir, BoxDeclFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return BoxDecl{}, fmt.Errorf("%s is not a box: it has no %s.\n\n"+
			"A box declares itself in that file -- its boxId, its domain, its escrow and\n"+
			"the defaults its clusters are declared from. Run this inside a box's gitops\n"+
			"repository, or pass --gitops-dir.",
			gitopsDir, filepath.Join(RegistryDir, BoxDeclFile))
	}
	var b BoxDecl
	if err := yaml.Unmarshal(raw, &b); err != nil {
		return BoxDecl{}, fmt.Errorf("read %s: %w", path, err)
	}
	if strings.TrimSpace(b.BoxID) == "" {
		return BoxDecl{}, fmt.Errorf("%s declares no boxId.\n\n"+
			"boxId is the box's permanent identity -- it keys the escrow path holding\n"+
			"this box's master keys, the DNS records it owns and its database's archive\n"+
			"prefix. A repository created by this platform always carries one.", path)
	}
	return b, nil
}

// specFor rebuilds the Spec that renders ONE cluster of this box.
//
// The box's declaration is the only source: a cluster added on another machine,
// months later, must be rendered from what the box recorded rather than from
// what that machine happens to have in its environment or its checkout. The
// caller supplies the cluster's own name and whatever it overrides.
func (b BoxDecl) specFor(cluster string, ov ClusterOverrides) Spec {
	s := Spec{
		TenantID:         b.TenantID,
		GitOrg:           b.GitOrg,
		PlatformAdmin:    b.PlatformAdminIdentity,
		Domain:           b.Domain,
		Subdomain:        b.Subdomain,
		Provider:         b.Provider,
		Region:           b.Region,
		Workers:          b.Workers,
		BoxID:            b.BoxID,
		ClusterName:      cluster,
		BundleVersion:    b.BundleVersion,
		PlatformRevision: b.PlatformRevision,
		PlatformRepoURL:  b.PlatformRepoURL,
		BundleRegistry:   b.BundleRegistry,
		EscrowURL:        b.Escrow.URL,
		EscrowProjectID:  b.Escrow.ProjectID,
		EscrowOrgID:      b.Escrow.OrganizationID,
	}
	if v := strings.TrimSpace(ov.Provider); v != "" {
		s.Provider = v
	}
	if v := strings.TrimSpace(ov.Region); v != "" {
		s.Region = v
	}
	if v := strings.TrimSpace(ov.BundleVersion); v != "" {
		s.BundleVersion = v
	}
	if ov.Workers != nil {
		s.Workers = ov.Workers
	}
	return s
}

// ClusterOverrides is what a cluster may differ from its box in.
//
// Deliberately small. A cluster that differed from its box in its domain, its
// escrow or its platform admin would be a second box sharing one repository,
// which is the collision boxId exists to prevent.
type ClusterOverrides struct {
	Provider      string
	Region        string
	BundleVersion string
	Workers       *int
}

// boxClock is the clock identity derivation reads.
//
// A package variable rather than a flag or an environment variable: a box's
// identity must not be settable from outside, because a chosen suffix is a
// second naming scheme and two boxes could then be given the same one
// deliberately. Tests replace it; nothing a user can pass reaches it.
var boxClock = time.Now

// BoxIdentity is everything a new box's names are derived from, resolved once.
//
// ONE TIMESTAMP, THREE NAMES. Each name is unique for a reason of its own, and
// they share a suffix so that a box's resources can be recognised as belonging
// together:
//
//   - boxId keys the escrow path holding this box's master keys and the DNS
//     owner id its records carry. Two boxes sharing it delete each other's TXT
//     records.
//   - the repository is where the box is declared, and a replacement box needs
//     its own (ADR-111 Revision 9 §4).
//   - the management cluster's name keys its own objects and its database's WAL
//     archive prefix (ADR-082), which collides between two boxes sharing one
//     object store. `-mgmt-`, not `-hub-`: the platform's vocabulary is a
//     management cluster and managed clusters, and "hub" is not in it.
//
// The git organisation and the tenant are NOT derived. The organisation exists
// and is not ours to name; the tenant is the customer.
type BoxIdentity struct {
	// Suffix is the Unix second the box was initialized at, as a string. It is
	// an identity, not a lifecycle date: it survives every cluster the box has
	// and says nothing about what the box is doing now.
	Suffix string
	// BoxID is the box's permanent identity.
	BoxID string
	// Repo is the GitOps repository's name, inside the organisation.
	Repo string
	// ManagementCluster is the name the box's first management cluster takes.
	ManagementCluster string
}

// DeriveBoxIdentity mints a new box's identity. Called once, for a new box only.
//
// A continuation reads the identity the box already recorded (ReadBoxDecl) and
// never comes here: a second call would produce a second box, which is the
// failure this function's single caller exists to prevent.
func DeriveBoxIdentity(tenantID string) BoxIdentity {
	suffix := strconv.FormatInt(boxClock().UTC().Unix(), 10)
	t := strings.TrimSpace(tenantID)
	return BoxIdentity{
		Suffix:            suffix,
		BoxID:             t + "-" + suffix,
		Repo:              t + "-gitops-" + suffix,
		ManagementCluster: t + "-mgmt-" + suffix,
	}
}

// Apply writes a minted identity onto the Spec that renders the box.
func (id BoxIdentity) Apply(s Spec) Spec {
	s.BoxID = id.BoxID
	s.GitopsRepo = id.Repo
	s.ManagementCluster = id.ManagementCluster
	return s
}

// SpecFromDecl rebuilds the Spec of a box that already exists.
//
// Every name comes from the declaration, which is why a continuation cannot
// rename anything: there is no derivation on this path.
func SpecFromDecl(b BoxDecl) Spec {
	return Spec{
		TenantID:          b.TenantID,
		GitOrg:            b.GitOrg,
		PlatformAdmin:     b.PlatformAdminIdentity,
		Domain:            b.Domain,
		Subdomain:         b.Subdomain,
		Provider:          b.Provider,
		Region:            b.Region,
		Workers:           b.Workers,
		BoxID:             b.BoxID,
		GitopsRepo:        b.GitopsRepo,
		ManagementCluster: b.ManagementCluster,
		BundleVersion:     b.BundleVersion,
		PlatformRevision:  b.PlatformRevision,
		PlatformRepoURL:   b.PlatformRepoURL,
		BundleRegistry:    b.BundleRegistry,
		EscrowURL:         b.Escrow.URL,
		EscrowProjectID:   b.Escrow.ProjectID,
		EscrowOrgID:       b.Escrow.OrganizationID,
	}
}

// RecordEscrowOrganization fills a box's declarations in with the Infisical
// organization its escrow belongs to, and reports which files changed.
//
// For the boxes that already exist. `organizationId` had one reader and no
// writer, so every box ever rendered declared an empty one -- and an empty one
// used to disable the check that an administrator id is only meaningful inside
// the organization that issued it. That check now refuses instead, which would
// strand a running box that cannot be re-created: its id, its repository, its
// clusters and its escrowed material all stay as they are.
//
// So the gap is CORRECTED rather than the box replaced. A declaration that
// already names a different organization is not an incomplete one: it is a box
// pointed somewhere else, and repointing it is not this function's business.
//
// IT CANNOT CHANGE WHO OWNS THE ESCROW. projectID is the project this run
// authenticated against, and a file is corrected only when it already names
// that same project -- so the organization written is the one that demonstrably
// holds the box's existing escrow, not whichever organization happens to be
// signed in. Without that scope an unbound declaration could be claimed by any
// authenticated organization simply by running this inside the checkout. A file
// naming another project is refused rather than adopted, and no project id, url
// or escrowed material is ever written or moved.
func RecordEscrowOrganization(gitopsDir, projectID, orgID string) ([]string, error) {
	orgID = strings.TrimSpace(orgID)
	projectID = strings.TrimSpace(projectID)
	if orgID == "" {
		return nil, fmt.Errorf("no organization to record")
	}
	if projectID == "" {
		return nil, fmt.Errorf("no escrow project to record an organization for")
	}

	paths := []string{filepath.Join(gitopsDir, RegistryDir, BoxDeclFile)}
	clusters := filepath.Join(gitopsDir, RegistryDir, "clusters")
	entries, err := os.ReadDir(clusters)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", clusters, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			paths = append(paths, filepath.Join(clusters, e.Name(), "values.yaml"))
		}
	}

	var changed []string
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		updated, did, err := setEscrowOrganization(string(raw), projectID, orgID)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if !did {
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, []byte(updated), info.Mode()); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, p); err != nil {
			return nil, err
		}
		changed = append(changed, p)
	}
	return changed, nil
}

// setEscrowOrganization rewrites the one line, leaving the rest of the file
// byte for byte as it was.
//
// Line-wise rather than through the YAML marshaller: these declarations are
// written to be read and edited by the person who owns the box, and a
// round-trip through a marshaller returns a file with every comment gone. The
// key is only matched inside the `escrow:` block, so a field of the same name
// elsewhere in a values file is left alone.
func setEscrowOrganization(src, projectID, orgID string) (string, bool, error) {
	lines := strings.Split(src, "\n")
	inEscrow := false
	declaredProject := ""
	orgLine := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// A top-level key ends whatever block was open and may open this one.
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inEscrow = strings.HasPrefix(trimmed, "escrow:")
			continue
		}
		if !inEscrow {
			continue
		}
		if strings.HasPrefix(trimmed, "projectId:") {
			declaredProject = yamlScalar(trimmed, "projectId:")
			continue
		}
		if !strings.HasPrefix(trimmed, "organizationId:") {
			continue
		}
		value := yamlScalar(trimmed, "organizationId:")
		switch {
		case value == orgID:
			return src, false, nil
		case value != "" && value != "<ESCROW_ORG_ID>":
			return src, false, fmt.Errorf("already declares organization %s, which is not %s.\n\n"+
				"An incomplete declaration is filled in; one that names another organization is\n"+
				"a box pointed somewhere else, and moving it is not something this corrects",
				value, orgID)
		}
		orgLine = i
	}

	if orgLine < 0 {
		return src, false, nil
	}
	// THE OWNERSHIP CHECK. The organization is only written into a declaration
	// that already names the project this run authenticated against. A
	// declaration naming some other project describes an escrow this session has
	// not been shown to hold, and filling its organization in would let an
	// unrelated authority claim the box.
	if declaredProject != projectID {
		return src, false, fmt.Errorf("declares escrow project %s, and this session holds %s.\n\n"+
			"The organization is recorded only for the escrow it was proven against, so a\n"+
			"declaration naming another project is left untouched rather than claimed",
			orDash(declaredProject), projectID)
	}
	line := lines[orgLine]
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	lines[orgLine] = indent + "organizationId: " + orgID
	return strings.Join(lines, "\n"), true, nil
}

// yamlScalar is the value of a `key: value` line, unquoted.
func yamlScalar(trimmed, key string) string {
	return strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, key)), `"'`)
}

// orDash names an absent value in an error, so "declares escrow project " does
// not read as a blank the reader has to guess at.
func orDash(v string) string {
	if v == "" {
		return "none"
	}
	return v
}
