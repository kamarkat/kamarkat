package tenant

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Visibility is how an application's charts or images are published.
type Visibility string

const (
	Public  Visibility = "public"
	Private Visibility = "private"
)

// ParseVisibility accepts "", "public" or "private".
func ParseVisibility(s string) (Visibility, error) {
	switch v := Visibility(strings.ToLower(strings.TrimSpace(s))); v {
	case "", Public, Private:
		return v, nil
	default:
		return "", fmt.Errorf("%q is not a visibility: use public or private", s)
	}
}

// RegistryDecl is what an application declares about its packages, in
// values.yaml `registry:`. Charts and images are independent: a private chart
// needs a credential on the management cluster's GitOps engine, private images a
// pull secret in the application's namespace, and neither implies the other.
type RegistryDecl struct {
	Charts          Visibility `yaml:"charts"`
	Images          Visibility `yaml:"images"`
	ChartRepository string     `yaml:"chartRepository"`
}

// RegistryIO is everything ResolveRegistry needs from outside: questions to the
// person, and the box's escrowed credential. The escrow is touched only once a
// private package is found, so a public application never needs a session.
type RegistryIO interface {
	AskVisibility(kind string, pkgs []Package) (Visibility, error)
	AskPublished(p Package) (bool, error)
	LoadCredential(ctx context.Context) (*RegistryCredential, error)
	RequestCredential(reason string) (*RegistryCredential, error)
	SaveCredential(ctx context.Context, c *RegistryCredential) error
	Say(format string, args ...any)
}

// OtherPrivateApp is another application on this box whose private packages a
// replacement credential must keep reading.
type OtherPrivateApp struct {
	Name     string
	Packages []Package
}

// RegistryInput is one application's declaration as add-app knows it.
type RegistryInput struct {
	Charts          []Package
	DeclaredCharts  Visibility
	DeclaredImages  Visibility
	ChartRepository string
	Others          []OtherPrivateApp
}

// ResolveRegistry decides the application's registry declaration, reusing or
// requesting a credential only when a private package requires one.
//
// Charts first: a private chart is read with the credential, and only a read
// chart tells which images the application deploys. So a credential is settled
// for the charts before the images are looked at, and widened only if an image
// needs more than the charts did.
func ResolveRegistry(ctx context.Context, in RegistryInput, io RegistryIO) (RegistryDecl, error) {
	decl := RegistryDecl{Charts: Public, Images: Public, ChartRepository: in.ChartRepository}
	r := &resolver{ctx: ctx, io: io, others: in.Others}

	// ── Charts: what the registry says, anonymously ─────────────────────────
	var deniedCharts, missingCharts, readableCharts []Package
	for _, c := range in.Charts {
		switch CheckPackage(ctx, c, nil) {
		case AccessOK:
			readableCharts = append(readableCharts, c)
		case AccessMissing:
			missingCharts = append(missingCharts, c)
		case AccessDenied:
			deniedCharts = append(deniedCharts, c)
		default:
			return decl, unavailable(c)
		}
	}
	switch {
	case len(deniedCharts)+len(missingCharts) == 0:
	case in.DeclaredCharts != "":
		decl.Charts = in.DeclaredCharts
	default:
		v, err := io.AskVisibility("charts", append(append([]Package{}, deniedCharts...), missingCharts...))
		if err != nil {
			return decl, err
		}
		decl.Charts = v
	}
	if decl.Charts == Public {
		// Declared or answered public: a refused chart is simply not published.
		deniedCharts = nil
	}
	if decl.Charts == Private {
		if err := r.ensure(deniedCharts); err != nil {
			return decl, err
		}
	}

	// ── Images: from every chart that can now be read ───────────────────────
	readable := append([]Package{}, readableCharts...)
	if !r.cred.empty() {
		for _, c := range deniedCharts {
			if CheckPackage(ctx, c, r.cred) == AccessOK {
				readable = append(readable, c)
			}
		}
	}
	imagesKnown := false
	var privateImages []Package
	for _, c := range readable {
		var ccred *RegistryCredential
		if !containsPkg(readableCharts, c) {
			ccred = r.cred
		}
		images, ok, err := ChartImages(ctx, c, ccred)
		if err != nil {
			return decl, unavailable(c)
		}
		if !ok {
			continue
		}
		imagesKnown = true
		for _, img := range images {
			switch CheckPackage(ctx, img, nil) {
			case AccessOK:
			case AccessDenied, AccessMissing:
				privateImages = append(privateImages, img)
			default:
				return decl, unavailable(img)
			}
		}
	}
	switch {
	case imagesKnown && len(privateImages) == 0:
	case imagesKnown:
		decl.Images = Private
	case in.DeclaredImages != "":
		decl.Images = in.DeclaredImages
	default:
		v, err := io.AskVisibility("images", nil)
		if err != nil {
			return decl, err
		}
		decl.Images = v
	}
	if decl.Images == Private {
		if err := r.ensure(privateImages); err != nil {
			return decl, err
		}
	}

	switch {
	case !r.requested:
	case r.verified:
		io.Say("✓ Registry access verified")
	default:
		io.Say("✓ Registry credential saved. Its access is checked once your packages are published")
	}
	return decl, nil
}

