{{/*
The globals every platform-owned component receives.

One definition, because nine hand-written copies is how a component ends up
without one. hubDomain was added to two of them and missed in the rest, so the
HubEnvironment -- the object ADR-051 makes the authority for the base domain --
rendered spec.domain empty and the API server rejected it: "spec.domain in body
should match ...". Six boundaries had already deployed.

Extra globals are ignored by a chart that does not read them, so passing the
same set everywhere costs nothing and removes the question of which component
needs which.

Takes the root context.
*/}}
{{/*
The git organisation that owns this box's repositories, as scheme://host/org.

ArgoCD matches a repo-creds credential by URL PREFIX, so the organisation covers
every repository this box reads -- the tenant's gitops repository and any other
in the same organisation -- with one credential.

Derived from instanceRepoURL rather than carried separately: two values naming
one organisation is two things to keep in step, and the one that drifts is the
one nobody looks at.
*/}}
{{/*
The cluster this chart is rendering for. Used as the default external-dns TXT
owner id, which must be unique per box.
*/}}
{{- define "environment-manager.clusterName" -}}
{{- .Values.clusterName | default (printf "%s-%s" .Values.environmentSlug .Values.provider) -}}
{{- end -}}

{{- define "environment-manager.gitOrgURL" -}}
{{- $repo := .Values.instanceRepoURL | default "" | trimSuffix ".git" -}}
{{- if hasPrefix "git@" $repo -}}
{{- $repo = printf "https://%s" (replace ":" "/" (trimPrefix "git@" $repo)) -}}
{{- end -}}
{{- $parts := splitList "/" $repo -}}
{{- /* else, not a bare guard: `fail` does not stop evaluation here, so a bare
       guard still reaches the slice below and panics with "slice index out of
       bounds" -- burying the message that says what is actually wrong. */ -}}
{{- if lt (len $parts) 4 -}}
{{- fail (printf "cannot read an organisation out of instanceRepoURL %q: expected https://host/org/repo. ArgoCD needs it to hold one credential for every repository in this box's organisation (ADR-062)." $repo) -}}
{{- else -}}
{{- join "/" (slice $parts 0 4) -}}
{{- end -}}
{{- end -}}

{{/*
The OCI registry holding this box's workload charts.
Derived from the git organisation rather than configured separately: a tenant
publishes its charts beside its repositories, and two settings that must agree
are two settings that can disagree (ADR-073).

No scheme. ArgoCD strips oci:// when it normalises a repository URL, so a
credential recorded with one matches nothing and fails as a 401 on a chart that
is certainly present.
*/}}
{{- define "environment-manager.chartRegistryURL" -}}
{{- $org := last (splitList "/" (include "environment-manager.gitOrgURL" .)) -}}
{{- printf "ghcr.io/%s" $org -}}
{{- end -}}

