package spoke

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/hcloud"
)

// Orchestrator manages spoke cluster operations
type Orchestrator struct {
	Debug       bool
	Force       bool
	ClusterName string // Optional: specific spoke cluster name, if empty will delete all
	// MgmtCluster is the management cluster, excluded from discovery.
	//
	// kubefirst's vocabulary (kubefirst-api/pkg/types/cluster.go: Type is one of
	// mgmt|workload), which internal/kakt-cli/tenant already follows -- see
	// WorkloadCluster.MgmtCluster and `tenant add-cluster --mgmt-cluster`.
	//
	// CAPH labels the management cluster's servers caph-cluster-<name>=owned
	// exactly as it labels a workload cluster's, so a discovery keyed on that
	// label alone cannot tell them apart. The old code excluded it by accident,
	// matching only names beginning "spoke-pool-"; with tenant-chosen names
	// (ADR-082) that accident is gone and the exclusion has to be stated.
	MgmtCluster string
}

// Run executes the spoke operation
func (o *Orchestrator) Run(ctx context.Context) error {
	if o.Debug {
		fmt.Println("[DEBUG] Spoke.Orchestrator.Run() started")
		fmt.Printf("[DEBUG] ClusterName: %s, Force: %v\n", o.ClusterName, o.Force)
	}

	if o.Force {
		return o.forceDelete(ctx)
	}

	return o.gracefulDelete(ctx)
}

// gracefulDelete attempts to delete spoke clusters via Kubernetes API first
func (o *Orchestrator) gracefulDelete(ctx context.Context) error {
	fmt.Println("\n[spoke-teardown] Starting graceful deletion of spoke clusters...")
	
	// For graceful deletion, we would need access to the hub cluster
	// Since hub might already be deleted, we'll fall back to force deletion
	fmt.Println("[spoke-teardown] Hub cluster not accessible, falling back to force deletion")
	return o.forceDelete(ctx)
}

// forceDelete deletes spoke clusters directly via Hetzner API
func (o *Orchestrator) forceDelete(ctx context.Context) error {
	fmt.Println("\n[spoke-teardown] ⚠️  Force deletion mode enabled")
	fmt.Println("[spoke-teardown] This will delete spoke resources directly via Hetzner API")

	hcloudToken := os.Getenv("HCLOUD_TOKEN")
	if hcloudToken == "" {
		return fmt.Errorf("HCLOUD_TOKEN environment variable required for force deletion")
	}

	client := hcloud.NewClient(hcloud.WithToken(hcloudToken))

	// Discover spoke clusters
	spokeClusters, err := o.discoverSpokeClusters(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to discover spoke clusters: %w", err)
	}

	if len(spokeClusters) == 0 {
		fmt.Println("[spoke-teardown] No spoke clusters found")
		return nil
	}

	fmt.Printf("[spoke-teardown] Found %d spoke cluster(s) to delete\n", len(spokeClusters))

	// Delete each spoke cluster
	for _, spokeName := range spokeClusters {
		if o.ClusterName != "" && spokeName != o.ClusterName {
			continue // Skip if specific cluster name requested but doesn't match
		}
		
		fmt.Printf("[spoke-teardown] Deleting spoke cluster: %s\n", spokeName)
		if err := o.deleteSpokeCluster(ctx, client, spokeName); err != nil {
			fmt.Printf("[spoke-teardown] ⚠️  Failed to delete spoke cluster %s: %v\n", spokeName, err)
		} else {
			fmt.Printf("[spoke-teardown] ✓ Deleted spoke cluster: %s\n", spokeName)
		}
	}

	fmt.Println("[spoke-teardown] ✓ Spoke cluster deletion complete")
	return nil
}