// resolver carries the credential across the chart and image steps, so the
// escrow is read at most once and a person is asked at most once per need.
type resolver struct {
	ctx       context.Context
	io        RegistryIO
	others    []OtherPrivateApp
	cred      *RegistryCredential
	loaded    bool
	requested bool
	verified  bool
	covered   []Package
}

// ensure makes r.cred read every published package in need, reusing the box's
// credential when it does and requesting a replacement when it does not.
func (r *resolver) ensure(need []Package) error {
	for _, p := range need {
		if p.Host != ghcrHost {
			return fmt.Errorf("%s is private on %s; the platform holds registry credentials "+
				"for %s only, so it cannot pull this package", p, p.Host, ghcrHost)
		}
	}
	if !r.loaded {
		r.loaded = true
		c, err := r.io.LoadCredential(r.ctx)
		if err != nil {
			return err
		}
		r.cred = c
	}
	lacking, err := checkCredential(r.ctx, r.cred, need, r.io)
	if err != nil {
		return err
	}
	if !r.cred.empty() && len(lacking) == 0 {
		r.covered = append(r.covered, need...)
		if len(need) > 0 && r.requested {
			r.verified = true
		}
		return nil
	}

	reason := "Your application contains private packages on GitHub Container Registry.\n" +
		"Kamarkat needs read-only access to deploy your application."
	if !r.cred.empty() {
		reason = fmt.Sprintf("The registry credential this box holds cannot read %s.\n"+
			"Provide one that can read it and every private package your other applications use.",
			joinPkgs(lacking))
	}
	all := append(append([]Package{}, r.covered...), need...)
	for _, o := range r.others {
		all = append(all, o.Packages...)
	}
	for attempt := 0; attempt < 3; attempt++ {
		next, err := r.io.RequestCredential(reason)
		if err != nil {
			return err
		}
		bad, err := checkCredential(r.ctx, next, all, nil)
		if err != nil {
			return err
		}
		if len(bad) > 0 {
			reason = fmt.Sprintf("That credential cannot read %s. The box holds one registry "+
				"credential, so it must read every private package its applications use.", joinPkgs(bad))
			continue
		}
		if err := r.io.SaveCredential(r.ctx, next); err != nil {
			return err
		}
		r.cred, r.requested = next, true
		r.covered = all
		r.verified = r.verified || anyPublished(r.ctx, next, all)
		return nil
	}
	return fmt.Errorf("no credential with access to %s was provided", joinPkgs(need))
}

// anyPublished reports whether cred actually read at least one of pkgs, which is
// the only thing that may be reported as verified access.
func anyPublished(ctx context.Context, cred *RegistryCredential, pkgs []Package) bool {
	for _, p := range pkgs {
		if CheckPackage(ctx, p, cred) == AccessOK {
			return true
		}
	}
	return false
}

// checkCredential returns the packages cred cannot read. A package the
// credential is refused for is asked about, because ghcr refuses an
// unpublished package the same way: only a published one makes it a lack of
// access. With io nil every refusal counts as a lack of access.
func checkCredential(ctx context.Context, cred *RegistryCredential, pkgs []Package, io RegistryIO) ([]Package, error) {
	if cred.empty() {
		return pkgs, nil
	}
	var lacking []Package
	for _, p := range pkgs {
		switch CheckPackage(ctx, p, cred) {
		case AccessOK, AccessMissing:
		case AccessDenied:
			if io == nil {
				lacking = append(lacking, p)
				continue
			}
			published, err := io.AskPublished(p)
			if err != nil {
				return nil, err
			}
			if published {
				lacking = append(lacking, p)
			}
		default:
			return nil, unavailable(p)
		}
	}
	return lacking, nil
}

