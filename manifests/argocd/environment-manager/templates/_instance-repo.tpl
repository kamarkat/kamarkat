{{/*
Resolve the instance repository this box reconciles, refusing to render without
one.

ADR-062 gives every box its own `<tenant>-gitops` repository, so there is no
correct default: whichever repository the chart named would be some other box's,
and a published chart carries its defaults to every cluster that installs it.

The refusal is the point. An unset value would render an ApplicationSet whose
git generator matches nothing, which ArgoCD reports as Healthy having generated
zero Applications -- indistinguishable from a box with no tenants yet. Boundaries
05 and 06 are exactly the two whose inventory is legitimately zero
(internal/kakt-cli/health/boundary_inventory.go), so nothing downstream catches
it either.

Day-0 supplies the value, as it already supplies bundleRegistry: the CLI runs
inside the repository and knows its URL.
*/}}
{{- define "environment-manager.instanceRepoURL" -}}
{{- $url := .Values.instanceRepoURL -}}
{{- if not $url -}}
{{- fail "instanceRepoURL is not set: this chart cannot tell which repository holds this box's cluster and tenant declarations (ADR-062). Day-0 supplies it from the gitops repository it runs inside; the seed carries it as a parameter." -}}
{{- end -}}
{{- $url -}}
{{- end -}}
