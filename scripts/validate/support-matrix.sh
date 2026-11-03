#!/usr/bin/env bash
# Assert the whole support matrix, not only the half that works.
#
# Every environment crossed with every provider is evaluated. A combination the
# chart declares supported must render and must publish every chart it names. A
# combination it does not declare must be refused, by name, with a reason.
#
# Asserting both halves is what stops the matrix drifting. Checking only that
# supported combinations render would let someone add prod+hybrid to
# supportedMatrix, see the release stay green because nothing tests the
# combinations that are meant to fail, and believe it supported -- while the
# sources behind it do not exist. Checking only that unsupported ones fail would
# let a supported combination quietly stop rendering.
#
# Usage: support-matrix.sh <chart-dir> <version> <published-chart-dir>
set -euo pipefail

CHART="${1:?usage: support-matrix.sh <chart-dir> <version> <published-chart-dir>}"
VERSION="${2:?}"
PUBLISHED="${3:?}"

# Environments and providers are enumerated from the tree rather than listed
# here, so a new one is evaluated the moment it exists: a directory nobody
# added to supportedMatrix must be refused, and this cannot notice that if the
# set it iterates is a copy of the matrix it is checking.
environments=$(ls -d manifests/environments/*/ 2>/dev/null | xargs -n1 basename | grep -v '^base$' || true)
providers=$(ls -d manifests/providers/*/ 2>/dev/null | xargs -n1 basename || true)
supported=$(python3 -c "
import yaml
m = yaml.safe_load(open('manifests/argocd/environment-manager/values.yaml')).get('supportedMatrix') or {}
print('\n'.join(f'{e}+{p}' for e, ps in m.items() for p in ps))
")

[ -n "$environments" ] && [ -n "$providers" ] || {
    echo "support matrix: no environments or providers found; nothing was checked" >&2
    exit 1
}

failures=0
checked=0
while read -r env; do
    [ -n "$env" ] || continue
    while read -r provider; do
        [ -n "$provider" ] || continue
        checked=$((checked + 1))
        rendered=$(helm template platform-bundle "$CHART" \
            --set environmentSlug="$env" --set boxId=example-box --set provider="$provider" \
            --set instanceRepoURL=https://github.com/example-org/example-gitops \
            --set hubDomain=dev.example.test \
            --set previewDomain=example-preview.test \
            --set 'alerting.emailTo[0]=alerts@example.test' \
            --set publicTlsIssuer=letsencrypt-prod --set hubIngressAddress=127.0.0.1 \
            --set bundleVersion="$VERSION" 2>&1) && ok=0 || ok=1

        if grep -qx "${env}+${provider}" <<<"$supported"; then
            if [ "$ok" -ne 0 ]; then
                echo "  FAIL  ${env}+${provider} is declared supported but does not render:"
                sed 's/^/          /' <<<"$rendered" | head -3
                failures=$((failures + 1))
                continue
            fi
            if ! missing=$(python3 scripts/validate/bundle-completeness.py "$PUBLISHED" <<<"$rendered"); then
                echo "  FAIL  ${env}+${provider} renders but names charts this release does not publish:"
                sed 's/^/          /' <<<"$missing"
                failures=$((failures + 1))
                continue
            fi
            echo "  ok    ${env}+${provider} supported: renders, all charts published"
        else
            if [ "$ok" -eq 0 ]; then
                echo "  FAIL  ${env}+${provider} is not in the supported matrix but rendered anyway;"
                echo "        a combination with no sources behind it would report healthy while managing nothing"
                failures=$((failures + 1))
                continue
            fi
            if ! grep -q "unsupported topology" <<<"$rendered"; then
                echo "  FAIL  ${env}+${provider} is refused, but not by the support matrix:"
                sed 's/^/          /' <<<"$rendered" | head -3
                failures=$((failures + 1))
                continue
            fi
            echo "  ok    ${env}+${provider} unsupported: refused by name"
        fi
    done <<<"$providers"
done <<<"$environments"

echo "support matrix: ${checked} combination(s) checked, ${failures} failure(s)"
[ "$failures" -eq 0 ]
