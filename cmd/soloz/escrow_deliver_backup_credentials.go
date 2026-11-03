package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/soloz-io/zero-ops/internal/platform/escrow"
	"github.com/spf13/cobra"
)

// What the management-state backups need that must come from OUTSIDE the box.
//
// Delivered from escrow straight into the backup namespace, and deliberately not
// through this box's Infisical or ESO. The whole purpose of these two values is to
// work when the hub that wrote the backups -- and the Infisical inside it -- is gone,
// so routing their delivery through Infisical would reintroduce the circularity
// ADR-076 exists to break: the credential that reaches the bucket would live in the
// database that has to be recovered from that bucket.
//
// Two values, because encryption and authentication are different authorities
// (ADR-111 §10): the customer key the objects are encrypted under, and the credential
// that reads the bucket at all.
const (
	// cnpgSecretName is the Secret CloudNativePG's barmanObjectStore reads for the one
	// cluster that cannot take its archive credential from Infisical (ADR-112 §4):
	// infisical-db references it by this name.
	cnpgSecretName = "s3-credentials-escrowed"
	// sseCSecretName and sseCSecretKey are what the upstream AWS plugin is pointed at
	// by `customerKeyEncryptionSecret: <name>/<key>`.
	sseCSecretName = "velero-sse-c-key"
	sseCSecretKey  = "customer-key"
	// storeSecretName and storeSecretKey are Velero's conventional credential shape:
	// one Secret whose `cloud` key holds the provider's INI credential file.
	storeSecretName = "velero-backup-store"
	storeSecretKey  = "cloud"
	// sseCKeyBytes is fixed by the mechanism: SSE-C takes a 256-bit key, and the
	// plugin refuses anything else. Checked here so a wrong length is a refusal now
	// rather than a backup that cannot be written later.
	sseCKeyBytes = 32
)

type deliverOpts struct {
	box, namespace, kubeconfig, kubeContext string
	dryRun                                  bool
	// cnpgNamespace is where the CloudNativePG-shaped copy of the store credential
	// goes: the clusters' namespace, not Velero's.
	cnpgNamespace string
	// region is the object store's region, when the enrolled credential does not
	// declare one. CNPG takes it as a discrete key; the INI Velero reads need not
	// carry it.
	region string
	// rotateStore permits REPLACING the delivered object-store credential, and
	// applies to that credential only.
	//
	// The two artefacts this command delivers have different lifetimes (ADR-111
	// §10). An SSE-C key is immutable while any backup written under it is retained,
	// because a replacement decrypts none of them -- so there is no flag for it, and
	// a conflict there is always a refusal. An access credential is rotated
	// deliberately, and rotating it is part of how a replaced hub is fenced (§6),
	// while changing nothing about what is already readable.
	rotateStore bool
}

// newEscrowDeliverBackupCredentialsCmd puts the escrowed backup authorities into the
// cluster, without either of them passing through Git, a log, an argument or a file.
//
// Create-only and non-destructive, exactly as `spoke restore-pki` is and for the same
// reasons: an identical Secret already present is accepted, a different one is a
// refusal, and nothing is overwritten. On a successor hub this is what must run
// BEFORE Velero is pointed at the historical backups -- without the key they cannot
// be decrypted, and a missing key must never be answered by generating one.
func newEscrowDeliverBackupCredentialsCmd() *cobra.Command {
	var o deliverOpts

	cmd := &cobra.Command{
		Use:   "deliver-backup-credentials",
		Short: "deliver the escrowed backup encryption key and store credential into the cluster",
		Long: `Creates the two Secrets Velero needs to read this box's management-state backups:
the SSE-C customer key the objects are encrypted under, and the object-store
credential that reaches the bucket.

Both come from tenant-controlled external escrow, not from this box's secret store,
because they must work when the hub that wrote the backups is gone.

Create-only. An identical Secret is accepted as already present; a different one is
refused and left alone. A missing escrowed key fails closed -- it is never replaced
by a new one, because a new key cannot decrypt a backup written under the old.

Key material never reaches Git, a log, a command argument or a file.`,
		RunE: func(c *cobra.Command, _ []string) error {
			return runDeliverBackupCredentials(c.Context(), o)
		},
	}

	f := cmd.Flags()
	f.StringVar(&o.box, "box", "", "the logical hub whose escrow holds them (required)")
	f.StringVar(&o.namespace, "namespace", "velero", "the namespace Velero runs in")
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "kubeconfig for the hub to deliver into")
	f.StringVar(&o.kubeContext, "context", "", "kubeconfig context")
	f.StringVar(&o.cnpgNamespace, "cnpg-namespace", "platform-data",
		"namespace of the CloudNativePG clusters that read the archive credential")
	f.StringVar(&o.region, "region", "",
		"object-store region, when the enrolled credential does not declare one")
	f.BoolVar(&o.dryRun, "dry-run", false, "compare only; write nothing")
	f.BoolVar(&o.rotateStore, "rotate-store-credential", false,
		"replace an already-delivered object-store credential. Applies to that credential "+
			"only: the encryption key is never replaced, because a new one cannot decrypt "+
			"backups written under the old")
	_ = cmd.MarkFlagRequired("box")
	return cmd
}

