package bootstrap

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/openbao"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// Postconditions for the OpenBao Day-0 phases (ADR-117). Each checks the phase's
// EFFECT, so a completion record that outlived its effect re-runs the phase.

func kubeClient(kubeconfig string) (kubernetes.Interface, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// openBaoPrerequisitesPresent: Secret openbao-seal, the listener CA Secret, and
// the listener trust ConfigMaps in every trust namespace.
func (o *Orchestrator) openBaoPrerequisitesPresent(ctx context.Context, kubeconfig string) error {
	cs, err := kubeClient(kubeconfig)
	if err != nil {
		return err
	}
	if _, err := cs.CoreV1().Secrets(openbao.Namespace).Get(ctx, openbao.SealSecretName, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("%s/%s: %w", openbao.Namespace, openbao.SealSecretName, err)
	}
	if _, err := cs.CoreV1().Secrets("cert-manager").Get(ctx, openbao.ListenerCASecret, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("cert-manager/%s: %w", openbao.ListenerCASecret, err)
	}
	for _, ns := range openbao.ListenerTrustNamespaces {
		if _, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, openbao.ListenerTrustConfig, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("%s/%s: %w", ns, openbao.ListenerTrustConfig, err)
		}
	}
	return nil
}

// recoveryKeyEscrowed: the box escrow holds the PGP-encrypted recovery key.
func (o *Orchestrator) recoveryKeyEscrowed(ctx context.Context) error {
	store, err := escrow.NewEscrowClient(ctx, "")
	if err != nil {
		return err
	}
	v, err := store.RestoreProjectArtifact(ctx, openbao.ArtifactRecoveryKeyEncrypted)
	if err != nil {
		return err
	}
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("the escrow holds no encrypted OpenBao recovery key (%s)", openbao.ArtifactRecoveryKeyEncrypted)
	}
	return nil
}

// intermediateSigned: OpenBao's default issuer chains to the box's root.
func (o *Orchestrator) intermediateSigned(ctx context.Context, kubeconfig string) error {
	store, err := escrow.NewEscrowClient(ctx, "")
	if err != nil {
		return err
	}
	rootPEM, err := store.RestoreProjectArtifact(ctx, openbao.ArtifactRootCertificate)
	if err != nil {
		return err
	}
	cs, err := kubeClient(kubeconfig)
	if err != nil {
		return err
	}
	c, err := openbao.Connect(ctx, cs, kubeconfig)
	if err != nil {
		return err
	}
	defer c.Close()
	ok, err := openbao.IntermediateSigned(ctx, c, []byte(rootPEM))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("OpenBao's issuing intermediate is not signed by this box's root")
	}
	return nil
}
