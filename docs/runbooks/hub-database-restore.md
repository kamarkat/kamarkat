# Restoring a hub database from its archive

Use this when one of the hub's databases is gone and the object-store archive is
the only copy left: its node was re-provisioned, its `local-path` volume went with
it, or the Cluster was deleted.

**One cluster at a time.** Each platform application owns its own (ADR-112), so a
restore is scoped to the application whose data was lost and the others keep
running. Throughout, `<cluster>` is that cluster's name and `<application>` is the
directory it lives in:

| Application | `<cluster>` | `<application>` |
|---|---|---|
| Infisical (the PKI root and every platform secret) | `infisical-db` | `infisical` |
| Zitadel | `zitadel-db` | `identity/zitadel` |
| Grafana | `grafana-db` | `grafana` |
| Credit Authority | `credits-db` | `credit-authority` |

Restoring `infisical-db` is the one case that is not a single application's
problem: ESO resolves every platform secret out of Infisical, so until it is back
nothing that needs a credential reconciles.

**This bounds data loss; it does not eliminate it.** Recovery restores to the last
archived WAL. Anything written after that is gone. On a dev box the clusters run
`instances: 1`, so there is no replica to fail over to — that is a topology
decision (ADR-070), not something this procedure can recover.

Do not run this to "refresh" a working database. `bootstrap` is read once, at
Cluster creation, so this procedure necessarily destroys the Cluster object first.

## Getting access

Every command below needs cluster access. Authenticate as yourself through the
box's identity provider (ADR-076); the admin kubeconfig is break-glass and lives in
the escrow, not on anyone's laptop.

If the cluster is unreachable — which is one of the cases this runbook is for —
retrieve the escrowed kubeconfig:

```
export INFISICAL_ESCROW_URL=https://app.infisical.com
export INFISICAL_ESCROW_PROJECT_ID=…
export INFISICAL_ESCROW_CLIENT_ID=…
export INFISICAL_ESCROW_CLIENT_SECRET=…

kakt kubeconfig --cluster <name> --out ./break-glass.kubeconfig
export KUBECONFIG=./break-glass.kubeconfig
```

That credential is cluster-admin and is not tied to a person. Use it, then remove
it — and prefer the identity provider wherever it is working.

## Before you start

1. **Confirm the data is actually gone.** A Pending pod is not a lost volume. If
   the PVC still exists and the node is coming back, wait — restoring loses every
   unarchived write for nothing.

   ```
   kubectl -n platform-data get cluster <cluster> -o wide
   kubectl -n platform-data get pvc -l cnpg.io/cluster=<cluster>
   ```

2. **Confirm the archive is readable and find a backup to restore to.** Recovery
   by explicit `backupID` is well defined; a bare "latest" over this prefix is not,
   because the archive holds WALs from more than one incarnation of the cluster
   (see the note in `manifests/hub-core-services/<application>/database/<cluster>.yaml`).

   ```
   kubectl -n platform-data get backup -l cnpg.io/cluster=<cluster> \
     -o custom-columns=\
NAME:.metadata.name,PHASE:.status.phase,ID:.status.backupId,STOPPED:.status.stoppedAt
   ```

   If no Backup object survives, list the prefix directly with the S3 credentials
   in the `s3-credentials` Secret. If the archive is empty, stop: there is nothing
   to restore and `initdb` is the only path forward.

3. **Check which prefix the lost cluster was writing to.**

   ```
   kubectl -n platform-data get cluster <cluster> \
     -o jsonpath='{.spec.backup.barmanObjectStore.serverName}'
   ```

   That value is what `externalClusters[0].barmanObjectStore.serverName` in
   `manifests/hub-core-services/<application>/database-recovery/kustomization.yaml`
   must name.
   The `backup` serverName in the same file must be a **different, unused** prefix.
   If they match, the replacement archives into the WALs it restored from and
   `barman-cloud-check-wal-archive` blocks every backup afterwards.

## Restoring

1. Point that application's database Application at the recovery overlay. Its
   descriptor (`manifests/argocd/components/02/platform-<application>-db.yaml`)
   carries `path: manifests/hub-core-services/<application>/database`; the restore
   is that path changed to `.../database-recovery`, and nothing else. Release the
   bundle and let ArgoCD sync — the transition is a declaration, which is why the
   overlay is a sibling directory rather than a flag (ADR-092).

   The other three Applications are untouched. This is the whole point of one
   cluster per application: the window in which the repository holds a
   data-destroying manifest is scoped to the one that lost its data.

2. Delete the Cluster object. Nothing else recreates it, and `bootstrap` is only
   read at creation.

   ```
   kubectl -n platform-data delete cluster <cluster>
   ```

3. Watch the replacement come up. CNPG creates a recovery Job before any instance
   pod; that Job is where a bad `backupID` or an unreadable prefix surfaces.

   ```
   kubectl -n platform-data get pods -w
   kubectl -n platform-data logs -l cnpg.io/jobRole=full-recovery -f
   ```

4. **Verify the data is actually there** before declaring success. A recovered
   cluster reports Healthy whether or not it holds what you expected.

   ```
   kubectl -n platform-data exec -it <cluster>-1 -- psql -U postgres -c '\l'
   ```

   One database, the application's own: `infisical`, `zitadel`, `grafana` or
   `credits`. A cluster holding anything else is not the one you meant to restore.

5. **Confirm archiving restarted on the new prefix.** If it has not, the guard is
   refusing the prefix — which means the two serverNames were not distinct.

   ```
   kubectl -n platform-data get cluster <cluster> \
     -o jsonpath='{.status.conditions[?(@.type=="ContinuousArchiving")]}'
   ```

## Afterwards

Revert the descriptor's `path` to `.../database` and **fold the new serverName into
the base manifest**. The recovery overlay is written for the next recovery, and
leaving `<cluster>-v3` only in the overlay means the base still names the prefix
the cluster no longer writes to.

Leaving the descriptor pointed at `database-recovery` is the state ADR-092 exists
to close: a rebuild in that window restores the old incarnation and discards
everything written since. `scripts/validate/preflight/092-recovery-is-exceptional.sh`
fails on a steady-state manifest declaring `spec.bootstrap.recovery`, but it cannot
tell you that a descriptor is still pointed at the overlay — that part is yours.

Do not bump the base serverName at any other time. `check-wal-archive` runs only
before a cluster's first archive, so renaming on a running cluster re-arms the
guard against a prefix that may not be empty and stops archiving — trading a
working backup for a broken one.

## Related

- `manifests/hub-core-services/<application>/database-recovery/` — the overlay this uses
- ADR-112 — one cluster per platform application, and why a restore is scoped to one
- ADR-046 §11 — placement classes and why `local-path` has no snapshots
- ADR-070 — why single-instance is a topology decision, and what HA would require
- ADR-075 — on-prem nodes as a capability, and why moving placement is a restore
