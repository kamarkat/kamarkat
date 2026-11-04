package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/versions"
)

func newBundleVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bundle-version",
		Short: "Print the bundle version this build carries",
		Long: `Print the bundle version this build carries.

It reports; it does not derive. The value was written into the binary by the
release pipeline, and ADR-068 forbids computing it a second time: the version a
cluster requests and the version that was published are the same string because
they are the same constant, not because two implementations agree.

"development" means no version was injected. That is what an ordinary build is,
and it means platform content is reconciled from the working tree rather than
from a published bundle.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Println(versions.BundleVersion)
			return nil
		},
	}
}
