{{/*
Resolve the workload cluster this box's burst-capacity autoscaler watches.

The autoscaler is management-side infrastructure ABOUT a workload cluster: the
clusterapi provider needs a client for that cluster to see which pods are
pending, so the cluster is named explicitly rather than discovered.

A VALUE THE BOX DECLARES, not a lookup keyed on environment and provider.

It was `burstSpokePool: {dev: {hetzner: ..., hybrid: ...}, ...}` in this chart's
own values -- a table of workload-cluster names inside the published bundle, so
every box that pulled the bundle resolved the same name. That is the defect
templated-fields exists to prevent for hubDomain and the Infisical ids: a per-box
fact must not be a literal in a published chart.

It also could not express what a box actually has. A repository holds one
management cluster and as many workload clusters as it declares (kubefirst's
layout, `kakt tenant add-cluster`), and two of those can share an environment
and a provider -- at which point one key has two answers and the table is
silently wrong for one of them.

Refusing beats defaulting, for the same reason as hubDomain: a default would be
some other box's cluster, and the failure it produces is silent -- an autoscaler
that never scales looks like a cluster with nothing to scale. Empty is legal and
means "no burst capacity on this box", which a box with no cloud burst pool
genuinely has; the autoscaler component is simply not enabled then.
*/}}
{{- define "environment-manager.burstSpokePool" -}}
{{- .Values.burstSpokePool | default "" -}}
{{- end -}}
