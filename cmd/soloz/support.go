package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/soloz-io/zero-ops/internal/platform"
	"github.com/soloz-io/zero-ops/internal/support"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// soloz support — read what the Support Agent would send, before it sends it.
//
// ADR-077 takes this from insights-client's --no-upload: a tenant asked to trust
// an outbound stream should be able to read it first, and an engineer debugging
// a gap should see exactly what the agent sees. It is a security feature and a
// support feature at once, and it is the reason the allowlist is a contract
// rather than an assurance.
func newSupportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "support",
		Short: "Inspect the evidence this box would report to the platform",
	}
	cmd.AddCommand(newSupportPreviewCmd(), newSupportScopeCmd())
	return cmd
}

// loadAllowlist reads the shipped ConfigMap and applies the tenant's denylist.
func loadAllowlist(deny []string) (*support.Allowlist, error) {
	raw, err := platform.ReadFile(filepath.Join(
		"manifests", "hub-core-services", "support-agent", "allowlist.yaml"))
	if err != nil {
		return nil, err
	}
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		return nil, fmt.Errorf("the allowlist ConfigMap does not parse: %w", err)
	}
	body, ok := cm.Data["allowlist.yaml"]
	if !ok {
		return nil, fmt.Errorf("the allowlist ConfigMap has no allowlist.yaml key")
	}
	a, err := support.Parse([]byte(body))
	if err != nil {
		return nil, err
	}
	return a.Effective(deny), nil
}

func newSupportScopeCmd() *cobra.Command {
	var deny []string
	cmd := &cobra.Command{
		Use:   "scope",
		Short: "List every field that may leave this box",
		Long: "The effective scope: the platform's allowlist minus anything this\n" +
			"tenant has denied. Nothing outside this list can be emitted, and no\n" +
			"value can add to it — the platform widens it only by publishing a\n" +
			"bundle version, which arrives as a pull request (ADR-064).",
		RunE: func(*cobra.Command, []string) error {
			a, err := loadAllowlist(deny)
			if err != nil {
				return err
			}
			for _, f := range a.Fields() {
				fmt.Println(f)
			}
			fmt.Fprintf(os.Stderr, "\n%d field(s) across %d collector(s)\n",
				len(a.Fields()), len(a.Collectors))
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&deny, "deny", nil,
		"collectors or fields this tenant subtracts, as `collector` or `collector.field`")
	return cmd
}

func newSupportPreviewCmd() *cobra.Command {
	var (
		gitopsDir string
		certFile  string
		keyFile   string
		deny      []string
		asJSON    bool
	)
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Render the payload this box would send, and send nothing",
		Long: "Runs every collector in the effective scope against this cluster and\n" +
			"prints the result. Nothing is transmitted.\n\n" +
			"A collector that could not run is listed with the reason rather than\n" +
			"omitted: a payload missing a collector silently would look the same as\n" +
			"a box where the thing it watches is fine.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := loadAllowlist(deny)
			if err != nil {
				return err
			}
			// The identity comes from the certificate, exactly as the agent
			// takes it. Typing a cluster name here would let preview render a
			// payload labelled differently from the one that would actually be
			// sent, which defeats the point of being able to read it first.
			enr := support.Enrolment{CertFile: certFile, KeyFile: keyFile}
			identity, err := enr.Identity()
			if err != nil {
				// Not an error. An unenrolled box is the normal state, and
				// showing what it WOULD send is most of why this exists.
				identity = "(not enrolled: no client certificate)"
			}

			r := &support.ClusterReader{GitopsDir: gitopsDir}
			p := support.Collect(cmd.Context(), a, r, identity)

			if asJSON {
				b, err := json.MarshalIndent(p, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(b))
				return nil
			}
			printPayload(cmd.Context(), p)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&gitopsDir, "gitops-dir", ".", "the tenant repository artefact collectors read")
	f.StringVar(&certFile, "cert", "/etc/support-agent/tls/tls.crt",
		"the client certificate whose subject is the identity this box reports as")
	f.StringVar(&keyFile, "key", "/etc/support-agent/tls/tls.key", "the matching key")
	f.StringSliceVar(&deny, "deny", nil, "collectors or fields this tenant subtracts")
	f.BoolVar(&asJSON, "json", false, "print the payload as JSON")
	return cmd
}

func printPayload(_ context.Context, p support.Payload) {
	fmt.Printf("reporting as %s, collected %s\n\n", p.Cluster, p.Collected)

	byCollector := map[string][]map[string]string{}
	var order []string
	for _, r := range p.Records {
		if _, seen := byCollector[r.Collector]; !seen {
			order = append(order, r.Collector)
		}
		byCollector[r.Collector] = append(byCollector[r.Collector], r.Fields)
	}
	for _, name := range order {
		rows := byCollector[name]
		fmt.Printf("%s  (%d record(s))\n", name, len(rows))
		for _, row := range rows {
			keys := make([]string, 0, len(row))
			for k := range row {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, k+"="+row[k])
			}
			fmt.Println("    " + strings.Join(parts, " "))
		}
		fmt.Println()
	}

	if len(p.Skipped) > 0 {
		names := make([]string, 0, len(p.Skipped))
		for n := range p.Skipped {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Println("not collected:")
		for _, n := range names {
			fmt.Printf("    %-22s %s\n", n, p.Skipped[n])
		}
		fmt.Println()
	}
	fmt.Println("Nothing was transmitted. This is the whole payload; no field outside")
	fmt.Println("the scope above can appear in it (soloz support scope).")
}
