package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"text/template"

	"github.com/spf13/cobra"

	"github.com/soloz-io/zero-ops/internal/assets"
	"github.com/soloz-io/zero-ops/internal/platform/escrow"
)

// newEncryptionRotateCmd replaces a cluster's at-rest encryption key.
//
// WHY THIS IS STAGED AND NOT ONE COMMAND.
//
// `secretbox` writes with the FIRST key in the provider list and reads with any of
// them. So the only safe rotation is ordered:
//
//	new key first, old key retained  ->  rewrite every Secret  ->  verify every
//	object now carries the NEW key's name  ->  remove the old key  ->  verify again
//
// Removing the old key before the rewrite is verified makes every object still sealed
// under it unreadable. That is why this command has two stages and refuses to do the
// second until the first is proven, rather than being a single apply that happens to
// be run twice.
//
// WHY THE KEY NAME CARRIES A GENERATION. The API server writes the key's NAME into
// every stored object -- `k8s:enc:secretbox:v1:<name>:<nonce><ciphertext>` -- so after
// a rotation the prefix in etcd says which generation sealed each Secret. That is what
// turns "every Secret has been rewritten" from an assumption into a count. With one
// fixed name the two generations are indistinguishable in etcd and the rewrite cannot
// be proven, which after a key disclosure is the same as not having done it.
//
// THIS EXISTS BECAUSE A KEY WAS DISCLOSED, not as a routine capability. ADR-003 §6
// recorded rotation as unsupported, which was true and is why the disclosure on
// 2026-10-05 had no remedy. Separate from ADR-100: KMS v2 makes rotation the key
// store's concern, and this is the procedure for the provider in force today.
func newEncryptionRotateCmd() *cobra.Command {
	var (
		box         string
		cluster     string
		namespace   string
		kubeconfig  string
		kubeContext string
		finalize    bool
		dryRun      bool
	)
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Replace a cluster's at-rest encryption key, in two verified stages",
		Long: `Replace the key Secrets are encrypted with, without losing the data already written.

STAGE 1 (default) mints a new key, makes it the write key, and RETAINS the current one
for reads. Nothing is unreadable at any point. Then, per
docs/runbooks/encrypt-secrets-at-rest.md: roll the control plane, rewrite every Secret,
and verify in etcd that every object carries the new key's name.

STAGE 2 (--finalize) removes the retained key. It refuses unless the live configuration
is actually mid-rotation, and it is the step that makes a disclosed key worthless --
so it must not be run until the rewrite has been verified, because anything still
sealed under the old key becomes unreadable.

Both stages escrow before they apply (ADR-076). During a rotation the escrow holds BOTH
keys, because a cluster that loses its control plane mid-rotation needs both to recover.

WHEN TO RUN IT. The cryptoperiod is ` + strconv.Itoa(assets.EncryptionRotationWindowDays()) + ` days for
both providers, and it is the same constant the Cloud KMS key uses -- the key that sits on
the control-plane host has no business outliving the non-exportable one.

That window governs keys that have NOT left their trust boundary. One that has is an
incident, not a scheduling question: it is replaced now. Waiting for the next window on
a key you know was disclosed keeps a compromised key in force for a quarter.`,
		RunE: func(c *cobra.Command, _ []string) error {
			return runEncryptionRotate(c.Context(), rotateOpts{
				cluster: cluster, box: box, namespace: namespace, kubeconfig: kubeconfig,
				kubeContext: kubeContext, finalize: finalize, dryRun: dryRun,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&cluster, "cluster", "", "cluster whose key to rotate (required)")
	f.StringVar(&box, "box", "", "the logical hub the escrow is keyed by; box-scoped artefacts live under it (required)")
	f.StringVar(&namespace, "namespace", "platform-capi", "namespace holding the configuration Secret")
	f.StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig for the MANAGEMENT cluster, where platform-capi lives")
	f.StringVar(&kubeContext, "context", "", "kubeconfig context")
	f.BoolVar(&finalize, "finalize", false,
		"stage 2: remove the retained previous key. Only after etcd shows every Secret "+
			"carrying the new key's name — anything still under the old key becomes unreadable")
	f.BoolVar(&dryRun, "dry-run", false, "print what would be applied")
	_ = cmd.MarkFlagRequired("cluster")
	_ = cmd.MarkFlagRequired("box")
	return cmd
}

type rotateOpts struct {
	cluster, box, namespace, kubeconfig, kubeContext string
	finalize, dryRun                                 bool
}

// keyEntry is one `- name: keyN` / `secret: ...` pair read back from the live Secret.
type keyEntry struct {
	name       string
	generation int
	b64        string
}

var keyLine = regexp.MustCompile(`(?m)^\s*- name:\s*(\S+)\s*$`)
var secretLine = regexp.MustCompile(`(?m)^\s*secret:\s*(\S+)\s*$`)
var genFromName = regexp.MustCompile(`^key(\d+)$`)

// readLiveKeys parses the provider configuration currently on the cluster.
//
// THE LIVE SECRET IS THE SOURCE OF TRUTH FOR THE GENERATION, not the escrow and not a
// counter kept here. The cluster is what the API server is actually using, and a
// rotation that numbered the next key from anywhere else could reuse a name that
// already appears in etcd -- which would make two generations indistinguishable in the
// stored prefix and the rewrite unprovable.
func readLiveKeys(ctx context.Context, o rotateOpts) ([]keyEntry, error) {
	args := []string{"-n", o.namespace, "get", "secret",
		assets.EncryptionSecretName(o.cluster), "-o", `jsonpath={.data.enc\.yaml}`}
	if o.kubeconfig != "" {
		args = append([]string{"--kubeconfig", o.kubeconfig}, args...)
	}
	if o.kubeContext != "" {
		args = append([]string{"--context", o.kubeContext}, args...)
	}
	out, err := exec.CommandContext(ctx, "kubectl", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("reading %s in %s from the management cluster: %w. A rotation "+
			"replaces an existing configuration; use `soloz encryption enable` for a cluster "+
			"that has none", assets.EncryptionSecretName(o.cluster), o.namespace, err)
	}
	raw, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(out)))
	if err != nil {
		return nil, fmt.Errorf("the stored configuration is not base64: %w", err)
	}

	names := keyLine.FindAllStringSubmatch(string(raw), -1)
	secrets := secretLine.FindAllStringSubmatch(string(raw), -1)
	if len(names) == 0 || len(names) != len(secrets) {
		return nil, fmt.Errorf("the live configuration has %d key names and %d key values; "+
			"refusing to guess which belongs to which", len(names), len(secrets))
	}

	var out2 []keyEntry
	for i := range names {
		e := keyEntry{name: names[i][1], b64: secrets[i][1]}
		if m := genFromName.FindStringSubmatch(e.name); m != nil {
			e.generation, _ = strconv.Atoi(m[1])
		} else {
			// Refused rather than renamed. A key whose name is not keyN cannot be
			// ordered against the others, and inventing a generation for it would
			// produce a name that may already be in etcd.
			return nil, fmt.Errorf("the live configuration contains a key named %q, which is "+
				"not of the form keyN. This rotation numbers generations from the live "+
				"configuration and cannot order an unrecognised name", e.name)
		}
		out2 = append(out2, e)
	}
	return out2, nil
}

