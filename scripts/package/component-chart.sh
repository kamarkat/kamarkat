#!/usr/bin/env bash
# Package one platform-owned component descriptor as a Helm chart.
#
# ADR-063 publishes the bundle as versioned charts; ADR-062 keeps instance data
# out of the types repository. This script is the seam between them, and its
# most important behaviour is a refusal: a component whose source reaches into a
# generated/ directory is carrying per-cluster instance data (ADR-045), and
# publishing it would bake one tenant's Infisical project, DNS target or
# bootstrap config into an artefact every tenant pulls. Those must become chart
# values before they can be packaged, so this exits non-zero rather than
# producing a chart that is wrong in a way nobody would notice until a second
# tenant existed.
#
# The unit is a DESCRIPTOR, not a path. Two descriptors share
# manifests/argocd-principal and are separated only by directoryInclude at
# different sync waves, so packaging by path would merge two components that the
# platform deliberately sequences apart.
set -euo pipefail

DESCRIPTOR="${1:?usage: component-chart.sh <descriptor.yaml> <version> [outdir]}"
VERSION="${2:?missing chart version}"
OUTDIR="${3:-dist/charts}"

field() { grep -m1 "^$1:" "$DESCRIPTOR" | sed "s/^$1: *//; s/^['\"]//; s/['\"]$//"; }

APP=$(field appName)
SRC=$(field path)
OWNED=$(field isPlatformOwned)
INCLUDE=$(field directoryInclude)
RECURSE=$(field directoryRecurse)

[[ -n "$APP" ]] || { echo "no appName in $DESCRIPTOR" >&2; exit 2; }

if [[ "$OWNED" != "true" ]]; then
    echo "skip $APP: third-party chart, already published upstream"
    exit 0
fi
[[ -n "$SRC" ]] || { echo "skip $APP: platform-owned with no path" >&2; exit 0; }
[[ -d "$SRC" ]] || { echo "$APP: source path does not exist: $SRC" >&2; exit 1; }

# The refusal described above.
if [[ -d "$SRC/generated" ]] || grep -rqs "generated/" "$SRC"/kustomization.yaml 2>/dev/null; then
    cat >&2 <<MSG
REFUSED $APP
  $SRC consumes Day-0 generated artifacts, which are per-cluster instance data.
  Publishing it would place one cluster's values in a chart every cluster pulls.
  Convert those inputs to chart values (ADR-063) before packaging this component.
MSG
    exit 3
fi

CHART="$OUTDIR/$APP"
rm -rf "$CHART"; mkdir -p "$CHART/templates" "$CHART/files"

cat > "$CHART/Chart.yaml" <<MSG
apiVersion: v2
name: $APP
description: Platform component $APP, packaged from $SRC
type: application
version: $VERSION
appVersion: "$VERSION"
MSG

# Kustomize builds are rendered at package time so the published chart carries
# the same objects the cluster applies today. Kustomize is a build step here,
# not a deploy-time one, which is why rendering it away changes nothing that
# was environment-dependent -- ADR-061 already keeps environment-parameterised
# components out of descriptors and in the boundary template.
if [[ -f "$SRC/kustomization.yaml" ]]; then
    command -v kustomize >/dev/null || { echo "$APP: kustomize not found" >&2; exit 1; }
    # Helm 3, checked here rather than discovered inside kustomize.
    #
    # kustomize's HelmChartInflationGenerator shells out to `helm version -c`, a flag
    # Helm 4 removed, so on a Helm 4 toolchain every component that inflates a chart
    # from its kustomization (headlamp, infisical) fails with
    #
    #   Error: unknown shorthand flag: 'c' in -c
    #   : unable to run: 'helm version -c --short' ... (is 'helm' installed?)
    #
    # which names a flag nobody wrote and asks whether a binary that is present is
    # installed. CI pins v3.16.4 for this reason (publish-platform-charts.yml); the
    # local release path has no such pin, so the version is asserted here and the
    # failure says what to do about it.
    #
    # ONLY for a component that actually inflates a chart. The check used to fire for
    # every kustomization on a Helm 4 toolchain, which is broader than its own reason:
    # a kustomization with no `helmCharts` never shells out to helm at all, so it was
    # refused for a dependency it does not have. That took out the per-application
    # database components (ADR-112), whose kustomizations exist only so a recovery
    # overlay can reference the directory.
    inflates_chart=""
    if grep -rqls '^helmCharts:' "$SRC" 2>/dev/null; then
        inflates_chart="yes"
    fi
    if [[ -n "$inflates_chart" ]] && command -v helm >/dev/null; then
        helm_major="$(helm version --short 2>/dev/null | sed -n 's/^v\([0-9]*\).*/\1/p')"
        if [[ -n "$helm_major" && "$helm_major" -ge 4 ]]; then
            echo "$APP: helm $(helm version --short) is installed, and kustomize inflates charts" >&2
            echo "  by calling 'helm version -c', which Helm 4 removed. Packaging this component" >&2
            echo "  needs Helm 3 (CI pins v3.16.4). Either install Helm 3, or point kustomize at" >&2
            echo "  one for this run:" >&2
            echo "    kustomize build --enable-helm --helm-command /path/to/helm3 $SRC" >&2
            exit 1
        fi
    fi
    # --enable-helm because several components inflate a chart from their
    # kustomization (headlamp, infisical). Without it kustomize refuses the
    # build with "trouble configuring builtin HelmChartInflationGenerator",
    # which reads as a malformed config rather than a missing flag.
    # Built to a temporary file and checked before it becomes the chart's
    # content. `kustomize build ... > file` creates the file whether or not the
    # build succeeds, and a chart whose only file is zero bytes packages and
    # pushes without complaint -- it just applies nothing. That is how a failing
    # HelmChartInflationGenerator produced a published chart containing no
    # objects at all.
    if ! kustomize build --enable-helm "$SRC" > "$CHART/files/rendered.yaml" 2>"$CHART/.err"; then
        echo "$APP: kustomize build failed:" >&2; sed 's/^/  /' "$CHART/.err" >&2
        rm -rf "$CHART"; exit 1
    fi
    rm -f "$CHART/.err"
