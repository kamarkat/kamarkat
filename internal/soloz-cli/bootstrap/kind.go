package bootstrap

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

// KindManager manages Kind cluster lifecycle
type KindManager struct {
	ClusterName string
	ConfigPath  string // optional path to kind config (e.g., for extraPortMappings)
}

// kindRemoteAPIPort is the API server port used when bootstrapping against a
// remote docker host. Fixed (not random) so the Mac's kubeconfig can be
// rewritten to a stable, reachable endpoint.
const kindRemoteAPIPort = "6443"

// kindNodeImage is the kind node image used for the ephemeral bootstrap cluster.
// Pinned to the platform's target Kubernetes minor, so a local Kind run tests
// the version the boxes actually get. v1.35.8 is the newest 1.35 node image kind
// publishes; kind does not publish one per upstream patch, so this trails
// KUBERNETES_VERSION (1.35.10) by patch and that is expected.
const kindNodeImage = "kindest/node:v1.35.8"

// Create creates a kind cluster. When the active docker context targets a
// remote host (ssh:// or tcp://), the cluster is created on that host and the
// API server is bound on all interfaces at a fixed port so the local machine
// can reach it over the network.
func (m *KindManager) Create(ctx context.Context) error {
	args := []string{"create", "cluster",
		"--name", m.ClusterName,
	}

	// Detect a remote docker host (ssh:// or tcp:// active context).
	remoteHost, err := remoteDockerHost(ctx)
	if err == nil && remoteHost != "" {
		// Write a kind config that binds the API server on all interfaces at a
		// fixed port AND registers the reachable host as the API server address
		// so kind embeds it in the serving cert SANs (otherwise kubectl rejects
		// the TLS cert: valid for 10.96.0.1/172.18.0.2/0.0.0.0, not <remoteHost>).
		// kind's own --wait is skipped because it waits on the pre-patch
		// endpoint; we wait below.
		cfgPath, cleanup, cfgErr := writeRemoteKindConfig(remoteHost)
		if cfgErr != nil {
			return fmt.Errorf("write remote kind config: %w", cfgErr)
		}
		defer cleanup()
		args = append(args, "--config", cfgPath)
	} else {
		// Local docker: kind can wait on 127.0.0.1 itself.
		args = append(args, "--wait", "2m")
		if m.ConfigPath != "" {
			args = append(args, "--config", m.ConfigPath)
		}
	}

	// Pin a modern node image regardless of kind's built-in default.
	args = append(args, "--image", kindNodeImage)

	cmd := exec.CommandContext(ctx, "kind", args...)
	cmd.Env = dockerSubprocessEnv(ctx)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kind create failed: %w\n%s", err, output)
	}

	// Rewrite the merged kubeconfig so the API server points at the remote
	// host (kind writes 0.0.0.0/127.0.0.1 which is unreachable from here),
	// then wait for the cluster to be ready via the reachable endpoint.
	if remoteHost != "" {
		if err := patchRemoteKindKubeconfig(ctx, m.ClusterName, remoteHost); err != nil {
			return fmt.Errorf("patch remote kind kubeconfig: %w", err)
		}
		if err := waitForRemoteKindReady(ctx, m.ClusterName); err != nil {
			return fmt.Errorf("wait for remote kind cluster: %w", err)
		}
	}

	return nil
}

func (m *KindManager) Delete(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "kind", "delete", "cluster",
		"--name", m.ClusterName,
	)
	cmd.Env = dockerSubprocessEnv(ctx)
	return cmd.Run()
}

func (m *KindManager) Exists(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "kind", "get", "clusters")
	cmd.Env = dockerSubprocessEnv(ctx)
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	clusters := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, cluster := range clusters {
		if cluster == m.ClusterName {
			return true
		}
	}
	return false
}

// dockerSubprocessEnv returns the current environment with DOCKER_DEFAULT_PLATFORM
// normalized to the docker daemon's native architecture. The host shell may export
// DOCKER_DEFAULT_PLATFORM for local workflows (e.g. linux/arm64 on Apple Silicon);
// leaking that into kind/docker subprocesses forces a cross-arch node image that
// Docker runs under QEMU emulation, where seccomp is unsupported and the control
// plane never starts. Pin to the daemon arch (or drop the override) instead.
func dockerSubprocessEnv(ctx context.Context) []string {
	env := os.Environ()
	platform := daemonPlatform(ctx)

	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "DOCKER_DEFAULT_PLATFORM=") {
			continue
		}
		out = append(out, kv)
	}
	if platform != "" {
		out = append(out, "DOCKER_DEFAULT_PLATFORM="+platform)
	}
	return out
}

// daemonPlatform returns the docker daemon's native platform (linux/<arch>) by
// querying the active context's docker daemon, or "" when unknown.
func daemonPlatform(ctx context.Context) string {
	cmd := exec.CommandContext(ctx, "docker", "info", "--format", "{{.Architecture}}")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	switch strings.TrimSpace(string(out)) {
	case "x86_64", "amd64":
		return "linux/amd64"
	case "aarch64", "arm64":
		return "linux/arm64"
	default:
		return ""
	}
}

