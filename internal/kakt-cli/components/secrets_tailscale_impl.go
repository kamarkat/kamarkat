package components

import (
	"context"
	"fmt"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// InstallTailscalePSK creates the tailscale-hybrid-psk Secret used by the hybrid
// provider cell (ADR-046). It carries the Tailscale auth key + CP node hostname
// that the shared ClusterClass Tailscale pre-kubeadm hook consumes
// (contentFrom.secret → /etc/tailscale-authkey + /etc/tailscale-hostname).
//
// This mirrors the GHCR/hetzner Secret Zero pattern: the CLI reads the values
// from k8-secrets (gitignored) and injects them via client-go. Infisical is the
// long-term authority — hub-operator uploads this Secret to Infisical via
// CLISecretMappings, and ESO syncs it back (see secret_mappings.go).
func (i *Installer) InstallTailscalePSK(ctx context.Context, authkey, hostname string) error {
	fmt.Println("[bootstrap] Creating tailscale-hybrid-psk secret...")

	// Load kubeconfig and create clientset
	config, err := clientcmd.BuildConfigFromFlags("", i.Kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	namespace := constants.NamespaceCAPI

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tailscale-hybrid-psk",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "zero-ops-hub-cli",
				"app.kubernetes.io/component":  "secret-zero",
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"authkey":  authkey,
			"hostname": hostname,
		},
	}

	// Try to create, if exists then update
	_, err = clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		// Secret might already exist, try to update
		_, err = clientset.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create or update tailscale-hybrid-psk: %w", err)
		}
		fmt.Println("[bootstrap] ✓ tailscale-hybrid-psk updated")
	} else {
		fmt.Println("[bootstrap] ✓ tailscale-hybrid-psk created")
	}

	return nil
}
