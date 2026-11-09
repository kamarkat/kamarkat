package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"text/tabwriter"

	"github.com/kamarkat/kamarkat/internal/platform/escrow"
	"github.com/spf13/cobra"
)

// Cluster API's own names for what it writes, read from the deployed v1.10.0
// (sigs.k8s.io/cluster-api/api/core/v1beta1/common_types.go, util/secret).
//
// The type and the label are not decoration. CAPI finds a cluster's certificate
// authorities by the label, and `clusterctl move` selects on it, so a Secret
// restored without it holds the right bytes and is invisible to everything that
// needs them.
const (
	capiClusterSecretType = "cluster.x-k8s.io/secret"
	capiClusterNameLabel  = "cluster.x-k8s.io/cluster-name"
	capiNamespace         = "platform-capi"
)

// pkiDecision is what the escrow and the hub together say should happen to one
// trust root. It is the whole creation-versus-recovery rule, in one place and
// without a cluster or an escrow to reach, so the refusals can be asserted.
type pkiDecision string

const (
	// pkiRestore: the escrow holds it and this hub does not. The recovery case.
	pkiRestore pkiDecision = "restore"
	// pkiAlreadyPresent: identical bytes already in place. The retry case.
	pkiAlreadyPresent pkiDecision = "already-present"
	// pkiConflict: both hold it and they differ. Either this hub generated its own
	// trust root, or the cluster name belongs to a different cluster than the
	// escrow's -- a reused name. Neither is overwritten.
	pkiConflict pkiDecision = "conflict"
	// pkiMissing: recovery was requested and the escrow holds nothing. Fail closed:
	// generating a replacement would produce an authority the running nodes do not
	// trust, which is worse than stopping.
	pkiMissing pkiDecision = "missing"
)

// decidePKI decides from the material alone, never from intent.
//
// Intent is carried by WHICH PATH RAN -- the declarative provisioning path for a new
// cluster, this command for an existing one -- so this function never has to guess it.
// That separation is deliberate: presence of an escrow artefact or of a Secret means
// opposite things in the two lifecycles, and a rule that read presence as intent is
// exactly how a cluster with a reused name inherits a predecessor's trust roots.
func decidePKI(escrowed, live map[string][]byte, liveFound bool) pkiDecision {
	switch {
	case len(escrowed) == 0:
		return pkiMissing
	case !liveFound:
		return pkiRestore
	case reflect.DeepEqual(live, escrowed):
		return pkiAlreadyPresent
	default:
		return pkiConflict
	}
}

