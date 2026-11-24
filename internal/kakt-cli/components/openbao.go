package components

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/openbao"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// DefaultSigningWait is how long Day-0's pki-signed phase waits for the
// administrator's `kakt pki sign-intermediate` before failing with that reason
// (ADR-117 §6). Bounded, configurable through KAKT_PKI_SIGNING_WAIT.
const DefaultSigningWait = 2 * time.Hour

func (i *Installer) clientset() (kubernetes.Interface, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", i.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return kubernetes.NewForConfig(cfg)
}

// boxEscrow is the box escrow project, opened with the automation identity Day-0
// runs as. It is never custody: no CI identity can open custody.
func boxEscrow(ctx context.Context) (escrow.EscrowClient, error) {
	store, err := escrow.NewEscrowClient(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("open the box escrow: %w", err)
	}
	return store, nil
}

// PrepareOpenBao is Day-0's openbao-prerequisites phase, between boundaries 01
// and 02. Everything OpenBao's first start needs that is not GitOps-declared:
//
//   - the listener CA (cert-manager's openbao-listener issuer) and its public
//     trust ConfigMaps
//   - the seal configuration in force, delivered from the escrow as Secret
//     openbao-seal and recorded in the escrow's delivery history
//   - the escrow-delivered platform credentials (s3-credentials-escrowed and the
//     optional ones), as before
//
// Idempotent: an existing listener CA is kept, and re-delivering the configuration
// in force records nothing new.
func (i *Installer) PrepareOpenBao(ctx context.Context) error {
	cs, err := i.clientset()
	if err != nil {
		return err
	}
	if err := openbao.EnsureListenerCA(ctx, cs, time.Now()); err != nil {
		return err
	}
	store, err := boxEscrow(ctx)
	if err != nil {
		return err
	}
	raw, err := store.RestoreProjectArtifact(ctx, openbao.ArtifactSeal)
	if err != nil {
		return fmt.Errorf("read the seal record from the escrow: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("the escrow holds no OpenBao seal record (%s). It is created by "+
			"`kakt tenant add-management`; run it for this box, then re-run Day-0", openbao.ArtifactSeal)
	}
	rec, err := openbao.ParseSealRecord(raw)
	if err != nil {
		return err
	}
	// A reconstructed hub declares openbao-db in recovery mode from the start, so
	// the configuration in force at its recovery point is delivered, not the latest.
	cfg, _, err := openbao.SealConfigForStore(ctx, openbao.ClusterLive{CS: cs, Kubeconfig: i.Kubeconfig}, rec)
	if err != nil {
		return err
	}
	updated, err := openbao.Deliver(ctx, cs, rec, cfg, time.Now())
	if err != nil {
		return err
	}
	if len(updated.History) != len(rec.History) {
		payload, err := updated.Marshal()
		if err != nil {
			return err
		}
		if err := store.BackupProjectArtifact(ctx, openbao.ArtifactSeal, payload); err != nil {
			return fmt.Errorf("record the seal delivery in the escrow: %w", err)
		}
	}
	fmt.Println("✓ OpenBao listener CA, trust and seal configuration delivered")
	return i.InstallPlatformCredentials(ctx)
}

// OpenBaoTrustBootstrap is Day-0's openbao-trust-bootstrap phase, after boundary
// 03: the PGP-encrypted recovery-key enrolment, as the one-shot identity.
func (i *Installer) OpenBaoTrustBootstrap(ctx context.Context) error {
	cs, err := i.clientset()
	if err != nil {
		return err
	}
	store, err := boxEscrow(ctx)
	if err != nil {
		return err
	}
	if err := openbao.TrustBootstrap(ctx, cs, i.Kubeconfig, store); err != nil {
		return err
	}
	fmt.Println("✓ OpenBao recovery key enrolled (PGP-encrypted, escrowed) and the bootstrap identity retired")
	return nil
}

// ErrSigningPending is the specific failure of the pki-signed phase. It is a
// failed phase, never a reported success (ADR-117 §6).
type ErrSigningPending struct{ Waited time.Duration }

func (e ErrSigningPending) Error() string {
	return fmt.Sprintf("administrator signing pending: the issuing intermediate was not signed within %s.\n\n"+
		"Run, as the box's administrator, with this box's repository checked out:\n\n"+
		"  kakt pki sign-intermediate\n\n"+
		"then re-run this workflow. Phases already complete are skipped, and the\n"+
		"platform is not reported as bootstrapped or accepted until it is signed.", e.Waited)
}

// WaitIntermediateSigned is Day-0's pki-signed phase: wait, bounded, for the
// default issuer to chain to the box's root certificate.
func (i *Installer) WaitIntermediateSigned(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		wait = DefaultSigningWait
	}
	cs, err := i.clientset()
	if err != nil {
		return err
	}
	store, err := boxEscrow(ctx)
	if err != nil {
		return err
	}
	rootPEM, err := store.RestoreProjectArtifact(ctx, openbao.ArtifactRootCertificate)
	if err != nil || strings.TrimSpace(rootPEM) == "" {
		return fmt.Errorf("the box escrow holds no root certificate (%s): run `kakt escrow init` "+
			"as the administrator: %v", openbao.ArtifactRootCertificate, err)
	}
	c, err := openbao.Connect(ctx, cs, i.Kubeconfig)
	if err != nil {
		return err
	}
	defer c.Close()
	deadline := time.Now().Add(wait)
	announced := false
	for {
		signed, err := openbao.IntermediateSigned(ctx, c, []byte(rootPEM))
		if err != nil {
			return err
		}
		if signed {
			fmt.Println("✓ issuing intermediate signed by this box's root")
			return nil
		}
		if !announced {
			fmt.Println("[pki-signed] waiting for the administrator to run `kakt pki sign-intermediate` " +
				"(see the CLI reference). Certificate-dependent components converge once it is signed.")
			announced = true
		}
		if time.Now().After(deadline) {
			return ErrSigningPending{Waited: wait}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}