func runEncryptionRotate(ctx context.Context, o rotateOpts) error {
	if ctx == nil {
		ctx = context.Background()
	}

	store, err := escrow.NewEscrowClient(ctx, "")
	if err != nil {
		return fmt.Errorf("a rotation needs an escrow to keep both keys in while it runs "+
			"(ADR-076): %w", err)
	}

	live, err := readLiveKeys(ctx, o)
	if err != nil {
		return err
	}
	fmt.Printf("[rotate] live configuration: ")
	for i, e := range live {
		if i > 0 {
			fmt.Print(", ")
		}
		// keyFingerprint, not fingerprint: this value is base64 and the escrow's is hex.
		// Digesting the encoding rather than the key made the two incomparable.
		fmt.Printf("%s (%s)", e.name, keyFingerprint([]byte(e.b64)))
	}
	fmt.Println()

	if o.finalize {
		return finalizeRotation(ctx, o, store, live)
	}
	return stageRotation(ctx, o, store, live)
}

// stageRotation adds a new write key and retains the current one for reads.
func stageRotation(ctx context.Context, o rotateOpts, store escrow.EscrowClient, live []keyEntry) error {
	if len(live) > 1 {
		return fmt.Errorf("the live configuration already holds %d keys, so a rotation is "+
			"already in progress under %s. Finish it: rewrite every Secret, verify in etcd that "+
			"none still carries %s, then run with --finalize. Starting a second rotation would "+
			"leave three generations in etcd at once",
			len(live), live[0].name, live[len(live)-1].name)
	}

	current := live[0]
	nextGen := current.generation + 1
	nextName := assets.EncryptionKeyName(nextGen)

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("generating the new key: %w", err)
	}
	newHex := hex.EncodeToString(raw)

	// THE DRY RUN MUST NOT PRINT A FINGERPRINT FOR THIS KEY, because this key is
	// discarded. A dry run generates one to prove it can and then persists nothing, so the
	// real run generates a DIFFERENT key with a different digest.
	//
	// It printed one anyway, and the consequence showed up on the first real rotation: the
	// dry run reported key2 as sha256:f72c484779f8, the apply reported sha256:5f92c1136dfd,
	// and an operator comparing the two reasonably asked what had changed the key between
	// planning and applying. Nothing had. The tool invited a comparison that could never
	// hold -- the same defect as digesting an encoding instead of a value, in a different
	// place.
	//
	// So a dry run says a key WILL be generated, and the apply is the only thing that
	// prints a digest, because it is the only thing whose digest will still be true
	// afterwards.
	if o.dryRun {
		fmt.Printf("[rotate] dry-run: would generate a new write key %s and retain %s for "+
			"reads. No fingerprint is shown: the key a dry run generates is discarded, and "+
			"printing its digest would not match the one the apply records.\n",
			nextName, current.name)
	} else {
		fmt.Printf("[rotate] new write key %s (%s); %s retained for reads\n",
			nextName, keyFingerprint(raw), current.name)
	}

	// BOTH KEYS ARE ESCROWED BEFORE ANYTHING IS APPLIED.
	//
	// A cluster that loses its control plane mid-rotation has objects under both
	// generations, so recovering it needs both. Escrowing only the new one would make
	// the window between here and the verified rewrite unrecoverable -- which is the
	// precise failure ADR-076 exists to prevent, introduced by the procedure meant to
	// improve security.
	if o.dryRun {
		fmt.Printf("[rotate] dry-run: would escrow %s as the previous key and %s as current\n",
			escrow.ArtifactSecretEncryptionKeyPrevious, escrow.ArtifactSecretEncryptionKey)
		fmt.Println("[rotate] dry-run: would apply the two-key configuration")
		return nil
	}

	// The OLD key moves to the previous slot first. If this fails, nothing has changed.
	oldHex, err := decodeKeyToHex(current.b64)
	if err != nil {
		return fmt.Errorf("the live key %s is not a 32-byte base64 value, so it cannot be "+
			"escrowed as the previous key: %w", current.name, err)
	}
	if err := store.BackupArtifact(ctx, escrow.ForCluster(o.box, o.cluster),
		escrow.ArtifactSecretEncryptionKeyPrevious, oldHex); err != nil {
		return fmt.Errorf("escrowing the outgoing key as %s: %w",
			escrow.ArtifactSecretEncryptionKeyPrevious, err)
	}
	fmt.Printf("[rotate] escrowed the outgoing key as %s\n", escrow.ArtifactSecretEncryptionKeyPrevious)

	if err := store.BackupArtifact(ctx, escrow.ForCluster(o.box, o.cluster), escrow.ArtifactSecretEncryptionKey, newHex); err != nil {
		return fmt.Errorf("the new key was generated but could not be escrowed, so nothing was "+
			"applied: %w", err)
	}
	fmt.Println("[rotate] escrowed the new key as the current one")

	body, err := renderConfig(o, nextName, base64.StdEncoding.EncodeToString(raw),
		current.name, current.b64)
	if err != nil {
		return err
	}
	if err := applyManifest(ctx, o, body); err != nil {
		return err
	}

	fmt.Println()
	fmt.Printf("STAGE 1 APPLIED. %s is now the write key and %s reads older objects.\n",
		nextName, current.name)
	fmt.Println()
	fmt.Println("Nothing is encrypted under the new key yet. Next, from")
	fmt.Println("docs/runbooks/encrypt-secrets-at-rest.md:")
	fmt.Println()
	fmt.Println("  1. roll the control plane, so the API server reads this configuration")
	fmt.Println("  2. rewrite every Secret (step 7)")
	fmt.Printf("  3. verify in etcd that NO object still carries %s (step 8)\n", current.name)
	fmt.Printf("  4. then: soloz encryption rotate --cluster %s --finalize\n", o.cluster)
	fmt.Println()
	fmt.Printf("Until step 3 passes, %s is still required to read this cluster.\n", current.name)
	return nil
}

