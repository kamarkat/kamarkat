package openbao

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// OpenBao refuses a leaf that outlives the issuing intermediate
// (leaf_not_after_behavior=err), and the intermediate is valid
// DefaultIntermediateValidity from signing. A Certificate asking for that long
// or longer is refused on its first issuance: argocd-agent-jwt asked for 43800h
// against a 43800h intermediate and stayed not Ready.
func TestNoOpenBaoLeafOutlivesTheIntermediate(t *testing.T) {
	type doc struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec struct {
			Vault     map[string]any `yaml:"vault"`
			Duration  string         `yaml:"duration"`
			IssuerRef struct {
				Name string `yaml:"name"`
			} `yaml:"issuerRef"`
		} `yaml:"spec"`
	}
	var docs []struct {
		file string
		d    doc
	}
	filepath.WalkDir(filepath.Join("..", "..", "..", "manifests"), func(path string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil || !bytes.Contains(b, []byte("cert-manager.io")) {
			return nil
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		for {
			var d doc
			err := dec.Decode(&d)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				break // templated files do not parse; their Certificates are covered by their sources
			}
			docs = append(docs, struct {
				file string
				d    doc
			}{path, d})
		}
		return nil
	})

	vaultIssuers := map[string]bool{}
	for _, x := range docs {
		if (x.d.Kind == "ClusterIssuer" || x.d.Kind == "Issuer") && x.d.Spec.Vault != nil {
			vaultIssuers[x.d.Metadata.Name] = true
		}
	}
	// The hub's Vault ClusterIssuers are rendered by a Helm range over a dict of
	// names, which no YAML decoder sees.
	tpl, err := os.ReadFile(filepath.Join("..", "..", "..", "manifests", "hub-core-services",
		"security", "templates", "openbao-issuers.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`"(openbao-[a-z-]+)" \.Values`).FindAllSubmatch(tpl, -1) {
		vaultIssuers[string(m[1])] = true
	}
	if !vaultIssuers["openbao-signing"] {
		t.Fatalf("openbao-signing not found among the Vault issuers %v; the scan is broken", vaultIssuers)
	}
	if len(vaultIssuers) == 0 {
		t.Fatal("no OpenBao-backed issuer found; this test would prove nothing")
	}
	for _, x := range docs {
		if x.d.Kind != "Certificate" || !vaultIssuers[x.d.Spec.IssuerRef.Name] || x.d.Spec.Duration == "" {
			continue
		}
		dur, err := time.ParseDuration(x.d.Spec.Duration)
		if err != nil {
			continue
		}
		if dur >= DefaultIntermediateValidity {
			t.Errorf("%s: Certificate %s asks %s from %s, not less than the %s intermediate; OpenBao refuses it",
				x.file, x.d.Metadata.Name, x.d.Spec.Duration, x.d.Spec.IssuerRef.Name, DefaultIntermediateValidity)
		}
	}
}
