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
// the box's CA was refusing them, and resets the Certificates that own them so
// cert-manager reissues immediately instead of waiting out its backoff.
//
// cert-manager counts failures per Certificate and backs off exponentially: two
// rejections put the next attempt more than an hour out. The count lives on the
// Certificate's status (failedIssuanceAttempts), not on the CertificateRequest,
// so deleting the failed request alone does not reset it. When the next
// CertificateRequest is nil, trigger_controller.go logs "next CertificateRequest
// not available, skipping checking if Certificate matches the CertificateRequest"
// and falls through to the backoff delay — the spec change is never compared, so
// the fact that the fix changed the spec does not help.
//
// Two things must happen:
//
//  1. Delete the failed CertificateRequest(s) — a failed request holds no
//     certificate, and its reason has already been reported by the Certificate
//     it belongs to.
//
//  2. For every Certificate that issued through an Infisical issuer and is not
//     Ready, delete the Certificate AND its Secret. ArgoCD (selfHeal: true)
//     immediately recreates the Certificate from Git with zero failure history,
//     cert-manager generates a new private key (clearing any stale key mismatch),
//     and submits a new CertificateRequest without backoff.
//
// Only Infisical-issued resources are touched. A pending request is in flight
// and deleting it would restart work that is progressing; an ACME failure is a
// different cause this phase cannot repair.
func (o *Orchestrator) clearFailedInfisicalCertificateRequests(ctx context.Context, kubeconfig string) error {
	// Step 1: Delete failed CertificateRequests.
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "certificaterequests.cert-manager.io", "-A",
		"-o", "jsonpath={range .items[*]}{.metadata.namespace}|{.metadata.name}|{.spec.issuerRef.name}|{range .status.conditions[?(@.type=='Ready')]}{.reason}{end};{end}").Output()
	if err == nil {
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
			if reason != "Failed" && reason != "Denied" {
				continue
			}
			if err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
				"delete", "certificaterequest", name, "-n", ns).Run(); err != nil {
				fmt.Printf("[bootstrap-infisical-api] could not clear failed request %s/%s: %v\n", ns, name, err)
				continue
			}
			cleared++
		}
		if cleared > 0 {
			fmt.Printf("[bootstrap-infisical-api] ✓ cleared %d failed certificate request(s)\n", cleared)
		}
	}

	// Step 2: Reset Certificates stuck in backoff by deleting the Certificate
	// and its Secret. ArgoCD selfHeal recreates the Certificate from Git with
	// failedIssuanceAttempts=0, and cert-manager generates a fresh key into a
	// new Secret.
	certOut, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "certificates.cert-manager.io", "-A",
		"-o", "jsonpath={range .items[*]}{.metadata.namespace}|{.metadata.name}|{.spec.issuerRef.name}|{.spec.secretName}|{range .status.conditions[?(@.type=='Ready')]}{.status}{end}|{.status.failedIssuanceAttempts};{end}").Output()
	if err != nil {
		// No cert-manager or no cluster. The CertificateRequest cleanup above
		// is the best-effort path; this is additive.
		return nil
	}

	reset := 0
	for _, entry := range strings.Split(strings.TrimSpace(string(certOut)), ";") {
		parts := strings.Split(strings.TrimSpace(entry), "|")
		if len(parts) != 6 || parts[1] == "" {
			continue
		}
		ns, name, issuer, secretName, ready, failedAttempts := parts[0], parts[1], parts[2], parts[3], parts[4], parts[5]
		if !strings.HasPrefix(issuer, "infisical-") {
			continue
		}
		// Only reset certificates that are not Ready AND have failed attempts.
		// A certificate that is Ready is working. A certificate with zero
		// failures is not in backoff.
		if ready == "True" {
			continue
		}
		if failedAttempts == "" || failedAttempts == "0" {
			continue
		}

		// Delete the Secret first. It may carry a stale key (e.g. EC when the
		// spec now says RSA) that causes SecretMismatch even after backoff clears.
		if secretName != "" {
			if err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
				"delete", "secret", secretName, "-n", ns, "--ignore-not-found").Run(); err != nil {
				fmt.Printf("[bootstrap-infisical-api] could not delete stale secret %s/%s: %v\n", ns, secretName, err)
			}
		}

		// Delete the Certificate. ArgoCD (selfHeal: true) recreates it from Git
		// within seconds: the new object has no status, so failedIssuanceAttempts
		// is nil and cert-manager issues immediately.
		if err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
			"delete", "certificate", name, "-n", ns).Run(); err != nil {
			fmt.Printf("[bootstrap-infisical-api] could not reset certificate %s/%s: %v\n", ns, name, err)
			continue
		}
		reset++
		fmt.Printf("[bootstrap-infisical-api] reset %s/%s (failedIssuanceAttempts=%s) — ArgoCD will recreate it from Git\n", ns, name, failedAttempts)
	}
	if reset > 0 {
		fmt.Printf("[bootstrap-infisical-api] ✓ reset %d certificate(s) stuck in backoff; cert-manager will reissue immediately\n", reset)
	}
	return nil
}