// finalizeRotation removes the retained previous key.
func finalizeRotation(ctx context.Context, o rotateOpts, store escrow.EscrowClient, live []keyEntry) error {
	if len(live) < 2 {
		return fmt.Errorf("the live configuration holds one key (%s), so there is no rotation to "+
			"finalise. --finalize removes a RETAINED previous key; it does not start one",
			live[0].name)
	}
	primary, previous := live[0], live[len(live)-1]

	fmt.Printf("[rotate] removing %s; %s becomes the only key\n", previous.name, primary.name)
	fmt.Println()
	fmt.Println("THIS IS THE IRREVERSIBLE STEP. Any object in etcd still carrying")
	fmt.Printf("k8s:enc:secretbox:v1:%s: becomes UNREADABLE once the control plane rolls.\n", previous.name)
	fmt.Printf("Step 8 of the runbook must have shown zero such objects. If it has not,\n")
	fmt.Println("stop and rewrite the Secrets first.")
	fmt.Println()

	if o.dryRun {
		fmt.Println("[rotate] dry-run: would apply the single-key configuration")
		return nil
	}

	body, err := renderConfig(o, primary.name, primary.b64, "", "")
	if err != nil {
		return err
	}
	if err := applyManifest(ctx, o, body); err != nil {
		return err
	}

	// The previous key is NOT deleted from the escrow here, deliberately. Until the
	// control plane has rolled onto this configuration the running API server is still
	// the old one, and a recovery in that window needs both. Removing it is a separate
	// decision once the roll is verified -- and for a DISCLOSED key, removing the
	// escrow copy achieves nothing anyway, because the disclosure is what made it
	// worthless, not the escrow.
	fmt.Println()
	fmt.Printf("STAGE 2 APPLIED. %s is the only key in the configuration.\n", primary.name)
	fmt.Println("Roll the control plane, then re-run step 8: it must still show every Secret")
	fmt.Println("encrypted and none under the removed key.")
	fmt.Printf("\nThe escrow still holds %s. That is intentional: the running control plane\n",
		escrow.ArtifactSecretEncryptionKeyPrevious)
	fmt.Println("is on the old configuration until the roll completes.")
	return nil
}

