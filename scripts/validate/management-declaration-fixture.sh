#!/usr/bin/env bash
# Print the management declaration a box is actually created with, as values.
#
# ONE FIXTURE, because there were three. Every gate that rendered the platform
# chart carried its own list of `--set` flags, and one of those flags was
# `alerting.emailTo[0]=alerts@example.test` -- a value the tenant declaration
# template did not have. The chart requires it while observability is on, so
# every gate passed against a shape no box ever has, and the first real
# declaration ArgoCD rendered failed: no ApplicationSets at all, and boundary 01
# waiting out its whole budget on "no ApplicationSet targets project
# boundary-01", which names neither the chart nor the key.
#
# Rendering the template itself is the fix, and rendering it from ONE place is
# what keeps the fix: a required value missing from the file `kakt box init`
# writes now fails every gate at once, and a new token added to that template
# fails here rather than reaching a chart as the literal string "<SOMETHING>".
#
# The values are facts a box would have, not a shape a box would not. Usage:
#
#     helm template <release> <chart> -f <(scripts/validate/management-declaration-fixture.sh)
#
# A caller that needs to vary one of them appends its own --set, which is what
# per-render values like bundleVersion are for.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
DECL="${ROOT}/manifests/tenants/gitops-template/templates/mgmt/values.yaml"

if [[ ! -f "$DECL" ]]; then
    echo "management-declaration-fixture: no declaration template at $DECL" >&2
    exit 1
fi

rendered=$(sed \
    -e 's|<GITOPS_REPO_URL>|https://github.com/example-org/example-gitops|g' \
    -e 's|<PLATFORM_REPO_URL>|https://github.com/kamarkat/kamarkat|g' \
    -e 's|<PLATFORM_ADMIN_EMAIL>|alerts@example.test|g' \
    -e 's|<PLATFORM_ADMIN_IDENTITY>|00000000-0000-0000-0000-000000000000|g' \
    -e 's|<ESCROW_URL>|https://app.infisical.com|g' \
    -e 's|<ESCROW_PROJECT_ID>|example-project|g' \
    -e 's|<ESCROW_ORG_ID>|example-org|g' \
    -e 's|<BOX_ID>|example-1700000000|g' \
    -e 's|<CLUSTER_NAME>|example-mgmt-1700000000|g' \
    -e 's|<TENANT_ID>|example|g' \
    -e 's|<DOMAIN>|example.test|g' \
    -e 's|<SUBDOMAIN>|dev|g' \
    -e 's|<CLOUD_PROVIDER>|hetzner|g' \
    -e 's|<CLOUD_REGION>|hel1|g' \
    -e 's|<WORKER_COUNT>|2|g' \
    -e 's|<BUNDLE_VERSION>|0.0.0-test|g' \
    -e 's|<PUBLIC_TLS_ISSUER>|letsencrypt-prod|g' \
    -e 's|<PREVIEW_DOMAIN>|example-preview.test|g' \
    "$DECL")

# A token this fixture has no value for would reach the chart as "<SOMETHING>",
# which is truthy -- so a `required` would pass and the value would be wrong on
# every box. Refused here instead.
left=$(grep -oE "<[A-Z_]+>" <<< "$rendered" | sort -u | tr '\n' ' ' || true)
if [[ -n "${left// /}" ]]; then
    echo "management-declaration-fixture: the declaration carries tokens this fixture gives no value to: ${left}" >&2
    echo "management-declaration-fixture:   add them here, beside the rest, so every render gate sees the same box." >&2
    exit 1
fi

printf '%s\n' "$rendered"