// discoverSpokeClusters finds all spoke clusters in Hetzner
func (o *Orchestrator) discoverSpokeClusters(ctx context.Context, client *hcloud.Client) ([]string, error) {
	spokeNames := make(map[string]bool)

	// Method 1: Look for servers with spoke-related labels
	allServers, err := client.Server.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list servers: %w", err)
	}

	for _, server := range allServers {
		// CAPH labels every server it owns caph-cluster-<cluster>=owned, and the
		// cluster name is the whole of what follows the prefix.
		//
		// This read it three wrong ways at once, all of them rooted in the name
		// spoke-pool-<region>-<env>-01 no longer existing. It matched the prefix
		// "caph-cluster-spoke", which a tenant-named cluster (nutgraf-01, ADR-082)
		// never carries, so nothing was discovered and `kakt spoke teardown` was
		// a no-op that reported success. It then took parts[2:5] of the split,
		// which even for the old name yielded "spoke-pool-eu" rather than the
		// "spoke-pool-eu-prod-01" its own comment claimed -- a name matching no
		// cluster, so the deletes that followed found nothing either. And it fell
		// back to server names beginning "spoke-pool-", a prefix a tenant-named
		// spoke does not have.
		for labelKey := range server.Labels {
			name := strings.TrimPrefix(labelKey, "caph-cluster-")
			if name == labelKey || name == "" {
				continue // not a CAPH ownership label
			}
			// The management cluster carries the same label shape and must never be
			// swept by a workload teardown: ClusterName is optional here, and empty
			// means "every workload cluster", which without this exclusion would
			// mean the management cluster too.
			if o.MgmtCluster != "" && name == o.MgmtCluster {
				continue
			}
			spokeNames[name] = true
		}
	}

	// Method 2: Look for load balancers with spoke labels
	allLBs, err := client.LoadBalancer.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list load balancers: %w", err)
	}

	for _, lb := range allLBs {
		for labelKey := range lb.Labels {
			if strings.HasPrefix(labelKey, "caph-cluster-spoke") {
				parts := strings.Split(labelKey, "-")
				if len(parts) >= 5 {
					spokeName := strings.Join(parts[2:5], "-")
					spokeNames[spokeName] = true
				}
			}
		}
	}

	// Convert map to slice
	var result []string
	for spokeName := range spokeNames {
		result = append(result, spokeName)
	}

	return result, nil
}

