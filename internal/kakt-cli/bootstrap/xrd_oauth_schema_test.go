package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every field the tenant chart writes onto the XR must exist in the XRD schema.
//
// Crossplane's XRD schema is not advisory and it does not merely drop unknown
// fields -- the server-side apply is REFUSED:
//
//	failed to create typed patch object (/waypoint; nutgraf.in/v1alpha1,
//	Kind=AINativeSaaS): .spec.oauth.clients[0].redirectPaths:
//	field not declared in schema
//
// The consequence is not local to the field. The fleet's XR never applies, so
// the tenant record is never created, so no OAuth client is provisioned, so the
// gateway's ExternalSecrets never resolve and its pod sits in
// CreateContainerConfigError -- four symptoms away from the schema that caused
// it.
//
// This drifted because the two are owned by different concerns and neither
// referenced the other: the chart was taught to pass redirectPaths when Zitadel
// replaced Hydra (Zitadel has no OAuth2Client controller, so the registration
// data has to travel on the XR), while the XRD kept a description asserting
// that redirect URIs "reach the registration controller through the chart, not
// through this resource" -- which had been true and silently stopped being so.
func TestXRDSchemaDeclaresEveryOAuthClientFieldTheChartWrites(t *testing.T) {
	root := filepath.Join("..", "..", "..")

	tmpl, err := os.ReadFile(filepath.Join(root, "manifests", "tenants", "charts",
		"universal-tenant", "templates", "ainativesaas.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	written := oauthClientFieldsWritten(string(tmpl))
	if len(written) == 0 {
		t.Fatal("no oauth client fields parsed from the chart: the parser has drifted " +
			"from the template, and an empty set would make this gate vacuous")
	}

	raw, err := os.ReadFile(filepath.Join(root, "manifests", "hub-core-services", "crossplane",
		"tenant-platform", "xrds", "ainativesaas-v1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var xrd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties struct {
									OAuth struct {
										Properties struct {
											Clients struct {
												Items struct {
													Properties map[string]any `yaml:"properties"`
												} `yaml:"items"`
											} `yaml:"clients"`
										} `yaml:"properties"`
									} `yaml:"oauth"`
								} `yaml:"properties"`
							} `yaml:"spec"`
						} `yaml:"properties"`
					} `yaml:"openAPIV3Schema"`
				} `yaml:"schema"`
			} `yaml:"versions"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &xrd); err != nil {
		t.Fatal(err)
	}
	if len(xrd.Spec.Versions) == 0 {
		t.Fatal("the XRD declares no versions")
	}
	declared := xrd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.
		Properties.OAuth.Properties.Clients.Items.Properties

	for _, f := range written {
		if _, ok := declared[f]; !ok {
			t.Errorf("the chart writes spec.oauth.clients[].%s but the XRD does not declare it: "+
				"Crossplane refuses the whole apply, so the fleet's XR never lands", f)
		}
	}
}

// oauthClientFieldsWritten reads the field names the chart emits inside the
// `clients:` range. Textual because the file is a Helm template and does not
// parse as YAML.
func oauthClientFieldsWritten(tmpl string) []string {
	i := strings.Index(tmpl, "    clients:")
	if i < 0 {
		return nil
	}
	// Start AFTER the `clients:` line itself: it is the list's key, not a field
	// of a list item.
	if nl := strings.IndexByte(tmpl[i:], '\n'); nl >= 0 {
		i += nl + 1
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(tmpl[i:], "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "{{") || t == "" {
			continue
		}
		t = strings.TrimPrefix(t, "- ")
		k, _, ok := strings.Cut(t, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		// Values rendered from the template, not field names.
		if k == "" || strings.HasPrefix(k, "{{") || strings.Contains(k, " ") {
			continue
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}