func runDeliverBackupCredentials(ctx context.Context, o deliverOpts) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(o.box) == "" {
		return fmt.Errorf("--box is required: these are the box's authorities, and they " +
			"outlive every cluster in it (ADR-111 §1)")
	}

	store, err := escrow.NewEscrowClient(ctx, "")
	if err != nil {
		return fmt.Errorf("no escrow to deliver from: %w", err)
	}
	// Box-scoped, so the cluster name is the box's own: these artefacts are not keyed
	// to any incarnation, which is what lets a successor find them.
	id := escrow.ForCluster(o.box, o.box)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SECRET\tACTION\tFINGERPRINT")

	var failures []string

	// The encryption key, decoded to the raw 32 bytes the mechanism takes. The escrow
	// holds it hex-encoded; the Secret must hold the key itself, not its encoding.
	sseC, err := store.RestoreArtifact(ctx, id, escrow.ArtifactBackupSSECKey)
	switch {
	case err != nil:
		failures = append(failures, fmt.Sprintf("%s: reading the escrow: %v", sseCSecretName, err))
		fmt.Fprintf(w, "%s\tFAILED\t-\n", sseCSecretName)
	case sseC == "":
		// Fail closed. A generated replacement cannot decrypt a single existing
		// backup, so producing one here would convert a recoverable box into an
		// unrecoverable one while reporting success.
		failures = append(failures, fmt.Sprintf("%s: the escrow holds no %s for box %q. "+
			"Recovery fails closed here: a new key cannot decrypt backups written under the "+
			"old one, so none is generated", sseCSecretName, escrow.ArtifactBackupSSECKey, o.box))
		fmt.Fprintf(w, "%s\tMISSING\t-\n", sseCSecretName)
	default:
		raw, derr := decodeSSECKey(sseC)
		if derr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", sseCSecretName, derr))
			fmt.Fprintf(w, "%s\tFAILED\t-\n", sseCSecretName)
		} else {
			o.deliver(ctx, w, &failures, sseCSecretName, map[string][]byte{sseCSecretKey: raw}, false)
		}
	}

	// The store credential, delivered verbatim: its shape is the provider's, not ours.
	cred, err := store.RestoreArtifact(ctx, id, escrow.ArtifactBackupStoreCredential)
	switch {
	case err != nil:
		failures = append(failures, fmt.Sprintf("%s: reading the escrow: %v", storeSecretName, err))
		fmt.Fprintf(w, "%s\tFAILED\t-\n", storeSecretName)
	case cred == "":
		failures = append(failures, fmt.Sprintf("%s: the escrow holds no %s for box %q, so "+
			"nothing here can reach the bucket the backups are in", storeSecretName,
			escrow.ArtifactBackupStoreCredential, o.box))
		fmt.Fprintf(w, "%s\tMISSING\t-\n", storeSecretName)
	default:
		o.deliver(ctx, w, &failures, storeSecretName,
			map[string][]byte{storeSecretKey: []byte(cred)}, o.rotateStore)

		// The SAME credential, in the shape CloudNativePG reads, for the one cluster
		// whose archive must be reachable without Infisical (ADR-112 §4).
		//
		// Not a second enrolment: barman and Velero reach one bucket, so a second
		// credential would be a second thing to rotate and a second way for the two to
		// disagree about which key is current. The shapes differ because the consumers
		// do -- Velero reads an INI file, CNPG reads three discrete keys -- and that is
		// a rendering difference, not two authorities.
		keyID, secretKey, region, perr := parseStoreCredential(cred, o.region)
		if perr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", cnpgSecretName, perr))
			fmt.Fprintf(w, "%s\tFAILED\t-\n", cnpgSecretName)
		} else {
			o.deliverTo(ctx, w, &failures, o.cnpgNamespace, cnpgSecretName, map[string][]byte{
				"access-key-id":     []byte(keyID),
				"secret-access-key": []byte(secretKey),
				"region":            []byte(region),
			}, o.rotateStore)
		}
	}

	_ = w.Flush()

	if len(failures) > 0 {
		return fmt.Errorf("the management-state backups cannot be read from this hub yet:\n  %s",
			strings.Join(failures, "\n  "))
	}
	fmt.Printf("\nBoth backup authorities are in %s. Velero may be pointed at this box's "+
		"backup location; restoring Class M is still not authorisation to unpause Cluster "+
		"API (ADR-111 §3).\n", o.namespace)
	if o.rotateStore {
		// A replaced Secret is not a switched consumer. Velero reads the credential
		// when it uses the backup location, and a process already running may still
		// hold the previous one -- so revoking now can break a backup that is in
		// flight. Revocation is the LAST step and a separate decision.
		fmt.Println("\nThe object-store credential was replaced. It is NOT yet safe to revoke " +
			"the previous one: confirm Velero is using the new credential -- a successful " +
			"backup or a successful backup-location validation after the change -- and revoke " +
			"the old credential at the provider only then.")
	}
	return nil
}

