package capi

import "testing"

// The exact error observed at capi-init must be classified as retryable, and a
// genuine manifest error must not be — retrying that would only delay the real
// failure behind two minutes of backoff.
func TestTransientWebhookErrorClassification(t *testing.T) {
	retryable := []string{
		`Error from server (InternalError): error when creating "STDIN": Internal error occurred: failed calling webhook "vcoreprovider.kb.io": failed to call webhook: Post "https://capi-operator-webhook-service.platform-capi.svc:443/mutate-operator-cluster-x-k8s-io-v1alpha2-coreprovider?timeout=10s": tls: failed to verify certificate: x509: certificate signed by unknown authority`,
		`failed to call webhook: Post "https://...": dial tcp 10.96.0.1:443: connect: connection refused`,
		`Internal error occurred: failed calling webhook "x": no endpoints available for service "y"`,
		`Post "https://...": context deadline exceeded`,
		// Observed at pivot-move: cert-manager's webhook on the home-lab worker,
		// still starting behind a ~200ms link.
		`Internal error occurred: failed calling webhook "webhook.cert-manager.io": failed to call webhook: Post "https://cert-manager-webhook.cert-manager.svc:443/validate?timeout=30s": net/http: TLS handshake timeout`,
	}
	for _, msg := range retryable {
		if !transientWebhookError(msg) {
			t.Errorf("should retry but did not:\n%s", msg)
		}
	}

	fatal := []string{
		`error validating "STDIN": error validating data: unknown field "spec.bogus"`,
		`The CoreProvider "cluster-api" is invalid: spec.version: Invalid value: "vX"`,
		`error: unable to recognize "STDIN": no matches for kind "CoreProvider"`,
	}
	for _, msg := range fatal {
		if transientWebhookError(msg) {
			t.Errorf("should fail fast but was treated as retryable:\n%s", msg)
		}
	}
}
