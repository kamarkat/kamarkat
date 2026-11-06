#!/usr/bin/env bash
# Package the platform-owned components declared inline in the boundary
# templates rather than as component descriptors.
#
# ADR-061 puts a component inline when it needs the environment or provider
# dimensions in scope, which descriptors do not carry. Nothing connected that
# list to the packager, so a released bundle named charts no release published
# and the gap was invisible until a cluster tried to reconcile one.
#
# Where the path is parameterised the content is packaged per combination, the
# same way boundary 02's provider overlays and the spoke catalogue are: the
# overlay is the variation, and reconstructing it from values would let the
# combinations drift apart.
set -euo pipefail

VERSION="${1:?usage: inline-charts.sh <version> [outdir]}"
OUTDIR="${2:-dist/charts}"
mkdir -p "$OUTDIR"

# Wrap rendered content as a chart that emits it verbatim. Content goes to
# files/ rather than templates/ because Helm templates everything under
# templates/, and platform manifests legitimately contain Go template
# delimiters -- an ExternalSecret's {{ .apikey }} rendered to an empty string
# once, blanking a credential silently.
emit_chart() {
    local name="$1" chart="$2"
    cat > "$chart/Chart.yaml" <<CHART
apiVersion: v2
name: $name
description: Platform component $name, packaged for release $VERSION
type: application
version: $VERSION
appVersion: "$VERSION"
CHART
    cat > "$chart/templates/content.yaml" <<'TMPL'
{{- range $path, $_ := .Files.Glob "files/*.yaml" }}
---
{{ $.Files.Get $path }}
{{- end }}
TMPL
    local objects
    objects=$(helm template "$name" "$chart" 2>/dev/null | grep -c '^kind:' || true)
    if [[ "${objects:-0}" -eq 0 ]]; then
        echo "$name: renders no objects; refusing" >&2
        rm -rf "$chart"; exit 1
    fi
    echo "packaged $name -> $chart ($objects object(s))"
}

