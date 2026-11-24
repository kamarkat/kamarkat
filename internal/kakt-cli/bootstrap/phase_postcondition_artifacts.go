package bootstrap

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// clearFailedCertificateRequests removes the requests that failed while
// the box's intermediate was not yet signed (ADR-117 §6), and resets the Certificates that own them so
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
//  2. For every Certificate that issued through an OpenBao issuer and is not
//     Ready, delete the Certificate AND its Secret. ArgoCD (selfHeal: true)
//     immediately recreates the Certificate from Git with zero failure history,
//     cert-manager generates a new private key (clearing any stale key mismatch),
//     and submits a new CertificateRequest without backoff.
//
// Only OpenBao-issued resources are touched. A pending request is in flight
// and deleting it would restart work that is progressing; an ACME failure is a
// different cause this phase cannot repair.
func (o *Orchestrator) clearFailedCertificateRequests(ctx context.Context, kubeconfig string) error {
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
			if !strings.HasPrefix(issuer, "openbao-") {
				continue
			}
			if reason != "Failed" && reason != "Denied" {
				continue
			}
			if err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
				"delete", "certificaterequest", name, "-n", ns).Run(); err != nil {
				fmt.Printf("[pki-signed] could not clear failed request %s/%s: %v\n", ns, name, err)
				continue
			}
			cleared++
		}
		if cleared > 0 {
			fmt.Printf("[pki-signed] ✓ cleared %d failed certificate request(s)\n", cleared)
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
		if !strings.HasPrefix(issuer, "openbao-") {
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
				fmt.Printf("[pki-signed] could not delete stale secret %s/%s: %v\n", ns, secretName, err)
			}
		}

		// Delete the Certificate. ArgoCD (selfHeal: true) recreates it from Git
		// within seconds: the new object has no status, so failedIssuanceAttempts
		// is nil and cert-manager issues immediately.
		if err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
			"delete", "certificate", name, "-n", ns).Run(); err != nil {
			fmt.Printf("[pki-signed] could not reset certificate %s/%s: %v\n", ns, name, err)
			continue
		}
		reset++
		fmt.Printf("[pki-signed] reset %s/%s (failedIssuanceAttempts=%s) — ArgoCD will recreate it from Git\n", ns, name, failedAttempts)
	}
	if reset > 0 {
		fmt.Printf("[pki-signed] ✓ reset %d certificate(s) stuck in backoff; cert-manager will reissue immediately\n", reset)
	}
	return nil
}
