package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"text/template"

	"github.com/kamarkat/kamarkat/internal/assets"
)

// The command exists because nothing reconciles at-rest encryption onto an existing
// cluster: both ClusterClasses are applied from the CLI's embedded assets, so a
// release puts neither on a running cluster. These pin the two things that would
// make it useless — not finding a class, and finding the wrong one.

func TestBothClusterClassesAreReachable(t *testing.T) {
	// They live in DIFFERENT embeds: the CLI's own assets hold the management
	// cluster's, the packaged platform manifests hold a workload cluster's. A single
	// accessor would silently miss whichever tree it did not look in, and the command
	// would then work for one cluster and fail for the other with "file not found".
	for _, path := range []string{defaultHubClass, defaultSpokeClass} {
		body, err := readClusterClass(path)
		if err != nil {
			t.Fatalf("readClusterClass(%q): %v", path, err)
		}
		if !strings.Contains(string(body), "kind: ClusterClass") {
			t.Fatalf("%q does not contain a ClusterClass", path)
		}
	}
}

func TestBothClassesDeliverTheProviderConfigAndNowRequireIt(t *testing.T) {
	// THE v4 CONTRACT. This test asserted the argument's ABSENCE until 2026-10-05,
	// and flipping it is the recorded phase change -- deliberately a code change, so
	// the boundary could not move by accident.
	//
	// What cleared it: the v3 roll on nutgraf-01 replaced the control-plane node, the
	// node self-assigned providerID hcloud://168731792 with no manual patch, and
	// /etc/kubernetes/enc/enc.yaml was present as root-owned 0600 listing secretbox
	// (key1) before identity. The render was observed on a real replaced node before
	// any API server was made to depend on it, which is what v3 existed to buy.
	//
	// All three pieces are now required together, and that triple is the point: the
	// argument without the mount is an API server that cannot see a file that exists;
	// the argument without a per-cluster Secret is a node that never finishes
	// bootstrapping; the mount and Secret without the argument is v3, which encrypts
	// nothing.
	argLine := regexp.MustCompile(`(?m)^\s*encryption-provider-config:\s*/etc/kubernetes/enc/enc\.yaml\s*$`)
	mountLine := regexp.MustCompile(`(?m)^\s*mountPath:\s*/etc/kubernetes/enc\s*$`)
	perCluster := `"name": "{{ .builtin.cluster.name }}` + assets.EncryptionSecretName("")

	for _, path := range []string{defaultHubClass, defaultSpokeClass} {
		body, err := readClusterClass(path)
		if err != nil {
			t.Fatalf("readClusterClass(%q): %v", path, err)
		}
		text := string(body)

		if !argLine.MatchString(text) {
			t.Errorf("%q does not set encryption-provider-config, so adopting it rolls the "+
				"control plane and encrypts nothing -- the most expensive possible no-op", path)
		}
		if !mountLine.MatchString(text) {
			t.Errorf("%q does not mount /etc/kubernetes/enc into the API server. With the "+
				"argument set and no mount, the API server fails to start on a file that "+
				"EXISTS on the node, which reads as a missing file rather than a missing mount", path)
		}
		if !strings.Contains(text, perCluster) {
			t.Errorf("%q does not deliver the provider configuration under a per-cluster name "+
				"(%s...); a Secret named literally in a ClusterClass is one object for every "+
				"cluster of that class", path, perCluster)
		}
	}
}

func TestTheDay0TemplateTakesItsNameRatherThanSpellingOne(t *testing.T) {
	// The other half of the same name. The Go side CREATES the Secret and the CAPI
	// patch READS it, and the two are written in languages that cannot see each other
	// -- so a disagreement produces a Secret under one name referenced under another,
	// which CAPI reports as a node that never finishes bootstrapping rather than as a
	// name it could not find.
	body, err := readEncryptionConfigTemplate()
	if err != nil {
		t.Fatalf("readEncryptionConfigTemplate: %v", err)
	}
	if !strings.Contains(string(body), "name: {{ .SecretName }}") {
		t.Fatal("the Day-0 Secret template spells a name instead of taking {{ .SecretName }}; " +
			"a literal here is one object for every cluster the management plane builds")
	}
	if got := assets.EncryptionSecretName("nutgraf-01"); got != "nutgraf-01-encryption-config" {
		t.Fatalf("EncryptionSecretName changed shape to %q; the ClusterClass patches build the "+
			"same name from {{ .builtin.cluster.name }} and preflight 003-encryption-secret-is-per-cluster pins them together", got)
	}
}

func TestAMissingClassNamesBothTreesItLookedIn(t *testing.T) {
	// A "not found" naming one tree sends the next person looking in the wrong place.
	_, err := readClusterClass("classes/does-not-exist.yaml")
	if err == nil {
		t.Fatal("expected an error for a class that does not exist")
	}
	for _, want := range []string{"CLI assets", "platform manifests"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error does not mention %q: %v", want, err)
		}
	}
}

