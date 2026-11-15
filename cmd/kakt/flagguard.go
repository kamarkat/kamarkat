package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// refuseSwallowedFlagValue catches a value flag typed with no value, which takes
// the NEXT FLAG as its value.
//
//	kakt box init --tenant --git-org nutgraf-inc --domain nutgraf.in
//
// pflag does exactly what it is told: `--tenant` wants a value and `--git-org`
// is the next word, so the tenant becomes "--git-org", `--git-org` is never set,
// and "nutgraf-inc" becomes a positional argument. The command then failed on
// the unset `--git-org` and explained at length how to obtain a GitHub
// credential -- which is neither the cause nor near it. The operator reads a
// page about tokens when what they did was omit one word.
//
// Nothing this CLI takes as a value begins with a hyphen -- not a tenant, an
// organisation, a domain, a region, a version, a path or a URL -- so the test is
// exact rather than a heuristic. "-" alone is left alone, because it is the
// conventional name for standard input.
func refuseSwallowedFlagValue(cmd *cobra.Command) error {
	var err error
	check := func(f *pflag.Flag) {
		if err != nil || f.Value.Type() != "string" {
			return
		}
		v := f.Value.String()
		if v == "-" || !strings.HasPrefix(v, "-") {
			return
		}
		err = fmt.Errorf(`--%s was given %q as its value, which is another flag.

A flag that takes a value and is typed without one takes the next word instead,
so --%s has swallowed it and %s is now unset:

  --%s %s          <- what was read
  --%s <value> %s  <- what was meant

Nothing here takes a value beginning with "-".`,
			f.Name, v, f.Name, v, f.Name, v, f.Name, v)
	}
	cmd.Flags().Visit(check)
	cmd.InheritedFlags().Visit(check)
	return err
}
