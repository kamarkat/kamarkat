# Plan: Kubernetes / CNPG upgrade track, then database restore drills (ADR-111 Revision 5)

**Owner:** Platform team (lead: Claude; execution: team member; approvals: user)
**Started:** 2026-10-15 · **Last updated:** 2026-11-03
**Status:** Phase 1 research complete. **Waiting on the user's decision D-TARGET** before the upgrade plan is written.

**Restore validation moves to staging (user, 2026-11-03).** Restoration is NOT exercised in
dev. It is tested as part of the **hub recreation test on staging**, once staging has been
created with all current changes. Nothing in this plan blocks creating staging.

That makes the recreation test the restore proof, which is the right shape: ADR-111 is
reconstruction, and recreating a hub is the only exercise that proves the whole chain --
escrowed master keys, the at-rest encryption key, a managed cluster's trust roots, and the
object archive -- rather than one restore in isolation. It also exercises the commands added
for it: `soloz escrow verify`, `escrow verify-recovery`, `spoke restore-pki` and
`spoke match-inventory`, none of which has been run against a live box.

Sequence: staging is created -> hub recreation test on staging -> that result is the restore
evidence -> prod. Phases 7-8 follow it rather than preceding it.

> **Rule for whoever picks this up:** no implementation until the user approves the upgrade
> plan and the read-only backup-access design. Verify every third-party claim at the
> DEPLOYED and TARGET release tags (never `main`/`master`); cite `URL@tag:line`.
> Several team-member claims were wrong when checked (see "Corrections"); do not reuse a
> version number from a report without re-checking it.

---

## 1. Why

- Both dev clusters (hub `nutgraf-hub`, spoke `nutgraf-01`) run **Kubernetes v1.31.6**, end of life.
- **CNPG 1.29.0** is end of life (1.29.x EOL 2026-09-29), and three patches behind.
- CNPG's built-in Barman (`spec.backup.barmanObjectStore`, used by both clusters) is
  **removed in CNPG 1.31.0** (CNPG v1.30 release notes; 1.31.0 not yet released).
- Restore drills were the original goal. They are now sequenced **after** the upgrade, and
  designed for the Barman Cloud **plugin**, the long-term backup mechanism.

## 2. Approved decisions (user, 2026-10-15)

1. Upgrade track first: Kubernetes **minor by minor** (no skipping); CNPG to a release
   supported on the target; built-in Barman → Barman Cloud plugin as its own validated change.
   Document every transitional version pair. *(Target version is now open: see D-TARGET.)*
2. ~~One-off **interim restore proof** of the CURRENT built-in archive, before migrating.~~
   **Superseded 2026-11-03:** restoration is not exercised in dev. The proof is the hub
   recreation test on staging. The isolated-capacity and read-only-access conditions carry
   over to that test unchanged -- they are what make any restore proof trustworthy.
3. Read-only restore key from a **separate Hetzner project**, bucket policy granting only
   read actions on the two source prefixes; **test that write/delete are denied**. Separate
   evidence-writer key and evidence bucket.
4. Capacity gate covers restored size, WAL replay, temp files, PVC/storage-class limits,
   peak recovery CPU/memory and production headroom. A skip = durable evidence + alert,
   never green.
5. Monthly drills; GitOps `CronJob` (UTC), `concurrencyPolicy: Forbid` + Lease +
   idempotent run id; CI only tests/releases the runner. **Amended 2026-11-03:** not in dev.
   The drill runs where the data it protects is, which is staging once it exists, then prod.
6. Validation: database/owner checks + versioned app assertions **relative to the backup's
   recovery point**; row counts are sanity only.
7. Janitor never deletes an active restore: absent owner + expired Lease + timeout + grace;
   verify PVC/PV per reclaim policy.
8. Alerts in the in-box evaluator (ADR-078 add.5 replaced Grafana Cloud); failed / skipped / overdue (35 days) are distinct; a skip does
   not reset the last-success clock.
9. GitOps owns durable config; the runner has namespaced rights over drill resources only;
   a hub-database drill's scheduler must be **outside the hub's failure domain**.

## 3. Decisions taken (2026-11-03)