// remoteDockerHost returns the host of the active docker context when it is
// remote (ssh:// or tcp://), or "" when the context is local (unix://).
func remoteDockerHost(ctx context.Context) (string, error) {
	// 1. Resolve the active context name.
	nameCmd := exec.CommandContext(ctx, "docker", "context", "show")
	nameOut, err := nameCmd.Output()
	if err != nil {
		return "", fmt.Errorf("docker context show: %w", err)
	}
	contextName := strings.TrimSpace(string(nameOut))
	if contextName == "" || contextName == "default" {
		return "", nil
	}

	// 2. Read the docker endpoint host for that context.
	inspectCmd := exec.CommandContext(ctx, "docker", "context", "inspect",
		contextName, "--format", "{{.Endpoints.docker.Host}}")
	hostOut, err := inspectCmd.Output()
	if err != nil {
		return "", fmt.Errorf("docker context inspect %s: %w", contextName, err)
	}
	endpoint := strings.TrimSpace(string(hostOut))
	if endpoint == "" || strings.HasPrefix(endpoint, "unix://") {
		return "", nil
	}

	// 3. Normalize ssh://user@host[:port] or tcp://host[:port] to host[:port].
	return normalizeDockerEndpoint(endpoint), nil
}

// normalizeDockerEndpoint extracts host[:port] from a docker context endpoint
// (ssh://user@host:port or tcp://host:port). The user component and scheme are
// stripped; the port is preserved for tcp endpoints (ssh uses port 22 by
// default, reachable on the host name alone).
func normalizeDockerEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return strings.TrimPrefix(endpoint, "ssh://")
	}

	host := u.Hostname()
	if host == "" {
		return ""
	}

	switch u.Scheme {
	case "ssh":
		// SSH port is 22; the host is sufficient for kubectl connectivity.
		return host
	case "tcp":
		if port := u.Port(); port != "" {
			return host + ":" + port
		}
		return host
	default:
		return host
	}
}

// writeRemoteKindConfig writes a kind config that binds the API server on all
// interfaces at kindRemoteAPIPort and advertises apiServerHost as the API
// server address (added to the serving cert SANs, making kubectl TLS happy).
// Returns the temp path and a cleanup func.
func writeRemoteKindConfig(apiServerHost string) (string, func(), error) {
	content := fmt.Sprintf(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  apiServerAddress: %s
  apiServerPort: %s
`, apiServerHost, kindRemoteAPIPort)

	dir, err := os.MkdirTemp("", "kind-remote-*")
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	return path, cleanup, nil
}

// patchRemoteKindKubeconfig rewrites the kind-<name> context (server, CA,
// client cert) in the default kubeconfig to point at the remote API server so
// kubectl on this machine can reach the API server on the remote docker host.
// It merges the full kind-generated cluster, context and user entries (kind
// does not reliably write them into an existing kubeconfig), so that
// waitForRemoteKindReady's `--context kind-<name>` poll succeeds.
func patchRemoteKindKubeconfig(ctx context.Context, clusterName, remoteHost string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	kubeconfig := filepath.Join(home, ".kube", "config")
	contextName := "kind-" + clusterName

	// Fetch the authoritative kubeconfig kind generated for this cluster.
	// It carries the cluster entry (with CA data), the context and the user
	// (with client cert/key) that `kind create` did not merge into ~/.kube/config.
	getCmd := exec.CommandContext(ctx, "kind", "get", "kubeconfig", "--name", clusterName)
	kindOut, err := getCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kind get kubeconfig %s: %w\n%s", clusterName, err, kindOut)
	}

	kindCfg, err := clientcmd.Load(kindOut)
	if err != nil {
		return fmt.Errorf("parse kind kubeconfig %s: %w", clusterName, err)
	}

	cfg, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		return fmt.Errorf("load kubeconfig %s: %w", kubeconfig, err)
	}

	// Take the kind-generated entries wholesale, then pin the server (and
	// associated SAN-safe address) to the reachable remote host.
	kindCluster, ok := kindCfg.Clusters[contextName]
	if !ok {
		return fmt.Errorf("kind kubeconfig missing cluster %q", contextName)
	}
	kindCluster.Server = fmt.Sprintf("https://%s:%s", remoteHost, kindRemoteAPIPort)
	cfg.Clusters[contextName] = kindCluster

	if ctxObj, ok := kindCfg.Contexts[contextName]; ok {
		cfg.Contexts[contextName] = ctxObj
	} else {
		return fmt.Errorf("kind kubeconfig missing context %q", contextName)
	}

	if userObj, ok := kindCfg.AuthInfos[contextName]; ok {
		cfg.AuthInfos[contextName] = userObj
	} else {
		return fmt.Errorf("kind kubeconfig missing user %q", contextName)
	}

	cfg.CurrentContext = contextName

	if err := clientcmd.WriteToFile(*cfg, kubeconfig); err != nil {
		return fmt.Errorf("write kubeconfig %s: %w", kubeconfig, err)
	}
	return nil
}

// waitForRemoteKindReady polls the remote kind cluster until the API server is
// reachable (and a node is Ready), or the context is cancelled.
func waitForRemoteKindReady(ctx context.Context, clusterName string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	kubeconfig := filepath.Join(home, ".kube", "config")
	contextName := "kind-" + clusterName

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Fail fast if the context is missing or malformed in the kubeconfig
		// (e.g. a prior patchRemoteKindKubeconfig failure) instead of polling
		// forever — this surfaces the diagnostic instead of hanging the boot.
		if _, err := clientcmd.LoadFromFile(kubeconfig); err != nil {
			return fmt.Errorf("wait for remote kind: load kubeconfig %s: %w", kubeconfig, err)
		}

		cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
			"--context", contextName, "get", "nodes",
			"-o", "jsonpath={.items[*].status.conditions[?(@.type==\"Ready\")].status}")
		out, err := cmd.CombinedOutput()
		if err == nil && strings.Contains(string(out), "True") {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
