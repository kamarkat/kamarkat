---
name: release
description: Make a Kamarkat platform release end to end with `kakt release` — readiness, publishing a bundle version, promoting the tenant, verifying the box and syncing the release pages in the Kamarkat docs. Use this whenever anyone mentions kakt release, release.sh, cutting or publishing an rc, a bundle version, promoting a box to a new version, a failed publish or promotion, the release gate, components.json, the component inventory or catalog, or adding a release to the docs (kakt release docs), even if they only ask "can we release now?" or "why did the publish fail?".
---

# Release

A release turns what is on `main` into a bundle version a box runs. It is one
ordered operation — **prepare, publish, promote, verify, document** — and each
step exists because skipping it once shipped something broken that reported
green. Follow the order; do not improvise around a gate.

## Roles

| Who | Does | Never |
| :- | :- | :- |
| Contributor | Lands changes on `main`, keeps CI green, readies the catalog and packaging | Runs `kakt release publish`, publishes, promotes |
| Release owner | Runs `kakt release`, decides the version | Edits code to get past a gate |
| Verifier | Checks the box once, syncs the docs | Re-publishes, re-promotes |

`kakt release` runs from the root of a kamarkat checkout: each subcommand runs
the checkout's own `scripts/release.sh` or `scripts/argocd-hard-sync.sh`. Without
an installed `kakt`, use `go run ./cmd/kakt release …`. Flags common to all:
`--gitops-dir` (default `.local-e2e/nutgraf-gitops`), `--owner` (default
`soloz-io`), `--version`.

One person can hold several roles, but only the release owner publishes. A
version is consumed the moment publishing starts (ADR-063), so two people
publishing is two burned versions.

## Versions

- **Prerelease** `X.Y.Z-rc.N`: `kakt release publish` publishes from a laptop and
  promotes the tenant. With no `--version` it takes the tenant's current rc + 1.
- **Stable** `X.Y.Z`: a GitHub Release triggers
  `.github/workflows/publish-platform-charts.yml`, which publishes the bundle and
  attaches the CLI. Once that run is green, the release owner promotes it with
  `kakt release promote --version X.Y.Z`, which confirms the version
  is in the registry and then promotes, syncs and verifies exactly as below.
- **A version is published once.** Never re-publish, re-tag or delete one. If a
  publish fails part way, the next attempt takes the next version.

## 1. Prepare — before anyone publishes

Work through all of it. Each item is something a gate will otherwise catch late,
after minutes of packaging, or something no gate catches at all.

1. **Everything is on `main` and pushed.** Commits go straight to `main`. Stage
   files by name; others may have work staged in the same tree, so never
   `git add -A`, never `git stash`, never commit a file you did not change.
2. **Hooks passed.** A failing pre-commit hook is a stop, not an obstacle. Never
   `--no-verify`: the embedded-assets hook, for one, guards the copy the CLI ships.
3. **Image builds finished and their pins landed.** Each operator and service
   image is built by its own workflow, which then commits the digest back
   (`chore(<component>): update image to <sha>`). Wait for every triggered
   workflow to go green, then `git pull --rebase`. Publishing before the pin
   commit lands ships new charts against old images, and nothing reports it.
4. **Every new artefact is catalogued.** Any new image, package or binary needs
   an entry in `manifests/architecture/component-catalog.yaml` with
   `description`, `license` (SPDX, verified at the upstream source — permitted:
   MIT, Apache, BSD, GPL, AGPL; flag anything commercial), `layer`, `repo` and
   `match`. Rules for `repo`, `repos` and `tag` are in the file's header.
   An uncatalogued artefact fails the release (inventory exit 3).
5. **Every deployable component is packaged.** A component a box can deploy must
   be packaged by exactly one route: a descriptor in
   `manifests/argocd/components/<boundary>/`, a `package_path` or existing-chart
   entry in `scripts/package/inline-charts.sh`, `provider-charts.sh`, or
   `tenant-and-catalog-charts.sh`. Value-gated components (switched by
   `capabilities.*.enabled`, `onPrem.enabled`, `burstSpokePool`) stay inline in
   the appset **and** need an `inline-charts.sh` line — a descriptor would ship
   them unconditionally. The release gate renders every supported combination
   with every optional component on, and fails on a chart the bundle lacks.
6. **Node template changes use new names.** ClusterClass templates are
   immutable: change a template by adding `<name>-vN+1` and pointing the class
   at it, never by editing in place.
7. **Operator defaults still hold.** The inventory lists pods operators create
   (Grafana, VictoriaMetrics, Alertmanager) from defaults read at a pinned
   operator version. If you bumped grafana-operator or the VictoriaMetrics
   operator, re-read the defaults at the new tag and update `OPERATED` in
   `scripts/package/inventory.py`, or the inventory refuses to run.
8. **The tenant checkout is clean.** `GITOPS_DIR` (default
   `.local-e2e/nutgraf-gitops`) has no uncommitted changes under `registry/` or
   `environments/`, and every cluster is on one `environmentRevision`.