// deliver creates one Secret, or confirms the identical one already there.
// deliver puts a secret in Velero's namespace. deliverTo puts one anywhere, which the
// CloudNativePG copy needs: the clusters are not in Velero's namespace, and the escrow
// is the authority for both.
func (o deliverOpts) deliver(ctx context.Context, w *tabwriter.Writer,
	failures *[]string, name string, data map[string][]byte, replaceable bool,
) {
	o.deliverTo(ctx, w, failures, o.namespace, name, data, replaceable)
}

func (o deliverOpts) deliverTo(ctx context.Context, w *tabwriter.Writer,
	failures *[]string, ns, name string, data map[string][]byte, replaceable bool,
) {
	fp := fingerprintData(data)

	live, found, err := o.liveSecretDataIn(ctx, ns, name)
	if err != nil {
		*failures = append(*failures, fmt.Sprintf("%s: %v", name, err))
		fmt.Fprintf(w, "%s\tFAILED\t%s\n", name, fp)
		return
	}
	if found {
		if maps.EqualFunc(live.Data, data, bytes.Equal) {
			fmt.Fprintf(w, "%s\talready-present\t%s\n", name, fp)
			return
		}
		if !replaceable {
			*failures = append(*failures, fmt.Sprintf("%s: the Secret in %s does not match the "+
				"escrowed value (live %s, escrowed %s). The escrow is the authority for this "+
				"material; refusing to overwrite, because the live copy may be what the "+
				"existing backups were written under", name, ns,
				fingerprintData(live.Data), fp))
			fmt.Fprintf(w, "%s\tCONFLICT\t%s\n", name, fp)
			return
		}
		// A rotation that was asked for. `replace`, not `apply`: it requires the
		// object to exist, and it writes no last-applied-configuration annotation,
		// so the credential is not copied into the object's own metadata.
		if o.dryRun {
			fmt.Fprintf(w, "%s\twould-rotate\t%s\n", name, fp)
			return
		}
		if err := o.replaceSecretIn(ctx, ns, name, data, live); err != nil {
			*failures = append(*failures, fmt.Sprintf("%s: %v", name, err))
			fmt.Fprintf(w, "%s\tFAILED\t%s\n", name, fp)
			return
		}
		fmt.Fprintf(w, "%s\trotated\t%s\n", name, fp)
		return
	}

	if o.dryRun {
		fmt.Fprintf(w, "%s\twould-create\t%s\n", name, fp)
		return
	}
	if err := o.createSecretIn(ctx, ns, name, data); err != nil {
		if errors.Is(err, errSecretExists) {
			// Appeared in the window. Re-read and re-decide rather than overwrite.
			live, found, rerr := o.liveSecretDataIn(ctx, ns, name)
			if rerr == nil && found && maps.EqualFunc(live.Data, data, bytes.Equal) {
				fmt.Fprintf(w, "%s\talready-present\t%s\n", name, fp)
				return
			}
			*failures = append(*failures, fmt.Sprintf("%s: it appeared while being delivered "+
				"and does not match the escrowed value; refusing to overwrite", name))
			fmt.Fprintf(w, "%s\tCONFLICT\t%s\n", name, fp)
			return
		}
		*failures = append(*failures, fmt.Sprintf("%s: %v", name, err))
		fmt.Fprintf(w, "%s\tFAILED\t%s\n", name, fp)
		return
	}
	fmt.Fprintf(w, "%s\tcreated\t%s\n", name, fp)
}

