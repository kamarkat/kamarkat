package errors

import "fmt"

// ErrorCode represents a specific error type
type ErrorCode string

const (
	ErrDockerNotRunning       ErrorCode = "DOCKER_NOT_RUNNING"
	ErrKindNotFound           ErrorCode = "KIND_NOT_FOUND"
	ErrInvalidHetznerToken    ErrorCode = "INVALID_HETZNER_TOKEN"
	ErrProviderAPIRefused     ErrorCode = "PROVIDER_API_REFUSED"
	ErrClusterExists          ErrorCode = "CLUSTER_EXISTS"
	ErrKindCreateFailed       ErrorCode = "KIND_CREATE_FAILED"
	ErrOperatorInstallFailed  ErrorCode = "OPERATOR_INSTALL_FAILED"
	ErrProvisioningTimeout    ErrorCode = "PROVISIONING_TIMEOUT"
	ErrPivotFailed            ErrorCode = "PIVOT_FAILED"
	ErrComponentInstallFailed ErrorCode = "COMPONENT_INSTALL_FAILED"
)

// BootstrapError represents an error with remediation steps
type BootstrapError struct {
	Code        ErrorCode
	Message     string
	Remediation string
	Cause       error
}

func (e *BootstrapError) Error() string {
	msg := fmt.Sprintf("[%s] %s", e.Code, e.Message)
	if e.Remediation != "" {
		msg += fmt.Sprintf("\n\nRemediation:\n%s", e.Remediation)
	}
	if e.Cause != nil {
		msg += fmt.Sprintf("\n\nCause: %v", e.Cause)
	}
	return msg
}

func (e *BootstrapError) Unwrap() error {
	return e.Cause
}

// New creates a new BootstrapError
func New(code ErrorCode, message string, remediation string, cause error) *BootstrapError {
	return &BootstrapError{
		Code:        code,
		Message:     message,
		Remediation: remediation,
		Cause:       cause,
	}
}

// Common error constructors
func DockerNotRunning(cause error) *BootstrapError {
	return New(
		ErrDockerNotRunning,
		"Docker daemon is not running",
		"Start Docker:\n  - macOS: Open Docker Desktop\n  - Linux: sudo systemctl start docker",
		cause,
	)
}

func KindNotFound(cause error) *BootstrapError {
	return New(
		ErrKindNotFound,
		"Kind binary not found in PATH",
		"Install Kind:\n  - macOS: brew install kind\n  - Linux: curl -Lo ./kind https://kind.sigs.k8s.io/dl/latest/kind-linux-amd64 && chmod +x ./kind && sudo mv ./kind /usr/local/bin/kind",
		cause,
	)
}

func InvalidHetznerToken(cause error) *BootstrapError {
	return New(
		ErrInvalidHetznerToken,
		"Invalid or expired Hetzner API token",
		"Check the credential this box is built with -- the escrowed one, since that is the "+
			"only one read:\n"+
			"  1. kakt escrow verify --box <box> --cluster <cluster> --management reports whether it is there\n"+
			"  2. Ensure the token has read and write permissions\n"+
			"  3. Generate a new one at https://console.hetzner.cloud/ and enrol it with\n"+
			"     kakt escrow enroll-provider-credential --gitops-dir . --cluster <cluster> --provider hetzner --rotate < token",
		cause,
	)
}

// ProviderAPIRefused is the provider's API failing for a reason that is NOT the
// credential.
//
// It exists because there was no such reason available. Every failure of the
// token check returned InvalidHetznerToken, so when Hetzner removed
// GET /v1/datacenters after 1 October 2026 and began answering 410
// `deprecated_api_endpoint`, a released platform calling a withdrawn endpoint
// reported the tenant's credential as invalid or expired. Two runs were spent on
// the credential. The API had said exactly what was wrong, in a field this
// buried.
//
// So the cause is quoted and the remediation says whose problem it is: an
// operator cannot fix a withdrawn endpoint, and should not be sent to their
// provider's console to try.
func ProviderAPIRefused(provider string, cause error) *BootstrapError {
	return New(
		ErrProviderAPIRefused,
		fmt.Sprintf("The %s API refused a read this box's credential is entitled to make", provider),
		"The credential was accepted; the call itself was not. The cause below is what the\n"+
			"provider returned.\n\n"+
			"  deprecated_api_endpoint  this platform version calls an endpoint the provider has\n"+
			"                           withdrawn. It is a platform defect, not a credential\n"+
			"                           problem: take a later version, and report it if none\n"+
			"                           carries the fix.\n"+
			"  rate_limit_exceeded      too many calls to this project; retry shortly.\n"+
			"  anything else            the provider's status page, then this cause verbatim.",
		cause,
	)
}

func ClusterExists(clusterName string) *BootstrapError {
	return New(
		ErrClusterExists,
		fmt.Sprintf("Cluster '%s' already exists", clusterName),
		"Options:\n  1. Use --upgrade flag to reconcile existing cluster\n  2. Use different --name\n  3. Teardown existing cluster: zero-ops mgmt teardown --name "+clusterName,
		nil,
	)
}

func ProvisioningTimeout(clusterName string, cause error) *BootstrapError {
	return New(
		ErrProvisioningTimeout,
		fmt.Sprintf("Cluster '%s' provisioning timed out", clusterName),
		"Diagnostics:\n  1. Check cluster status: kubectl get clusters.cluster.x-k8s.io "+clusterName+" -n platform-capi -o yaml\n  2. Check machines: kubectl get machines -n platform-capi\n  3. Check Hetzner Console for VM status\n  4. Use --keep-bootstrap to preserve Kind cluster for debugging",
		cause,
	)
}

func ComponentInstallFailed(component string, cause error) *BootstrapError {
	return New(
		ErrComponentInstallFailed,
		fmt.Sprintf("Failed to install component: %s", component),
		"Diagnostics:\n  1. Check component pods: kubectl get pods -n <namespace>\n  2. Check logs: kubectl logs -n <namespace> <pod-name>\n  3. Verify network connectivity\n  4. Use --debug for verbose output",
		cause,
	)
}
