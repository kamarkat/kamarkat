package tenant

import (
	"os"
	"strings"
	"testing"
)

// GitHub's 500 must not cost a whole scaffold.
//
// SetSecrets runs after the repository has been created and pushed to, and there
// is no way back: the error aborts the run with the repository existing, so the
// next attempt fails on "already exists" before reaching this code and the
// operator has to delete the repository by hand. One transient 500 did exactly
// that:
//
//	Error: setting S3_ACCESS_KEY_ID on soloz-io/acme-gitops: exit status 1
//	failed to set secret "S3_ACCESS_KEY_ID": HTTP 500: Server Error
func TestTransientGitHubFailuresAreRetried(t *testing.T) {
	// 5xx, timeouts and reset connections are GitHub's problem: retry.
	for _, out := range []string{
		`failed to set secret "S3_ACCESS_KEY_ID": HTTP 500: Server Error`,
		"HTTP 502: Bad Gateway",
		"HTTP 503: Service Unavailable",
		"error connecting to api.github.com: connection reset by peer",
		"context deadline exceeded",
	} {
		if permanentSecretFailure(out) {
			t.Errorf("treated as permanent, so never retried: %q", out)
		}
	}

	// A rejected token or a missing repository answers the same way four times.
	for _, out := range []string{
		"HTTP 401: Bad credentials",
		"HTTP 403: Resource not accessible by integration",
		"HTTP 404: Not Found",
		"HTTP 422: Validation Failed",
	} {
		if !permanentSecretFailure(out) {
			t.Errorf("treated as transient, so a clear error becomes a slow one: %q", out)
		}
	}
}

// The value must never reach the process table.
func TestSecretValuesGoThroughStdinNotArgv(t *testing.T) {
	src, err := os.ReadFile("handover.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	fn := body[strings.Index(body, "func setSecretWithRetry"):]
	fn = fn[:strings.Index(fn, "\nfunc ")]

	if !strings.Contains(fn, "cmd.Stdin = strings.NewReader(value)") {
		t.Error("the secret value is not passed on stdin")
	}
	if strings.Contains(fn, `"gh", "secret", "set", name, value`) {
		t.Error("the secret value is passed as an argument; anything on the machine can " +
			"read it out of the process table while the call runs")
	}
}

// A partial failure must be reproducible, which random map order prevented.
func TestSecretsAreWrittenInADeterministicOrder(t *testing.T) {
	src, err := os.ReadFile("handover.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	fn := body[strings.Index(body, "func SetSecrets("):]
	fn = fn[:strings.Index(fn, "\nfunc ")]

	if !strings.Contains(fn, "sort.Strings(names)") {
		t.Error("secrets are written in map order, which Go randomises: a failure part-way " +
			"through leaves a different subset set on every run")
	}
	if strings.Contains(fn, "for name, value := range values {") {
		t.Error("still ranging over the map directly")
	}
}
