{{/*
The two axes, in one place (ADR-088).

`tenantId` is the organisation whose box this is. `appId` is a product running
on it. They were one field until 2026-09-24, which meant the platform could not
tell two products of one customer from two customers -- and resolved that, when
it came up, by putting one product in the other's namespace.

Neither defaults to the other. A chart rendered without `appId` fails here
rather than silently reproducing the collapse it replaces: an appId that falls
back to tenantId is exactly the old model wearing the new field's name.
*/}}

{{- define "universal-tenant.tenantId" -}}
{{- required "tenantId must be supplied: the organisation whose box this is (ADR-088)" .Values.tenantId -}}
{{- end -}}

{{- define "universal-tenant.appId" -}}
{{- required "appId must be supplied: the product this release renders (ADR-088). It does NOT default to tenantId -- that default is the collapse ADR-088 removes." .Values.appId -}}
{{- end -}}

{{/*
The namespace an app's workloads run in.

`tenant-` is kept as the prefix. It distinguishes tenant namespaces from the
platform's own `platform-*` ones at a glance, and dropping it to save five
characters would make `nutgraf-waypoint` indistinguishable from a platform
component by name alone.
*/}}
{{- define "universal-tenant.namespace" -}}
tenant-{{ include "universal-tenant.tenantId" . }}-{{ include "universal-tenant.appId" . }}
{{- end -}}

{{/*
Required on every tenant workload by the spoke's Kyverno ABI
(enforce-tenant-abi). BOTH are required, because attribution needs both: a cost
line against the tenant alone cannot be split between its products, and one
against the product alone cannot be billed to anyone.
*/}}
{{- define "universal-tenant.abiLabels" -}}
tenant-id: {{ include "universal-tenant.tenantId" . }}
app-id: {{ include "universal-tenant.appId" . }}
{{- end -}}

{{/*
Where this app's secret material lives (ADR-031 cell scoping, ADR-087 layout).

  /spoke-pool/<cell>/tenants/<app>/KEY

The segment is the APP, and always has been: before ADR-088 the field feeding it
was called tenantId but held a product. So the CONTENT of this path was already
right and only its label was wrong.

ADR-088 first moved it to /tenants/<tenant>/apps/<app>/, which reads correctly
and requires every existing key to be copied to a new location. That is a data
migration bought for a naming improvement, and it was the riskiest step in a
change that otherwise only renames identifiers -- an ExternalSecret fails as a
WHOLE object (ADR-087), so a partial copy withholds unrelated keys from
unrelated workloads.

Deferred deliberately. The path is corrected on its own, when moving it is the
only thing that can go wrong, rather than on the day the namespaces change too.
*/}}
{{- define "universal-tenant.secretPrefix" -}}
/spoke-pool/{{ required "clusterName must be supplied" .Values.clusterName }}/tenants/{{ include "universal-tenant.appId" . }}
{{- end -}}

{{/*
This app's logical database on the spoke's shared CloudNativePG cluster.

One per APP, not per tenant. Being siblings grants nothing: two apps of one
customer are as separated in their data as two apps of different customers,
because the namespace is the isolation boundary and a datastore reachable from
two namespaces is a hole in both. Sharing is possible only through an explicit
`sharedData` declaration naming both sides (ADR-088).
*/}}
{{- define "universal-tenant.databaseName" -}}
{{ include "universal-tenant.tenantId" . }}-{{ include "universal-tenant.appId" . }}-db
{{- end -}}