9. **Tools.** Helm 3 (Helm 4 breaks kustomize's chart inflation), `kustomize`,
   `yq`, `gh` authenticated, `helm registry login ghcr.io`, Python with PyYAML.
10. **Rehearse, and check every component behind latest has a reason.**
    ```bash
    kakt release check
    ```
    Runs every packaging step and gate for the next version and pushes nothing;
    the version stays free. Then it lists every component behind today's latest
    release with no reason in the catalog (one newly behind, or one whose pin
    changed), and every reason whose `target` is this release or an earlier one
    while this release still ships the version: a promise the release breaks. Each release page explains why a red version is behind, from the
    entry's `behind` items (format in the catalog header), and the docs sync
    refuses a version it cannot explain. Add or update the item on `main`.
    When it fails, read the release check section of
    [references/gates.md](references/gates.md): check the pin before writing a reason.
    It ends `Ready. <version> can be released.`
    Then `kakt release publish --dry-run` shows the version, files and sync a
    release would touch.

## 2. Publish and promote — the release owner

```bash
kakt release publish                        # next rc after the tenant's current version
kakt release publish --version X.Y.Z-rc.N
kakt release promote --version X.Y.Z        # already published by CI
kakt release sync --version <v>             # re-sync a promoted version (KUBECONFIG = hub)
```

What it does, in this order, and why the order matters:

1. **Pulls the tenant repository** — the current version is read from it.
2. **Decides the version** from the tenant's `environmentRevision`, and refuses
   one already referenced.
3. **Publishes** via `make publish-local`: refuses a tree behind its upstream,
   packages, runs every gate, writes the inventory (`components.json`), refuses
   a version already in the registry, pushes. If it fails, nothing is promoted.
4. **Promotes every reference** under `registry/` (a bundle carries the version
   twice), verifies none of the old one survives, commits, rebases, pushes.
5. **Hard-syncs and verifies** the box reached the version
   (`scripts/argocd-hard-sync.sh --promote`). Its exit code is the answer.

`--no-promote` publishes only; `--no-sync` stops after the push.

Success ends with: `Done. <version> is published, promoted and running.`

When a gate fails, read [references/gates.md](references/gates.md): it names
each gate, what its message means and what to change. Fix the cause on `main`
through the normal path, then release again.

### When it fails part way

| Message says | State | Do |
| :- | :- | :- |
| `publish failed — nothing has been promoted` | Version possibly partly pushed | Fix the cause; release the **next** version |
| `version … is already published` | Consumed | Release the next version |
| `<v> IS PUBLISHED but not promoted` | Registry has it, git does not | Resolve in `GITOPS_DIR`, push. Do not re-publish |
| `the box did NOT converge` | Git says new, box says old | Fix the named Applications, run `kakt release sync --version <v>` again. Do not re-publish |
| `clusters are on different versions` | A half-applied promotion | Bring them to one version, or pass `--version` |
| `could not confirm <v> in oci://…` (`promote`) | Not published, or no registry login | Check the publish run; `helm registry login ghcr.io` |

Never force-push the tenant repository: a rejected push is someone else's
commit, usually a CI bot's.

## 3. Verify the box — once, then report

Check each thing once and report what you see. Do not loop waiting for a state.

- **Argo CD:** every Application `Synced` and `Healthy`. Treat these as not
  healthy: `Healthy` with no resources tracked (it manages nothing), a
  `ComparisonError`, a sync hook stuck running.
- **What changed:** for each component in the release's change list, confirm it
  on the box (image tag on the pod, node runtime version, CRD present).
- **Node changes:** new Machines come from the new template names; nodes are
  `Ready` with the expected runtime. Node pins only appear on nodes created after
  the release; say which pins remain unproven because no such node exists yet.
- **Pre-existing failures:** name them as pre-existing, with since-when. They are
  not caused by the release unless the evidence says so.

## 4. Sync the docs

Each published version gets a release page in the Kamarkat docs. Nothing does
this automatically. Follow [references/doc-sync.md](references/doc-sync.md).
The short form, in `kamarkat/` on `main` with `kamarkat-docs/` checked out beside it:

```bash
go run ./cmd/kakt release docs <version>
cd ../kamarkat-docs && mint validate && mint broken-links
git add data/releases versions snippets/releases docs.json
git commit -m "docs(releases): <version>" && git push
```

## 5. Report

End every release with this report, filled in:

```
Release <version>
  Published: <yes/no> at <registry time>; promoted: <commit sha in GITOPS_DIR>
  Box: <N> Applications Synced/Healthy; not healthy: <list, each marked new or pre-existing>
  Changed components verified: <list>; unproven: <list and why>
  Docs: <kamarkat-docs commit sha>; <behind> of <total> components behind latest
  Follow-ups: <anything the release revealed>
```

## Standards

- **Never fall back.** A required value with no default, an error never
  swallowed, one delivery path. A gate that fails is reporting a real defect.
- **Commit messages:** conventional (`feat(scope): …`, `fix(scope): …`,
  `docs(releases): …`), stating what changed and why. No `Co-Authored-By`
  trailer in commits you write.
- **Never commit** kubeconfigs, credentials or `.state/` files.
- **Changes to cluster state go through Git.** If a box needs a fix, change the
  manifest and release it; a `kubectl` edit is reverted by the next sync.
- **Record decisions** in the ADR they belong to as you make them.
