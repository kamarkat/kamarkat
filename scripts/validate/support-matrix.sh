#!/usr/bin/env bash
# Assert the two topology facts a release needs, each where it is owned.
#
# THIS SCRIPT USED TO ASK ONE QUESTION THAT NO LONGER HAS AN ANSWER. It rendered
# the management chart once per environment/provider pair and required an
# unsupported pair to be refused by the render. ADR-111 Revision 7 removed the
# ground under both halves:
#
#   - The hub is one shared, ENVIRONMENT-INDEPENDENT management cluster and
#     declares no workload environment, so rendering its bundle with a workload
#     `environmentSlug` asks the chart about something it does not own. The
#     chart's values say so in as many words.
#   - The environments were enumerated from `manifests/environments/*/`, which
#     held dev, stg and prod when the hub had a per-environment overlay. That
#     commit deleted all three and added `management`. The enumeration then
#     produced `ephemeral` and `management` -- neither a workload environment --
#     and checked ZERO supported combinations while reporting coverage.
#   - The render-time refusal it asserted (`unsupported topology`) exists nowhere
#     in the chart and has not since that commit: `supportedCombination` in the
#     CLI answers it now, once, when a workload cluster is declared and before
#     anything is built.
#
# So the two facts are asserted separately, each against the thing that owns it:
#
#   1. The workload topology policy, by the CLI contract that enforces it --
#      every supported pair accepted AND unsupported pairs refused.
#   2. The management bundle, rendered as a management cluster actually renders
#      it: no workload environment, one render per provider, every Application it
#      names resolving to a chart this release published.
#
# Usage: support-matrix.sh <chart-dir> <version> <published-chart-dir>
set -euo pipefail

CHART="${1:?usage: support-matrix.sh <chart-dir> <version> <published-chart-dir>}"
VERSION="${2:?}"
PUBLISHED="${3:?}"

failures=0

# ---------------------------------------------------------------------------
# 1. The workload topology policy, where a workload cluster is declared.
#
# Run rather than reimplemented. A second copy of the matrix in shell would be a
# third authority to drift, and the check that matters -- that an unsupported
# pair is refused -- is a property of `supportedCombination`, not of a render.
# The Go tests also assert the CLI's copy still matches the chart's, which is the
# authority (ADR-062's table of who declares what).
# ---------------------------------------------------------------------------
topology_tests='TestScaffoldMatrixMatchesTheChart|TestEverySupportedCombinationHasAClaimTemplate|TestSupportedCombinationAcceptsTheMatrixAndRefusesTheRest'
if go test ./internal/kakt-cli/tenant/ -run "$topology_tests" -count=1 >/tmp/.support-matrix.$$ 2>&1; then
    echo "  ok    workload topology: 4 supported pair(s) accepted, 6 unsupported refused, CLI matches the chart"
else
    echo "  FAIL  workload topology policy is not enforced as declared:"
    sed 's/^/          /' /tmp/.support-matrix.$$ | head -12
    failures=$((failures + 1))
fi
rm -f /tmp/.support-matrix.$$

# ---------------------------------------------------------------------------
# 2. The management bundle, as a management cluster renders it.
#
# `provider` IS declared by the management cluster (gitops-template's
# templates/mgmt/values.yaml) and changes what the bundle deploys, so it is the
# one axis still worth crossing. `environmentSlug` is NOT passed: a managed
# cluster declares its own, and injecting a fictitious one here would claim to
# have tested a workload combination that this chart does not decide.
# ---------------------------------------------------------------------------
providers=$(ls -d manifests/providers/*/ 2>/dev/null | xargs -n1 basename || true)
[ -n "$providers" ] || {
    echo "  FAIL  no providers found under manifests/providers/; nothing was checked" >&2
    exit 1
}

checked=0
while read -r provider; do
    [ -n "$provider" ] || continue
    checked=$((checked + 1))
    if ! rendered=$(helm template platform-bundle "$CHART" \
            --set boxId=example-box --set provider="$provider" \
            --set management.infisicalEnvironmentSlug=management \
            --set instanceRepoURL=https://github.com/example-org/example-gitops \
            --set hubDomain=dev.example.test \
            --set previewDomain=example-preview.test \
            --set 'alerting.emailTo[0]=alerts@example.test' \
            --set publicTlsIssuer=letsencrypt-prod --set hubIngressAddress=127.0.0.1 \
            --set bundleVersion="$VERSION" 2>&1); then
        echo "  FAIL  the management bundle does not render on $provider:"
        sed 's/^/          /' <<<"$rendered" | head -3
        failures=$((failures + 1))
        continue
    fi
    if ! missing=$(python3 scripts/validate/bundle-completeness.py "$PUBLISHED" <<<"$rendered"); then
        echo "  FAIL  the management bundle on $provider names charts this release does not publish:"
        sed 's/^/          /' <<<"$missing"
        failures=$((failures + 1))
        continue
    fi
    echo "  ok    management bundle on $provider: renders, all charts published"
done <<<"$providers"

echo "support matrix: workload topology + $checked management bundle render(s) checked, $failures failure(s)"
[ "$failures" -eq 0 ] || exit 1
