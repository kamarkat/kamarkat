package bootstrap

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/teardown"
	"github.com/kamarkat/kamarkat/internal/platform"
)

// hubHostnamePattern finds the hostnames the hub Gateway publishes, one per
// listener, from the chart itself so the list cannot drift from it.
var hubHostnamePattern = regexp.MustCompile(`hostname: ([a-z0-9-]+)\.\{\{ \.Values\.global\.hubDomain \}\}`)

// hubHostnames returns the labels of every hostname the hub Gateway publishes.
func hubHostnames() ([]string, error) {
	raw, err := platform.ReadFile(filepath.Join("manifests", "hub-core-services", "gateway",
		"templates", "hub-gateway.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read the hub Gateway template: %w", err)
	}
	seen := map[string]bool{}
	var labels []string
	for _, m := range hubHostnamePattern.FindAllStringSubmatch(string(raw), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			labels = append(labels, m[1])
		}
	}
	if len(labels) == 0 {
		return nil, fmt.Errorf("the hub Gateway template declares no hostnames under global.hubDomain")
	}
	return labels, nil
}

// dnsForeignOwners is a seam for tests.
var dnsForeignOwners = teardown.ForeignDNSOwners

// checkHubHostnamesUnclaimed refuses to bootstrap a box whose hub hostnames are
// held by another external-dns owner -- in practice a torn-down box whose
// records were never released. external-dns will not touch them and logs
// nothing, so without this the run fails 15 minutes after finalize on whatever
// first calls a hub hostname, far from the cause.
//
// Runs on every bootstrap, fresh or resumed, because it is cheap and the zone
// can change between runs. A box that declares no Hetzner DNS is not checked.
// An API failure is reported and does not stop the run: this check must not
// make DNS availability a precondition of bootstrapping.
func (o *Orchestrator) checkHubHostnamesUnclaimed(ctx context.Context) error {
	owner, zone, err := teardown.DeclaredDNSOwnership(o.GitopsDir, o.ClusterName)
	if err != nil || owner == "" || zone == "" {
		return nil
	}
	token := teardown.DNSToken("")
	if token == "" {
		fmt.Println("[dns-check] ⚠️  no Hetzner token in the environment; hostname ownership not checked")
		return nil
	}
	labels, err := hubHostnames()
	if err != nil {
		return err
	}
	hosts := make([]string, 0, len(labels))
	for _, l := range labels {
		hosts = append(hosts, l+"."+zone)
	}
	foreign, err := dnsForeignOwners(ctx, token, zone, owner, hosts)
	if err != nil {
		fmt.Printf("[dns-check] ⚠️  hostname ownership could not be checked: %v\n", err)
		return nil
	}
	if len(foreign) == 0 {
		fmt.Printf("[dns-check] ✓ no hostname this box publishes under %s is held by another owner\n", zone)
		return nil
	}
	byOwner := map[string][]string{}
	for h, o := range foreign {
		byOwner[o] = append(byOwner[o], h)
	}
	owners := make([]string, 0, len(byOwner))
	for o := range byOwner {
		owners = append(owners, o)
	}
	sort.Strings(owners)
	var b strings.Builder
	fmt.Fprintf(&b, "hostnames this box publishes under %s are held by another DNS owner, "+
		"not this box (%s).\nThis box never updates a record another owner holds, so they keep "+
		"pointing at the other box's address and nothing can reach this box by them:\n", zone, owner)
	for _, o := range owners {
		sort.Strings(byOwner[o])
		fmt.Fprintf(&b, "  owner %s: %s\n", o, strings.Join(byOwner[o], ", "))
	}
	b.WriteString("\nIf that owner is a box that no longer exists, release its records (this " +
		"deletes public DNS records; confirm the owner first):\n")
	for _, o := range owners {
		fmt.Fprintf(&b, "  kakt teardown --dns-only --dns-owner %s --dns-zone %s --confirm\n", o, zone)
	}
	b.WriteString("then re-run the bootstrap.")
	return fmt.Errorf("%s", b.String())
}