// deleteSpokeCluster deletes all resources for a specific spoke cluster
func (o *Orchestrator) deleteSpokeCluster(ctx context.Context, client *hcloud.Client, spokeName string) error {
	labelPattern := fmt.Sprintf("caph-cluster-%s", spokeName)

	matchesSpoke := func(name string, labels map[string]string) bool {
		if strings.HasPrefix(name, spokeName) || strings.Contains(name, spokeName) {
			return true
		}
		for k, v := range labels {
			if strings.HasPrefix(k, labelPattern) || strings.Contains(k, spokeName) {
				return true
			}
			if v == spokeName || strings.Contains(v, spokeName) {
				return true
			}
		}
		return false
	}

	deletedServerIDs := make(map[int]bool)

	// 1. Delete servers
	allServers, err := client.Server.All(ctx)
	if err == nil {
		for _, server := range allServers {
			if matchesSpoke(server.Name, server.Labels) {
				deletedServerIDs[server.ID] = true
				fmt.Printf("[spoke-teardown] Deleting server: %s (ID: %d)\n", server.Name, server.ID)
				if _, _, err := client.Server.DeleteWithResult(ctx, server); err != nil {
					fmt.Printf("[spoke-teardown] ⚠️  Failed to delete server %s: %v\n", server.Name, err)
				} else {
					fmt.Printf("[spoke-teardown] ✓ Deleted server: %s\n", server.Name)
				}
			}
		}
	} else {
		fmt.Printf("[spoke-teardown] ⚠️  Failed to list servers: %v\n", err)
	}

	// 2. Delete load balancers
	allLBs, err := client.LoadBalancer.All(ctx)
	if err == nil {
		for _, lb := range allLBs {
			if matchesSpoke(lb.Name, lb.Labels) {
				fmt.Printf("[spoke-teardown] Deleting load balancer: %s (ID: %d)\n", lb.Name, lb.ID)
				if _, err := client.LoadBalancer.Delete(ctx, lb); err != nil {
					fmt.Printf("[spoke-teardown] ⚠️  Failed to delete load balancer %s: %v\n", lb.Name, err)
				} else {
					fmt.Printf("[spoke-teardown] ✓ Deleted load balancer: %s\n", lb.Name)
				}
			}
		}
	} else {
		fmt.Printf("[spoke-teardown] ⚠️  Failed to list load balancers: %v\n", err)
	}

	// 3. Delete volumes
	allVolumes, err := client.Volume.All(ctx)
	if err == nil {
		for _, volume := range allVolumes {
			isAttached := volume.Server != nil && deletedServerIDs[volume.Server.ID]
			if matchesSpoke(volume.Name, volume.Labels) || isAttached {
				fmt.Printf("[spoke-teardown] Deleting volume: %s (ID: %d, Size: %d GB)\n", volume.Name, volume.ID, volume.Size)
				if volume.Server != nil {
					client.Volume.Detach(ctx, volume)
				}
				if _, err := client.Volume.Delete(ctx, volume); err != nil {
					fmt.Printf("[spoke-teardown] ⚠️  Failed to delete volume %s: %v\n", volume.Name, err)
				} else {
					fmt.Printf("[spoke-teardown] ✓ Deleted volume: %s\n", volume.Name)
				}
			}
		}
	}

	// 4. Delete Placement Groups
	allPGs, err := client.PlacementGroup.All(ctx)
	if err == nil {
		for _, pg := range allPGs {
			if matchesSpoke(pg.Name, pg.Labels) {
				fmt.Printf("[spoke-teardown] Deleting placement group: %s (ID: %d)\n", pg.Name, pg.ID)
				if _, err := client.PlacementGroup.Delete(ctx, pg); err != nil {
					fmt.Printf("[spoke-teardown] ⚠️  Failed to delete placement group %s: %v\n", pg.Name, err)
				} else {
					fmt.Printf("[spoke-teardown] ✓ Deleted placement group: %s\n", pg.Name)
				}
			}
		}
	}

	// 5. Delete Firewalls
	allFirewalls, err := client.Firewall.All(ctx)
	if err == nil {
		for _, fw := range allFirewalls {
			if matchesSpoke(fw.Name, fw.Labels) {
				fmt.Printf("[spoke-teardown] Deleting firewall: %s (ID: %d)\n", fw.Name, fw.ID)
				if _, err := client.Firewall.Delete(ctx, fw); err != nil {
					fmt.Printf("[spoke-teardown] ⚠️  Failed to delete firewall %s: %v\n", fw.Name, err)
				} else {
					fmt.Printf("[spoke-teardown] ✓ Deleted firewall: %s\n", fw.Name)
				}
			}
		}
	}

	// 6. Delete networks
	allNetworks, err := client.Network.All(ctx)
	if err == nil {
		for _, network := range allNetworks {
			if matchesSpoke(network.Name, network.Labels) {
				fmt.Printf("[spoke-teardown] Deleting network: %s (ID: %d)\n", network.Name, network.ID)
				if _, err := client.Network.Delete(ctx, network); err != nil {
					time.Sleep(2 * time.Second)
					if _, err := client.Network.Delete(ctx, network); err != nil {
						fmt.Printf("[spoke-teardown] ⚠️  Failed to delete network %s: %v\n", network.Name, err)
					} else {
						fmt.Printf("[spoke-teardown] ✓ Deleted network: %s (on retry)\n", network.Name)
					}
				} else {
					fmt.Printf("[spoke-teardown] ✓ Deleted network: %s\n", network.Name)
				}
			}
		}
	}

	return nil
}