{{/*
The postgres role an app's database credential authenticates as.

THE ONLY PLACE THIS NAME IS BUILT. Every consumer -- the Role that creates it,
the database it owns, the DefaultPrivileges granted on it, the role it is a
member of, and the username templated into both ExternalSecrets -- reads
spec.dbRoleName from the XR. Before this it was rebuilt at six patch sites in the
composition and once more in the hub operator, and the operator built it on a
different axis (tenantId, not appId), so the credential named a role that did not
exist. Nothing compared the two: the only symptom was

  psql: FATAL: password authentication failed for user "tenant-<tenant>-user"

logged by the baseline migration Job, whose visible effect was a fleet with no
users table.

'_' IS A STRUCTURAL DELIMITER, NOT A STYLE CHOICE. A postgres role is
CLUSTER-WIDE -- pg_authid and pg_auth_members are shared catalogs, so two
databases in one cluster cannot hold distinct roles of the same name -- and one
spoke serves several tenants (ADR-051). The name must therefore be injective over
(tenantId, appId). Delimiting with '-' is NOT: '-' is legal inside both
components, so ("foo", "bar-baz") and ("foo-bar", "baz") both render
"foo-bar-baz", and the two tenants would have been handed a single role that owns
both their databases. '_' is forbidden by the identifier grammar
(^[a-z0-9]([-a-z0-9]*[a-z0-9])?$, enforced by the API server), so a name built
with it has exactly one parse.

LENGTH IS A CORRECTNESS PROPERTY. postgres truncates an identifier longer than 63
BYTES to 63 with a NOTICE and then succeeds, so two apps sharing a 51-byte prefix
would collide by truncation -- reintroducing exactly what the delimiter prevents.
The fixed parts cost 14 bytes -- the bound follows the LONGEST generated role,
tenant_<tenantId>_<appId>_owner (ADR-093), not the shorter _user -- leaving 49 for
the two components together; the
grammar admits only single-byte ASCII, so characters are bytes and the bound is
exact. Both XRDs carry it as a CEL rule, and this fails first with a message that
names the two lengths.
*/}}
{{/*
The postgres role that OWNS an app's database objects and runs its migrations.

SEPARATE FROM THE RUNTIME ROLE, AND THAT IS THE WHOLE POINT (ADR-093).

PostgreSQL exempts a table's OWNER from its own row-level security policies. While
the application connected as the owner, every policy the tenant baseline ships was
inert -- measured on a live spoke, a query as the app's own role with no claims set
returned every row where the policy would have matched none. Setting the RLS
context correctly (ADR-057) changed nothing, because the policies were never
evaluated.

So the migration Job connects as THIS role, which owns what it creates, and the
application connects as universal-tenant.dbRoleName, which owns nothing and holds
DML through DefaultPrivileges -- and is therefore subject to the policies.

THIS IS AN OWNER CREDENTIAL. It can perform any DDL and ownership operation in the
tenant's database. Only the baseline migration Job carries it; a workload that
mounts it is back to the state ADR-093 exists to remove, and silently so.

Composed here, once, for the same reason dbRoleName is: two names that must agree
with what the composition creates, rebuilt at neither of their consumers.

DEPRECATED 2026-10-01 (ADR-099). The owner role is no longer created: the platform
provisions a database and a role and puts nothing inside the database, so there is
nothing for a second role to own. The value is still emitted onto the XR because
the field is still present on both XRDs -- optional and ignored -- and removing the
two together is one change too many for one release. When the property goes, so
does this helper, and the role-length budget it drives loosens with it.
*/}}
{{- define "universal-tenant.dbOwnerRoleName" -}}
{{- $t := include "universal-tenant.tenantId" . -}}
{{- $a := include "universal-tenant.appId" . -}}
{{- include "universal-tenant.assertRoleBudget" . -}}
tenant_{{ $t }}_{{ $a }}_owner
{{- end -}}

{{/*
The shared budget both role names are bound by.

ONE ASSERTION, NOT ONE PER NAME. The bound follows the LONGEST generated role --
tenant_<t>_<a>_owner, whose fixed parts cost 14 bytes -- so checking it in
dbRoleName alone would pass a pair that only dbOwnerRoleName overflows, which is
the truncation collision ADR-090 exists to prevent arriving through the name
ADR-090 did not cover.
*/}}
{{- define "universal-tenant.assertRoleBudget" -}}
{{- $t := include "universal-tenant.tenantId" . -}}
{{- $a := include "universal-tenant.appId" . -}}
{{- if gt (add (len $t) (len $a)) 49 -}}
{{- fail (printf "tenantId (%q, %d chars) and appId (%q, %d chars) total %d, over the 49 available: the postgres role tenant_%s_%s_owner would exceed postgres's 63-byte identifier limit, and postgres truncates rather than failing -- which would silently give two apps one role" $t (len $t) $a (len $a) (add (len $t) (len $a)) $t $a) -}}
{{- end -}}
{{- end -}}

{{- define "universal-tenant.dbRoleName" -}}
{{- $t := include "universal-tenant.tenantId" . -}}
{{- $a := include "universal-tenant.appId" . -}}
{{- include "universal-tenant.assertRoleBudget" . -}}
tenant_{{ $t }}_{{ $a }}_user
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
{{- define "universal-tenant.publicAssetPaths" -}}
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
{{- define "universal-tenant.publicRoutes" -}}
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
{{- define "universal-tenant.publicAssetDirectories" -}}
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
