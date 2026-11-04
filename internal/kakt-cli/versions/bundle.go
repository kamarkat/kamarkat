package versions

// DevelopmentBundle is what a build carries when no version was injected.
//
// It is not an error and not a fallback: an ordinary `go build` is an
// unreleased build, and ADR-068 gives that a defined meaning — platform content
// is resolved from the working tree rather than from a published bundle. That is
// what allows a change to be exercised without first tagging it, and therefore
// what keeps a tag meaning "this set was tested".
const DevelopmentBundle = "development"

// BundleVersion is the published bundle this build requests.
//
// Injected by the release pipeline:
//
//	-ldflags "-X github.com/kamarkat/kamarkat/internal/kakt-cli/versions.BundleVersion=0.2.0"
//
// It is a variable rather than a constant only because the linker can write it.
// Nothing else may: ADR-068 forbids a second derivation, because the version a
// cluster requests and the version the pipeline published are the same string or
// the cluster asks for a chart that does not exist. ADR-063 records two version
// pairs that already drifted by each side computing its own.
var BundleVersion = DevelopmentBundle

// IsReleaseBuild reports whether this build carries a published version.
func IsReleaseBuild() bool { return BundleVersion != DevelopmentBundle }
