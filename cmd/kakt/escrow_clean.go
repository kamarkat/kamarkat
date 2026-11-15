package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// newEscrowCleanCmd forgets this workstation's escrow configuration.
//
// LOCAL ONLY, and that is the whole of it. A box is not its workstation: the
// escrow project, every artefact in it, the machine identity and its client
// secrets, the box's repository secrets, its GitOps repository and its clusters
// are all untouched. The box keeps running, because its CI authenticates with
// the secrets on its own repository and its escrow stays the authority for its
// recovery.
//
// It exists because the workstation holds one escrow configuration and nothing
// in it says which box that is. Moving to another box therefore has to be
// deliberate -- `escrow init` refuses rather than adopting what it finds, and
// this is the command that refusal names.
func newEscrowCleanCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Forget this workstation's escrow configuration (local only)",
		Long: `Remove this workstation's escrow configuration and its box binding.

Nothing remote is touched: not the escrow project, not the artefacts in it, not
the machine identity or its client secrets, not the box's repository secrets,
and not any cluster. The box this workstation was configured for keeps running.

Afterwards no previous coordinates are inherited: the next ` + "`kakt escrow init`" + `
starts from nothing, and a box may be created from it.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			dir, err := escrow.StorePath()
			if err != nil {
				return err
			}
			project, box := escrow.ConfiguredProject(), escrow.ActiveBox()
			if project == "" && box == "" {
				fmt.Printf("nothing to clean: %s holds no escrow configuration\n", dir)
				return nil
			}

			// WHAT IS BEING FORGOTTEN, before anything is removed. The project id
			// is the only way back to a box whose repository is gone, so an
			// operator who did not mean this has to be able to see it and stop.
			fmt.Println("This clears the escrow configuration on THIS MACHINE only:")
			fmt.Printf("  store      %s\n", dir)
			if project != "" {
				fmt.Printf("  project    %s\n", project)
			}
			if u := escrow.ConfiguredURL(); u != "" {
				fmt.Printf("  escrow     %s\n", u)
			}
			if box != "" {
				fmt.Printf("  box        %s\n", box)
			}
			fmt.Println()
			fmt.Println("The project, its escrowed artefacts, the machine identity, the box's")
			fmt.Println("repository secrets and its clusters are NOT touched. The box keeps running.")
			fmt.Println()
			if project != "" {
				fmt.Println("Keep the project id above if this box has to be reached again from here.")
				fmt.Println()
			}

			if !yes {
				fmt.Print("Clear it? [y/N] ")
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
					return fmt.Errorf("not cleared")
				}
			}

			removed, err := escrow.ClearLocal()
			if err != nil {
				return err
			}
			if len(removed) == 0 {
				fmt.Println("nothing was there to remove")
				return nil
			}
			for _, name := range removed {
				fmt.Printf("  removed %s\n", name)
			}
			fmt.Println("\nThis workstation holds no escrow configuration. Next:")
			fmt.Println("  kakt escrow init --name <name>")
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Do not ask for confirmation")
	return cmd
}
