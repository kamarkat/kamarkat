package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/versions"
	"github.com/kamarkat/kamarkat/internal/platform/boxmcp"
)

func newMCPCmd() *cobra.Command {
	var kubeconfig, kubecontext, disable string
	var enableVerify bool
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve this box's platform state to a local MCP client, over stdio",
		Long: `Serve this box's platform state to a local MCP client, over stdio (ADR-116 §4).

The same read-only tools the box's kamarkat-mcp serves to kakt -- clusters,
boundaries, Applications, certificates, backups, an app's workloads and logs --
for any MCP client on this machine: Claude Code, Cursor, an IDE. Nothing is
hosted and nothing leaves the machine except the calls to your own cluster.

It reads with YOUR kubeconfig, so it sees what you may see. Use the OIDC
kubeconfig your box's identity provider issues you (ADR-076), or, when the
identity provider is the thing that is down, the break-glass one from
` + "`kakt kubeconfig`" + `. Nothing here changes the box: changes are commits.

Register it with a client, e.g.:

  claude mcp add kamarkat -- kakt mcp --kubeconfig ~/.kube/acme-mgmt.kubeconfig`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rules := clientcmd.NewDefaultClientConfigLoadingRules()
			if kubeconfig != "" {
				rules.ExplicitPath = kubeconfig
			}
			cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
				&clientcmd.ConfigOverrides{CurrentContext: kubecontext}).ClientConfig()
			if err != nil {
				return fmt.Errorf("load the kubeconfig: %w", err)
			}
			cfg.UserAgent = "kakt-mcp/" + versions.BundleVersion
			core, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return err
			}
			dyn, err := dynamic.NewForConfig(cfg)
			if err != nil {
				return err
			}
			disc, err := discovery.NewDiscoveryClientForConfig(cfg)
			if err != nil {
				return err
			}
			clients := &boxmcp.Clients{Core: core, Dynamic: dyn, Discovery: disc}

			var disabled []string
			for _, c := range strings.Split(disable, ",") {
				if c = strings.TrimSpace(c); c != "" {
					disabled = append(disabled, c)
				}
			}
			boxmcp.Version = versions.BundleVersion
			server := boxmcp.NewServer(boxmcp.Options{
				Disabled:     disabled,
				EnableVerify: enableVerify,
				Clients:      func(_ context.Context) (*boxmcp.Clients, error) { return clients, nil },
			})
			// stdout is the protocol; anything human goes to stderr.
			fmt.Fprintf(os.Stderr, "kakt mcp: serving %s over stdio\n", strings.Join(boxmcp.Options{Disabled: disabled, EnableVerify: enableVerify}.EnabledCategories(), ", "))
			return server.Run(cmd.Context(), &mcp.StdioTransport{})
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig to read the box with (default: KUBECONFIG or ~/.kube/config)")
	cmd.Flags().StringVar(&kubecontext, "context", "", "kubeconfig context to use (default: the current one)")
	cmd.Flags().StringVar(&disable, "disable", "", "comma-separated tool categories to leave out (box, clusters, gitops, platform, apps, certs, backups, envs)")
	cmd.Flags().BoolVar(&enableVerify, "enable-verify", false, "serve the verify category")
	return cmd
}
