package components

import (
	"context"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
)

// escrowEnv are the four values hub-operator reads to reach the escrow, in the
// order a human supplies them. Declared once because three places must agree on
// them: this installer, the operator's Deployment, and the scaffolding that asks.
var escrowEnv = []string{
	"INFISICAL_ESCROW_URL",
	"INFISICAL_ESCROW_CLIENT_ID",
	"INFISICAL_ESCROW_CLIENT_SECRET",
	"INFISICAL_ESCROW_PROJECT_ID",
}

// InstallEscrowCredentials creates the Secret hub-operator reads to reach the
// escrow that holds this box's Infisical master keys (ADR-076).
//
// The escrow is an Infisical the TENANT controls and this cluster does not host --
// Infisical Cloud, or one they run elsewhere. It cannot be the box's own Infisical:
// the keys being escrowed are the keys that decrypt it, and it is unreachable
// exactly when they are needed.
//
// The credentials are the tenant's and stay theirs. ADR-065 says the platform holds
// no secret credential belonging to a tenant, and the master keys are the one that
// unlocks all the others.
func (i *Installer) InstallEscrowCredentials(ctx context.Context) error {
	values := map[string]string{}
	var missing []string
	for _, name := range escrowEnv {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			values[name] = v
		} else {
			missing = append(missing, name)
		}
	}

	if len(missing) == len(escrowEnv) {
		// Refused, not reported. This used to print a notice and continue, on the
		// reasoning that whether a box may be built without an escrow is decided
		// before this point -- and the decision made before this point is
		// RequireEscrow, which runs in a DIFFERENT PROCESS during scaffolding and
		// checks values passed to it as flags. Passing it proves the values were
		// typed, not that they reached the box.
		//
		// They did not, on the local path: scaffolding accepted them, wrote them
		// nowhere, and Day-0 read an empty environment. The notice was printed,
		// nothing failed, and the box ran for weeks with its master keys existing
		// only inside it -- which is the single outcome ADR-076 makes the escrow
		// mandatory to prevent.
		//
		// A gate whose failure mode is a line of output is not a gate. This is the
		// last point at which the absence is still cheap to fix, so it is the
		// point that refuses.
		return fmt.Errorf("no escrow is configured for this box.\n\n"+
			"hub-operator copies this box's Infisical master keys -- the root secret\n"+
			"without which its secret store cannot be decrypted -- to an Infisical you\n"+
			"control. Without it, losing this cluster loses every secret the platform\n"+
			"manages for it, and the escrow cannot be added after the fact (ADR-076).\n\n"+
			"Set these in the environment Day-0 runs in:\n  %s",
			strings.Join(escrowEnv, "\n  "))
	}
	if len(missing) > 0 {
		return fmt.Errorf("the escrow is partially configured; missing %s.\n"+
			"hub-operator reads all four from one Secret, so a partial set produces "+
			"an operator that fails every backup while reporting an escrow exists",
			strings.Join(missing, ", "))
	}

	config, err := clientcmd.BuildConfigFromFlags("", i.Kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	obj := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hub-operator-escrow-credentials",
			Namespace: constants.NamespaceOps,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kakt-cli",
				"app.kubernetes.io/component":  "secret-zero",
				"app.kubernetes.io/part-of":    "hub-operator",
			},
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: values,
	}

	secrets := clientset.CoreV1().Secrets(constants.NamespaceOps)
	if _, err := secrets.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		if _, uerr := secrets.Update(ctx, obj, metav1.UpdateOptions{}); uerr != nil {
			return fmt.Errorf("failed to create or update the escrow credentials: %w", uerr)
		}
		fmt.Println("[bootstrap] ✓ escrow credentials updated")
		return nil
	}
	fmt.Println("[bootstrap] ✓ escrow credentials created")
	return nil
}