{{- /*
environmentSlugOverride lets a caller emit the SAME global block with one key
replaced, instead of appending a second `global:` mapping after it.

That distinction is not cosmetic. YAML has no merge for duplicate keys at the
same level: a second `global:` REPLACES the first outright, so every other
global -- provider, hubDomain, clusterName, tenantId, the Infisical ids -- is
silently dropped. The spoke catalogue did exactly that to override the spoke's
environment, and the comment beside it said "last wins in Helm", which is true
of a scalar and false of the mapping that carries it.

What it cost: global.provider arrived nil on every spoke, the catalogue's
provider-gated template block rendered NOTHING, and ArgoCD pruned the objects
that block owns -- VMSingle, VLogs and the query endpoint's ConfigMap were
deleted as no longer declared, while the Application reported Synced with 354
resources synced and 11 pruned. Verified on nutgraf-01 2026-09-20.
*/ -}}
{{- define "environment-manager.globalValues" -}}
{{- /* Called either with the root context, or with
   (dict "root" $ "environmentSlug" "<expr>") to replace that one key. */ -}}
{{- $root := .root | default . -}}
global:
  {{- /*
    An override is emitted VERBATIM, already quoted by the caller. It carries a
    Go template action the ApplicationSet controller evaluates later, and
    `quote` would escape its inner quotes as \" -- which that controller
    rejects with `unexpected "\\" in operand`, generating 0 applications while
    the ApplicationSet itself looks correct.
  */}}
  {{- if .environmentSlug }}
  environmentSlug: {{ .environmentSlug }}
  {{- else }}
  environmentSlug: {{ $root.Values.environmentSlug | quote }}
  {{- end }}
  provider: {{ $root.Values.provider | quote }}
  hubDomain: {{ include "environment-manager.hubDomain" $root | quote }}
  {{- /* ADR-108: the registrable domain session previews are served from. The
         preview-access service refuses to start without it. */}}
  previewDomain: {{ $root.Values.previewDomain | quote }}
  {{- /* ADR-078 addenda 4 and 5: who each cluster's Alertmanager emails. */}}
  alerting:
    emailTo: {{ $root.Values.alerting.emailTo | toJson }}
  {{- /*
    ADR-111 §1: the logical hub. Keys the escrow path holding this box's Infisical
    master keys and its identity provider's encryption key, and the DNS records
    this box owns -- everything that must survive the CLUSTER below being
    replaced.

    `required` rather than defaulted from clusterName. A default would make the
    first successor inherit its predecessor's cluster name as the box id on one
    box and not another, so the master keys would be looked for under a name
    nothing wrote them under -- the secret store unrecoverable at exactly the
    moment the escrow exists to serve. Failing to render is the loud end of that
    choice; hub-operator refuses the same value for the same reason.
  */}}
  boxId: {{ required "boxId is required (ADR-111): it keys everything in the escrow that outlives a cluster, and there is no safe value to assume for it" $root.Values.boxId | quote }}
  clusterName: {{ include "environment-manager.clusterName" $root | quote }}
  {{- /*
    The box's own tenant id, for ADR-078 add.1 §7: Alloy stamps `cluster` AND
    `tenant` on everything it sends. `cluster` alone is unique within a box and
    not at a corporate Prometheus receiving three of them.

    Distinct from the `tenantId` the fleet ApplicationSets carry, which names a
    customer OF the box. This is the box's owner.
  */}}
  tenantId: {{ $root.Values.tenantId | default "platform" | quote }}
  gitOrgURL: {{ include "environment-manager.gitOrgURL" $root | quote }}
  chartRegistryURL: {{ include "environment-manager.chartRegistryURL" $root | quote }}
  dns:
    {{- /* The DNS SERVICE this box's zone lives in -- a platform-level choice,
           not external-dns's --provider flag. "hetzner" means Hetzner DNS,
           which external-dns reaches through a webhook sidecar; the translation
           to `--provider=webhook` happens where the flag is written, in
           external-dns's templated-fields.yaml. */}}
    provider: {{ $root.Values.dns.provider | default "hetzner" | quote }}
    {{- /* The TXT ownership key. Defaults to the cluster name because
           --policy=sync makes a shared key destructive: external-dns deletes
           the records it believes it owns, so two boxes sharing one would
           delete each other's. */}}
    ownerId: {{ $root.Values.dns.ownerId | default (include "environment-manager.clusterName" $root) | quote }}
  {{- /* The box's OIDC issuer. ONE producer, because three things must agree
         about it: the spoke's VMAuth validates a viewer's token against it, the
         hub Grafana sends people to it to sign in, and the tenants' gateways
         authenticate against it. Derived from the hub domain unless the box
         federates to an issuer of its own, which `oidcIssuer` sets -- and a box
         that does federate had the spoke deriving `id.<hubDomain>` while
         everything else used the declared one, which is a mismatch no error
         names: the token is simply refused as issued by somebody else. */}}
  oidcIssuer: {{ include "environment-manager.oidcIssuer" $root | quote }}
  {{- /* ADR-078 add.5 §7: the audience a viewer's query token must carry.
         Minted by this box's issuer when its observability client is
         registered, so it is empty until the box declares it -- and an empty
         one leaves the spoke's viewer endpoint unrendered rather than matching
         every token the issuer signed (spoke-catalog/templated-fields.yaml).
         Emitted even when empty: the catalogue reads it through `dig`, and a
         key that is absent on some boxes and present on others is read wrong
         exactly once. */}}
  observability:
    viewer:
      audience: {{ dig "viewer" "audience" "" ($root.Values.observability | default dict) | quote }}
      {{- /* The organisation the viewer role is held in. Required alongside the
             audience, because the issuer's role claim records WHERE a role is
             held: the claim the spoke matches is
             urn:zitadel:iam:org:project:<audience>:roles.viewer.<orgId>, and a
             claim name missing the organisation matches nothing at all -- which
             denies every query rather than admitting too many, but denies them
             with a VMUser that looks correct. Published by hub-operator as
             hub-observability-org-id when the client is registered, declared
             here once, quoted because an unquoted id is a number and a number
             formatted into a claim name is not the same string. */}}
      orgId: {{ dig "viewer" "orgId" "" ($root.Values.observability | default dict) | quote }}
  {{- /* The spoke this box's burst-capacity autoscaler watches. A global for the
         same reason as hubDomain: it is a fact about the box, and the
         alternative was the component carrying one box's spoke name as a
         literal. */}}
  burstSpokePool: {{ include "environment-manager.burstSpokePool" $root | quote }}
  {{- /* The box's own Infisical organisation and projects.

         A global for the same reason as hubDomain: it is a fact about the box,
         and the component that needs it (hub-environment, which renders
         hub-bootstrap-config) is reached through this helper rather than through
         the tenant's own values.

         That indirection is the whole bug this closes. The identity arrived in
         the tenant's repository, was passed to environment-manager as $values,
         and stopped here -- every child Application got a `global:` block this
         helper wrote, and this helper did not know about it. hub-environment
         therefore rendered the ConfigMap with empty ids while the correct ones
         sat one chart away, and hub-operator answered every SpokePool reconcile
         with "Project  not found" against the empty string.

         `default dict` because a box that has not bootstrapped Infisical yet has
         no identity to declare, and a nil map here fails the render of every
         boundary rather than of the one component that cares. */}}
  {{- /* BOTH levels guarded. environment-manager declares no `global` in its own
         values.yaml, so `$root.Values.global` is nil unless a box supplies one, and
         `$root.Values.global.infisical` on a nil map fails every boundary's render --
         not just this component's. */}}
  {{- $infisical := ($root.Values.global | default dict).infisical | default dict }}
  infisical:
    organizationId: {{ $infisical.organizationId | default "" | quote }}
    projectId: {{ $infisical.projectId | default "" | quote }}
    secretsProjectId: {{ $infisical.secretsProjectId | default "" | quote }}
    {{- /* The FLEET PKI project and the machine identity that signs against it.
           A different project from the two above -- this is where the issuer that
           signs spoke certificates lives -- and it arrives from the ADR-045
           artifact platform-pki-values.yaml, NOT under .global.

           Passed on because the SPOKE needs it. Its infisical-fleet-issuer shipped
           the platform's url, clientId and projectId as literals, so every spoke
           asked the PLATFORM's Infisical to sign its certificates, got EOF, and
           left every certificate naming that issuer pending forever -- the
           argocd-agent, alloy and support-agent client certs among them. */}}
    {{- $fleet := ($root.Values.infisical | default dict).fleet | default dict }}
    fleet:
      projectId: {{ $fleet.projectId | default "" | quote }}
      clientId: {{ $fleet.clientId | default "" | quote }}
{{- end -}}

{{/*
The same globals, with `provider` resolved to the ADR-046 §11 placement class.

For the three stateful components the variant selector must follow where the
DATA is placed, not what the box is: the subchart resolves files/<env>-<provider>
then files/<env> then files/<provider>, and a hybrid box placing its data on-prem
would otherwise render the cloud overlay and pin a storage class its nodes cannot
provision (ADR-075).

Those elements used to emit globalValues and then restate `provider:` under the
same `global:` mapping. It produced the right answer only because YAML takes the
last of a duplicated key -- a correctness that depended on emission order, in a
document nothing parsed strictly. One emitter, so the override cannot be undone
by moving a line.
*/}}
{{- define "environment-manager.placementGlobalValues" -}}
{{- $g := fromYaml (include "environment-manager.globalValues" .) -}}
{{- $_ := set $g.global "provider" (include "environment-manager.placementClass" .) -}}
{{- toYaml $g -}}
{{- end -}}