// clusterIsPaused reports whether Cluster API is held off this cluster, by either
// mechanism the contract allows (ADR-111 §3).
func clusterIsPaused(raw []byte) (bool, error) {
	var c struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Paused bool `json:"paused"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return false, err
	}
	if c.Spec.Paused {
		return true, nil
	}
	_, annotated := c.Metadata.Annotations["cluster.x-k8s.io/paused"]
	return annotated, nil
}

type restorePKIOpts struct {
	box, cluster, kubeconfig, kubeContext string
	dryRun                                bool
}

// newClusterRestorePKICmd restores a cluster's escrowed trust roots into a
// replacement hub, before anything is allowed to reconcile that cluster.
//
// THIS COMMAND IS THE RECOVERY INTENT. Creation and recovery are not distinguished
// by looking at whether an escrow artefact or a Secret happens to exist -- both are
// present in situations that mean opposite things, and a cluster whose NAME was reused
// would inherit a predecessor's trust roots on that reasoning. They are
// distinguished by which path is taken: creating a cluster is the ordinary declarative
// path, where Cluster API generates PKI as it always has and nothing here runs;
// recovering one is this command, invoked deliberately against a named box and
// cluster.
//
// Being a command rather than a field is also what keeps the intent exceptional
// (ADR-092): there is no steady-state declaration to forget to remove, so recovery
// cannot persist into normal operation, and the exit is enforced by there being
// nothing to exit from.
func newClusterRestorePKICmd() *cobra.Command {
	var o restorePKIOpts

	cmd := &cobra.Command{
		Use:   "restore-pki",
		Short: "restore a cluster's escrowed certificate authorities into this hub",
		Long: `Writes a cluster's escrowed Cluster API trust roots -- ca, etcd, sa and proxy --
into platform-capi on the hub this kubeconfig points at.

For reconstruction, not for provisioning. A new cluster gets new PKI from Cluster API
on the ordinary declarative path; this restores the PKI an EXISTING cluster is already
running with, so a replacement hub can issue node certificates for it again.

Refuses to run against a cluster Cluster API is not paused on. Fails closed when the
escrow holds nothing. Fails explicitly when a live Secret disagrees with the escrow,
and overwrites neither. Safe to re-run: material already in place is left alone.

Reports fingerprints, never key material.`,
		RunE: func(c *cobra.Command, _ []string) error {
			return runClusterRestorePKI(c.Context(), o)
		},
	}

	f := cmd.Flags()
	f.StringVar(&o.box, "box", "", "the logical hub the escrow is keyed by (required)")
	f.StringVar(&o.cluster, "cluster", "", "the cluster whose PKI to restore (required)")
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "kubeconfig for the hub holding platform-capi")
	f.StringVar(&o.kubeContext, "context", "", "kubeconfig context")
	f.BoolVar(&o.dryRun, "dry-run", false, "compare only; write nothing")
	_ = cmd.MarkFlagRequired("box")
	_ = cmd.MarkFlagRequired("cluster")
	return cmd
}

func runClusterRestorePKI(ctx context.Context, o restorePKIOpts) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(o.box) == "" || strings.TrimSpace(o.cluster) == "" {
		return fmt.Errorf("--box and --cluster are both required: the escrow keys a " +
			"cluster's PKI by the box that owns it (ADR-111)")
	}

	store, err := escrow.NewEscrowClient(ctx, "")
	if err != nil {
		return fmt.Errorf("no escrow to restore from: %w", err)
	}
	id := escrow.ForCluster(o.box, o.cluster)

	if err := o.requirePaused(ctx); err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PURPOSE\tACTION\tFINGERPRINT")

	var failures []string
	restored, present := 0, 0

	for _, purpose := range escrow.SpokePKIPurposes {
		artifact := escrow.SpokePKIArtifact(purpose)
		secretName := o.cluster + "-" + purpose

		payload, err := store.RestoreArtifact(ctx, id, artifact)
		if err != nil {
			// Unreadable is not absent. Reporting it as missing would invite the
			// operator to treat a reachability problem as a lost authority.
			failures = append(failures, fmt.Sprintf("%s: reading the escrow: %v", purpose, err))
			fmt.Fprintf(w, "%s\tFAILED\t-\n", purpose)
			continue
		}

		var escrowed map[string][]byte
		if payload != "" {
			if err := json.Unmarshal([]byte(payload), &escrowed); err != nil {
				failures = append(failures, fmt.Sprintf("%s: the escrowed %s is not readable "+
					"as a secret's data: %v", purpose, artifact, err))
				fmt.Fprintf(w, "%s\tFAILED\t-\n", purpose)
				continue
			}
		}

		live, found, err := o.liveSecret(ctx, secretName)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: reading %s: %v", purpose, secretName, err))
			fmt.Fprintf(w, "%s\tFAILED\t-\n", purpose)
			continue
		}

		fp := "-"
		if len(escrowed) > 0 {
			fp = fingerprintData(escrowed)
		}

		switch decidePKI(escrowed, live, found) {
		case pkiMissing:
			failures = append(failures, fmt.Sprintf("%s: the escrow holds no %s for %s, and "+
				"recovery must not fall back to generating one -- a fresh authority cannot "+
				"talk to the nodes already running under the original",
				purpose, artifact, o.cluster))
			fmt.Fprintf(w, "%s\tMISSING\t-\n", purpose)

		case pkiAlreadyPresent:
			// The retry path, and it is left alone rather than rewritten: a
			// rewrite is one more chance to write something else.
			present++
			fmt.Fprintf(w, "%s\t%s\t%s\n", purpose, pkiAlreadyPresent, fp)

		case pkiConflict:
			failures = append(failures, fmt.Sprintf("%s: %s on this hub does NOT match the "+
				"escrowed %s (live %s, escrowed %s). Refusing to overwrite either: this is a "+
				"regenerated trust root or a reused cluster name, and resolving it by writing "+
				"is how the original is lost", purpose, secretName, artifact,
				fingerprintData(live), fp))
			fmt.Fprintf(w, "%s\tCONFLICT\t%s\n", purpose, fp)

		case pkiRestore:
			if o.dryRun {
				fmt.Fprintf(w, "%s\twould-restore\t%s\n", purpose, fp)
				continue
			}
			err := o.createSecret(ctx, secretName, escrowed)
			if errors.Is(err, errSecretExists) {
				// Something created it in the window -- Velero restoring the same
				// name is the realistic case. Re-read and re-decide, so the
				// conflict rule applies to what is actually there rather than
				// being overwritten by this write.
				live, found, rerr := o.liveSecret(ctx, secretName)
				if rerr != nil {
					failures = append(failures, fmt.Sprintf("%s: %s appeared while "+
						"restoring and could not be re-read: %v", purpose, secretName, rerr))
					fmt.Fprintf(w, "%s\tFAILED\t%s\n", purpose, fp)
					continue
				}
				switch decidePKI(escrowed, live, found) {
				case pkiAlreadyPresent:
					present++
					fmt.Fprintf(w, "%s\t%s\t%s\n", purpose, pkiAlreadyPresent, fp)
				default:
					failures = append(failures, fmt.Sprintf("%s: %s appeared while restoring "+
						"and does NOT match the escrowed %s (live %s, escrowed %s). The escrow "+
						"is the authority for this material; refusing to overwrite what another "+
						"restorer wrote", purpose, secretName, artifact,
						fingerprintData(live), fp))
					fmt.Fprintf(w, "%s\tCONFLICT\t%s\n", purpose, fp)
				}
				continue
			}
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", purpose, err))
				fmt.Fprintf(w, "%s\tFAILED\t%s\n", purpose, fp)
				continue
			}
			restored++
			fmt.Fprintf(w, "%s\t%s\t%s\n", purpose, pkiRestore+"d", fp)
		}
	}
	_ = w.Flush()

	fmt.Printf("\n%d restored, %d already present, %d of %d purposes unresolved.\n",
		restored, present, len(failures), len(escrow.SpokePKIPurposes))

	if len(failures) > 0 {
		return fmt.Errorf("%s cannot be managed from this hub yet, because its trust roots are "+
			"not all in place:\n  %s\n\nDo NOT unpause Cluster API for this cluster until every "+
			"purpose reads restored or already-present", o.cluster, strings.Join(failures, "\n  "))
	}
	fmt.Printf("\nEvery trust root for %s is in place. Cluster API may be unpaused for it "+
		"once its machines have been matched against the provider's inventory (ADR-111 §3).\n",
		o.cluster)
	return nil
}

// requirePaused refuses to write PKI while a controller could act on it.
//
// An absent Cluster is allowed: there is nothing for a controller to reconcile, which
// is the ordinary state part-way through a reconstruction that restores PKI before
// the object graph. A PRESENT and unpaused Cluster is refused, because writing a
// trust root underneath a running control-plane controller is the race the three-tier
// pause contract exists to prevent (ADR-111 §3).
func (o restorePKIOpts) requirePaused(ctx context.Context) error {
	raw, found, err := o.kubectlJSON(ctx, "cluster", o.cluster)
	if err != nil {
		return fmt.Errorf("reading Cluster %s: %w", o.cluster, err)
	}
	if !found {
		fmt.Printf("[restore-pki] Cluster %s is not present on this hub yet; nothing can "+
			"reconcile it, so its PKI can be placed first\n", o.cluster)
		return nil
	}

	paused, err := clusterIsPaused(raw)
	if err != nil {
		return fmt.Errorf("reading Cluster %s: %w", o.cluster, err)
	}
	if paused {
		return nil
	}
	return fmt.Errorf("Cluster %s is not paused, so Cluster API is free to act on it while "+
		"this writes. Pause it first -- spec.paused, or the cluster.x-k8s.io/paused "+
		"annotation -- and release it only after every trust root is in place and the "+
		"machines have been matched against the provider's inventory (ADR-111 §3)", o.cluster)
}

func (o restorePKIOpts) liveSecret(ctx context.Context, name string) (map[string][]byte, bool, error) {
	raw, found, err := o.kubectlJSON(ctx, "secret", name)
	if err != nil || !found {
		return nil, found, err
	}
	var s struct {
		Data map[string][]byte `json:"data"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, true, fmt.Errorf("reading %s: %w", name, err)
	}
	return s.Data, true, nil
}

