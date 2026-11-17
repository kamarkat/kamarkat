#!/usr/bin/env bash
# Package the remaining platform content a released cluster reconciles:
# the two tenant charts, and the spoke catalogue per environment and provider.
#
# Without these a released cluster reaches into the platform's repository for
# the chart that renders every tenant's namespace and for the catalogue its
# spokes run, which ADR-063 makes a condition of the runtime being supported
# rather than a preference.
set -euo pipefail

VERSION="${1:?usage: tenant-and-catalog-charts.sh <version> [outdir]}"
OUTDIR="${2:-dist/charts}"
mkdir -p "$OUTDIR"

# The tenant charts are already Helm charts; only their version is set here, so
# every artefact in a bundle carries the bundle's version and a cluster asks for
# one version rather than a set of them.
for src in manifests/tenants/charts/*/; do
    name=$(basename "$src")
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
    echo "packaged $name -> $chart"
done

# The catalogue is a Kustomize overlay per PROVIDER, the same shape as boundary
# 02's placement classes: the overlay IS the variation, so packaging it keeps the
# variation intact rather than reconstructing it from values.
#
# Per provider and no longer per environment as well. That axis produced six
# charts carrying three identical copies of each: cnpg-cluster.yaml and
# scheduled-backup.yaml matched byte for byte across dev, stg and prod once
# comments were stripped, and the only files unique to any of them justify
# themselves by provider -- "on hybrid spokes only", "a pure-Hetzner spoke has no
# such conflict". They lived under dev/ because dev was the only environment with
# a hybrid spoke.
#
# kubefirst splits platform content by provider and by nothing else; environment
# directories there hold tenant workloads, which is what ours hold too. ADR-051's
# amendment of 2026-09-18 removes the environment as an axis of platform content.
for prov_dir in manifests/spoke/spoke-catalog/providers/*/; do
    [[ -f "$prov_dir/kustomization.yaml" ]] || continue
    prov=$(basename "$prov_dir")
    name="platform-spoke-catalog-${prov}"
    chart="$OUTDIR/$name"
    rm -rf "$chart"; mkdir -p "$chart/files" "$chart/templates"
    cat > "$chart/Chart.yaml" <<CHART
apiVersion: v2
name: $name
description: Spoke catalogue for $prov, packaged from $prov_dir
type: application
version: $VERSION
appVersion: "$VERSION"
CHART
    if ! kustomize build --enable-helm "$prov_dir" > "$chart/files/rendered.yaml" 2>"$chart/.err"; then
        echo "$name: kustomize build failed:" >&2; sed 's/^/  /' "$chart/.err" >&2
        rm -rf "$chart"; exit 1
    fi
    rm -f "$chart/.err"

    # Objects whose content differs per spoke become chart templates; the
    # rest stays verbatim. As a Kustomize source these were patches on the
    # Application, and a Helm source cannot carry those (ADR-063).
    if ! scripts/package/templated-fields.py \
         manifests/spoke/spoke-catalog/templated-fields.yaml "$chart" \
         "$chart/files/rendered.yaml"; then
        rm -rf "$chart"; exit 1
    fi

    # Helm refuses any chart file over 5 MiB, and vendored CRDs put these
    # well past it. Split at document boundaries so no object is divided.
    scripts/package/split-rendered.py "$chart/files/rendered.yaml" "$chart/files" >/dev/null
    rm -f "$chart/files/rendered.yaml"
    cat > "$chart/templates/content.yaml" <<'TMPL'
{{- range $path, $_ := .Files.Glob "files/*.yaml" }}
---
{{ $.Files.Get $path }}
{{- end }}
TMPL
    # Rendered with the values a cluster supplies, because the templated
    # objects need them: rendering without would count zero and delete a
    # chart that is correct.
    # BOTH WAYS ROUND for the preview domain, because a box may serve no
    # previews and that is the path most boxes take.
    #
    # It used to render only WITH one, because ADR-108 made the value required.
    # It is optional now -- declaring the domain is what turns previews on -- and
    # the two branches are not the same chart: with a domain the preview zone
    # joins the issuers' dnsZones, external-dns is filtered to it and
    # preview-access runs; without, the zone is absent, external-dns is confined
    # to the hub zone and preview-access is scaled to zero. A gate that only ever
    # rendered one of them is a gate that has not seen what most boxes get.
    #
    # Helm's own error is kept: discarding it reported a missing value as
    # "renders no objects", which names the symptom and hides the cause.
    for preview in example-preview.test ""; do
        if ! rendered=$(helm template "$name" "$chart" \
            --set spokeName=probe --set global.provider="$prov" \
            --set global.previewDomain="$preview" 2>&1); then
            echo "$name: does not render${preview:+ with a preview domain}${preview:-" without a preview domain"}: ${rendered}" >&2
            rm -rf "$chart"; exit 1
        fi
        objects=$(grep -c '^kind:' <<< "$rendered" || true)
        [[ "${objects:-0}" -gt 0 ]] || { echo "$name: renders no objects; refusing" >&2; rm -rf "$chart"; exit 1; }
    done
    echo "packaged $name -> $chart ($objects object(s))"
done
