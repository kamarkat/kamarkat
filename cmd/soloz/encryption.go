package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os/exec"
	"text/template"

	"github.com/soloz-io/zero-ops/internal/assets"
	"github.com/soloz-io/zero-ops/internal/platform/embedded"
	"github.com/soloz-io/zero-ops/internal/platform/escrow"
	"github.com/spf13/cobra"
)

var (
	encClusterName string
	encBoxID       string
	encNamespace   string
	encKubeconfig  string
	encContext     string
	encClassFile   string
	encApplyClass  bool
	encNoFallback  bool
	encDryRun      bool
)

// newEncryptionCmd adopts at-rest encryption of Kubernetes Secrets on a cluster
// that already exists (ADR-003 §6).
//
// # WHY THIS COMMAND EXISTS
//
// The provider configuration and the control-plane template are Day-0 artefacts:
// the provisioner creates the Secret and applies the ClusterClass while building a
// cluster, and NOTHING reconciles either afterwards. Neither ClusterClass is synced
// by ArgoCD -- they are applied from the CLI's embedded assets -- so a release does
// not put the new template on a running cluster and a running cluster has no
// provider configuration at all.
//
// That left every existing box unable to adopt the control the ADR requires, with a
// runbook whose first step was to sync something nothing syncs. This is that step.
//
// It is deliberately not a reconciler. Adopting the template ROLLS THE CONTROL
// PLANE, and the migration that follows rewrites every Secret in the cluster with a
// verification step between each stage; a controller doing that unattended would be
// a component able to take a cluster's Secrets with it if its idea of the current
// key were ever wrong (ADR-100 takes the same position on rotation).
func newEncryptionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "encryption",
		Short: "Manage at-rest encryption of Kubernetes Secrets",
	}
	cmd.AddCommand(newEncryptionEnableCmd())
	cmd.AddCommand(newEncryptionRotateCmd())
	return cmd
}

func newEncryptionEnableCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Put the at-rest encryption key and control-plane template on an existing cluster",
		Long: `Deliver what at-rest encryption needs to a cluster that already exists.

ONE thing is applied: the Secret the control plane reads its encryption provider
configuration from. It is per cluster, sourced from that cluster's escrow, and
nothing reconciles it -- which is why this command exists.

THE CLUSTERCLASS IS NOT APPLIED BY DEFAULT, AND THE TWO CLASSES DIFFER:

  a WORKLOAD cluster's    manifests/providers/<provider>, synced by ArgoCD's
                          infrastructure-provider ApplicationSet. Promoting a bundle
                          puts it on the box. Applying it from here would make the
                          CLI a second writer for a GitOps-owned object.

  the MANAGEMENT cluster's  internal/assets/manifests/classes/, embedded in this
                            binary and synced by NOTHING. It is applied once at Day-0
                            and never reconciled, so --apply-class is the only way it
                            ever changes -- and its live state can differ from the
                            repository with nothing reporting it.

So: no flag for a spoke, --apply-class for the hub.

Adopting a new template ROLLS THE CONTROL PLANE -- each node is replaced, because
the file and the mount are part of a node's bootstrap and nothing rewrites them in
place. That roll is triggered by the sync, not by this command.

RUN THIS AGAINST THE MANAGEMENT CLUSTER, for any cluster. The Secret lives in
platform-capi beside the CAPI objects, which are on the hub even for a workload
cluster -- --cluster names whose key it is, --kubeconfig says where CAPI lives.

IT IS TWO PHASES. The template adopted here (v3) delivers the file and the mount and
nothing that READS them, so a node that comes up with the file missing or misnamed is
an ordinary healthy node you can inspect. The 'encryption-provider-config' argument --
which makes the API server REQUIRE the file, and refuse to start without it -- arrives
in v4, after a replaced node has been checked. Each phase is a separate roll.

The key is restored from the escrow when one holds it and generated otherwise. A
generated key is escrowed before anything is applied: a cluster encrypting etcd with
a key held nowhere else is worse off than one not encrypting it, because losing the
control plane then takes every Secret AND every backup of them.

This is step 1 of docs/runbooks/encrypt-secrets-at-rest.md. The steps that follow --
verify, rewrite every Secret, verify in etcd, remove the plaintext fallback, verify
again -- are not performed here and are not optional.`,
		RunE: runEncryptionEnable,
	}
	f := cmd.Flags()
	f.BoolVar(&encNoFallback, "no-plaintext-fallback", false,
		"render the provider configuration WITHOUT the identity provider. Only after every "+
			"Secret has been rewritten and verified encrypted in etcd (runbook step 8): any "+
			"Secret still in plaintext becomes UNREADABLE")
	f.BoolVar(&encApplyClass, "apply-class", false,
		"also apply the ClusterClass. REQUIRED for the management cluster, whose class nothing "+
			"syncs, and for a bootstrap cluster that has no ArgoCD yet. NOT needed for a workload "+
			"cluster: ArgoCD's infrastructure-provider ApplicationSet already carries that class. "+
			"NOTE: this replaces the WHOLE class, not the encryption parts of it")
	f.StringVar(&encClusterName, "cluster", "", "cluster name; the escrow is scoped to it (required)")
	f.StringVar(&encBoxID, "box", "", "the logical hub the escrow is keyed by; box-scoped artefacts live under it (required)")
	f.StringVar(&encNamespace, "namespace", "platform-capi", "namespace the control plane reads the Secret from")
	f.StringVar(&encKubeconfig, "kubeconfig", "", "kubeconfig to apply through")
	f.StringVar(&encContext, "context", "", "kubeconfig context")
	f.StringVar(&encClassFile, "class", "", fmt.Sprintf(
		"ClusterClass asset to apply; %q for the management cluster (default), %q for a workload cluster",
		defaultHubClass, defaultSpokeClass))
	f.BoolVar(&encDryRun, "dry-run", false, "say what would be applied, change nothing")
	_ = cmd.MarkFlagRequired("cluster")
	_ = cmd.MarkFlagRequired("box")
	return cmd
}

