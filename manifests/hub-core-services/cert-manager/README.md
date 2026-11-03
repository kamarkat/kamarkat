# Hub cert-manager tuning

cert-manager itself is installed **once**, at pivot, from the vendored upstream
asset `internal/assets/manifests/core/cert-manager/install.yaml`
(`internal/soloz-cli/pivot/orchestrator.go`). That asset is upstream v1.21.3 and is
deliberately left unmodified so it can be re-vendored on a version bump without
carrying merge conflicts.

This Application carries the hub-specific **deviations** from those upstream
defaults as partial, server-side-apply patches. The appset already sets
`ServerSideApply=true`, so each manifest here owns only the fields it names and
leaves the rest of the Deployment to the installer.

## Why these patches exist

Upstream ships all three cert-manager Deployments with **no resource requests**,
which makes them `BestEffort` — the lowest CPU shares on the node and the first
candidates for eviction. The webhook additionally ships `timeoutSeconds: 1` on
both probes (upstream gives the *controller* 15s).

On the hub node that combination fails hard. Observed 2026-08-23 on
`hub-hybrid-dev`, with the node at 2.80/3.0 cores and 913Mi free:

1. The 1s probe deadline is missed under CPU contention.
2. The kubelet SIGTERMs the webhook — it exits **0 / "Completed"**, so this reads
   as a clean shutdown, not a crash. 29 restarts in 8h.
3. Its Service loses its only endpoint, and Cilium socket-LB then fails every
   admission call with `EPERM` — `connect: operation not permitted`.
4. `ingress-shim` cannot create **any** Certificate, so no TLS is issued anywhere
   on the hub. `kubectl get certificate` simply returns nothing, which makes this
   look like a cert-manager config problem rather than a scheduling one.

The controller failed the same way (13 restarts, `BestEffort`), and each kill
dropped an in-flight ACME order.

## Re-vendoring cert-manager

These patches are additive and version-independent, but re-check them on a major
bump: if upstream starts shipping resource requests or wider probe deadlines,
delete the corresponding file rather than keeping a redundant override.

## Note on the underlying cause

These patches make cert-manager survive a saturated node; they do not fix the
saturation. The hub worker runs 59 pods on 3 vCPU / 5.8Gi with the control-plane
node tainted `NoSchedule`, and ~50 containers across it were restart-looping.
That is tracked separately.
