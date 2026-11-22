package tenant

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AppTemplateDir and WorkloadTemplateDir are the retained templates an
// application is hydrated from. They stay in the repository after the box is rendered
// because, unlike the management cluster's, they are consumed once per
// application rather than once per box.
const (
	AppTemplateDir      = "templates/app"
	WorkloadTemplateDir = "templates/workload"
)

// AppSpec is one application, in one environment.
//
// Not "on one cluster": an application is deployed to every workload cluster
// serving its environment, so it has no destination to declare.
type AppSpec struct {
	// GitopsDir is the tenant repository this application is declared in.
	GitopsDir string
	// Environment is the promotion stage, the directory the declaration sits
	// under, AND what decides where it runs: every cluster declaring this
	// environmentSlug deploys it.
	Environment string
	// AppID is the product, and its isolation unit -- its namespace, database,
	// secret path and network policy all derive from it (ADR-088).
	AppID string
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
// <FLEET_ID> -- none of which were in TOKENS.md.
//
// The cluster name used to be the worst of it: nothing read the declaration
// until the first `kakt app secrets` call, so a wrong cluster sat committed
// until then, and a clusters generator matching nothing reported Healthy while
// generating no Applications. The field is gone and that class of mistake with
// it -- there is nothing here to get wrong about where the application runs.
func AddApp(s AppSpec) ([]string, error) {
	missing := []string{}
	for name, value := range map[string]string{
		"environment": s.Environment,
		"app":         s.AppID,
		"owner-email": s.OwnerEmail,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing: %s", strings.Join(missing, ", "))
	}

	// NO PLACEMENT CHECK, because there is no placement to check. An application
	// is declared in an environment and deployed to every cluster serving it, so
	// what mattered here -- does the named cluster exist, is it a workload
	// cluster, does it serve this environment -- is now a property of the clusters
	// rather than of this declaration.
	//
	// An environment with no cluster yet is allowed: the definition is
	// independent of any cluster's lifecycle, and the boundary ApplicationSets
	// generate nothing until one serves it.

	appDir := filepath.Join(s.GitopsDir, "environments", s.Environment, s.AppID)
	if _, err := os.Stat(appDir); err == nil {
		return nil, fmt.Errorf("%s already exists: an application is declared once per "+
			"environment, and overwriting one would discard the quota, hostnames and "+
			"secret declarations already in it",
			filepath.Join("environments", s.Environment, s.AppID))
	}

	tokens, err := appTokens(s)
	if err != nil {
		return nil, err
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

	workloadFiles, err := writeWorkloads(s, appDir, tokens)
	written = append(written, workloadFiles...)
	if err != nil {
		return written, err
	}

	return written, nil
}

// appTokens is what an application's templates are hydrated with.
//
// One definition, because adding a workload to an existing application hydrates
// the same workload template and must substitute the same values. The owner
// email is only in the application's own values.yaml, so a workload added later
// needs no owner and is given none rather than an empty token -- hydrate refuses
// a file that still carries one, which is how a missing value is caught.
func appTokens(s AppSpec) (map[string]string, error) {
	return map[string]string{
		"<APP_ID>":      s.AppID,
		"<ENVIRONMENT>": s.Environment,
		"<OWNER_EMAIL>": s.OwnerEmail,
	}, nil
}

// writeWorkloads creates one directory and Chart.yaml per workload.
//
// Factored out of AddApp so that adding a workload to an application that
// already exists runs the same code. It used to live only here, and AddApp
// refuses an application whose directory is present -- so the documented way to
// add a workload later, re-running add-app with more --workload flags, could
// only ever report "shop already exists". There was no second path; this is it.
func writeWorkloads(s AppSpec, appDir string, tokens map[string]string) ([]string, error) {
	var written []string
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

// AddWorkloads adds workloads to an application that is already declared.
//
// The application must exist: a workload is a deployable INSIDE one, and
// creating the application as a side effect of naming a workload would write an
// app whose values.yaml nobody filled in. A workload that already exists is left
// exactly as it is, including a chart version a build has pinned -- re-running
// this to add a second workload must not reset the first.
func AddWorkloads(s AppSpec) ([]string, error) {
	if strings.TrimSpace(s.Environment) == "" || strings.TrimSpace(s.AppID) == "" {
		return nil, fmt.Errorf("an environment and an application are required: a workload is " +
			"a directory inside one application in one environment")
	}
	if len(s.Workloads) == 0 {
		return nil, fmt.Errorf("no workload named: pass --workload once per deployable to add")
	}

	appDir := filepath.Join(s.GitopsDir, "environments", s.Environment, s.AppID)
	if _, err := os.Stat(appDir); err != nil {
		return nil, fmt.Errorf("%s is not declared in %s: declare the application first with "+
			"`kakt tenant add-app --env %s --app %s`, which writes the values.yaml a workload "+
			"is deployed from", s.AppID, s.Environment, s.Environment, s.AppID)
	}

	var skipped, add []string
	for _, w := range s.Workloads {
		if _, err := os.Stat(filepath.Join(appDir, w, "Chart.yaml")); err == nil {
			skipped = append(skipped, w)
			continue
		}
		add = append(add, w)
	}
	for _, w := range skipped {
		fmt.Printf("  %s already exists, left as it is\n", w)
	}
	if len(add) == 0 {
		return nil, nil
	}

	tokens, err := appTokens(s)
	if err != nil {
		return nil, err
	}
	return writeWorkloads(AppSpec{GitopsDir: s.GitopsDir, Environment: s.Environment,
		AppID: s.AppID, Workloads: add}, appDir, tokens)
}

// hydrate copies one template file, substituting tokens, and refuses to write a
// file that still carries one.
//
// A missing template is reported as a repository that predates the command
// rather than as a missing file. The templates a repository keeps are copied in
// when the box is rendered and never updated afterwards, so a box rendered before a
// template was renamed still holds the old one -- and "no such file or
// directory" does not tell anyone that.
func hydrate(src, dst string, tokens map[string]string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("this repository has no %s.\n\n"+
				"The templates a repository keeps are written once, when it is rendered,\n"+
				"and are not updated afterwards. A box rendered before an\n"+
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
