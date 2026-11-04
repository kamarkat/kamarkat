package tenant

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/app"
)

// AppTemplateDir and WorkloadTemplateDir are the retained templates an
// application is hydrated from. They stay in the repository after scaffolding
// because, unlike the management cluster's, they are consumed once per
// application rather than once per box.
const (
	AppTemplateDir      = "templates/app"
	WorkloadTemplateDir = "templates/workload"
)

// AppSpec is one application, in one environment, on one cluster.
type AppSpec struct {
	// GitopsDir is the tenant repository this application is declared in.
	GitopsDir string
	// Environment is the promotion stage, and the directory the declaration
	// sits under. It is not a destination: ClusterName is.
	Environment string
	// AppID is the product, and its isolation unit -- its namespace, database,
	// secret path and network policy all derive from it (ADR-088).
	AppID string
	// ClusterName is the managed cluster this application runs on.
	ClusterName string
	// OwnerEmail is who is paged for it.
	OwnerEmail string
	// Workloads are the deployables inside it, one directory and one Chart.yaml
	// each. An application with none is legal and declares nothing to run yet.
	Workloads []string
}

// AddApp hydrates templates/app and templates/workload into
// environments/<env>/<app>/, and returns the files it wrote.
//
// This was four shell steps and a hand-edit. `mkdir -p`, two `cp`s, then filling
// in tokens the two templates spelled five ways between them -- an application
// was <APP_ID>, <APP> and <APPLICATION>, its owner both <TENANT_ID> and
// <FLEET_ID> -- none of which were in TOKENS.md. The cluster name was the worst
// of it: nothing read the declaration until the first `kakt app secrets` call,
// so a wrong cluster sat committed until then, and a clusters generator that
// matches nothing reports Healthy while generating no Applications.
//
// So the cluster is verified FIRST, before anything is written. Everything this
// command knows how to check, it checks at the point the mistake is made.
func AddApp(s AppSpec) ([]string, error) {
	missing := []string{}
	for name, value := range map[string]string{
		"environment": s.Environment,
		"app":         s.AppID,
		"cluster":     s.ClusterName,
		"owner-email": s.OwnerEmail,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing: %s", strings.Join(missing, ", "))
	}

	// The cluster has to exist, be a workload cluster, and serve this
	// environment. Same check the declaration is read under later, run here so
	// the two cannot disagree.
	if err := app.VerifyTargetCluster(s.GitopsDir, s.Environment, s.ClusterName); err != nil {
		return nil, err
	}

	appDir := filepath.Join(s.GitopsDir, "environments", s.Environment, s.AppID)
	if _, err := os.Stat(appDir); err == nil {
		return nil, fmt.Errorf("%s already exists: an application is declared once per "+
			"environment, and overwriting one would discard the quota, hostnames and "+
			"secret declarations already in it",
			filepath.Join("environments", s.Environment, s.AppID))
	}

	tokens := map[string]string{
		"<APP_ID>":         s.AppID,
		"<ENVIRONMENT>":    s.Environment,
		"<TARGET_CLUSTER>": s.ClusterName,
		"<OWNER_EMAIL>":    s.OwnerEmail,
	}

	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", appDir, err)
	}

	var written []string
	valuesDst := filepath.Join(appDir, "values.yaml")
	if err := hydrate(filepath.Join(s.GitopsDir, AppTemplateDir, "values.yaml"), valuesDst, tokens); err != nil {
		return nil, err
	}
	written = append(written, valuesDst)

	for _, workload := range s.Workloads {
		if strings.TrimSpace(workload) == "" {
			return written, fmt.Errorf("a workload with no name: it is a directory and a chart, and neither can be unnamed")
		}
		workloadDir := filepath.Join(appDir, workload)
		if err := os.MkdirAll(workloadDir, 0o755); err != nil {
			return written, fmt.Errorf("create %s: %w", workloadDir, err)
		}
		workloadTokens := map[string]string{"<WORKLOAD>": workload}
		for k, v := range tokens {
			workloadTokens[k] = v
		}
		dst := filepath.Join(workloadDir, "Chart.yaml")
		if err := hydrate(filepath.Join(s.GitopsDir, WorkloadTemplateDir, "Chart.yaml"), dst, workloadTokens); err != nil {
			return written, err
		}
		written = append(written, dst)
	}

	return written, nil
}

// hydrate copies one template file, substituting tokens, and refuses to write a
// file that still carries one.
//
// A missing template is reported as a repository that predates the command
// rather than as a missing file. The templates a repository keeps are copied in
// at scaffolding and never updated afterwards, so a box scaffolded before a
// template was renamed still holds the old one -- and "no such file or
// directory" does not tell anyone that.
func hydrate(src, dst string, tokens map[string]string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("this repository has no %s.\n\n"+
				"The templates a repository keeps are written once, when it is scaffolded,\n"+
				"and are not updated afterwards. A repository scaffolded before an\n"+
				"application's template was renamed still holds the previous layout -- look\n"+
				"for templates/fleet/, and bring it in line with the platform's current\n"+
				"templates/app/ and templates/workload/ before running this again.", src)
		}
		return fmt.Errorf("read %s: %w", src, err)
	}

	text := string(raw)
	for token, value := range tokens {
		text = strings.ReplaceAll(text, token, value)
	}
	if left := leftoverToken(text); left != "" {
		return fmt.Errorf("%s still carries %s after substitution: this command knows "+
			"the tokens the platform's current template uses, and that is not one of them",
			dst, left)
	}
	return os.WriteFile(dst, []byte(text), 0o644)
}
