{{/* Sanitize a hostname into a valid Gateway listener (SectionName): dots
     become dashes, alphanumeric+dashes, <= 63 chars.

     IDENTICAL to tenant-public-tls.listenerName, and it must stay identical:
     the HTTPRoute that chart renders attaches to this Gateway, and a listener
     named differently from what the route expects attaches to nothing while
     every status still reports Programmed. */}}
{{- define "platform-spoke-gateway.listenerName" -}}
{{- . | replace "." "-" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* The spoke's query endpoint hostname, DERIVED and never declared.

     One label on the box's own domain, the same shape as every other platform
     endpoint (credits.<domain>, grafana.<domain>), with the spoke's name in it
     because a box has more than one spoke and each publishes its own telemetry.
     Derived rather than configured so the hostname the listener terminates, the
     hostname the certificate is issued for and the hostname the route matches
     cannot drift from each other. */}}
{{- define "platform-spoke-gateway.viewerHost" -}}
{{- printf "obs-%s.%s" .Values.spokeName .Values.hubDomain -}}
{{- end -}}

{{- define "platform-spoke-gateway.requirements" -}}
{{- with .Values.observability.viewer -}}
{{- if .audience -}}
{{- if not $.Values.spokeName -}}
{{- fail "observability.viewer.audience is declared but spokeName is empty: the query endpoint's hostname is derived from it, and a host of \"obs-.<domain>\" is not a name anything can resolve" -}}
{{- end -}}
{{- if not $.Values.hubDomain -}}
{{- fail "observability.viewer.audience is declared but hubDomain is empty: there is no domain to publish the query endpoint on" -}}
{{- end -}}
{{- if not .issuer -}}
{{- fail "observability.viewer.audience is declared but no issuer was passed: the certificate would be issued by nothing and the listener would terminate TLS with a Secret that never appears" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- range .Values.publicHosts -}}
{{- if not .host -}}
{{- fail "publicHosts entry with no host: the aggregate is generated, so an entry missing its hostname means the producer is broken, not the input" -}}
{{- end -}}
{{- if not .tlsSecret -}}
{{- fail (printf "publicHosts entry %q has no tlsSecret: a listener with no certificate cannot terminate TLS, and Gateway API reports that as a listener condition rather than a render error" .host) -}}
{{- end -}}
{{- if hasPrefix "*." .host -}}
{{- fail (printf "wildcard hostname %q: unissuable under the HTTP-01 solver model (ADR-051), and a wildcard certificate covering every app on the spoke is a security boundary this platform has not chosen (ADR-096)" .host) -}}
{{- end -}}
{{- end -}}
{{- end -}}
