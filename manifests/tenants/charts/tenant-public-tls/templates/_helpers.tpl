{{/* Sanitize a hostname into a valid Gateway listener (SectionName) name:
     dots become dashes; result is alphanumeric+dashes, ≤63 chars. */}}
{{- define "tenant-public-tls.listenerName" -}}
{{- . | replace "." "-" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "tenant-public-tls.certificateName" -}}
{{- printf "%s-public-tls" (include "tenant-public-tls.scope" .) -}}
{{- end -}}

{{- define "tenant-public-tls.secretName" -}}
{{- printf "%s-public-tls" (include "tenant-public-tls.scope" .) -}}
{{- end -}}

{{/* Hard guards: the chart refuses to render without tenant identity and an
     explicitly supplied environment issuer. Missing values must FAIL, never
     silently inherit — staging is an ephemeral-cluster policy, not a default. */}}
{{- define "tenant-public-tls.requirements" -}}
{{- if not .Values.appId -}}
{{- fail "appId must be supplied: this chart routes to ONE app's gateway (ADR-088). It does not default to tenantId." -}}
{{- end -}}
{{- if not .Values.tenantId -}}
{{- fail "tenantId is required (fleet-registry value)" -}}
{{- end -}}
{{- if not .Values.issuer -}}
{{- fail "issuer is required with no default: environment bootstrap must pass publicTlsIssuer (ephemeral=letsencrypt-staging, dev/stg/prod=letsencrypt-prod)" -}}
{{- end -}}
{{- if and .Values.public.hosts (not .Values.gateway.enabled) -}}
{{- fail (printf "public.hosts declares %d hostname(s) but gateway.enabled is not set for app %q: the backend this chart routes to, Service agentgateway-%s, is rendered by universal-tenant under that same flag, together with the ReferenceGrant that permits a platform-ops route to reach it. Without it this chart would publish an HTTPRoute to a Service that was never asked for, and Gateway API reports that as RefNotPermitted -- which reads as a permission fault and is really a backend nobody declared. Set gateway.enabled in this app's values file, or remove public.hosts." (len .Values.public.hosts) .Values.appId .Values.appId) -}}
{{- end -}}
{{- range .Values.public.hosts -}}
{{- if hasPrefix "*." . -}}
{{- fail (printf "wildcard hostname %q declared: wildcard certificates are unissuable under the HTTP-01 solver model (ADR-051)" .) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
The namespace the app's AgentGateway runs in (ADR-088).

Both axes: the Service is app-named (agentgateway-<app>) but it lives in
tenant-<tenant>-<app>, and this chart renders into the SHARED platform-ops
namespace -- so the objects it creates there need the scope in their NAMES too,
or waypoint's public ingress and oranger's would be one object.
*/}}
{{- define "tenant-public-tls.appNamespace" -}}
tenant-{{ .Values.tenantId }}-{{ .Values.appId }}
{{- end -}}

{{- define "tenant-public-tls.scope" -}}
{{ .Values.tenantId }}-{{ .Values.appId }}
{{- end -}}

{{- /*
  The paths this application serves WITHOUT a login (gateway.publicAssets.paths),
  validated, as a JSON list. Empty when none are declared.

  For static files a browser fetches outside the user's session -- a web app
  manifest and its icons, a service worker. The rules below are what keep the
  setting to that:

    - EXACT paths only: no wildcard, no regex, no query, no trailing slash, and
      no character outside [A-Za-z0-9._~-] in a segment. A prefix would make
      whatever is added under it later public by accident.
    - never "/" or "/index.html" (the app shell), and nothing whose first
      segment is api, internal, v1, oauth, health or healthz (case-insensitive):
      those are the APIs, the webhook surface, the login flow and the probes.
    - no "." or ".." segment, no duplicates, at most 32 paths (each renders two
      Gateway API matches, GET and HEAD, and a rule holds at most 64).

  The SAME rules are defined in universal-tenant and tenant-public-tls, because
  both charts render from the one per-app values file and must refuse the same
  input: the gateway bind and the public route that reaches it. preflight
  101-public-assets checks the two still agree.
*/ -}}
{{- define "tenant-public-tls.publicAssetPaths" -}}
{{- $paths := ((.Values.gateway).publicAssets).paths | default list -}}
{{- if not (kindIs "slice" $paths) -}}
{{- fail "gateway.publicAssets.paths must be a list of exact paths, e.g. [/manifest.json, /sw.js]" -}}
{{- end -}}
{{- if gt (len $paths) 32 -}}
{{- fail (printf "gateway.publicAssets.paths declares %d paths; at most 32 are allowed" (len $paths)) -}}
{{- end -}}
{{- $seen := dict -}}
{{- range $p := $paths -}}
{{- if not (kindIs "string" $p) -}}
{{- fail (printf "gateway.publicAssets.paths entry %v is not a string" $p) -}}
{{- end -}}
{{- if not (regexMatch "^(/[A-Za-z0-9._~-]+)+$" $p) -}}
{{- fail (printf "gateway.publicAssets.paths entry %q is not an exact path: it must start with /, have no trailing slash, wildcard, query or regex, and use only [A-Za-z0-9._~-] in each segment" $p) -}}
{{- end -}}
{{- $segments := splitList "/" (trimPrefix "/" $p) -}}
{{- range $s := $segments -}}
{{- if or (eq $s ".") (eq $s "..") -}}
{{- fail (printf "gateway.publicAssets.paths entry %q contains a %q segment" $p $s) -}}
{{- end -}}
{{- end -}}
{{- if has (lower (first $segments)) (list "api" "internal" "v1" "oauth" "health" "healthz") -}}
{{- fail (printf "gateway.publicAssets.paths entry %q is under /%s, which always requires a login" $p (first $segments)) -}}
{{- end -}}
{{- if eq (lower $p) "/index.html" -}}
{{- fail "gateway.publicAssets.paths may not include /index.html: the app shell always requires a login" -}}
{{- end -}}
{{- if hasKey $seen $p -}}
{{- fail (printf "gateway.publicAssets.paths lists %q more than once" $p) -}}
{{- end -}}
{{- $_ := set $seen $p true -}}
{{- end -}}
{{- toJson $paths -}}
{{- end -}}

