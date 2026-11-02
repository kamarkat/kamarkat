# Plan: Kubernetes / CNPG upgrade track, then database restore drills (ADR-111 Revision 5)

**Owner:** Platform team (lead: Claude; execution: team member; approvals: user)
**Started:** 2026-10-15 · **Last updated:** 2026-10-20
**Status:** Phase 1 research complete. **Waiting on the user's decision D-TARGET** before the upgrade plan is written.

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
2. One-off **interim restore proof** of the CURRENT built-in archive, before migrating:
   only with isolated capacity and read-only source access; otherwise recorded BLOCKED.
3. Read-only restore key from a **separate Hetzner project**, bucket policy granting only
   read actions on the two source prefixes; **test that write/delete are denied**. Separate
   evidence-writer key and evidence bucket.
4. Capacity gate covers restored size, WAL replay, temp files, PVC/storage-class limits,
   peak recovery CPU/memory and production headroom. A skip = durable evidence + alert,
   never green.
5. Monthly dev drills; GitOps `CronJob` (UTC), `concurrencyPolicy: Forbid` + Lease +
   idempotent run id; CI only tests/releases the runner.
6. Validation: database/owner checks + versioned app assertions **relative to the backup's
   recovery point**; row counts are sanity only.
7. Janitor never deletes an active restore: absent owner + expired Lease + timeout + grace;
   verify PVC/PV per reclaim policy.
8. Alerts in the in-box evaluator (ADR-078 add.5 replaced Grafana Cloud); failed / skipped / overdue (35 days) are distinct; a skip does
   not reset the last-success clock.
9. GitOps owns durable config; the runner has namespaced rights over drill resources only;
   a hub-database drill's scheduler must be **outside the hub's failure domain**.

## 3. Open decision for the user

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
| 5 | **Interim restore proof** (decision 2): one manual restore of the hub archive on `flatcar-hub-node-1`, user-run from a runbook, read-only key | Not started | depends on 4 |
| 6 | Execute the upgrade, step by step | Not started | each step's checks; user-run |
| 7 | Built-in Barman → Barman Cloud plugin migration (own ADR) | Not started | after 6 reaches a CNPG version with the plugin path validated |
| 8 | **ADR-111 Revision 5 §6** restore drills (plugin-based) + implementation: namespace/quota/netpol/RBAC, CronJob + Lease, runner image via CI, fail-closed preflight `111-restore-drill.sh`, evidence store, janitor, in-box alerts (ADR-078 add.5) | Not started | after 7 |

## 7. Immediate next step

1. User answers **D-TARGET**.
2. Lead re-confirms the three "re-confirm" rows in §4 (ArgoCD, cert-manager, Postgres image)
   at their tags, then writes the upgrade plan (phase 3) and the access design (phase 4)
   for approval.

## 8. Artefacts

- Team-member reports (may be git-ignored; this plan carries their verified content):
  `scratch-pad/cnpg-restore-drill-phase1-report.md`, `scratch-pad/k8s-cnpg-upgrade-phase1-report.md`.
- Memory note: `db-restore-drill-and-upgrade-decisions`.
- Existing recovery runbooks to reuse (not to run for drills): `docs/runbooks/platform-db-restore.md`,
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

