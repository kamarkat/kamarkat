{{/*
Resolve the base domain this box publishes on, refusing to render without one.

ADR-051 makes the environment overlay the system of record for the base domain,
and DeriveHubEndpoints (internal/kakt-cli/bootstrap/hubdomain.go) derives every
public host from it. The components never took part in that: agentgateway's
config carried the platform's own `dev.nutgraf.in` as a literal, so every box
built from a released bundle trusted the PLATFORM's Zitadel as its OIDC issuer
and fetched signing keys from it -- the runtime dependency on the vendor that
ADR-065 and ADR-066 exist to prevent, in the component fronting all traffic.

The refusal is the point, and it is the same argument as instanceRepoURL: there
is no correct default, because whichever domain the chart named would be some
other box's, and a published chart carries its defaults to every cluster that
installs it. A box that omits it must fail to render rather than quietly
authenticate against someone else's identity provider.

Day-0 supplies it: scaffolding knows the tenant's domain, having been given it.
*/}}
{{/*
Compose the domain every public hostname on this box sits under.

The PARTS are the record and the composition happens here, which is kubefirst's
shape: it stores DomainName and SubdomainName as separate fields on the cluster
and joins them at render time as fmt.Sprintf("%s.%s", SubdomainName, DomainName),
replacing its <DOMAIN_NAME> token with the result. ADR-051's amendment of
2026-09-18 adopts that separation -- a box declares a zone and an optional label
under it, and the environment has no part in either.

hubDomain remains accepted and wins when given. A box scaffolded before the split
has only that value and must keep working; one scaffolded after has domain and
subdomain, and this composes them.
*/}}
{{- define "environment-manager.hubDomain" -}}
{{- $d := .Values.hubDomain -}}
{{- if not $d -}}
{{- $domain := .Values.domain | default "" -}}
{{- if not $domain -}}
{{- fail "neither hubDomain nor domain is set: this chart cannot tell which domain this box publishes on, and the components that need it would fall back to the platform's own (ADR-051, ADR-065). Scaffolding writes them into registry/clusters/<name>/values.yaml." -}}
{{- end -}}
{{- $sub := .Values.subdomain | default "" | trimAll "." -}}
{{- if $sub -}}
{{- $d = printf "%s.%s" $sub $domain -}}
{{- else -}}
{{- $d = $domain -}}
{{- end -}}
{{- end -}}
{{- $d -}}
{{- end -}}