// createSecret writes one authority back, with the type and label Cluster API looks
// for and with no ownerReferences.
//
// CREATE, NOT APPLY, for two reasons that both matter for this object in particular.
//
// `kubectl apply` records what it submitted in the
// `kubectl.kubernetes.io/last-applied-configuration` annotation -- so applying a
// Secret writes a second copy of the private key into the object's own metadata,
// where nothing expects key material to be and where it survives every later change.
// A create does not write that annotation.
//
// And apply is an upsert. Between the read that decided this purpose needed
// restoring and the write that restores it, a Secret can appear -- Velero restoring
// the same name is the realistic case (ADR-111 Rev6 §1). Apply would overwrite it,
// which is precisely the conflict refusal `decidePKI` exists to enforce, defeated by
// the write. Create fails with AlreadyExists instead, and the caller re-reads and
// re-decides.
//
// No ownerReferences deliberately: a UID from the cluster that is gone resolves to
// nothing here, and an ownerReference naming a UID that does not exist makes its
// object eligible for garbage collection -- a faithful restore would invite
// Kubernetes to delete the fleet's trust roots (ADR-111 §5). Cluster API re-establishes
// the reference itself on the first reconcile it is allowed.
//
// It returns errSecretExists when the name was taken in that window.
func (o restorePKIOpts) createSecret(ctx context.Context, name string, data map[string][]byte) error {
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": capiNamespace,
			"labels":    map[string]string{capiClusterNameLabel: o.cluster},
		},
		"type": capiClusterSecretType,
		"data": data,
	})
	if err != nil {
		return fmt.Errorf("serialise %s: %w", name, err)
	}

	args := o.kubectlFlags("create", "-f", "-")
	c := exec.CommandContext(ctx, "kubectl", args...)
	c.Stdin = bytes.NewReader(body)
	// Stderr is captured separately and stdout discarded: a failure can quote the
	// payload, and the payload is a private key. Only the classification of the
	// failure leaves this function, never its body.
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		if strings.Contains(stderr.String(), "AlreadyExists") {
			return errSecretExists
		}
		return fmt.Errorf("creating %s failed (%w); kubectl's output is withheld "+
			"because it can echo the key material", name, err)
	}
	return nil
}

// errSecretExists means the name was taken between the read and the create, so the
// decision has to be made again against what is now there.
var errSecretExists = errors.New("the secret already exists")

func (o restorePKIOpts) kubectlJSON(ctx context.Context, kind, name string) ([]byte, bool, error) {
	args := o.kubectlFlags("get", kind, name, "-n", capiNamespace, "-o", "json")
	out, err := exec.CommandContext(ctx, "kubectl", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "NotFound") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("kubectl get %s/%s: %w", kind, name, err)
	}
	return out, true, nil
}

func (o restorePKIOpts) kubectlFlags(args ...string) []string {
	if o.kubeContext != "" {
		args = append([]string{"--context", o.kubeContext}, args...)
	}
	if o.kubeconfig != "" {
		args = append([]string{"--kubeconfig", o.kubeconfig}, args...)
	}
	return args
}

// fingerprintData identifies a secret's contents without disclosing them. Go sorts
// map keys when it marshals, so the same data always yields the same fingerprint.
func fingerprintData(data map[string][]byte) string {
	body, err := json.Marshal(data)
	if err != nil {
		return "unreadable"
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:12]
}