**D-TARGET resolved: Option A, Kubernetes 1.35.10.** The user chose 1.35 and chose to keep
Kyverno, which caps the target: Kyverno 1.19 supports K8s 1.33-1.35 and 1.19.2 is the
newest Kyverno. 1.35 reaches maintenance mode 2026-12-28 and EOL 2027-02-28, so 1.36 is a
follow-on. Dropping Kyverno was considered and declined; had it gone, the four validating
policies move to ValidatingAdmissionPolicy (GA 1.30) and the one mutating policy
(burst placement) to MutatingAdmissionPolicy, which is GA only at 1.36.

**D-POLICY: latest-compatible, not latest.** Every component in the release inventory is in
upgrade scope. Each takes its newest upstream release that is compatible with K8s 1.35 and
the platform's existing contracts; where the newest is incompatible, it takes the newest
supported alternative and the blocking constraint is named. A component is not excluded
merely because K8s 1.35 does not require moving it.

**D-CAPI: CAPI 1.13.7, not 1.14.3.** CAPH's compatibility table supports CAPI 1.10.x-1.13.x
for the v1.1.x line; v1.2.x (the v1beta2 line) is unreleased. CAPH 1.1.10 is tested on
K8s 1.35. CAPI Operator 0.29.0, whose own CAPI dependency is the 1.13 series. The operator
installs the provider versions declared in its CRs, so its dependency and the provider
versions are distinct contracts.

**D-XPLANE: Crossplane 2.4.3 with cluster-scoped XRDs.** The three XRDs move to
apiextensions.crossplane.io/v2 with `scope: Cluster` and no claims. Namespaced was rejected:
each composition composes cluster-scoped resources (Namespace and ProviderConfig for
SpokePool, ClusterIssuer and ClusterSecretStore for AINativeSaaS, the SQL provider's managed
resources for TenantDatabase), and a namespaced XR may only compose in its own namespace.
Nothing ever created the *Claim kinds, so dropping claims costs no consumer change. The
v1-line providers (provider-kubernetes 1.4.1, provider-sql 0.16.2) keep cluster-scoped
managed resources, so v2's namespaced-MR model is deliberately NOT adopted.

**D-CILIUM: 1.20.4 with per-cluster certgen Hubble TLS.** 1.17.18 supports K8s <1.33 and so
cannot carry 1.35; 1.20.4 supports 1.33-1.36. Ownership of Hubble TLS moves from the Helm
render (`hubble.tls.auto.method: helm`, which generated the CA and server keys at render time
and committed them into cilium-addon-template.yaml) to in-cluster certgen (`cronJob`), giving
each spoke its own CA and putting no private key in git. cert-manager was rejected as the
owner: the CNI is delivered by ClusterResourceSet during bootstrap, before cert-manager
exists on that cluster. Upstream supports consecutive-minor upgrades only, so 1.17 -> 1.20 is
valid for a NEW cluster only; existing spokes need a walk or a recreate.

**D-PG: per-application PostgreSQL versions, chosen per service.** No cluster relies on the
operator's default image, because that default moves with the operator. Each cluster pins the
newest major ITS OWN application supports; the versions deliberately differ:

| Cluster | Application | Pin | Constraint |
|---|---|---|---|
| grafana-db | Grafana 13.3.0 | 18.6 | requires 12+ and ties its ceiling to PostgreSQL's own support policy, so 18 is covered explicitly |
| zitadel-db | ZITADEL 4.20.0 | 17.11 | documents 14-18, but a fresh install on 18 is reported to fail at init step 34 (UNLOGGED tables), unresolved |
| infisical-db | Infisical 0.167.4 | 17.11 | documents a floor (14) and no ceiling; its own production compose still ships postgres:14 |
| credits-db | ledger 2.4.17 | 17.11 | documents "14 or higher" with no tested ceiling; a financial ledger is the wrong place to be first on a major |
| billing-db | billing engine | 17 (partman image) | the engine's own single-image build depends on 17 with the same PGDG partman package |
| spoke shared | tenant applications | 18.6 | no application constrains it on a fresh box; note the choice binds future tenants, since TenantDatabase does not expose the major |

Retiring the billing partman image for a CNPG extension volume needs PostgreSQL 18 AND
Kubernetes image volumes, which are stable only at 1.36.

**D-OS: Ubuntu stays on 24.04 for now, and the image is declared once.**