// providersOf returns the provider list of a RENDERED configuration, in order.
//
// Rendered, not raw. The template now makes the plaintext provider conditional, and a
// raw-text scan cannot tell a provider that is present from one behind a `{{ if }}`
// that is false -- it would report `identity` for both settings of the flag and the
// one assertion that matters would never fail.
//
// Comments are stripped, because the header explains WHY the plaintext provider is
// retained and a naive search finds the word "identity" above the providers and
// reports an order that is not the file's. That is what the first version of this
// test did.
func providersOf(t *testing.T, noFallback bool) []string {
	t.Helper()
	body, err := readEncryptionConfigTemplate()
	if err != nil {
		t.Fatalf("reading the template: %v", err)
	}
	tmpl, err := template.New("enc").Parse(string(body))
	if err != nil {
		t.Fatalf("parsing the template: %v", err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{
		"SecretName":          "c-encryption-config",
		"Namespace":           "platform-capi",
		"PrimaryKeyName":      assets.EncryptionKeyName(1),
		"PrimaryKeyB64":       "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"NoPlaintextFallback": noFallback,
	}); err != nil {
		t.Fatalf("rendering: %v", err)
	}
	var providers []string
	for _, line := range strings.Split(out.String(), "\n") {
		tl := strings.TrimSpace(line)
		if strings.HasPrefix(tl, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(tl, "- secretbox:"):
			providers = append(providers, "secretbox")
		case strings.HasPrefix(tl, "- identity:"):
			providers = append(providers, "identity")
		}
	}
	return providers
}

func TestByDefaultBothProvidersRenderInOrder(t *testing.T) {
	// secretbox first so every WRITE is encrypted; the plaintext provider second so
	// existing reads still succeed. Reversed, nothing is ever encrypted and the
	// cluster looks configured.
	got := providersOf(t, false)
	want := []string{"secretbox", "identity"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("default provider order is %v, want %v -- secretbox must be FIRST or writes "+
			"are never encrypted, and the plaintext provider must be second or existing reads "+
			"fail", got, want)
	}
}

func TestTheFallbackIsDroppedONLYWhenAsked(t *testing.T) {
	// THE IRREVERSIBLE ONE. While identity is in the list, a Secret this migration
	// missed is still readable and the control is merely partial. Once it is gone that
	// same Secret is UNREADABLE, and re-adding the fallback cannot decrypt what was
	// never encrypted -- so this is behind an explicit flag, and the default must stay
	// the safe direction.
	got := providersOf(t, true)
	if len(got) != 1 || got[0] != "secretbox" {
		t.Fatalf("with --no-plaintext-fallback the providers are %v, want [secretbox]; a "+
			"plaintext provider surviving that flag means step 9 of the runbook silently does "+
			"nothing and the control stays partial while reported complete", got)
	}

	// And the default must not be the dangerous one, which is the whole point.
	if d := providersOf(t, false); len(d) != 2 {
		t.Fatalf("without the flag the providers are %v; dropping the fallback by default would "+
			"make every adoption of encryption a data-loss risk on an existing cluster", d)
	}
}

func TestTheClusterClassIsNotAppliedByDefault(t *testing.T) {
	// THE DEFECT THIS PINS. The command applied the ClusterClass unconditionally, on
	// the belief that nothing reconciled it. ArgoCD's infrastructure-provider
	// ApplicationSet syncs manifests/providers/<provider>, whose kustomization pulls
	// in base/ and with it the spoke ClusterClass -- so promoting a bundle puts the
	// class on the box, and 0.1.16-rc.142 did exactly that with nobody running this
	// command.
	//
	// Applying it from here makes the CLI a second writer for a GitOps-owned object.
	// Byte-identical content hides it; the day the two disagree, whoever ran last
	// wins until the next sync, which reverts it and says nothing.
	//
	// Asserted on the FLAG's default rather than by running the command, because the
	// apply path needs a cluster. The flag is the decision; --apply-class exists only
	// for a Day-0 bootstrap cluster that has no ArgoCD yet.
	cmd := newEncryptionCmd()
	sub, _, err := cmd.Find([]string{"enable"})
	if err != nil {
		t.Fatalf("finding `enable`: %v", err)
	}
	f := sub.Flags().Lookup("apply-class")
	if f == nil {
		t.Fatal("--apply-class is gone; if applying the class became unconditional again, " +
			"the CLI is a second writer for an object ArgoCD syncs")
	}
	if f.DefValue != "false" {
		t.Fatalf("--apply-class defaults to %q, so the CLI applies a GitOps-owned "+
			"ClusterClass unless told not to; it must be opt-in", f.DefValue)
	}
}
