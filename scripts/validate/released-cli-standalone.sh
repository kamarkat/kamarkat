#!/usr/bin/env bash
# A released CLI must bootstrap from its binary alone.
#
# ADR-063 requires a release to carry a build a tenant can run. Bootstrap read
# platform manifests from the working directory, so the published binary worked
# only inside a checkout of the platform repository -- a hidden dependency on
# the vendor's repository, in the one artefact a tenant runs before it has
# anything else.
#
# The check runs the released binary from a directory that is not a checkout. A
# development build is expected to fail this and is not run: it reads the working
# tree by design (ADR-068), and a build that passed here in development mode
# would be one that had stopped reading the tree at all.
set -euo pipefail

VERSION="${1:?usage: released-cli-standalone.sh <version> [binary]}"
BIN="${2:-dist/bin/kakt-$(go env GOOS)-$(go env GOARCH)}"

if [[ ! -x "$BIN" ]]; then
    echo "released-cli-standalone: $BIN is not executable" >&2
    exit 1
fi
BIN="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")"

reported=$("$BIN" bundle-version 2>/dev/null || true)
if [[ "$reported" != "$VERSION" ]]; then
    echo "released-cli-standalone: binary reports '${reported}', expected '${VERSION}'" >&2
    exit 1
fi

# Somewhere with no manifests/ and no .git. If the binary reaches for either, it
# finds nothing, which is the tenant's situation exactly.
sandbox="$(mktemp -d)"
trap 'rm -rf "$sandbox"' EXIT

# `box init`, not `tenant scaffold`: scaffolding moved when box management landed,
# and this check went on naming the old command until a release found out. Cobra
# reported it as `unknown flag: --dry-run`, because `tenant` has no `scaffold`
# subcommand and read the word as a positional argument instead -- an error that
# names the flag and not the command that is missing.
#
# --dry-run renders and writes nothing, so this asks the one question that
# matters -- can the binary resolve the platform content Day-0 needs -- without
# provisioning anything or needing a cloud credential.
#
# AN ISOLATED HOME, so this operator's credential store cannot answer for the
# binary. It did: this check passed on a workstation whose ~/.kamarkat held every
# credential, and failed in CI, where the staging step found them missing,
# reported what was outstanding and returned success without rendering. A gate
# that reads the machine it runs on is testing the machine.
#
# The escrow values are synthetic and are NOT secrets: the three coordinates are
# names that get rendered into the declaration, and a dry run neither
# authenticates nor reaches them. --platform-admin likewise, which the binary
# otherwise reads from this operator's session and CI has none of.
home="$sandbox/home"
mkdir -p "$home"
if ! out=$(cd "$sandbox" && \
        HOME="$home" XDG_CONFIG_HOME="$home/.config" \
        INFISICAL_ESCROW_URL=https://escrow.probe.invalid \
        INFISICAL_ESCROW_PROJECT_ID=probe-project \
        INFISICAL_ESCROW_ORG_ID=probe-org \
        "$BIN" box init --dry-run \
        --tenant probe --box probe-org --domain probe.example \
        --provider hetzner --platform-admin probe-admin \
        --bundle-version "$VERSION" --out "$sandbox/render" 2>&1); then
    echo "released-cli-standalone: scaffolding failed outside a checkout, so the" >&2
    echo "release does not carry the content it needs (ADR-063):" >&2
    sed 's/^/  /' <<<"$out" >&2
    exit 1
fi

# The box's own declaration. A box is created with no clusters -- the first one is
# `kakt tenant add-management`, which mints identities and cannot run offline -- so
# there is no cluster to assert here, and this check asserted one for as long as it
# had been failing. What ADR-063 asks is whether the binary carries the platform
# content, and a rendered box answers it; the per-cluster render is gated by the
# bundle render checks.
if [[ ! -f "$sandbox/render/registry/box.yaml" ]]; then
    echo "released-cli-standalone: scaffolding reported success but rendered no" >&2
    echo "box; a silent empty render is the failure this check exists for" >&2
    exit 1
fi

# RESOLVED, not merely present. An empty or half-substituted declaration is a
# file on disk, so existence is not the question -- the question is whether the
# binary had the content to fill it. Each value below is one the renderer
# substitutes, so a template that reached disk unsubstituted fails here.
box="$sandbox/render/registry/box.yaml"
for pair in "probe:the tenant" "probe.example:the domain" "$VERSION:the bundle version" \
            "probe-org:the escrow organization" "probe-project:the escrow project" \
            "escrow.probe.invalid:the escrow address" "probe-admin:the administrator"; do
    want="${pair%%:*}" what="${pair#*:}"
    grep -q "$want" "$box" || {
        echo "released-cli-standalone: the rendered box does not carry ${what}," >&2
        echo "so the binary rendered a declaration it could not fill:" >&2
        sed 's/^/  /' "$box" >&2
        exit 1
    }
done

# UNDER registry/ ONLY. templates/ keeps its placeholders on purpose: those files
# are the templates a cluster is rendered FROM, filled in per cluster when one is
# declared. Scanning the whole tree is the obvious mistake and would refuse every
# release.
# Emptiness, not exit status: the status of `grep | sort` is sort's, which is 0
# whether grep matched anything or not, so testing it made this refuse every
# render -- the mirror of the gate that could never fail.
leftover=$(grep -ro "<[A-Z_]\{3,\}>" "$sandbox/render/registry" 2>/dev/null | sort -u || true)
if [[ -n "$leftover" ]]; then
    echo "released-cli-standalone: the rendered box still carries tokens:" >&2
    sed 's/^/  /' <<<"$leftover" >&2
    exit 1
fi

# templates/ MUST still carry placeholders. They are the files a cluster is
# rendered from, filled in when one is declared, so an empty set here would mean
# the binary shipped templates already substituted -- every cluster would then
# inherit this probe's values. Asserted rather than merely excluded above, so the
# exclusion cannot quietly become the reason this check passes.
if ! grep -rq "<[A-Z_]\{3,\}>" "$sandbox/render/templates" 2>/dev/null; then
    echo "released-cli-standalone: templates/ carries no placeholders, so the" >&2
    echo "binary ships cluster templates that are already substituted" >&2
    exit 1
fi

echo "released CLI: bootstraps standalone at ${VERSION}, no checkout required"