The node bootstrap turned out to be Ubuntu-release-agnostic: Kubernetes packages come
from the distro-independent `pkgs.k8s.io/core:/stable:/v<minor>/deb/ /` repository, and
containerd 2.4.2, runc 1.5.2 and Tailscale 1.104.4 are GitHub release tarballs installed
into `/usr/local`. No apt source names a release codename, so 26.04 is not blocked by
package wiring -- an earlier claim that it required "changed package sources" was wrong.
Those three runtime pins are also already at their latest.

Two gates remain, and both need provider credentials:

1. Whether Hetzner **Cloud** publishes an `ubuntu-26.04` system image, and under exactly
   what name. Their dedicated-server catalogue lists 26.04, which is a different
   catalogue; the Cloud images endpoint requires a token. Answered by
   `hcloud image list --type system --architecture x86`.
2. Whether the published image's CPU baseline suits the machine types in use. Ubuntu
   26.04 public-cloud images may be built for `amd64v3` even when labelled `amd64`, so
   availability and instruction-set compatibility are separate questions.

Until both are answered the image stays `ubuntu-24.04`, and the inventory keeps reporting
Ubuntu behind its latest with "provider catalogue unverified" as the constraint. A guessed
image name is not acceptable.

Independently of that, the image is now declared in **one** place. The management
ClusterClass declares the `imageId` variable with its default, and the spoke pool
composition already relied on that default rather than restating it. The CLI was the only
caller carrying a second copy, so `cfg.ImageID`, the Config field and the topology's
`imageId` variable are removed -- hub and spoke now resolve the OS the same way, and the
26.04 move becomes a one-line change in the class.

**Superseded:** the original D-TARGET option table.

## 3a. Former open decision (resolved above)

**D-TARGET: Kubernetes target version.** The approved target was 1.36, but **no released
Kyverno supports 1.36** (Kyverno 1.19.x = K8s 1.33–1.35; 1.20 does not exist).

| Option | Summary | Lead's view |
|---|---|---|
| **A** | Target **1.35** now; 1.36 as a follow-on once Kyverno supports it. 1.35 EOL 2027-02-28, so the follow-on is due within ~4 months. | **Recommended** |
| B | Keep 1.36; run Kyverno outside its matrix | Not recommended: Kyverno webhooks gate every deployment |
| C | Keep 1.36; wait for Kyverno before starting | Not recommended: already on EOL Kubernetes and CNPG |

## 4. Verified facts (re-checked by lead; sources)

### Deployed (live reads, 2026-10-15 to 2026-10-20)

| Component | Hub | Spoke `nutgraf-01` | Repo pin |
|---|---|---|---|
| Kubernetes | v1.31.6 | v1.31.6 | ClusterClass / topology |
| Control plane | kubeadm (`KubeadmControlPlane nutgraf-hub-74dl4`) | kubeadm (`nutgraf-01-xq2p2`) | — (no Talos in use; Talos assets archived) |
| CAPI core | v1.10.0 | — | `internal/soloz-cli/versions/versions.go:8` |
| kubeadm bootstrap / control-plane providers | **v1.8.5** (mismatched with core) | — | `versions.go:16-17` |
| CAPH | v1.0.7 | — | `versions.go:22` |
| CAPI operator | v0.13.0 | — | `versions.go:5` |
| Cilium | v1.17.18 | v1.17.18 | `internal/assets/manifests/addons/cilium-rendered.yaml` |
| CNPG operator | 1.29.0 | 1.29.0 | `manifests/hub-core-services/cloudnative-pg/application.yaml:13`, `manifests/spoke/spoke-catalog/infra/cloudnative-pg/controller.yaml:523` |
| Postgres image | `ghcr.io/cloudnative-pg/postgresql:16.2` | same | cluster spec |
| Barman (in image) | **3.10.0** | 3.10.0 | — |
| cert-manager | v1.20.2 | | `versions.go:30` |
| ArgoCD | v2.14.11 (chart 7.9.1) | | `versions.go:43` |
| Kyverno | v1.19.1 (chart 3.9.1) | | `manifests/argocd/environment-manager/templates/01-platform-infra-appset.yaml:205` |
| Crossplane | v1.20.5 | | `manifests/argocd/components/01/platform-crossplane.yaml:8` |

### Backups (live)

