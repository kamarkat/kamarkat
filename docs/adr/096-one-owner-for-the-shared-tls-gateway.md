# ADR-096: One owner for the shared TLS Gateway

**Date:** 2026-09-29
**Status:** Accepted — and INTERIM by construction. See "What supersedes this".
**Implements:** ADR-051's amendment of 2026-09-21, which already required that no
two owners write one Gateway's `spec.listeners`, with the machinery available
today. It does not change that requirement; it is the first implementation of it.
**Superseded when:** ADR-085 lands Gateway API 1.6 / Cilium 1.20 and ListenerSets
become available, which is the delegated model ADR-051 actually asks for.
**Relates to:** ADR-046 §8 (hostNetwork, ClusterIP gateway Service), ADR-047,
ADR-050 (no-bypass), ADR-088 (per-app axis)

## The invariant

> **No tenant Application may create, patch, or own the shared tenant TLS
> Gateway.**

Stated as an ownership rule, not as a technique. The previous arrangement failed
because its safety depended on an implementation detail of Server-Side Apply that
a future chart could forget; a rule about who owns the object cannot be forgotten
by editing a template.

## What happened

`oranger.dev.nutgraf.in` could not be reached. The cause was not oranger.

`tenant-public-tls` rendered a Gateway with a fixed name, `tenant-tls-gateway`,
once per application, each copy declaring only its own listener. Its own comment
said that was safe:

> *"spec.listeners is `x-kubernetes-list-type: map` keyed on `name`, so under
> ServerSideApply each contributor's entries survive the other's apply."*

That is true across distinct **field managers**. Every ArgoCD Application applies
as `argocd-controller`, so there is exactly one manager, and a manager declaring
a one-element list removes the entries it previously owned. The two Applications
overwrote each other every reconcile, and external-dns followed:

```
07:17  delete oranger  -> create waypoint
07:18  delete waypoint -> create oranger
07:20  delete oranger  -> create waypoint
```

**The public DNS of an application that was already working flapped on a ~60s
cycle.** It presented as "the new app is unreachable", which is the least
alarming description of it available. Waypoint answered when curled only because
the cycle happened to leave it up that minute.

**The most uncomfortable fact: this was already written down.** ADR-051's
amendment of 2026-09-21 says it almost verbatim —

> *"Arrangements in which two owners write one Gateway's `spec.listeners` do not
> satisfy it, because ArgoCD applies every Application under a single field
> manager — two Applications editing that field are not two ServerSideApply
> owners, and the later apply removes the earlier owner's listeners while the
> Gateway continues to report itself programmed."*

The analysis was correct and eight days old. The code contradicted it, and the
chart carried a comment asserting the opposite. Nothing reconciled the two,
because with one public hostname on the box both stories predicted the same
behaviour. **The gap was not a missing insight; it was a decision recorded and
not implemented** — which is the same shape as ADR-088's per-app axis sitting
unbuilt until a second application arrived, and is worth noticing as a pattern
rather than as two coincidences.

Two further facts, because they change what the fix has to be:

- The same comment claimed the object *"is now created by spoke-catalog"*. It is
  not, and never was. Every match for `tenant-tls-gateway` in the catalogue is a
  comment or the unapplied probe. The documented design was never built, and the
  gap stayed invisible while exactly one application declared a public host.
- Two Gateways cannot simply split the work. `probes/l2-dual-gateway-probe.yaml`
  proves on this spoke's exact Cilium build that two Gateways coexist on
  **different** ports and collide on the same one. Both would claim :443.

## Decision

One object, one Application, one field manager, one authoritative desired state.

```
environments/<env>/<app>/values.yaml  public.hosts     tenant intent
        |
        v  kakt fleet hosts aggregate                 one producer
registry/clusters/<spoke>/generated/values/public-hosts.yaml
        |
        v  platform-spoke-gateway                      one owner per spoke
tenant-tls-gateway
    waypoint listener
    oranger listener
```

and, independently:

```
tenant-public-tls
    Certificate
    HTTPRoute
```

`tenant-public-tls` no longer participates in Gateway ownership at all. It keeps
the Certificate and the HTTPRoute, which are genuinely per-application.

### Per-host listeners and certificates are kept

Gateway API selects a listener by SNI, so one Gateway terminates many per-tenant
certificates without a wildcard. A `*.dev.nutgraf.in` certificate would have made
the listener list trivial — and would have replaced per-application certificates
with one covering every hostname on the box. That is a different security
boundary, and adopting it to work around an ownership bug would be paying for the
fix in the wrong currency. HTTP-01 issuance is unchanged.

### The aggregate is generated, and never hand-edited

The declarations already exist, in each app's own values. Copying them into a
platform file by hand would recreate the duplicated-definition problem ADR-090,
ADR-093 and ADR-094 each removed somewhere else. `publicHosts` is a **derived
aggregate**, not an independent decision, so it has exactly one producer.

### The preflight is bidirectional and exact

Containment (`declared ⊆ aggregate`) is not enough, because the aggregate can be
wrong in three ways and two of them are silent:

```
missing     an app declares a host the aggregate omits
            -> no listener; the hostname does not resolve, and nothing reports it
stale       the aggregate carries a host no app declares
            -> a listener on a certificate nothing renews
duplicate   two apps declare the same hostname
            -> one listener, two Certificates racing for its certificateRef
```

So the check is set equality, and a hostname claimed by two applications is
refused outright — by the producer and again by the preflight.

## Consequences

- The class of failure is removed rather than mitigated. There is no arrangement
  of field managers to get right, because there is only one writer.
- **Adding a public hostname is now two steps**: declare it, then re-run the
  aggregation. That is the cost of a single producer, and the preflight is what
  stops the second step being forgotten silently.
- A spoke with no public application still owns an empty Gateway, so the first
  hostname adds a listener to an object that already exists.
- `prune: true` is safe on this Application where it was not before: it is the
  object's only declarer, so pruning removes what the aggregate no longer names
  rather than what another Application still wants.
- Oranger's `public.hosts` is held empty until this ships, and is restored then.
  That containment is recorded in the instance repository with the reason, not as
  a bare edit.

## What supersedes this

ADR-051 asks for **delegation**, not centralisation: the platform owns the
Gateway, the GatewayClass, the load balancer and `spec.allowedListeners`, and each
fleet owns a ListenerSet carrying its own hostname and certificate. That is
strictly better than this ADR — it removes the aggregation step entirely, so
there is no derived file to regenerate and nothing to drift.

It is not buildable today. There is no ListenerSet CRD on the spoke
(`kubectl get crd | grep listenerset` is empty) and the Gateway CRD serves
`v1 v1beta1`; ListenerSets arrive with the Gateway API 1.6 / Cilium 1.20 upgrade
in ADR-085, whose own diagram already draws `allowedListeners` on this Gateway.

So this ADR is the interim, and it is deliberately shaped to be thrown away:

```
today (this ADR)     one owner renders every listener from a generated aggregate
after ADR-085        platform owns Gateway + allowedListeners
                     each fleet owns its ListenerSet
                     the aggregate, its producer and its preflight are DELETED
```

The invariant at the top survives the change unaltered — a tenant Application
still never writes the Gateway object. What changes is that a fleet gets its own
object to write instead of a row in someone else's file.

## What this does not decide

Whether the aggregation should run in CI on a change to any app's `public.hosts`,
rather than being invoked. The preflight makes forgetting loud; making it
automatic is a workflow question, and doing it before the ownership model has run
in anger would be tuning a mechanism nobody has used yet.
