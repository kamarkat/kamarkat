package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newConfigureESOCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "configure-eso",
		Short: "DEPRECATED: Use 'hub init-secrets' instead",
		Long: `This command is DEPRECATED and will be removed in a future release.

Machine Identity credentials are now automatically created during:
  hub init-secrets

The init-secrets command handles the full Infisical Day-0 bootstrap:
1. Creates admin user and organization
2. Creates the hub-platform project
3. Creates a Machine Identity with Universal Auth
4. Generates client secret
5. Creates the infisical-auth Secret
6. Patches hub-bootstrap-config ConfigMap

Running this command will overwrite the auto-generated Machine Identity
and break all running operators.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("╔══════════════════════════════════════════════════════════════╗")
			fmt.Println("║  DEPRECATED                                               ║")
			fmt.Println("╠══════════════════════════════════════════════════════════════╣")
			fmt.Println("║  'hub configure-eso' is deprecated.                       ║")
			fmt.Println("║                                                           ║")
			fmt.Println("║  Machine Identity credentials are now automatically       ║")
			fmt.Println("║  created during 'hub init-secrets'.                       ║")
			fmt.Println("║                                                           ║")
			fmt.Println("║  Running this command would overwrite the auto-generated   ║")
			fmt.Println("║  credentials and break the cert-operator and hub-operator.║")
			fmt.Println("╚══════════════════════════════════════════════════════════════╝")
			return fmt.Errorf("'hub configure-eso' is deprecated — use 'hub init-secrets' instead")
		},
	}

	return cmd
}
