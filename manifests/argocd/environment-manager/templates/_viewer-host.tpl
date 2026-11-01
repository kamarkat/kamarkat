{{/*
The hostname a spoke publishes its telemetry query endpoint on (ADR-078 add.5 §7).

ONE PRODUCER, here, for a value that has to be identical in four places on two
clusters: the listener that terminates it, the certificate issued for it and the
route that matches it, all on the spoke -- and the Grafana datasource on the hub
that queries it. Nothing reconciles those against each other. A hostname the
first three agree on and the fourth does not is a datasource whose every query
fails TLS or resolves nowhere, and the spoke still reports Programmed, Ready and
Synced throughout.

It was derived inside the spoke's gateway chart, which could not be the answer
once the hub needed the same string: a second derivation in a second chart is
two producers, and the value they derive from (the spoke's name) reaches them
through different paths.

`{{ .name }}` is the ArgoCD cluster generator's, not Helm's -- this expands into
an ApplicationSet template, so the spoke's name is substituted per generated
Application. One label on the box's own domain, the same shape as every other
platform endpoint (grafana.<domain>, credits.<domain>), with the spoke's name in
it because a box has more than one spoke and each publishes its own telemetry.
*/}}
{{- define "environment-manager.viewerHost" -}}
obs-{{"{{"}} .name {{"}}"}}.{{ include "environment-manager.hubDomain" . }}
{{- end -}}
