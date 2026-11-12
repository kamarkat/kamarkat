package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// pflagFlag is pflag.Flag, aliased so the visitor signatures below stay short.
type pflagFlag = pflag.Flag

// THE WHOLE COMMAND TREE IS BUILT, because building it is where it breaks.
//
// Cobra registers flags when a command is constructed, and a duplicate flag name
// panics at that moment. `main()` constructs every command before parsing any
// argument, so one duplicate takes down EVERY invocation of this binary -- `kakt
// escrow init`, `kakt login`, `kakt --help`, all of it -- while the package still
// compiles and every other test still passes.
//
// That is exactly what shipped once: a second --gitops-dir added to `proposal
// preflight`, which already had one, and the panic appeared on an unrelated
// command. A compile cannot catch it and no other test constructed the tree.
func TestEveryCommandBuilds(t *testing.T) {
	root := newRootCmd()
	if root == nil {
		t.Fatal("no root command")
	}

	var walk func(c *cobra.Command, path string) int
	walk = func(c *cobra.Command, path string) int {
		// Touching the flag sets is what forces lazy registration to happen.
		c.Flags().VisitAll(func(*pflagFlag) {})
		n := 1
		for _, sub := range c.Commands() {
			n += walk(sub, path+" "+sub.Name())
		}
		return n
	}
	if n := walk(root, "kakt"); n < 20 {
		t.Errorf("walked %d commands, which is fewer than this CLI has: the tree did not build", n)
	}
}

// Flag names are unique per command, which is the invariant the panic enforces.
//
// Asserted here as well as by the walk above, because the panic reports the flag
// and not the command, and finding which of forty commands owns it is the slow
// part.
func TestNoCommandDeclaresAFlagTwice(t *testing.T) {
	var check func(c *cobra.Command, path string)
	check = func(c *cobra.Command, path string) {
		seen := map[string]int{}
		c.Flags().VisitAll(func(f *pflagFlag) { seen[f.Name]++ })
		for name, n := range seen {
			if n > 1 {
				t.Errorf("%s declares --%s %d times", strings.TrimSpace(path), name, n)
			}
		}
		for _, sub := range c.Commands() {
			check(sub, path+" "+sub.Name())
		}
	}
	check(newRootCmd(), "kakt")
}