| | Hub `platform-db` | Spoke `shared-cnpg` |
|---|---|---|
| Destination | `s3://hub-db-backups/nutgraf-hub/` | `s3://spoke-pool-backups/nutgraf-01/` |
| `serverName` | `nutgraf-hub-v1` | `shared-cnpg-v4` (recovered from `shared-cnpg-v3`) |
| Health (2026-10-15) | archiving True, last WAL <1 min, 19 completed backups, 1 old failure (2026-09-18), 3 transient WAL failures (last 2026-10-12) | archiving True, 0 WAL failures, 21 completed |
| Retention | 30d | 30d |
| Logical size | ~852 MB | ~176 MB |
| Latest base backup | ~289 MB | ~40 MB |
| WAL / 24h | ~1.57 GB | ~81 MB |
| Bucket versioning | off | off |
| Drill capacity | **Fits** on `flatcar-hub-node-1` (66 GB disk, ~2.8 CPU, ~2.5 Gi free) | **Restricted:** only `flatcar-spoke-node-1`, with a 5–10 Gi PVC (not 100 Gi) |

### Upstream support (checked at release tags / release lists)

| Item | Fact | Source |
|---|---|---|
| K8s EOL | 1.34 2026-10-27 · 1.35 2027-02-28 · 1.36 2027-06-28 · 1.37 2027-10-28 | `kubernetes/website data/releases/schedule.yaml` |
| CNPG | 1.29.x supports K8s 1.33–1.35, EOL 2026-09-29 · **1.30.x supports 1.34–1.36**, latest **v1.30.1** (2026-09-23), EOL ~Dec 2026 · built-in Barman removed in 1.31.0 | `cloudnative-pg docs/src/supported_releases.md`, `release_notes/v1.30.md:253`, releases |
| CNPG + Postgres | CNPG 1.30 supports Postgres 16 images → 16.2 → latest 16.x is a patch update, not `pg_upgrade` | team member R8 (accept; re-confirm image tag at plan time) |
| Barman plugin | latest v0.15.1 (2026-09-30) | releases |
| CAPI | v1.10, v1.11 EOL; v1.12 maintenance; current **v1.13.7**, **v1.14.3** (2026-10-07). **Skip upgrades: at most n-3 minors** | `cluster-api docs/book/src/reference/versions.md@v1.13.7:120-135`, releases |
| CAPH | 1.0.x = CAPI 1.8–1.10 · 1.1.x (latest **v1.1.8**) = CAPI 1.10–1.13 · CAPI 1.14 needs CAPH 1.2.x | `cluster-api-provider-hetzner docs/caph/01-getting-started/01-introduction.md:19-57`, tag v1.1.8 |
| Cilium | 1.17 = K8s 1.29–1.32 · 1.18 = 1.30–1.33 · 1.19 = 1.32–1.35 · 1.20 (latest **v1.20.2**) = 1.34–1.37. Latest patches: 1.18.14, 1.19.8 | `cilium Documentation/network/kubernetes/compatibility.rst@v1.17/v1.18/v1.19/main` |
| Kyverno | latest **v1.19.1** (2026-09-10); **1.19.x = K8s 1.33–1.35**; no 1.20 | `kyverno/website src/constants/version.ts:25`, releases |
| ArgoCD | latest v3.5.5 (2026-10-12). K8s support per team member: 2.14 = 1.28–1.31, 3.5 = 1.33–1.36 | releases; **matrix to re-confirm at plan time** |
| cert-manager | latest v1.21.2 (2026-09-11). Per team member: 1.20 = 1.32–1.35, 1.21 = 1.33–1.36 | releases; **matrix to re-confirm** |
| Crossplane | latest **v2.4.2**; 1.20 → 2.x is a **major** upgrade | releases |

### Restore mechanics (verified)

- Recovery at CNPG 1.29.0 (built-in): `spec.bootstrap.recovery.source` + `spec.externalClusters[].barmanObjectStore` with the source's exact `destinationPath` and `serverName` (`cloudnative-pg docs@v1.29.0 appendixes/backup_barmanobjectstore.md:273-319`).
- A recovery-only cluster must have **no `spec.backup`, no plugin WAL archiver, no custom archive command** (fail-closed preflight).
- Barman 3.10.0 restore needs only **`s3:ListBucket` + `s3:GetObject`** (`EnterpriseDB/barman barman/cloud_providers/aws_s3.py@release/3.10.0:203,254,281,305`); write calls belong to backup commands only.
- Hetzner: S3 keys are read/write across their project by default; bucket policies can restrict an individual key from another project to selected actions (Hetzner S3 credentials FAQ).

## 5. Corrections log (team-member claims found wrong)

