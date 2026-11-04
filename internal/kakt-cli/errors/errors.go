package errors

import "fmt"

// ErrorCode represents a specific error type
type ErrorCode string

const (
	ErrDockerNotRunning     ErrorCode = "DOCKER_NOT_RUNNING"
	ErrKindNotFound         ErrorCode = "KIND_NOT_FOUND"
	ErrInvalidHetznerToken  ErrorCode = "INVALID_HETZNER_TOKEN"
	ErrClusterExists        ErrorCode = "CLUSTER_EXISTS"
	ErrKindCreateFailed     ErrorCode = "KIND_CREATE_FAILED"
	ErrOperatorInstallFailed ErrorCode = "OPERATOR_INSTALL_FAILED"
	ErrProvisioningTimeout  ErrorCode = "PROVISIONING_TIMEOUT"
	ErrPivotFailed          ErrorCode = "PIVOT_FAILED"
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
		"Check your token:\n  1. Verify HCLOUD_TOKEN environment variable is set\n  2. Ensure token has read/write permissions\n  3. Generate new token at https://console.hetzner.cloud/",
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
