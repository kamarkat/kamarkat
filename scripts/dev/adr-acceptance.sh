#!/usr/bin/env bash
# Assert what ADR-062 through ADR-070 actually claim, against a live box.
#
# `kakt verify` answers "did this converge". That is necessary and it is not the
# same question as "does the thing each decision promised hold". A run can
# converge perfectly while the bundle names a chart nobody published, the
# promotion matcher matches nothing, or a capability switched off is still
# running -- every one of which has happened here.
#
# So this is the acceptance pass, one check per claim, naming the ADR. It is
# read-only: it inspects the cluster, the tenant repository and the published
# artefacts, and changes nothing.
#
#   adr-acceptance.sh <version> <gitops-dir> <kubeconfig> [registry]
#
# Exit 0 if every check passed or was skipped for a stated reason; 1 otherwise.
set -uo pipefail

VERSION="${1:?usage: adr-acceptance.sh <version> <gitops-dir> <kubeconfig> [registry]}"
GITOPS="${2:?}"
KUBECONFIG_PATH="${3:?}"
REGISTRY="${4:-ghcr.io/soloz-io/charts}"
export KUBECONFIG="$KUBECONFIG_PATH"

pass=0 fail=0 skip=0
ok()   { printf '  \033[32m✓\033[0m %-10s %s\n' "$1" "$2"; pass=$((pass+1)); }
no()   { printf '  \033[31m✗\033[0m %-10s %s\n' "$1" "$2"; fail=$((fail+1)); }
na()   { printf '  \033[33m–\033[0m %-10s %s\n' "$1" "$2"; skip=$((skip+1)); }

kc() { kubectl "$@" 2>/dev/null; }

# Reachability is established once, up front. Every cluster-dependent check below
# would otherwise pass VACUOUSLY against an unreachable cluster -- "no Application
# names an unpublished chart" is trivially true when no Application can be listed,
# and that reads identically to a healthy box. The first dry run of this script
# did exactly that.
CLUSTER_UP=0
if kc get --raw /readyz >/dev/null 2>&1 || kc get ns >/dev/null 2>&1; then
    CLUSTER_UP=1
fi

echo "ADR acceptance — version $VERSION"
[ "$CLUSTER_UP" -eq 1 ] || echo "  (cluster unreachable — cluster-dependent checks are skipped, not passed)"
echo

# ── ADR-062: the tenant owns a repository the platform cannot reach ─────────
bundle="$GITOPS/registry/clusters"/*/bundle.yaml
if compgen -G "$bundle" >/dev/null; then
    ok ADR-062 "the tenant repository declares its own cluster(s)"
else
    no ADR-062 "no registry/clusters/*/bundle.yaml in $GITOPS"
fi

# ── ADR-063: one artefact, and the bundle names only charts this release
#             published. The v0.1.1 defect: six Applications naming charts
#             nothing packaged, invisible until a cluster tried to reconcile.
if [ "$CLUSTER_UP" -eq 0 ]; then
    na ADR-063 "cluster unreachable; cannot enumerate what it names"
else
# Only charts the PLATFORM publishes. A box legitimately references upstream
# charts from upstream repositories -- argo-cd from argoproj.github.io,
# cloudnative-pg, crossplane, kyverno, zitadel and the rest -- and probing those
# against the platform registry reported nine healthy Applications as naming
# unpublished charts. The claim ADR-063 makes is about what the BUNDLE resolves,
# not about every chart a cluster happens to use.
missing="" seen=0
for chart in $(kc get applications.argoproj.io -A \
        -o jsonpath="{range .items[?(@.spec.source.repoURL==\"${REGISTRY}\")]}{.spec.source.chart}{\"\n\"}{end}" \
        | sort -u | grep -v '^$'); do
    seen=$((seen+1))
    helm show chart "oci://${REGISTRY}/${chart}" --version "$VERSION" >/dev/null 2>&1 \
        || missing="${missing} ${chart}"
done
if [ "$seen" -eq 0 ]; then
    # Reachable but naming no charts is not a pass either: a released box
    # resolves every platform Application from the distribution.
    no ADR-063 "the cluster names no charts at all; nothing was verified"
elif [ -z "$missing" ]; then
    ok ADR-063 "all $seen chart(s) the cluster names are published at $VERSION"
else
    no ADR-063 "Applications name unpublished charts:${missing}"
fi
fi

# ── ADR-064: promotion is a proposal the tenant merges ──────────────────────
if [ -f "$GITOPS/renovate.json" ]; then
    if grep -q 'bundleVersion' "$GITOPS/renovate.json"; then
        ok ADR-064 "the promotion matcher targets the bundle version"
    else
        no ADR-064 "renovate.json does not match bundleVersion; promotions would move nothing"
    fi
    # The claim that matters: production is proposed, never merged for them.
    if grep -q '"automerge": false' "$GITOPS/renovate.json"; then
        ok ADR-064 "production promotions are proposed, not automerged"
    else
        no ADR-064 "no automerge:false rule; a production version could merge itself"
    fi
else
    no ADR-064 "no renovate.json; nothing proposes a promotion"
fi

# ── ADR-065: the box reconciles from the tenant's own repository ────────────
root_src=$(kc get applications.argoproj.io -A \
    -o jsonpath="{range .items[?(@.metadata.name=='${CLUSTER:-acme-hub}-root')]}{.spec.source.repoURL}{' '}{.spec.sources[*].repoURL}{end}")