| Claim | Reality |
|---|---|
| CNPG 1.29 supports K8s 1.34–1.36 | 1.33–1.35 |
| CAPI target 1.11/1.12 | both EOL/maintenance; target 1.13 or 1.14 |
| Barman 3.18.0 in our image (read from `master`) | 3.10.0 |
| CNPG 1.30.2 | does not exist; v1.30.1 |
| Kyverno 1.20.0 supports 1.34–1.36; "ClusterPolicy removal" | Kyverno 1.20 does not exist; no policy rewrite needed |
| Crossplane target v2.1.0 | latest v2.4.2; major upgrade, own step |
| Uncited breaking-change claims (`system:kubelet-api-admin`, Cilium `CiliumEndpointSlice`, Kyverno report API) | dropped by the team member as uncitable in Revision 2 |

## 6. Phases and status

| # | Phase | Status | Gate to leave it |
|---|---|---|---|
| 1 | Research: capacity, reuse, read-only key design (Part A); inventory, compat matrix, CAPI mechanics, breaking changes (Part B) | **Done** (Revision 2, with lead corrections above) | — |
| 2 | **D-TARGET** decision | **Waiting on user** | user picks A/B/C |
| 3 | Write the **upgrade plan** (ADR): minor-by-minor sequence; provider order (CAPH 1.0.7→1.1.8 first; kubeadm providers need an intermediate stop since 1.8.5→1.13 exceeds n-3; CAPI core 1.10→1.13 in one move); exact versions per step; Crossplane 1→2 as its own step; Postgres 16.2→latest 16.x; rollback per step; hub before or after spokes; CAPI template immutability (new-named classes/templates) | Not started | user approves the plan |
| 4 | Write the **read-only backup-access design**: separate Hetzner project, bucket policy (`s3:ListBucket` on the prefix + `s3:GetObject`), denial test plan | Not started (design inputs complete) | user approves |
| 5 | ~~Interim restore proof in dev~~ | **Cancelled 2026-11-03** | replaced by the hub recreation test on staging |
| 6 | Execute the upgrade, step by step | Not started | each step's checks; user-run |
| 7 | Built-in Barman → Barman Cloud plugin migration (own ADR) | Not started | after 6 reaches a CNPG version with the plugin path validated |
| 8 | **ADR-111 Revision 5 §6** restore drills (plugin-based), on staging then prod, after the hub recreation test + implementation: namespace/quota/netpol/RBAC, CronJob + Lease, runner image via CI, fail-closed preflight `111-restore-drill.sh`, evidence store, janitor, in-box alerts (ADR-078 add.5) | Not started | after 7 |

## 7. Immediate next step

1. User answers **D-TARGET**. Nothing else in this plan can move, and staging waits on it
   transitively.
2. Lead re-confirms the three "re-confirm" rows in §4 (ArgoCD, cert-manager, Postgres image)
   at their tags, then writes the upgrade plan (phase 3) and the access design (phase 4)
   for approval.

## 8. Artefacts

- Team-member reports (may be git-ignored; this plan carries their verified content):
  `scratch-pad/cnpg-restore-drill-phase1-report.md`, `scratch-pad/k8s-cnpg-upgrade-phase1-report.md`.
- Memory note: `db-restore-drill-and-upgrade-decisions`.
- Existing recovery runbooks to reuse (not to run for drills): `docs/runbooks/hub-database-restore.md`,
  `docs/runbooks/spoke-database-loss-and-recovery.md`. Do **not** run `make cnpg-bump-incarnation`
  or `cnpg-settle-incarnation` for a drill: they change the production incarnation.


## Restore proofs and drills (moved from ADR-111 Revision 5 §6, 2026-11-02)

These decisions were approved on 2026-10-15. They are operational practice and are recorded
here rather than in ADR-111, which states recovery contracts and sets no acceptance or release
condition on demonstration.

These decisions were approved on 2026-10-15 and recorded only in the plan; they are decided
here.

1. **One interim proof of the current archive**, before any migration: a restore of
   `platform-db` from its in-tree archive, with isolated capacity and read-only access to the
   source. Without either, the proof is recorded BLOCKED, never skipped.
2. **Read-only restore access.** The restore key comes from a separate Hetzner project, with a
   bucket policy granting only read actions on the source prefixes, and a test that write and
   delete are denied. Evidence is written with a separate key to a separate bucket.