# Render a source directory, however it is built, into a chart.
package_path() {
    local name="$1" src="$2" chart="$OUTDIR/$1"
    rm -rf "$chart"; mkdir -p "$chart/files" "$chart/templates"
    if [[ -f "$src/kustomization.yaml" ]]; then
        if ! kustomize build --enable-helm "$src" > "$chart/files/rendered.yaml" 2>"$chart/.err"; then
            echo "$name: kustomize build failed:" >&2; sed 's/^/  /' "$chart/.err" >&2
            rm -rf "$chart"; exit 1
        fi
        rm -f "$chart/.err"
        # Helm refuses any chart file over 5 MiB and vendored CRDs exceed it.
        scripts/package/split-rendered.py "$chart/files/rendered.yaml" "$chart/files" >/dev/null
        rm -f "$chart/files/rendered.yaml"
    else
        local found=0
        for f in "$src"/*.yaml; do
            [[ -e "$f" ]] || continue
            [[ "$(basename "$f")" == kustomization.yaml ]] && continue
            case "$(basename "$f")" in templated-fields*.yaml) continue ;; esac
            cp "$f" "$chart/files/"; found=1
        done
        if [[ "$found" -eq 0 ]]; then
            echo "$name: no manifests at $src; refusing" >&2
            rm -rf "$chart"; exit 1
        fi
    fi

    # Objects whose content depends on the box become templates, exactly as
    # component-chart.sh does. This was absent here, so anything packaged by this
    # script kept its literals however carefully the declaration was written --
    # which is how the environment overlay went on shipping the platform's own
    # domain to every tenant while hub-core-services had been fixed.
    local decl="$src/templated-fields.${name}.yaml"
    [[ -f "$decl" ]] || decl="$src/templated-fields.yaml"
    if [[ -f "$decl" ]]; then
        local tf=("$chart"/files/*.yaml)
        if [[ -e "${tf[0]}" ]]; then
            if ! scripts/package/templated-fields.py "$decl" "$chart" "${tf[@]}"; then
                rm -rf "$chart"; exit 1
            fi
            # The templated objects read .Values.global.*, and a chart with no
            # default for it renders nothing at all: `global` is absent, so the
            # lookup is a nil map and helm fails the whole render. emit_chart
            # reads that as a component that produces no objects and refuses it,
            # which names the wrong cause.
            cat > "$chart/values.yaml" <<'VALS'
# Supplied by the environment-manager from the cluster's own values. Empty here
# so the chart renders without one; a box that means to publish on a domain
# supplies it, and one that does not is refused upstream by the hubDomain helper.
global:
  hubDomain: ""
  # The box's own Infisical identity, empty for the same reason: a chart with no
  # default for a key its templates read renders NOTHING -- the lookup is a nil
  # map and helm fails the whole render, which emit_chart reports as a component
  # producing no objects and blames the component.
  #
  # Empty is also the correct value to ship. These identify one box's Infisical
  # organisation and projects; a default here would be whichever box was
  # bootstrapped when it was written, which is exactly how the platform's own ids
  # reached every tenant.
  infisical:
    organizationId: ""
    projectId: ""
    secretsProjectId: ""
VALS
        fi
    fi
    emit_chart "$name" "$chart"
}

# Already Helm charts. Only the version is set, so every artefact in a bundle
# carries the bundle's version and a cluster asks for one version, not a set.
for src in manifests/hub-core-services/security manifests/hub-core-services/gateway manifests/hub-core-services/credit-authority manifests/hub-core-services/billing-engine; do
    name=$(awk '/^name:/ {print $2; exit}' "$src/Chart.yaml")
    chart="$OUTDIR/$name"
    rm -rf "$chart"; cp -R "$src" "$chart"
    python3 - "$chart/Chart.yaml" "$VERSION" <<'PY'
import sys, re, pathlib
p, v = pathlib.Path(sys.argv[1]), sys.argv[2]
t = p.read_text()
t = re.sub(r"(?m)^version:.*$", f"version: {v}", t)
t = re.sub(r"(?m)^appVersion:.*$", f'appVersion: "{v}"', t)
p.write_text(t)
PY
    echo "packaged $name -> $chart (existing chart)"
done

package_path platform-storage manifests/hub-core-services/storage

# ADR-078's collection layer is NOT here any more: ADR-066 addendum 4 makes it
# required machinery, so it is a descriptor again (components/03/grafana-alloy.yaml)
# and component-chart.sh packages it with every other descriptor. A component
# changing declaration route changes packager too, and a component in neither
# publishes a bundle naming a chart no release produced -- the defect
# bundle-completeness.py exists to catch, and the reason this script exists.

# ADR-078 add.5 §6: the Grafana Operator and its vendored CRDs, inline because the
# backend's presence is capabilities.observability.enabled.
package_path platform-dashboards manifests/hub-core-services/platform-dashboards
# ADR-021 amendment 2026-10-29: the Grafana operator and its CRDs stay in
# platform-dashboards; the instance, datasources, folder and dashboards it
# reconciles are a separate Application with its own lifecycle.
package_path grafana manifests/hub-core-services/grafana

# ADR-078 add.5: the cluster's stores, evaluators, Alertmanager and Loki, inline
# and capability-gated likewise.
package_path victoriametrics-operator manifests/hub-core-services/victoriametrics-operator
package_path victoriametrics manifests/hub-core-services/victoriametrics
package_path loki manifests/hub-core-services/loki

# ADR-077's agent, inline because its presence is a function of
# capabilities.support.enabled. Leaving it out here would publish a
# bundle naming a chart no release produced.
package_path support-agent manifests/hub-core-services/support-agent

# ADR-052's burst autoscaler, inline because its presence is a function of
# burstSpokePool. It was left out when it moved from a descriptor to inline, and
# every release named a chart none produced: a box with a burst pool got an
# Application that rendered nothing and reported Healthy.
package_path cluster-autoscaler manifests/hub-core-services/cluster-autoscaler

# Parameterised paths: one chart per combination the boundary can ask for.
# A directory a sibling overlay lists as a resource is a Kustomize base, not an
# environment a boundary can ask for. Detected rather than named: excluding
# "base" by name would hold only until the second base appeared, and the chart
# it published would be one no Application ever resolves.
bases=$(grep -h -oE '\.\./[a-z0-9-]+' manifests/environments/*/kustomization.yaml 2>/dev/null \
        | sed 's|\.\./||' | sort -u)
for d in manifests/environments/*/; do
    env=$(basename "$d")
    [[ -f "$d/kustomization.yaml" ]] || continue
    if grep -qx "$env" <<<"$bases"; then
        echo "skip hub-environment-${env}: a base other overlays build on, not an environment"
        continue
    fi
    # Unsuffixed, deliberately (ADR-111 Revision 7). There is one hub overlay --
    # the management plane's -- because the hub has no workload environment to
    # vary by. A suffix would make distribution.py read "management" as a VARIANT
    # and guard the component on `global.environmentSlug == "management"`, a value
    # the hub no longer declares: the guard would never match, the component would
    # emit nothing, and ArgoCD would prune the HubEnvironment that every
    # environment-scoped value derives from.
    package_path "hub-environment" "$d"
done

for d in manifests/providers/*/; do
    prov=$(basename "$d")
    [[ -f "$d/kustomization.yaml" ]] || continue
    package_path "infrastructure-provider-${prov}" "$d"
done

# manifests/spoke/spoke-pools/<env>/<provider> is NOT packaged, because it no
# longer exists: ADR-082 (02f9b01e) made a tenant declare its own workload
# clusters, so the claim is hydrated into registry/clusters/<name>/infrastructure/
# from the repository's own template and carries the tenant's chosen name.
#
# The loop that packaged it survived that commit and matched nothing, which is
# how the environment ever reached packaging in the first place: a chart named
# platform-spoke-pools-<env>-<provider> per combination, published once per
# environment the platform recognised. Nothing asks for those charts -- boundary
# 03 dropped platform-spoke-pools as a component -- so they are not produced and
# the environment has no remaining role in what a release contains.
