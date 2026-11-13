# The gates an upgrade trips

A release rehearsal refuses before it publishes, and an upgrade trips a
predictable subset of its gates. Each one below has fired on a real upgrade.
Prepare for them rather than discovering them one rehearsal at a time.

Run the rehearsal yourself only if that is yours to run; otherwise hand over the
command and fix what it reports. The full gate list belongs to the `release`
skill — this file is only the ones a version change causes.

## Contents

- [Before any of them: the mirror hook](#before-any-of-them-the-mirror-hook)
- [Gate: tree behind its upstream](#gate-tree-behind-its-upstream)
- [Gate: operator defaults](#gate-operator-defaults)
- [Gate: Day-0 seed parity](#gate-day-0-seed-parity)
- [Gate: a value the fixtures do not supply](#gate-a-value-the-fixtures-do-not-supply)
- [The kubeadm constants scraper](#the-kubeadm-constants-scraper)
- [The reasons check](#the-reasons-check)
- [Order to fix them in](#order-to-fix-them-in)

## Before any of them: the mirror hook

`internal/platform/embedded/` is a verbatim copy of the platform content a
released CLI carries, and a pre-commit hook refuses a commit whose copy has
drifted:

```
internal/platform/embedded is stale; run scripts/package/embed-platform-assets.sh and commit the result
```

Edit `manifests/` only, then run that script and stage the copy. It has to run
**after** the manifest edits and **before** every commit that touches embedded
content, so a change landed in pieces needs it each time. The hook syncs to a
temporary tree and diffs, so it cannot repair the drift it is checking for.

## Gate: tree behind its upstream

```
publish: this tree is N commit(s) behind its upstream.
```

Not a content check — it compares `HEAD..@{u}` only. Being *ahead* is fine; the
rehearsal packages your working tree either way.

It exists because several images are built by their own workflow, which tags by
commit and then **commits the resolved digest back**. That lands after your push,
so between pushing and pulling the tree still pins the previous digest, and a
publish in that window ships new manifests against an old image while everything
reports healthy. Pull after every CI pin-back.

## Gate: operator defaults

```
<Kind> is reconciled by <operator> <v>, but its defaults were read at <v>
```

`inventory.py` holds an `OPERATED` table recording, per operated instance, the
operator version its image defaults were read at. Bumping an operator without
re-reading those defaults fires this.

The subtler failure is the reverse, and it does **not** fire: pinning an
instance's image somewhere the inventory does not read. For a Grafana instance the
inventory reads `spec.version`; a pod-template image override sets the running
image while the release keeps reporting the operator's default. Pin where the
operator and the inventory agree, then check `operated_images` returns what you
expect before trusting it.

## Gate: Day-0 seed parity

```
Day-0 seeds argo-cd <v> but the bundle reconciles <v>
Day-0 seeds cert-manager <v> but the management cluster runs [<v>]
```

Two components are installed by the CLI before GitOps exists and then adopted by
GitOps, so the constant and the declaration must agree.

- **The GitOps chart**: `versions.ArgoCDChartVersion` against the
  `platform-argocd` descriptor's `targetRevision`. A mismatch makes the engine
  upgrade itself while it is still reconciling the first boundary.
- **cert-manager**: `versions.CertManagerVersion` against what the management
  cluster actually runs — which comes from the server-side-apply patches under
  `hub-core-services/cert-manager/`, not from the constant. Three places hold
  this version; see `references/delivery-map.md`.

Moving one half and not the others is the whole failure mode. Move them in one
commit.

## Gate: a value the fixtures do not supply

```
platform-bundle: released render failed:
  Error: execution error at (environment-manager/templates/04-tenant-services-appset.yaml:34:14):
  management.infisicalEnvironmentSlug is required (ADR-111 Revision 7) ...
```

The bundle chart requires a per-box value, and the gate's example values do not
include it. Not a pin problem and not a chart problem: the fixture is short.

Six places render this chart, each with its own hand-written `--set` list, so one
new `required` breaks all six and they fail one at a time:

| Renderer | What it checks |
| :- | :- |
| `scripts/package/bundle-chart.sh` | the released render produces the boundaries |
| `scripts/package/publish.sh` | application specs, per environment × provider |
| `scripts/validate/support-matrix.sh` | the support matrix, both halves |
| `scripts/validate/bundle-portability.sh` | no portability reference survives |
| `scripts/validate-argocd-seed-parity.sh` | Day-0 seeds what the bundle reconciles |
| `scripts/validate-cluster-name-contract.sh` | selector keys |
| `scripts/validate/preflight/10-manifest-renders.sh` | the chart renders at all |

Add the value to every one of them, with what a box declares. Never give the
chart a default to clear the gate — see
[traps.md](traps.md#adding-a-required-value-and-leaving-the-fixtures-behind) for
why, and for the nil-map variant that reports "renders no objects" and blames the
wrong component.

## The kubeadm constants scraper

```
inventory: kubeadm <v> constants name no CoreDNS, pause or etcd version
```

Not a gate on your pins — the inventory derives those images from kubeadm's
`constants.go` at the chosen Kubernetes version, and upstream restructures that
file. When it fires, the file changed shape: fix the scraper, never pin the
images. `references/delivery-map.md` records the change that has already
happened once.

## The reasons check

```
check: N of M components behind their latest release, K without a reason
```

Every component behind its upstream latest needs a `behind` item in
`manifests/architecture/component-catalog.yaml` naming the exact versions it
ships, or the docs sync refuses to document the release.

An upgrade both clears rows and creates them: a component you move to a newer
pin needs its old item removed or rewritten, because an item belongs to the
version it names. Expect to touch the catalog in the same change as the pins, not
after.

Two non-obvious failures:

- A version that looks numeric — `16.2`, `3.6`, `24.04` — parses as a YAML float
  in `versions: [...]` and silently matches nothing. Quote them.
- A component tracked against the wrong upstream project reads as far behind when
  it is current. Give it its own `repos` entry rather than a reason.

## Order to fix them in

1. Pull, so the digest pin-backs are in.
2. Fix pins and their paired constants together.
3. Run `embed-platform-assets.sh`, stage the mirror.
4. Update the catalog's `behind` items for every pin you moved.
5. Rehearse. Fix the cause on `main`; never patch the bundle or skip a gate.