{{- /*
  The application's login-free DYNAMIC routes (gateway.publicRoutes, ADR-103),
  validated, as a JSON list of {prefix, tokenLength, answeredBy, regex}. Empty
  when none.

  answeredBy (amendment 2026-10-12) names which of the app's two workloads
  answers: bff (the default) or frontend, whose built page then loads its files
  from a gateway.publicAssets directory and its data from a bff route.

  For a page the app serves to anyone holding an unguessable link -- a shared
  session -- which publicAssets cannot express: that is exact static paths to
  the frontend, and a share is one path per token, answered by the BFF.

  Each entry is ONE prefix segment plus ONE fixed-shape token segment, and the
  regex is built here, never taken from values:

      ^/<prefix>/[A-Za-z0-9]{<tokenLength>}$

    - prefix: a single lowercase segment ([a-z][a-z0-9-]*, at most 32), never
      api, internal, v1, oauth, health, healthz, ui, assets or static, and never
      the first segment of a gateway.publicAssets path;
    - tokenLength: 16..64 base62 characters (default 22, about 131 bits). The
      token is the only credential, so its shape is the platform's floor;
    - at most 4 entries, no duplicate prefixes.

  The SAME rules are defined in universal-tenant and tenant-public-tls: the bind
  and the public route that reaches it render from one values file and must
  refuse the same input. preflight 103-public-routes checks they agree.
*/ -}}
{{- define "tenant-public-tls.publicRoutes" -}}
{{- $routes := ((.Values.gateway).publicRoutes) | default list -}}
{{- if not (kindIs "slice" $routes) -}}
{{- fail "gateway.publicRoutes must be a list, e.g. [{prefix: /share, tokenLength: 22}]" -}}
{{- end -}}
{{- if gt (len $routes) 4 -}}
{{- fail (printf "gateway.publicRoutes declares %d routes; at most 4 are allowed" (len $routes)) -}}
{{- end -}}
{{- $assetFirst := dict -}}
{{- range $p := (((.Values.gateway).publicAssets).paths | default list) -}}
{{- if kindIs "string" $p -}}{{- $_ := set $assetFirst (lower (first (splitList "/" (trimPrefix "/" $p)))) true -}}{{- end -}}
{{- end -}}
{{- $seen := dict -}}
{{- $out := list -}}
{{- range $r := $routes -}}
{{- if not (kindIs "map" $r) -}}
{{- fail (printf "gateway.publicRoutes entry %v must be a map with prefix and optional tokenLength" $r) -}}
{{- end -}}
{{- $prefix := $r.prefix | default "" -}}
{{- if not (regexMatch "^/[a-z][a-z0-9-]{0,31}$" $prefix) -}}
{{- fail (printf "gateway.publicRoutes prefix %q must be one lowercase path segment, e.g. /share" $prefix) -}}
{{- end -}}
{{- $seg := trimPrefix "/" $prefix -}}
{{- if has $seg (list "api" "internal" "v1" "oauth" "health" "healthz" "ui" "assets" "static") -}}
{{- fail (printf "gateway.publicRoutes prefix %q is reserved and always requires a login" $prefix) -}}
{{- end -}}
{{- if hasKey $assetFirst $seg -}}
{{- fail (printf "gateway.publicRoutes prefix %q collides with a gateway.publicAssets path" $prefix) -}}
{{- end -}}
{{- if hasKey $seen $seg -}}
{{- fail (printf "gateway.publicRoutes lists prefix %q more than once" $prefix) -}}
{{- end -}}
{{- $_ := set $seen $seg true -}}
{{- $len := $r.tokenLength | default 22 -}}
{{- $numeric := or (kindIs "float64" $len) (kindIs "int" $len) (kindIs "int64" $len) -}}
{{- if or (not $numeric) (and $numeric (or (ne (float64 (int $len)) (float64 $len)) (lt (int $len) 16) (gt (int $len) 64))) -}}
{{- fail (printf "gateway.publicRoutes tokenLength %v for %q must be an integer in [16, 64]" $len $prefix) -}}
{{- end -}}
{{- $answeredBy := $r.answeredBy | default "bff" -}}
{{- if not (has $answeredBy (list "bff" "frontend")) -}}
{{- fail (printf "gateway.publicRoutes answeredBy %v for %q must be bff or frontend" $answeredBy $prefix) -}}
{{- end -}}
{{- $out = append $out (dict "prefix" $prefix "tokenLength" (int $len) "answeredBy" $answeredBy "regex" (printf "^%s/[A-Za-z0-9]{%d}$" $prefix (int $len))) -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{- /*
  The application's public static DIRECTORIES (gateway.publicAssets.directories,
  ADR-101 amendment 2026-10-12), validated, as a JSON list. Empty when none.

  For a built page served without a login (a public route answered by the
  frontend, ADR-103): its scripts and styles live in a build directory whose
  file names change with every build, which exact paths cannot name.

    - each entry is ONE segment with a trailing slash, e.g. /assets/, using
      only [a-z0-9._-]; no wildcard, regex or nesting;
    - never api, internal, v1, oauth, health or healthz;
    - never a gateway.publicRoutes prefix, and never a directory that holds a
      declared exact path (the two lists must not overlap);
    - at most 4 entries, no duplicates.

  What is in a declared directory is public: the chart cannot read the build.

  The SAME rules are defined in universal-tenant and tenant-public-tls.
  preflight 101-public-assets checks the two still agree.
*/ -}}
{{- define "tenant-public-tls.publicAssetDirectories" -}}
{{- $dirs := ((.Values.gateway).publicAssets).directories | default list -}}
{{- if not (kindIs "slice" $dirs) -}}
{{- fail "gateway.publicAssets.directories must be a list of one-segment directories, e.g. [/assets/]" -}}
{{- end -}}
{{- if gt (len $dirs) 4 -}}
{{- fail (printf "gateway.publicAssets.directories declares %d directories; at most 4 are allowed" (len $dirs)) -}}
{{- end -}}
{{- $pathCount := len ((((.Values.gateway).publicAssets).paths) | default list) -}}
{{- if gt (add $pathCount (len $dirs)) 32 -}}
{{- fail (printf "gateway.publicAssets declares %d paths and %d directories; together at most 32 are allowed (each renders two matches, and a rule holds 64)" $pathCount (len $dirs)) -}}
{{- end -}}
{{- $routePrefixes := dict -}}
{{- range $r := (((.Values.gateway).publicRoutes) | default list) -}}
{{- if and (kindIs "map" $r) (kindIs "string" ($r.prefix | default "")) -}}{{- $_ := set $routePrefixes (trimPrefix "/" ($r.prefix | default "")) true -}}{{- end -}}
{{- end -}}
{{- $assetFirst := dict -}}
{{- range $p := (((.Values.gateway).publicAssets).paths | default list) -}}
{{- if kindIs "string" $p -}}{{- $_ := set $assetFirst (lower (first (splitList "/" (trimPrefix "/" $p)))) true -}}{{- end -}}
{{- end -}}
{{- $seen := dict -}}
{{- range $d := $dirs -}}
{{- if not (kindIs "string" $d) -}}
{{- fail (printf "gateway.publicAssets.directories entry %v is not a string" $d) -}}
{{- end -}}
{{- if not (regexMatch "^/[a-z0-9._-]+/$" $d) -}}
{{- fail (printf "gateway.publicAssets.directories entry %q must be one segment with a trailing slash, e.g. /assets/, using only [a-z0-9._-]" $d) -}}
{{- end -}}
{{- $seg := trimSuffix "/" (trimPrefix "/" $d) -}}
{{- if or (eq $seg ".") (eq $seg "..") -}}
{{- fail (printf "gateway.publicAssets.directories entry %q is not a directory name" $d) -}}
{{- end -}}
{{- if has $seg (list "api" "internal" "v1" "oauth" "health" "healthz") -}}
{{- fail (printf "gateway.publicAssets.directories entry %q is under /%s, which always requires a login" $d $seg) -}}
{{- end -}}
{{- if hasKey $routePrefixes $seg -}}
{{- fail (printf "gateway.publicAssets.directories entry %q is a gateway.publicRoutes prefix" $d) -}}
{{- end -}}
{{- if hasKey $assetFirst $seg -}}
{{- fail (printf "gateway.publicAssets.directories entry %q holds a declared gateway.publicAssets path; declare one or the other" $d) -}}
{{- end -}}
{{- if hasKey $seen $seg -}}
{{- fail (printf "gateway.publicAssets.directories lists %q more than once" $d) -}}
{{- end -}}
{{- $_ := set $seen $seg true -}}
{{- end -}}
{{- toJson $dirs -}}
{{- end -}}