3. **A capacity gate** covering restored size, WAL replay, temporary files, PVC and
   storage-class limits, peak recovery CPU and memory, and headroom for the running
   platform. A skipped drill leaves durable evidence and an alert, never a green result.
4. **Monthly drills** in development, scheduled from GitOps in UTC, one at a time (a
   `concurrencyPolicy` of `Forbid`, a Lease and an idempotent run id). CI tests and releases
   the drill runner; it does not run drills.
5. **Validation** checks databases and owners, and versioned application assertions relative
   to the backup's recovery point. Row counts are a sanity check only.
6. **Cleanup** never removes an active restore: it requires an absent owner, an expired
   Lease, a timeout and a grace period, and verifies volumes by their reclaim policy.
7. **Alerts** distinguish failed, skipped and overdue (35 days). A skip does not reset the
   last-success clock. They are platform rules in the in-box evaluator (ADR-078 addendum 5).
8. **Rights.** GitOps owns the drill's durable configuration; the runner holds namespaced
   rights over drill resources only. A drill of a hub database is scheduled from outside the
   hub's failure domain.

Under ADR-112, every application cluster is drilled from its own archive.


## Abandoning a reconstruction (ADR-111 §3, Rev5 §5)

Operational procedure, recorded here rather than in the ADR because it is a practice and not a
decision. It exists because restored Cluster API objects keep their finalizers deliberately:
removing them would let a `Cluster` be deleted without its servers being released, so the cost
of that choice is that an abandoned restore cannot simply be deleted.

**The distinction that governs every step: deleting the reconstructed Kubernetes records is not
the same operation as destroying the Hetzner infrastructure they describe.** The servers in a
reconstruction are usually the ones still serving traffic.

1. **Decide the infrastructure's disposition first, in the provider**, not in Kubernetes. List
   the servers (`soloz spoke match-inventory` reports them) and establish which are live and
   which are genuinely orphaned. Nothing below is safe until this is known.
2. **Do not unpause a deleting object to clear its finalizers.** Resuming the controller is what
   makes it act, and for a `Cluster` or `Machine` being deleted, acting means deleting
   infrastructure. This is the step that turns an abandoned recovery into an outage.
3. **Remove the finalizer by hand, per object, once the disposition is known.** An explicit,
   exceptional edit — never a blanket patch across the namespace, and never scripted as routine
   cleanup.
4. **Keep the escrow and the backups untouched.** Abandoning a reconstruction discards the
   reconstructed records; it must not discard the material a later attempt needs. Class E stays,
   the Class M archive stays, legacy escrow copies stay.
5. **Record what was abandoned and why**, so the next attempt begins from the same facts rather
   than rediscovering them.

## Archive identity before ADR-112's application clusters (2026-11-03)

Not a prefix move. The hub's archive is already derived per box
(`providers/<class>/database/templated-fields.yaml`): `s3://hub-db-backups/<clusterName>/`
with `serverName: <clusterName>-v1`. Nothing needs relocating, and an earlier version of
this section wrongly described moving from a literal `hub/` prefix.

**Resolved 2026-11-03, when the application clusters were implemented.** Each cluster now
carries its own manifest, so the second dimension is its own `serverName`:

```
s3://hub-db-backups/<box>/infisical-db-v1/     zitadel-db-v1/
                          grafana-db-v1/       credits-db-v1/
```

`destinationPath` stays per box and `serverName` is per database, which is barman's own
layout -- so four clusters write four prefixes and none archives into another's WALs. The
collision described here cannot occur.

What is still open:

1. **The incarnation suffix is a literal `v1` in each cluster's manifest.** The first
   recreation is covered: `<application>/database-recovery/` reads `<cluster>-v1` and writes
   `<cluster>-v2`. A SECOND recreation needs `v3` and has nowhere to come from but a hand
   edit, so the per-database dimension still does not carry the incarnation.
2. **No live read has been taken.** This item previously concerned `platform-db`, which no
   longer exists; the four clusters do not exist on a live box yet either. The read becomes
   meaningful the first time one of them archives, and it is the check that confirms the
   layout above is what barman actually wrote.

The database migration this section was written to gate has already happened, on different
grounds: ADR-112 §5 records that the shared cluster was deleted rather than drained, because
the boxes held nothing worth keeping (user, 2026-11-03). The restore-proof gate therefore
never applied to it. It still applies to any box that holds data, which is why phases 7-8
gate prod.
