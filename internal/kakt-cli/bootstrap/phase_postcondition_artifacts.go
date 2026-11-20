package bootstrap

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
)

// generatedIdentityValuesPresent reports whether this box's own Infisical
// identity is on disk where the rest of the bootstrap reads it.
//
// Cheap, read-only, and a point-in-time probe, which is what a postcondition is
// allowed to be. It answers one question: did the effect of
// bootstrap-infisical-api survive to this run? On a fresh runner that means the
// file was committed, because nothing else carries it between runs.
//
// The path is the one secrets_database_impl.go writes
// (registry/clusters/<cluster>/generated/values/infisical-identity.yaml), and
// the two are kept in step by the ADR-045 artifact registry that validates it.
func (o *Orchestrator) generatedIdentityValuesPresent() error {
	if o.GitopsDir == "" {
		// Not a tenant bootstrap. The file lives under manifests/environments in
		// that case and the phase's own writer owns it; nothing here to assert.
		return nil
	}
	if o.ClusterName == "" {
		return fmt.Errorf("no cluster name, so the generated artifacts cannot be located; " +
			"these belong to one cluster rather than to the repository")
	}
	path := filepath.Join(o.GitopsDir, tenant.RegistryDir, "clusters", o.ClusterName,
		"generated", "values", "infisical-identity.yaml")
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("%s is absent: the phase that writes it is recorded complete, "+
			"but the file did not reach git, so this run has no copy of it",
			filepath.Join(tenant.RegistryDir, "clusters", o.ClusterName, "generated",
				"values", "infisical-identity.yaml"))
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("%s is empty, which renders no identity at all", path)
	}
	return nil
}

// certificateIssuersReady reports whether the box's own certificate issuers can
// actually sign.
//
// The Infisical API phase registers the platform's PKI application and its
// certificate profiles, and its postcondition asked only whether a generated
// values file existed on disk. A file that was written months ago, by a run
// against a different Infisical, satisfies that -- so the phase was skipped as
// "already completed, postcondition holds" and the profiles were never
// registered against the Infisical actually running. Every issuer then reported
// `unsupported authentication method: ""` and every certificate renewal failed
// behind it, with the bootstrap reporting the phase as done (2026-11-19).
//
// The file is still checked, by the caller: it proves the artifact was
// committed. This adds the effect the phase exists to produce, which is the
// claim a completion record cannot make from a filename.
//
// Read from the cluster rather than from Infisical's API. Issuer readiness is
// what every consumer of this phase depends on, it is already published as a
// condition, and checking it needs no credential of its own.
func (o *Orchestrator) certificateIssuersReady(ctx context.Context, kubeconfig string) error {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "clusterissuers.infisical-issuer.infisical.com",
		"-o", "jsonpath={range .items[*]}{.metadata.name}={range .status.conditions[?(@.type=='Ready')]}{.status}{end};{end}").Output()
	if err != nil {
		// No CRD, or no cluster to ask. Neither says this phase did not happen,
		// and reporting it as unmet would withdraw the record on every box whose
		// issuers are managed some other way.
		return nil
	}
	var notReady []string
	for _, entry := range strings.Split(strings.TrimSpace(string(out)), ";") {
		name, status, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || name == "" {
			continue
		}
		if status != "True" {
			notReady = append(notReady, name)
		}
	}
	if len(notReady) > 0 {
		return fmt.Errorf("certificate issuer(s) %s cannot sign, so this phase's "+
			"registration of the platform's PKI application and profiles did not take effect "+
			"against the Infisical now running", strings.Join(notReady, ", "))
	}
	return nil
}