// liveSecret is what a read of the delivered Secret returns, including the pieces a
// safe replacement needs: the resourceVersion that makes the write optimistic, and the
// labels and annotations that must survive it.
type liveSecret struct {
	Data            map[string][]byte
	ResourceVersion string
	Labels          map[string]string
	Annotations     map[string]string
}

func (o deliverOpts) liveSecretDataIn(ctx context.Context, ns, name string) (liveSecret, bool, error) {
	args := o.flags("get", "secret", name, "-n", ns, "-o", "json")
	out, err := exec.CommandContext(ctx, "kubectl", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "NotFound") {
			return liveSecret{}, false, nil
		}
		return liveSecret{}, false, fmt.Errorf("reading Secret %s: %w", name, err)
	}
	var parsed struct {
		Metadata struct {
			ResourceVersion string            `json:"resourceVersion"`
			Labels          map[string]string `json:"labels"`
			Annotations     map[string]string `json:"annotations"`
		} `json:"metadata"`
		Data map[string][]byte `json:"data"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return liveSecret{}, true, fmt.Errorf("reading Secret %s: %w", name, err)
	}
	return liveSecret{
		Data:            parsed.Data,
		ResourceVersion: parsed.Metadata.ResourceVersion,
		Labels:          parsed.Metadata.Labels,
		Annotations:     parsed.Metadata.Annotations,
	}, true, nil
}

// createSecret is create-only and stdin-delivered, for the reasons spelled out on
// the PKI equivalent: `apply` would copy the value into a
// last-applied-configuration annotation, and would upsert over something that
// appeared since the read.
func (o deliverOpts) createSecretIn(ctx context.Context, ns, name string, data map[string][]byte) error {
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"type":       "Opaque",
		"data":       data,
	})
	if err != nil {
		return fmt.Errorf("serialise %s: %w", name, err)
	}

	c := exec.CommandContext(ctx, "kubectl", o.flags("create", "-f", "-")...)
	c.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		if strings.Contains(stderr.String(), "AlreadyExists") {
			return errSecretExists
		}
		return fmt.Errorf("creating %s failed (%w); kubectl's output is withheld because it "+
			"can echo the value", name, err)
	}
	return nil
}

func (o deliverOpts) flags(args ...string) []string {
	if o.kubeContext != "" {
		args = append([]string{"--context", o.kubeContext}, args...)
	}
	if o.kubeconfig != "" {
		args = append([]string{"--kubeconfig", o.kubeconfig}, args...)
	}
	return args
}

// decodeSSECKey turns the escrowed hex into the key itself.
//
// The Secret must hold the 32 bytes, not their encoding: the plugin reads the value as
// the key. Length is checked here rather than left to the provider, so a wrong key is
// a refusal before anything is written instead of a backup that cannot be read later.
func decodeSSECKey(escrowed string) ([]byte, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(escrowed))
	if err != nil {
		return nil, fmt.Errorf("the escrowed key is not hex-encoded: %w", err)
	}
	if len(raw) != sseCKeyBytes {
		return nil, fmt.Errorf("the escrowed key decodes to %d bytes; SSE-C takes exactly %d "+
			"and the plugin refuses anything else", len(raw), sseCKeyBytes)
	}
	return raw, nil
}

// replaceSecret overwrites an existing Secret, for the one artefact whose
// replacement is a supported operation.
//
// `replace` rather than `apply` for the same reason the create path avoids apply: it
// writes no last-applied-configuration annotation, so the value is not duplicated
// into the object's own metadata. Unlike create it requires the object to exist,
// which is correct here -- this path is only reached because it does.
func (o deliverOpts) replaceSecretIn(
	ctx context.Context, ns, name string, data map[string][]byte, live liveSecret,
) error {
	// The resourceVersion of the object this replacement DECIDED against. Without it
	// the write is a blind overwrite: anything that changed the Secret between the
	// read above and this call is silently discarded, which is the same defeat of the
	// conflict rule that `apply` would have been. With it, a concurrent modification
	// comes back as a conflict and is reported as one.
	if live.ResourceVersion == "" {
		return fmt.Errorf("refusing to replace %s: the read returned no resourceVersion, so "+
			"the write could not be made conditional on it and would overwrite blindly", name)
	}

	meta := map[string]any{
		"name":            name,
		"namespace":       ns,
		"resourceVersion": live.ResourceVersion,
	}
	// A replace sends the whole object, so anything omitted is removed. The labels
	// and annotations already on it are not ours to drop -- Velero's own, or an
	// operator's, would go with them.
	if len(live.Labels) > 0 {
		meta["labels"] = live.Labels
	}
	if len(live.Annotations) > 0 {
		meta["annotations"] = live.Annotations
	}

	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   meta,
		"type":       "Opaque",
		"data":       data,
	})
	if err != nil {
		return fmt.Errorf("serialise %s: %w", name, err)
	}

	c := exec.CommandContext(ctx, "kubectl", o.flags("replace", "-f", "-")...)
	c.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		if strings.Contains(stderr.String(), "Conflict") ||
			strings.Contains(stderr.String(), "modified") {
			return fmt.Errorf("%s changed while it was being rotated, so the replacement was "+
				"refused rather than applied over whatever changed it. Re-read and decide "+
				"again", name)
		}
		return fmt.Errorf("replacing %s failed (%w); kubectl's output is withheld because it "+
			"can echo the value", name, err)
	}
	return nil
}

// parseStoreCredential reads the three values CloudNativePG needs out of the INI
// credential Velero reads, without echoing any of them.
//
// One enrolled credential, two shapes. The alternative -- enrolling a second credential
// for barman -- means two things to rotate and two chances for them to disagree about
// which key is current, against one bucket.
//
// Region is taken from the credential when it declares one and from --region otherwise.
// It is NOT defaulted: CNPG sends it to the provider on every request, and a wrong
// region fails in a way that reads as a missing bucket.
func parseStoreCredential(cred, regionFlag string) (keyID, secret, region string, err error) {
	for _, line := range strings.Split(cred, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") ||
			strings.HasPrefix(t, "[") {
			continue
		}
		k, v, found := strings.Cut(t, "=")
		if !found {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "aws_access_key_id":
			keyID = strings.TrimSpace(v)
		case "aws_secret_access_key":
			secret = strings.TrimSpace(v)
		case "region":
			region = strings.TrimSpace(v)
		}
	}
	if regionFlag != "" {
		region = strings.TrimSpace(regionFlag)
	}

	var missing []string
	if keyID == "" {
		missing = append(missing, "aws_access_key_id")
	}
	if secret == "" {
		missing = append(missing, "aws_secret_access_key")
	}
	if region == "" {
		missing = append(missing, "a region (declare it in the credential, or pass --region)")
	}
	if len(missing) > 0 {
		return "", "", "", fmt.Errorf("the escrowed credential cannot be rendered for "+
			"CloudNativePG: missing %s", strings.Join(missing, ", "))
	}
	return keyID, secret, region, nil
}