func decodeKeyToHex(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("decoded to %d bytes, want 32", len(raw))
	}
	return hex.EncodeToString(raw), nil
}

func renderConfig(o rotateOpts, primaryName, primaryB64, fallbackName, fallbackB64 string) ([]byte, error) {
	tmplData, err := readEncryptionConfigTemplate()
	if err != nil {
		return nil, fmt.Errorf("reading the provider configuration template: %w", err)
	}
	tmpl, err := template.New("enc").Parse(string(tmplData))
	if err != nil {
		return nil, fmt.Errorf("parsing the provider configuration template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{
		"SecretName":      assets.EncryptionSecretName(o.cluster),
		"Namespace":       o.namespace,
		"PrimaryKeyName":  primaryName,
		"PrimaryKeyB64":   primaryB64,
		"FallbackKeyName": fallbackName,
		"FallbackKeyB64":  fallbackB64,
	}); err != nil {
		return nil, fmt.Errorf("rendering the provider configuration: %w", err)
	}
	return buf.Bytes(), nil
}

func applyManifest(ctx context.Context, o rotateOpts, body []byte) error {
	args := []string{"apply", "-f", "-"}
	if o.kubeconfig != "" {
		args = append([]string{"--kubeconfig", o.kubeconfig}, args...)
	}
	if o.kubeContext != "" {
		args = append([]string{"--context", o.kubeContext}, args...)
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = bytes.NewReader(body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("applying the provider configuration: %w\n%s", err, out)
	}
	return nil
}