const (
	// The management cluster's class, from the CLI's own assets.
	defaultHubClass = "classes/hetzner-mgmt-ubuntu-v1.yaml"
	// A workload cluster's class, from the platform manifests packaged for shipping.
	defaultSpokeClass = "manifests/providers/hetzner/base/spokepool-clusterclass-v1.yaml"
)

// readClusterClass finds a ClusterClass in whichever embed holds it.
//
// Tries the CLI's assets first, then the packaged platform manifests, and reports
// BOTH paths when neither has it — a "file not found" naming one tree is the error
// that sends someone looking in the wrong place.
// readEncryptionConfigTemplate is the provider configuration the control plane reads.
// Named so a test can assert the provider ORDER without reaching into the command.
func readEncryptionConfigTemplate() ([]byte, error) {
	return assets.ReadManifest("secrets/secret-encryption-config.yaml")
}

func readClusterClass(path string) ([]byte, error) {
	if b, err := assets.ReadManifest(path); err == nil {
		return b, nil
	}
	if b, err := embedded.FS.ReadFile(path); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("not found in the CLI assets (as manifests/%s) nor in the packaged "+
		"platform manifests (as %s)", path, path)
}

func runEncryptionEnable(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	// The escrow first, and the command refuses without one.
	//
	// ADR-076 requires a box to have an escrow at all, so this adds no new input;
	// what it adds is the refusal. Generating a key with nowhere outside the cluster
	// to keep it produces a value that exists only inside the thing it decrypts, and
	// the gap cannot be closed after the cluster is gone.
	store, err := escrow.NewEscrowClient(ctx, "")
	if err != nil {
		return fmt.Errorf("at-rest encryption needs an escrow to keep the key in, and a box is "+
			"not built without one (ADR-076): %w", err)
	}

	key, err := store.RestoreArtifact(ctx, escrow.ForCluster(encBoxID, encClusterName), escrow.ArtifactSecretEncryptionKey)
	if err != nil {
		// Unreadable is not absent. Treating it as absent would mint a second key for
		// a cluster that already has one, and the first thing that key would do is
		// fail to decrypt everything already written.
		return fmt.Errorf("reading the escrow for %q: %w", encClusterName, err)
	}

	generated := false
	if key == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return fmt.Errorf("generating the encryption key: %w", err)
		}
		key = hex.EncodeToString(raw)
		generated = true
		fmt.Printf("[encryption] no key in the escrow for %q; generating one\n", encClusterName)
	} else {
		fmt.Printf("[encryption] key restored from the escrow for %q\n", encClusterName)
	}

	rawKey, err := hex.DecodeString(key)
	if err != nil || len(rawKey) != 32 {
		return fmt.Errorf("the encryption key must be 32 hex-encoded bytes, got %d (decode error: %v); "+
			"a key of any other length is refused by the API server at start", len(rawKey), err)
	}

	// Escrowed BEFORE anything is applied, and only when generated.
	//
	// Before, because the alternative is a cluster that comes up encrypted while the
	// backup attempt is still outstanding. Only when generated, because a restored key
	// is already there and rewriting it is one more chance to write something else.
	if generated {
		if encDryRun {
			fmt.Println("[encryption] dry-run: would escrow the generated key")
		} else if err := store.BackupArtifact(ctx, escrow.ForCluster(encBoxID, encClusterName), escrow.ArtifactSecretEncryptionKey, key); err != nil {
			return fmt.Errorf("the key was generated but could not be escrowed, so nothing was "+
				"applied: %w", err)
		} else {
			fmt.Println("[encryption] generated key escrowed")
		}
	}

	tmplData, err := readEncryptionConfigTemplate()
	if err != nil {
		return fmt.Errorf("reading the provider configuration template: %w", err)
	}
	tmpl, err := template.New("enc").Parse(string(tmplData))
	if err != nil {
		return fmt.Errorf("parsing the provider configuration template: %w", err)
	}
	var secretOut bytes.Buffer
	if encNoFallback {
		// Said out loud, because the consequence is not reversible by re-running this
		// command: a Secret that was missed is unreadable from the next roll onward,
		// and re-adding the fallback cannot decrypt what was never encrypted.
		fmt.Println("[encryption] rendering WITHOUT the identity provider; any Secret still in " +
			"plaintext will become unreadable once the control plane rolls")
	}
	if err := tmpl.Execute(&secretOut, map[string]any{
		"SecretName": assets.EncryptionSecretName(encClusterName),
		"Namespace":  encNamespace,
		// Generation 1, and no fallback key. `enable` is the FIRST adoption; a
		// rotation is `soloz encryption rotate`, which is a staged procedure because
		// the previous key has to survive until the rewrite is verified.
		"PrimaryKeyName":      assets.EncryptionKeyName(1),
		"PrimaryKeyB64":       base64.StdEncoding.EncodeToString(rawKey),
		"NoPlaintextFallback": encNoFallback,
	}); err != nil {
		return fmt.Errorf("rendering the provider configuration: %w", err)
	}

	// THE TWO CLUSTERCLASSES LIVE IN DIFFERENT EMBEDS, and the command must reach
	// both — the workload cluster is where this is adopted FIRST, because a failed
	// control-plane roll there leaves the management plane up to repair it.
	//
	//   the management cluster's   internal/assets/manifests/classes/...
	//   a workload cluster's       internal/platform/embedded/manifests/providers/...
	//
	// Two accessors rather than one because the two trees are embedded separately:
	// the CLI's own assets, and the platform manifests packaged for shipping. A
	// single read would silently miss whichever tree it did not look in.
	// The Secret BEFORE anything that references it. A control plane whose provider
	// configuration Secret does not exist cannot start -- and under v3 the Secret is
	// what a node's bootstrap data is rendered FROM, so a missing one stalls the
	// KubeadmConfig rather than the API server.
	stages := []struct {
		what string
		body []byte
	}{
		{fmt.Sprintf("provider configuration Secret in %s", encNamespace), secretOut.Bytes()},
	}

	// THE CLUSTERCLASS IS NOT APPLIED BY DEFAULT, BECAUSE ARGOCD ALREADY OWNS IT.
	//
	// This command used to apply it unconditionally, on the belief that nothing
	// reconciled it. That was wrong, and wrong in a way that took a failed task to
	// surface: the `infrastructure-provider` ApplicationSet (boundary 03) syncs
	// `manifests/providers/<provider>`, whose kustomization pulls in base/ and with
	// it spokepool-clusterclass-v1.yaml. A published and promoted bundle therefore
	// puts the class on the box by itself -- 0.1.16-rc.142 is how
	// spokepool-control-plane-v3 arrived, with nobody running this command.
	//
	// Applying it from here makes the CLI a second writer for a GitOps-owned
	// object. Harmless while the content is byte-identical, and exactly the kind of
	// drift that is invisible until the two disagree: whoever ran this last wins
	// until the next sync, and the sync is silent about having reverted it.
	//
	// So the only thing this command owns is the SECRET, which is deliberately not
	// in that kustomization (it carries key material sourced from the escrow, per
	// cluster; see the comment in base/kustomization.yaml).
	//
	// --apply-class remains for the one case with no ArgoCD to do it: the temporary
	// bootstrap cluster at Day-0, before the management cluster and its
	// ApplicationSets exist.
	if encApplyClass {
		// IT ADOPTS THE WHOLE CLASS, NOT THE ENCRYPTION PART OF IT.
		//
		// Said out loud because the management cluster's class is reconciled by nothing:
		// it is applied at Day-0 and then diverges from the repository for as long as
		// nobody runs this. On 2026-10-05 the hub's live class still referenced
		// `hetzner-mgmt-control-plane` -- an unversioned template from sixteen days
		// earlier -- and one `--apply-class` moved it to v4, which carries every change
		// made to the control-plane spec in between, not just the encryption argument.
		//
		// The roll that follows applies all of them. That is the correct end state and
		// it is not what the flag's name suggests, so the operator is told before it
		// happens rather than after.
		fmt.Println("[encryption] --apply-class replaces the ENTIRE ClusterClass, not only the")
		fmt.Println("             encryption parts. If this cluster's class has not been applied")
		fmt.Println("             for a while, the roll that follows adopts everything that")
		fmt.Println("             changed in between. Diff the live control-plane template against")
		fmt.Println("             the new one first if you do not know what that includes.")
		classFile := encClassFile
		if classFile == "" {
			classFile = defaultHubClass
		}
		classOut, err := readClusterClass(classFile)
		if err != nil {
			return fmt.Errorf("reading the ClusterClass %q: %w", classFile, err)
		}
		stages = append(stages, struct {
			what string
			body []byte
		}{fmt.Sprintf("ClusterClass from %s", classFile), classOut})
	}

	for _, st := range stages {
		if encDryRun {
			fmt.Printf("[encryption] dry-run: would apply the %s\n", st.what)
			continue
		}
		args := []string{"apply", "-f", "-"}
		if encKubeconfig != "" {
			args = append([]string{"--kubeconfig", encKubeconfig}, args...)
		}
		if encContext != "" {
			args = append([]string{"--context", encContext}, args...)
		}
		c := exec.CommandContext(ctx, "kubectl", args...)
		c.Stdin = bytes.NewReader(st.body)
		out, err := c.CombinedOutput()
		if err != nil {
			return fmt.Errorf("applying the %s: %w\n%s", st.what, err, out)
		}
		fmt.Printf("[encryption] applied the %s\n", st.what)
	}

	if encDryRun {
		return nil
	}

	fmt.Println()
	// WHAT HAPPENS NEXT DEPENDS ON WHAT WAS ACTUALLY APPLIED, and this used to claim
	// one thing unconditionally.
	//
	// It said "The control plane will roll to pick up the provider" after every run.
	// That is only true when the CLASS changed; this command's default applies the
	// SECRET alone, and CAPI has nothing to roll for a Secret. It also pointed at
	// "step 3 / step 4 / step 5" of a runbook that has since been restructured to ten
	// steps, and it told the operator "the plaintext fallback is still in force" on
	// the very run whose purpose was removing it. Three statements, all wrong at the
	// moment they were read, in the middle of a procedure.
	fmt.Println()
	if encApplyClass {
		fmt.Println("The ClusterClass changed, so THE CONTROL PLANE WILL ROLL. An API server")
		fmt.Println("argument only takes effect on a process start. Wait for every control-plane")
		fmt.Println("node to be replaced and Ready before continuing.")
	} else {
		fmt.Println("ONLY THE SECRET CHANGED, SO NOTHING ROLLS BY ITSELF. The API server reads")
		fmt.Println("this configuration once at start and nothing re-reads it")
		fmt.Println("(--encryption-provider-config-automatic-reload is not set), so the running")
		fmt.Println("control plane is still using the configuration it started with.")
	}
	fmt.Println()
	if encNoFallback {
		fmt.Println("The plaintext fallback is gone from the configuration, and it takes effect")
		fmt.Println("on the next control-plane roll -- not now. Re-run runbook step 8 after that")
		fmt.Println("roll: if any Secret was missed it is UNREADABLE from that point, which is")
		fmt.Println("why step 8 is run on both sides of this change.")
	} else {
		fmt.Println("Encryption applies to WRITES. Until step 7 has rewritten every existing")
		fmt.Println("Secret and step 8 has confirmed it by reading etcd, this cluster is")
		fmt.Println("PARTIALLY covered and the plaintext fallback is still in force.")
	}
	fmt.Println()
	fmt.Println("Procedure: docs/runbooks/encrypt-secrets-at-rest.md")
	return nil
}