else
    # directoryInclude is a glob the boundary template passes to ArgoCD; honour
    # it here so a chart contains exactly the objects its Application did.
    pattern="${INCLUDE//\"/}"; pattern="${pattern:-*.yaml}"
    # An empty array expanded under `set -u` is an error on bash 3.2, which is
    # what macOS ships and what killed every recursive component here first run.
    depth=(); [[ "$RECURSE" == "true" ]] || depth=(-maxdepth 1)
    found=0
    while IFS= read -r f; do
        # The declaration describes the content; it is not content. Without this
        # it matched *.yaml like anything else and was copied in as a manifest,
        # so a component rendered by file copy shipped its own packaging
        # instructions to the cluster.
        case "$(basename "$f")" in templated-fields*.yaml) continue ;; esac
        cp "$f" "$CHART/files/$(echo "${f#$SRC/}" | tr '/' '_')"; found=1
    done < <(find "$SRC" ${depth[@]+"${depth[@]}"} -type f -name "$pattern" 2>/dev/null | sort)
    [[ $found -eq 1 ]] || { echo "$APP: no files matched '$pattern' under $SRC" >&2; exit 1; }
fi

# Content lives under files/ and is emitted verbatim. Helm applies Go templating
# to everything in templates/, and platform manifests are full of syntax that is
# not Go templating: ExternalSecret bodies interpolate with {{ .apikey }},
# Kyverno policies with {{ request.* }}, Crossplane XRDs with {{ tenantId }}.
# Rendering those as templates does not fail loudly -- {{ .apikey }} becomes an
# empty string -- so a chart would publish an ExternalSecret with a blank
# credential and look correct.
cat > "$CHART/templates/content.yaml" <<'TMPL'
{{- range $path, $_ := .Files.Glob "files/*.yaml" }}
---
{{ $.Files.Get $path }}
{{- end }}
TMPL

# Objects whose content depends on the cluster become templates. As a Kustomize
# source these were patches on the Application; a Helm source cannot carry them,
# because ArgoCD permits one source type per source (ADR-063).
# Per component first, then per directory. Two components can share a source
# directory and select different files from it -- platform-appproject-infrastructure
# and platform-argocd-principal-certs both read manifests/argocd-principal, one
# taking the AppProject and the other the certificates -- so a directory-level
# declaration names objects the other component legitimately does not render, and
# the packager refuses it. Correctly: the declaration is about content, and they
# do not share content.
DECL="$SRC/templated-fields.${APP}.yaml"
[[ -f "$DECL" ]] || DECL="$SRC/templated-fields.yaml"
if [[ -f "$DECL" ]]; then
    # Every rendered file in one call. Per file, a declaration had to name
    # objects that live in a different file, so any component rendering into
    # more than one failed on its first entry -- reported as content the
    # component does not render, which is the opposite of what was wrong.
    tf_files=("$CHART"/files/*.yaml)
    if [[ -e "${tf_files[0]}" ]]; then
        if ! scripts/package/templated-fields.py \
             "$DECL" "$CHART" "${tf_files[@]}"; then
            rm -rf "$CHART"; exit 1
        fi
    fi
fi

# A chart that renders no objects is never what was intended, and it is the one
# defect that survives packaging, pushing and installing in silence.
objects=$(helm template "$APP" "$CHART" --set global.environmentSlug=probe 2>/dev/null | grep -c '^kind:' || true)
if [[ "${objects:-0}" -eq 0 ]]; then
    echo "$APP: packaged chart renders no objects; refusing to produce it" >&2
    rm -rf "$CHART"; exit 1
fi

echo "packaged $APP -> $CHART ($(find "$CHART/files" -type f | wc -l | tr -d ' ') file(s), $objects object(s))"