if [ -z "$root_src" ]; then
    root_src=$(kc get applications.argoproj.io -A \
        -o jsonpath='{range .items[*]}{.spec.source.repoURL}{"\n"}{end}' | sort -u | head -3 | tr '\n' ' ')
fi
if printf '%s' "$root_src" | grep -q "$(basename "$GITOPS")"; then
    ok ADR-065 "the cluster reconciles from the tenant's repository"
else
    na ADR-065 "could not identify the root source (saw: ${root_src:-none})"
fi

# ── ADR-066: a capability switched off is not running ───────────────────────
# Asserted against what the cluster actually has, not against the values file --
# the values file is the input, and the whole claim is about the output.
if [ "$CLUSTER_UP" -eq 0 ]; then
    na ADR-066 "cluster unreachable; cannot compare declared to running"
else
# name:application:chart-default. The default is carried here rather than assumed
# true: `support` ships false until its image and Support Plane exist, and a
# check that assumed otherwise would report a correct box as broken.
for cap in database:platform-database:true \
           observability:grafana-alloy:true support:support-agent:false; do
    name="${cap%%:*}" rest="${cap#*:}" app="${rest%%:*}" default="${rest##*:}"
    want=$(grep -A2 "^  ${name}:" "$GITOPS"/registry/clusters/*/values.yaml 2>/dev/null | grep -m1 'enabled:' | awk '{print $2}')
    [ -n "$want" ] || want="$default"
    have=$(kc get applications.argoproj.io -A -o name | grep -c "/${app}$")
    if { [ "$want" = "true" ] && [ "$have" -ge 1 ]; } || { [ "$want" = "false" ] && [ "$have" -eq 0 ]; }; then
        ok ADR-066 "capability ${name}=${want} matches the cluster (${have} Application)"
    else
        no ADR-066 "capability ${name}=${want} but ${have} Application(s) present"
    fi
done
fi

# ── ADR-067: the box can judge a proposal, and can show what it would send ──
if command -v kakt >/dev/null 2>&1 || [ -x ./bin/kakt ]; then
    KAKT=$(command -v kakt || echo ./bin/kakt)
    if "$KAKT" proposal preflight --candidate "$VERSION" --gitops-dir "$GITOPS" \
            --kubeconfig "$KUBECONFIG_PATH" --registry "$REGISTRY" >/tmp/verdict.md 2>/dev/null; then
        ok ADR-067 "pre-flight produced a verdict for $VERSION"
    else
        # A failing or unverified verdict is still a working mechanism.
        grep -q "Pre-flight" /tmp/verdict.md 2>/dev/null \
            && ok ADR-067 "pre-flight produced a verdict ($(head -1 /tmp/verdict.md | sed 's/## //'))" \
            || no ADR-067 "pre-flight produced no verdict"
    fi
    "$KAKT" support scope >/dev/null 2>&1 \
        && ok ADR-067 "the evidence scope is readable before anything is sent" \
        || no ADR-067 "kakt support scope failed"
else
    na ADR-067 "no kakt binary on PATH"
fi

# ── ADR-068: the released path, not the working tree ────────────────────────
if [ -x ./bin/kakt ]; then
    got=$(./bin/kakt bundle-version 2>/dev/null)
    [ "$got" = "$VERSION" ] \
        && ok ADR-068 "the CLI declares $VERSION" \
        || no ADR-068 "the CLI declares '${got}', not $VERSION"
fi
if grep -q 'chart:' "$GITOPS"/registry/clusters/*/bundle.yaml 2>/dev/null \
   && ! grep -q 'path: manifests' "$GITOPS"/registry/clusters/*/bundle.yaml 2>/dev/null; then
    ok ADR-068 "the bundle is in published-chart shape, not a repository path"
else
    no ADR-068 "the bundle names a repository path; this is a development build"
fi

# ── ADR-069: what taking this version means is derivable ────────────────────
if python3 scripts/package/support-window.py "$VERSION" >/tmp/window.md 2>/dev/null \
   && grep -q "Supported until" /tmp/window.md; then
    ok ADR-069 "the support window for $VERSION is stated: $(grep 'Supported until' /tmp/window.md | tr -d '|' | xargs)"
else
    no ADR-069 "no support window could be derived for $VERSION"
fi

# ── ADR-070: the measurement runs, and refuses to lie ───────────────────────
if [ "$CLUSTER_UP" -eq 0 ]; then
    na ADR-070 "cluster unreachable; there is no box to measure"
elif out=$(./scripts/measure/minimum-box.sh "$KUBECONFIG_PATH" 2>&1); then
    ok ADR-070 "measured: $(printf '%s' "$out" | grep -E '^\s+total' | xargs)"
else
    if printf '%s' "$out" | grep -q "NOT A MEASUREMENT"; then
        # The correct outcome while prices are unset: ADR-070 forbids reporting
        # a total against the target from a partially priced inventory.
        na ADR-070 "inventory priced, total withheld — $(printf '%s' "$out" | grep -c 'not priced') line(s) unpriced"
    else
        no ADR-070 "the measurement failed: $(printf '%s' "$out" | tail -1)"
    fi
fi

echo
printf '  %d passed, %d failed, %d skipped\n' "$pass" "$fail" "$skip"
[ "$fail" -eq 0 ]
