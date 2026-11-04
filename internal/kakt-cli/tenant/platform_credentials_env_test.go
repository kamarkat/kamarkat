package tenant

import "testing"

// Scaffold filled Secrets from flags only, and there are no flags for object
// storage or the registry -- so it prompted for values that were
// already in the environment, which Day-0 then read without asking. The operator
// answered questions whose answers the next phase already had.
func TestScaffoldResolvesCredentialsFromTheEnvironment(t *testing.T) {
	t.Setenv("S3_ACCESS_KEY_ID", "from-env")
	t.Setenv("GHCR_TOKEN", "from-env")

	var s Secrets
	s.fillFromEnvironment()

	for _, c := range platformCredentials() {
		switch c.Secret {
		case "S3_ACCESS_KEY_ID", "GHCR_TOKEN":
			if got := *c.Field(&s); got != "from-env" {
				t.Errorf("%s = %q, want it resolved from the environment", c.Secret, got)
			}
		}
	}
}

// A flag is an explicit statement by whoever ran the command; an environment
// variable is ambient. The explicit one wins, or running with a flag would
// silently do something else on a machine that happened to export the name.
func TestAnExplicitValueBeatsTheEnvironment(t *testing.T) {
	t.Setenv("GHCR_TOKEN", "ambient")

	var s Secrets
	for _, c := range platformCredentials() {
		if c.Secret == "GHCR_TOKEN" {
			*c.Field(&s) = "passed-as-a-flag"
		}
	}
	s.fillFromEnvironment()

	for _, c := range platformCredentials() {
		if c.Secret == "GHCR_TOKEN" && *c.Field(&s) != "passed-as-a-flag" {
			t.Errorf("the environment overwrote an explicitly supplied value")
		}
	}
}

// An exported-but-empty variable is an unset one, not a value that quietly
// satisfies a required credential and produces a box authenticating as nobody.
func TestABlankVariableIsNotAValue(t *testing.T) {
	t.Setenv("GHCR_TOKEN", "   ")

	var s Secrets
	s.fillFromEnvironment()

	for _, c := range platformCredentials() {
		if c.Secret == "GHCR_TOKEN" && *c.Field(&s) != "" {
			t.Errorf("a blank variable was taken as a value: %q", *c.Field(&s))
		}
	}
}
