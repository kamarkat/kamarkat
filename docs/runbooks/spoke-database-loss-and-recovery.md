# Recovering a spoke's database after losing its node

A hybrid spoke keeps `shared-cnpg` on `local-path`, which is node-pinned: the
volume lives on one home worker and dies with it. The only restore path is the
barman archive in Hetzner Object Storage.

This runbook is the 2026-09-25 incident written down. Recovery took four release
cycles, and three of those were spent on things this document now tells you
outright.

**Read [Before you start](#before-you-start) first.** Deleting the wrong thing in
the wrong order is how this incident lost thirteen hours it did not need to lose.

---

## What this looks like

The failure is silent at every layer until you go looking.

```
C: on the Hyper-V host fills to 0 bytes
  ↓
Hyper-V pauses, then powers off, every VM on that host
  ↓
kubelet freezes with the VM → nodes NotReady, minutes apart
  ↓  (no disk-pressure event: kubelet watches the GUEST disk, which is 2/3 empty,
  ↓   while the HOST underneath it has nothing left)
workloads reschedule, single-replica services do not
  ↓
`no healthy upstream` on the public URL
```

If the recovery then deletes the VM disks — `free-spoke.sh` did, in September
2026, because `Get-VHD .Attached` is FALSE for a VM that is merely Off — the
`local-path` volume goes with them and the database is gone.

Symptoms, in the order you meet them:

| you see | it means |
|---|---|
| `no healthy upstream` at the tenant URL | a single-replica service is stranded on a dead node |
| nodes `NotReady`, `Kubelet stopped posting node status` | the VM is off, not the node broken |
| CNPG pod `CrashLoopBackOff`, `pg_controldata: exit status 1` | PGDATA is empty — the volume is gone but the PVC remains |
| `Cluster is unrecoverable ... restore from a recent backup` | CNPG has classified it; it will not self-heal |
| SDK `connect EPERM <ip>:5432` | not a policy denial — Cilium socket LB reporting **no backend** |
| SDK `FATAL 28P01 password authentication failed` | the database came back EMPTY; see [the role trap](#the-role-trap) |

---

## Before you start

**Check the archive before destroying anything.** Everything below assumes a
usable backup exists. Confirm it, do not assume it.

```bash
export S=<spoke kubeconfig>
AK=$(kubectl --kubeconfig $S -n platform-data get secret s3-credentials -o jsonpath='{.data.access-key-id}' | base64 -d)
SK=$(kubectl --kubeconfig $S -n platform-data get secret s3-credentials -o jsonpath='{.data.secret-access-key}' | base64 -d)

kubectl --kubeconfig $S -n platform-data exec shared-cnpg-1 -c postgres -- env \
  AWS_ACCESS_KEY_ID="$AK" AWS_SECRET_ACCESS_KEY="$SK" AWS_REGION=hel1 \
  barman-cloud-backup-list --cloud-provider aws-s3 \
  --endpoint-url https://hel1.your-objectstorage.com \
  s3://spoke-pool-backups/<spoke>/ shared-cnpg-v<N>
```

`<N>` is the CURRENT `spec.backup.barmanObjectStore.serverName`. If this lists
nothing, stop — there is nothing to recover and the rest of this runbook does not
apply.

**Then find the real recovery point.** The newest base backup is the FLOOR, not
the answer. WAL replay carries you forward from it:

```bash
kubectl --kubeconfig $S -n platform-data exec shared-cnpg-1 -c postgres -- env \
  AWS_ACCESS_KEY_ID="$AK" AWS_SECRET_ACCESS_KEY="$SK" AWS_REGION=hel1 python3 -c "
import boto3
s3=boto3.client('s3',endpoint_url='https://hel1.your-objectstorage.com',region_name='hel1')
last=None
for page in s3.get_paginator('list_objects_v2').paginate(
        Bucket='spoke-pool-backups', Prefix='<spoke>/shared-cnpg-v<N>/wals/'):
    for o in page.get('Contents',[]):
        if last is None or o['LastModified']>last[1]: last=(o['Key'],o['LastModified'])
print('latest WAL:', last)"
```

On 2026-09-25 the newest base backup was 01:00 UTC and the newest archived WAL
was 13:55:40 — the node died at 13:53:53. The real loss was about two minutes,
not thirteen hours. **Do not decide the data is not worth recovering from the
backup timestamp alone.**

**A backup that exists is a reason to slow down.** The empty-rebuild path below is
faster to start and lands you in [the role trap](#the-role-trap). Recovery is
almost always the better choice when an archive is there.

---

## The role trap

Rebuilding empty rather than restoring produces a database that looks fine and
that nothing can log into.

The workload's credentials name `tenant-<appId>-user`. That role exists in the
ARCHIVE because it predates ADR-088. A fresh `initdb` cluster has only the roles
the `tenant-db` composition creates, so the SDK connects and is refused:

```
FATAL 28P01: password authentication failed for user "tenant-waypoint-user"
```

which reads like a rotated password and is not one. It also blocks the CNPG
`Database` CR:

```
while creating database "tenant-waypoint-db":
  ERROR: role "tenant-waypoint-user" does not exist (SQLSTATE 42704)
```

A successful restore brings the role back with the data and the problem does not
arise. This is the strongest practical argument for recovering over rebuilding.

---

## Restoring

CloudNativePG has **no in-place recovery**. Restoring a physical backup IS the
bootstrap of a new cluster, and `spec.bootstrap` is read exactly once, at
creation, then ignored. So this is a manifest change followed by a
delete/recreate — never a command against the running cluster, and never an edit
to a Cluster that already exists.

### 1. Transition the manifest

```bash
cd zero-ops
make cnpg-bump-incarnation PROVIDER=hybrid          # add DRY_RUN=1 to preview
```

This moves two fields that must agree and must never be equal:

```
bootstrap.recovery.source              →  the PREVIOUS incarnation (read-only)
backup.barmanObjectStore.serverName    →  the NEXT incarnation
```

The bump exists because barman refuses to write into a `serverName` that already
holds another cluster's WALs (`Expected empty archive`). Leave it and the new
cluster archives NOTHING while reporting healthy.

The script refuses to write anything unless the manifest is in exactly the shape
it expects. If it complains the provider is not settled, run
`make cnpg-settle-incarnation PROVIDER=<p>` first — it is idempotent and also
seeds `externalClusters` onto a provider that has never recovered.

> **Do not hand-edit these fields.** The repo carried a comment reading "BUMP THE
> SUFFIX whenever the cluster is recreated" for a year. It was honoured once and
> missed on the next rebuild, which is how the archive ended up unwritable.

### 2. Ship it

Build and promote a release, then bump the spoke's `environmentRevision` and
`targetRevision` in the gitops repo. **Verify the published chart, not the repo:**

```bash
helm pull oci://ghcr.io/kamarkat/charts/platform --version <ver> --untar
grep -n destinationPath platform/charts/platform-spoke-catalog/templates/hybrid-templated.yaml
```

**Both** lines must carry `{{ .Values.spokeName }}`. If only the first does, the
per-spoke prefix is missing from the recovery source — see
[externalClusters](#externalclusters-is-a-second-copy).

### 3. Sync, and pass ServerSideApply explicitly

```bash
kubectl -n platform-ops patch application platform-spoke-catalog-<spoke> \
  --type merge -p '{"operation":{"initiatedBy":{"username":"<you>"},
  "sync":{"revision":"<ver>","prune":true,
  "syncOptions":["ServerSideApply=true","CreateNamespace=true","RespectIgnoreDifferences=true"]}}}'
```

**A hand-triggered `operation` does NOT inherit `spec.syncPolicy.syncOptions`.**
Omit `ServerSideApply=true` and ArgoCD falls back to client-side apply, which
fails on the large CRDs:

```
CustomResourceDefinition "clusters.postgresql.cnpg.io" is invalid:
  metadata.annotations: Too long: must have at most 262144 bytes
```

Never pass `syncStrategy.apply.force` here — it forces client-side apply and
causes the same failure.

Confirm the live Cluster before going further:

```bash
kubectl --kubeconfig $S -n platform-data get cluster shared-cnpg \
  -o jsonpath='{.spec.bootstrap}{"\n"}{range .spec.externalClusters[*]}{.barmanObjectStore.destinationPath}{.barmanObjectStore.serverName}{"\n"}{end}'
```

The path must be `s3://spoke-pool-backups/<spoke>/shared-cnpg-v<N>/`. If the
`<spoke>/` segment is missing, the restore will fail and tell you nothing useful.

### 4. Delete the Cluster

Nothing restores until it is recreated.

```bash
kubectl --kubeconfig $S -n platform-data delete cluster shared-cnpg
kubectl --kubeconfig $S -n platform-data delete pvc shared-cnpg-1   # if it survived
```

ArgoCD `selfHeal` recreates it within ~3 minutes, this time with
`bootstrap.recovery`. A pod named `shared-cnpg-1-full-recovery-*` appears: that is
the restore.

### 5. Verify the restore, not just the pod

```bash
kubectl --kubeconfig $S -n platform-data exec shared-cnpg-1 -c postgres -- \
  psql -U postgres -tAc "select rolname from pg_roles where rolname like 'tenant%'"
kubectl --kubeconfig $S -n platform-data exec shared-cnpg-1 -c postgres -- \
  psql -U postgres -d <tenant-db> -tAc "select max(created_at) from workflow_runs"
```

Both pre- and post-ADR-088 roles should be present, and the newest row should sit
within minutes of the outage. A healthy pod proves nothing: an empty cluster is
also healthy.

### 6. Settle — not optional

Once the cluster is healthy **and** `ContinuousArchiving=True`:

```bash
make cnpg-settle-incarnation PROVIDER=hybrid
```

then ship it. Left in recovery, the NEXT rebuild restores the SAME old
incarnation and silently discards everything written since this restore. That
failure leaves no trace until someone goes looking for missing data.

`make cnpg-incarnation-check` reports any provider left in this state. It is
deliberately not a merge gate — the release that performs a recovery is
mid-recovery by construction.

---

## externalClusters is a second copy

The thing that cost two releases.

`spec.externalClusters[].barmanObjectStore` is a **complete, independent copy** of
the object-store configuration. It inherits nothing from
`spec.backup.barmanObjectStore`. They describe the same bucket and look
identical:

| block | role |
|---|---|
| `spec.backup.barmanObjectStore` | where the cluster **writes** |
| `externalClusters[0].barmanObjectStore` | where recovery **reads** |

The per-spoke prefix is applied at PACKAGING time by
`scripts/package/templated-fields.py`, driven by
`manifests/spoke/spoke-catalog/templated-fields.yaml` — **not** by the
ApplicationSet's `kustomize.patches`, which the generated Application does not
even carry. Both paths must be declared there. When only the backup path was,
recovery read the bucket root and failed with:

```
Recovering from external cluster ... sourceName=shared-cnpg-v3
Error while restoring a backup: no target backup found
```

which names the source it could not find and says nothing about the path it
searched — so it reads as a missing BACKUP rather than a wrong PREFIX. The
archive was intact and listable the entire time.

`externalClusters` is therefore declared in EVERY state, including the steady one
where `bootstrap` is `initdb` and nothing reads it: the templating assigns a
concrete path, so an entry that existed only during a recovery would fail
packaging for every spoke in between.

---

## Fixing the host, so it does not recur

The database loss is downstream of a full `C:` on the Hyper-V host.

```bash
./scripts/hybrid/free-spoke.sh
```

Guests are trimmed and their images pruned, then each VM is stopped, its VHDX
compacted, and restarted. `fstrim` alone reclaims NOTHING on the host: a
dynamically expanding VHDX never shrinks without `Optimize-VHD`, so the file
stays at its high-water mark forever.

The script reports, without touching, anything whose removal is a decision:
`hiberfil.sys`, `Windows.old`, shadow copies, checkpoints, and every `.vhdx` /
`.avhdx` over 1 GB anywhere on `C:`. A forgotten checkpoint redirects all writes
to a differencing `.avhdx` that grows without bound while its parent still looks
small — the most common cause of a host filling with no obvious culprit.

**Never run a disk sweep against a host whose VMs are merely Off without checking
what it deletes.** That is how the spoke VHDXes were destroyed. The current guard
is the VM CONFIGURATION, not the file handle: any path `Get-VMHardDiskDrive`
reports, for any VM in any state, is untouchable.

---

## Why none of this paged

Every condition above is compatible with `Cluster in healthy state`. The operator
is not wrong — Postgres is up and serving — it simply has no opinion about
whether the box is recoverable.

`spoke-catalog/infra/victoriametrics/cnpg-backup-alerts.yaml` now fires on
unarchived WAL backlog, a cluster that has never completed a backup, a stale
newest backup, failures newer than the last success, and an archive with no
recoverability point. `CNPGWalArchiveBacklog` is the one that would have caught
2026-09-25 within fifteen minutes.

Host free space is not covered by those: nothing inside the cluster can see the
Hyper-V host. `free-spoke.sh` exits non-zero below 20 GB free, which is only as
reliable as remembering to run it.

---

## Related

- ADR-014 — the backup contract, the recovery point definition, and the
  incarnation rule
- ADR-046 §11 — hybrid placement and the `local-path` consequence
- `docs/runbooks/hub-database-restore.md` — the hub's own database
- `docs/runbooks/backup-credential-chain-recovery.md` — when the S3 credential,
  not the data, is the problem
