package diagnostics

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
)

// OutputDiagnostics prints diagnostic information for troubleshooting
func OutputDiagnostics(ctx context.Context, clusterName, kubeContext string) {
	fmt.Println("\n=== Diagnostic Information ===")
	
	// Cluster status
	fmt.Println("\n[Cluster Status]")
	cmd := exec.CommandContext(ctx, "kubectl", "--context", kubeContext,
		// Fully qualified: the short name resolves to CNPG's Cluster once
		// cloudnative-pg is installed, so diagnostics would print an error about a
		// database operator instead of the cluster being diagnosed.
		"get", "clusters.cluster.x-k8s.io", clusterName, "-n", constants.NamespaceCAPI, "-o", "yaml")
	if output, err := cmd.CombinedOutput(); err == nil {
		fmt.Println(string(output))
	} else {
		fmt.Printf("Failed to get cluster status: %v\n", err)
	}
	
	// Machines
	fmt.Println("\n[Machines]")
	cmd = exec.CommandContext(ctx, "kubectl", "--context", kubeContext,
		"get", "machines", "-n", constants.NamespaceCAPI)
	if output, err := cmd.CombinedOutput(); err == nil {
		fmt.Println(string(output))
	}
	
	// CAPI Providers
	fmt.Println("\n[CAPI Providers]")
	cmd = exec.CommandContext(ctx, "kubectl", "--context", kubeContext,
		"get", "coreprovider,infrastructureprovider,bootstrapprovider,controlplaneprovider")
	if output, err := cmd.CombinedOutput(); err == nil {
		fmt.Println(string(output))
	}
	
	// Recent events
	fmt.Println("\n[Recent Events]")
	cmd = exec.CommandContext(ctx, "kubectl", "--context", kubeContext,
		"get", "events", "-n", constants.NamespaceCAPI, "--sort-by=.lastTimestamp")
	if output, err := cmd.CombinedOutput(); err == nil {
		lines := strings.Split(string(output), "\n")
		// Show last 10 events
		start := len(lines) - 11
		if start < 0 {
			start = 0
		}
		fmt.Println(strings.Join(lines[start:], "\n"))
	}
	
	fmt.Println("\n=== End Diagnostics ===")
	fmt.Println("\nTo preserve the bootstrap cluster for debugging, use --keep-bootstrap flag")
}
