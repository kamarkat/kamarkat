package main

import (
	"strings"
	"testing"
)

// ADR-068 permits pointing a build at another registry or repository and
// requires that doing so name the version to request. The clause guarded
// nothing until this: --bundle-registry and --platform-repo both existed, and
// --bundle-version defaulted to whatever the binary carried, so a scaffold
// against a fork silently pinned a version that fork had never published.
//
// Exercised through the command's own flags rather than a helper's arguments:
// what is being asserted is that the flag WAS NOT PASSED, and a test that
// constructed the state directly would pass while the wiring that detects it
// was missing.
func TestSourceOverrideRequiresAVersion(t *testing.T) {
	for _, override := range []string{"bundle-registry", "platform-repo"} {
		t.Run(override, func(t *testing.T) {
			cmd := newTenantScaffoldCmd()
			if err := cmd.Flags().Set(override, "example.invalid/charts"); err != nil {
				t.Fatalf("set --%s: %v", override, err)
			}
			err := refuseUndeclaredSourceOverride(cmd)
			if err == nil {
				t.Fatalf("--%s was accepted without --bundle-version; the "+
					"scaffolded repository would pin a version that source is "+
					"not known to hold", override)
			}
			if !strings.Contains(err.Error(), "--bundle-version") {
				t.Errorf("the refusal must name the flag that resolves it, got: %v", err)
			}
		})
	}
}

func TestSourceOverrideWithAVersionIsAccepted(t *testing.T) {
	cmd := newTenantScaffoldCmd()
	for flag, value := range map[string]string{
		"bundle-registry": "example.invalid/charts",
		"bundle-version":  "0.1.16",
	} {
		if err := cmd.Flags().Set(flag, value); err != nil {
			t.Fatalf("set --%s: %v", flag, err)
		}
	}
	if err := refuseUndeclaredSourceOverride(cmd); err != nil {
		t.Errorf("an override that names its version is exactly what ADR-068 "+
			"permits, and a platform developer testing against a fork needs it: %v", err)
	}
}

// The default path must stay clear of the refusal entirely. A released binary
// scaffolding against the registry it published to passes no flags at all, and
// a check that fired there would break every ordinary run.
func TestNoOverrideNeedsNoVersion(t *testing.T) {
	if err := refuseUndeclaredSourceOverride(newTenantScaffoldCmd()); err != nil {
		t.Errorf("scaffolding against the default source required a version: %v", err)
	}
}
