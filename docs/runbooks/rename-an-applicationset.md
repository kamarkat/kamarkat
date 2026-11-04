# Rename an ApplicationSet

An ApplicationSet owns the Applications it generates. Rename it and ArgoCD
applies a new one, prunes the old one, and the old one's deletion garbage-collects
its Applications through their `ownerReferences`. The child names do not change,
so what happens on the box is a delete and a recreate of the same names.

For boundary 05 that is the AINativeSaaS XR, which owns a tenant application's
database. Deleting it destroys the database.

This runbook is the sequence that avoids that. It applies to any ApplicationSet
rename, and it was written for the one that renamed
`tenant-fleet-{xr,spoke,workload}-provisioning` to `tenant-app-*`.

## Why it is not just a sed

`manifests/argocd/environment-manager/templates/05-tenant-app-appset.yaml` sets

```yaml
  syncPolicy:
    preserveResourcesOnDeletion: false
```

which is correct steady-state behaviour: an Application whose declaration is
gone should take what it owns with it. It is exactly wrong for one sync, because
during a rename the declaration is not gone — it moved.

Nothing detects this. The rename renders cleanly, the new ApplicationSet is
valid, and the generated Applications come back within a pass. What does not
come back is a restored database.

## Sequence

Run on the management cluster, with its kubeconfig.

1. **Record what exists**, so step 5 has something to compare against.

   ```sh
   kubectl -n platform-ops get applicationsets.argoproj.io
   kubectl -n platform-ops get applications.argoproj.io \
     -o custom-columns=NAME:.metadata.name,OWNER:.metadata.ownerReferences[0].name
   kubectl -n platform-ops get ainativesaas.nutgraf.in -o name
   ```

2. **Make the outgoing ApplicationSets keep their Applications.** Patch the live
   objects — not the chart, which must keep `false` as its steady state:

   ```sh
   for a in tenant-fleet-xr-provisioning tenant-fleet-spoke-provisioning tenant-fleet-workload-provisioning; do
     kubectl -n platform-ops patch applicationsets.argoproj.io "$a" --type merge \
       -p '{"spec":{"syncPolicy":{"preserveResourcesOnDeletion":true}}}'
   done
   ```

   A patch to a live object is reverted by the next sync of the Application that
   owns it, so do step 3 promptly or with auto-sync paused on the boundary.

3. **Let the rename sync.** The new ApplicationSets appear and adopt the existing
   Applications by name; the old ones are pruned and, because of step 2, leave
   their Applications behind rather than deleting them.

4. **Confirm the adoption** before trusting it. Every Application should now name
   a `tenant-app-*` owner:

   ```sh
   kubectl -n platform-ops get applications.argoproj.io \
     -o custom-columns=NAME:.metadata.name,OWNER:.metadata.ownerReferences[0].name
   ```

   An Application with no `ownerReferences` is orphaned, not adopted: it will
   keep syncing and will never again be created, retargeted or removed by any
   ApplicationSet. Delete the stale owner reference only once a `tenant-app-*`
   one has replaced it.

5. **Compare against step 1.** Same Application names, same XR count. A missing
   XR is the failure this runbook exists to prevent, and it is recoverable only
   from that application's database archive.

6. **Put the steady state back.** The chart still declares
   `preserveResourcesOnDeletion: false`, so the next sync of the boundary restores
   it on its own. Confirm rather than assume:

   ```sh
   kubectl -n platform-ops get applicationsets.argoproj.io tenant-app-xr-provisioning \
     -o jsonpath='{.spec.syncPolicy.preserveResourcesOnDeletion}{"\n"}'
   ```

## If an XR was deleted

Stop. Do not let the new ApplicationSet recreate it over an empty database —
a recreated XR provisions a new, empty PostgreSQL cluster, and that is the state
from which there is nothing to recover.

Suspend the boundary's auto-sync, then restore that application's database from
its own archive per `docs/runbooks/spoke-database-loss-and-recovery.md`, and only
then let the XR be recreated against it.

## What does not need this

Renaming an ApplicationSet's **file** changes nothing on the box: the resource
name is what owns the Applications, not the path it was rendered from. The same
goes for a `pathParamPrefix`, a generator's internal parameter name, or a
boundary's printed label — all of which were renamed alongside the one above with
no cluster impact.