func unavailable(p Package) error {
	return fmt.Errorf("%s could not be reached (rate limited, a server error or no network). "+
		"Nothing was asked or written; run this again once the registry answers", p.Host)
}

func containsPkg(list []Package, p Package) bool {
	for _, q := range list {
		if q.String() == p.String() {
			return true
		}
	}
	return false
}

func joinPkgs(ps []Package) string {
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		names = append(names, p.String())
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// WorkloadCharts reads the chart each workload of an application depends on,
// from environments/<env>/<app>/<workload>/Chart.yaml, and the single chart
// repository they share.
func WorkloadCharts(appDir string) ([]Package, string, error) {
	files, err := filepath.Glob(filepath.Join(appDir, "*", "Chart.yaml"))
	if err != nil {
		return nil, "", err
	}
	var charts []Package
	repo := ""
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, "", err
		}
		var c struct {
			Dependencies []struct {
				Name       string `yaml:"name"`
				Repository string `yaml:"repository"`
			} `yaml:"dependencies"`
		}
		if err := yaml.Unmarshal(raw, &c); err != nil {
			return nil, "", fmt.Errorf("%s: %w", f, err)
		}
		for _, d := range c.Dependencies {
			r := strings.TrimSuffix(strings.TrimPrefix(d.Repository, "oci://"), "/")
			if r == "" {
				continue
			}
			if repo != "" && repo != r {
				return nil, "", fmt.Errorf("the workloads of %s pull charts from two repositories (%s and %s); "+
					"an application's charts share one repository", appDir, repo, r)
			}
			repo = r
			p, err := ParsePackage(r+"/"+d.Name, "chart")
			if err != nil {
				return nil, "", err
			}
			charts = append(charts, p)
		}
	}
	return charts, repo, nil
}

// ReadRegistryDecl reads an application's `registry:` block.
func ReadRegistryDecl(appDir string) (RegistryDecl, error) {
	raw, err := os.ReadFile(filepath.Join(appDir, "values.yaml"))
	if err != nil {
		return RegistryDecl{}, err
	}
	var v struct {
		Registry RegistryDecl `yaml:"registry"`
	}
	if err := yaml.Unmarshal(raw, &v); err != nil {
		return RegistryDecl{}, err
	}
	return v.Registry, nil
}

// OtherPrivateApps lists the other applications in this repository that
// declare private charts, with the chart packages a replacement credential must
// keep reading. Images are not listed: a private image is reached through its
// published chart, which this covers.
func OtherPrivateApps(gitopsDir, env, app string) ([]OtherPrivateApp, error) {
	dirs, err := filepath.Glob(filepath.Join(gitopsDir, "environments", "*", "*", "values.yaml"))
	if err != nil {
		return nil, err
	}
	var out []OtherPrivateApp
	for _, v := range dirs {
		dir := filepath.Dir(v)
		if filepath.Base(dir) == app && filepath.Base(filepath.Dir(dir)) == env {
			continue
		}
		d, err := ReadRegistryDecl(dir)
		if err != nil || (d.Charts != Private && d.Images != Private) {
			continue
		}
		charts, _, err := WorkloadCharts(dir)
		if err != nil {
			continue
		}
		var published []Package
		for _, c := range charts {
			if d.Charts == Private {
				published = append(published, c)
			}
		}
		out = append(out, OtherPrivateApp{
			Name: filepath.Base(filepath.Dir(dir)) + "/" + filepath.Base(dir), Packages: published})
	}
	return out, nil
}

// WriteRegistryDecl replaces the `registry:` block of an application's
// values.yaml, leaving every other line as it is.
func WriteRegistryDecl(appDir string, d RegistryDecl) error {
	p := filepath.Join(appDir, "values.yaml")
	raw, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	block := fmt.Sprintf("registry:\n  charts: %s\n  images: %s\n  chartRepository: %q\n",
		d.Charts, d.Images, d.ChartRepository)
	lines := strings.Split(string(raw), "\n")
	var out []string
	replaced := false
	for i := 0; i < len(lines); i++ {
		if lines[i] == "registry:" {
			out = append(out, strings.TrimSuffix(block, "\n"))
			for i+1 < len(lines) && strings.HasPrefix(lines[i+1], "  ") {
				i++
			}
			replaced = true
			continue
		}
		out = append(out, lines[i])
	}
	s := strings.Join(out, "\n")
	if !replaced {
		s = strings.TrimRight(s, "\n") + "\n\n" + block
	}
	return os.WriteFile(p, []byte(s), 0o644)
}
