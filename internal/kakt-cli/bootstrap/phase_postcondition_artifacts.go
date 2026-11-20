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

	// ISSUER READY IS NOT ISSUANCE. A ClusterIssuer reports Ready as soon as its
	// Universal Auth credentials authenticate; it knows nothing about whether the
	// application permits the asking identity, whether a profile has API
	// enrollment enabled, or whether the policy allows the common name being
	// requested. All three were wrong on a built box while both issuers reported
	// Ready=True -- so this postcondition held, the phase was skipped, and the
	// release carrying the repair never ran it (2026-11-20).
	//
	// What the phase exists to make possible is a signed certificate, so that is
	// what is checked.
	return o.infisicalCertificatesIssued(ctx, kubeconfig)
}

// infisicalCertificatesIssued reports whether every certificate that issues
// through this box's own CA has one.
//
// Scoped to the Infisical issuers. A public ACME certificate failing is a DNS or
// rate-limit problem and re-running the Infisical registration would do nothing
// about it, so widening this would withdraw a record for work that is fine.
//
// A certificate stuck in cert-manager's backoff counts as not issued, which is
// the intended reading: the box does not work, and the phase that can repair the
// cause is this one.
func (o *Orchestrator) infisicalCertificatesIssued(ctx context.Context, kubeconfig string) error {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "certificates.cert-manager.io", "-A",
		"-o", "jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}|{.spec.issuerRef.name}|{range .status.conditions[?(@.type=='Ready')]}{.status}{end};{end}").Output()
	if err != nil {
		// No cert-manager, or no cluster to ask. Not evidence the phase did not
		// happen.
		return nil
	}

	var unsigned []string
	for _, entry := range strings.Split(strings.TrimSpace(string(out)), ";") {
		parts := strings.Split(strings.TrimSpace(entry), "|")
		if len(parts) != 3 || parts[0] == "" {
			continue
		}
		name, issuer, ready := parts[0], parts[1], parts[2]
		if !strings.HasPrefix(issuer, "infisical-") {
			continue
		}
		if ready != "True" {
			unsigned = append(unsigned, name)
		}
	}
	if len(unsigned) > 0 {
		return fmt.Errorf("certificate(s) %s are not signed, so the box's own CA is not issuing: "+
			"the asking identity's membership of the PKI application, a profile's API enrollment, "+
			"or the policy's allowed common names did not take effect",
			strings.Join(unsigned, ", "))
	}
	return nil
}

// clearFailedInfisicalCertificateRequests removes the requests that failed while
// the box's CA was refusing them.
//
// cert-manager counts failures per Certificate and backs off exponentially: two
// rejections put the next attempt more than an hour out, and it keeps the failed
// CertificateRequest as the record of why. So repairing the issuance path
// changes nothing for that hour -- the box stays broken, the Application stays
// Degraded, and the bootstrap that just fixed the cause times out waiting for
// the effect (2026-11-20, support-agent at failedIssuanceAttempts=2).
//
// Deleting the failed request is what clears it: cert-manager creates a new one
// immediately, against the configuration that now permits it. Nothing is lost --
// a failed request holds no certificate, and its reason has already been
// reported by the Certificate it belongs to.
//
// Only requests against the Infisical issuers, and only failed ones. A pending
// request is in flight and deleting it would restart work that is progressing;
// an ACME failure is a different cause this phase cannot repair.
func (o *Orchestrator) clearFailedInfisicalCertificateRequests(ctx context.Context, kubeconfig string) error {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "certificaterequests.cert-manager.io", "-A",
		"-o", "jsonpath={range .items[*]}{.metadata.namespace}|{.metadata.name}|{.spec.issuerRef.name}|{range .status.conditions[?(@.type=='Ready')]}{.reason}{end};{end}").Output()
	if err != nil {
		return nil
	}

	cleared := 0
	for _, entry := range strings.Split(strings.TrimSpace(string(out)), ";") {
		parts := strings.Split(strings.TrimSpace(entry), "|")
		if len(parts) != 4 || parts[1] == "" {
			continue
		}
		ns, name, issuer, reason := parts[0], parts[1], parts[2], parts[3]
		if !strings.HasPrefix(issuer, "infisical-") {
			continue
		}
		// Failed and Denied are the two terminal reasons. Anything else is in
		// flight or already issued.
		if reason != "Failed" && reason != "Denied" {
			continue
		}
		if err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
			"delete", "certificaterequest", name, "-n", ns).Run(); err != nil {
			// Reported, not fatal. The registration above is the repair; a
			// request that outlives it costs a retry interval, not the box.
			fmt.Printf("[bootstrap-infisical-api] could not clear failed request %s/%s: %v\n", ns, name, err)
			continue
		}
		cleared++
	}
	if cleared > 0 {
		fmt.Printf("[bootstrap-infisical-api] ✓ cleared %d failed certificate request(s), so cert-manager retries now rather than after its backoff\n", cleared)
	}
	return nil
}
