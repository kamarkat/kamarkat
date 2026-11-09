# ADR-072: Tenant-Controlled Day-0 and Declarative Cluster Lifecycle

**Date:** 2026-09-10

**Status:** Proposed

## Context

ADR-040 divides a cluster's life at Day-0: a CLI-executed sequence run exactly once establishes trust, identity and configuration, after which controllers reconcile continuously. It says what Day-0 does and says nothing about where it runs.

In practice it has run on an operator's laptop, from a checkout of `zero-ops`, with the platform's own credentials. Three things follow from that, and none of them are what the surrounding decisions claim.

**The CLI has been the source of truth for a cluster's configuration.** It renders the seed Application itself, computing values as Helm parameters at Day-0. ADR-062 says the tenant's repository holds those values and that the platform never writes them. Both cannot be true, and today the repository is the one that is not consulted.

**A promotion would change a file nothing reads.** ADR-064 opens a pull request against the tenant's repository changing the pinned version. A cluster whose seed carries CLI-computed values does not read that file, so merging the proposal would change nothing and report success.

**The credentials are the platform's.** ADR-065 states that the platform operates no tenant infrastructure and holds no cloud, cluster or secret credential. An operator running the bootstrap holds all three for the duration.

The scaffolding half is now real: a tenant repository exists, holds a hydrated cluster instance pinning a published bundle version, and carries the configuration that instance runs on. What is missing is that anything uses it.

### What a comparable platform does

kubefirst splits this at the first cluster. Its API shells out to `terraform init/apply` for the management cluster, with one retry after a ten-second sleep and no phases — the bootstrap has nowhere else to run, and the shape of the retry says how far that was taken.

Every cluster after the first goes through Atlantis, running inside the management cluster: the customer edits Terraform in its own repository, opens a pull request, Atlantis plans, and applies on merge. A pull request is the trigger and a controller in the customer's own cluster does the work.

That is the right pattern where Terraform needs an apply step someone must authorise. It is not the right pattern here, and the reason is that this platform already has the thing Atlantis provides.

## Decision

**Day-0 executes under tenant control and produces tenant-owned desired state. Everything after Day-0 is a declaration in the tenant's repository, reconciled inside the tenant's own box.**

### Day-0 needs an external execution context, and that is all it needs

The first cluster cannot be reconciled into existence, because nothing is running that could reconcile it. Day-0 therefore executes somewhere outside the box it creates, and the CLI stays the thing that executes it — ADR-040's phase boundary is unchanged, and ADR-063's standalone binary is what makes running it anywhere possible.

What changes is where "outside" is. It is the tenant's execution context, under the tenant's credentials, writing to the tenant's repository. Not an operator's machine holding the tenant's cloud token.

**GitHub Actions is the implementation and must not become the architecture.** It supplies what this needs — modular jobs, repository-scoped secrets, protected environments, reruns, and reusable workflows maintained centrally and called by thin workflows in the tenant's repository, pinned to an immutable release. Those are properties, and another runner supplying them is equally admissible. A tenant on a self-hosted runner, or on a different CI system entirely, is running the same architecture.

The six-hour cap that makes modularity necessary belongs to GitHub-hosted runners specifically, not to GitHub Actions or to CI in general. Modular phases are worth having regardless: bootstrap already has phase state and resumption because a multi-hour sequence that must restart from nothing on a network fault is one nobody completes.

### Day-0 may write instance data; after Day-0 the repository is the only authority

Day-0 produces facts that do not exist until it runs — Infisical coordinates, the control-plane address, trust material. ADR-045 makes these generated artifacts and ADR-062 makes them the tenant's instance data, so Day-0 commits them to the tenant's repository.

That is the one write the platform's tooling makes outside a pull request, and it is bounded: it happens during the bootstrap the tenant itself invoked, to a repository that has no cluster yet, before anything is running to be affected by it.

**After Day-0, the tenant's repository is the sole authority for desired state, and platform maintenance is proposal-only.** A cluster reads its configuration from that repository; the CLI does not supply it. This is what makes ADR-064's promotion take effect at all — the field a proposal changes is the field the cluster reads.

### A spoke is a declaration, not a workflow

Adding a cluster after the first is not a Day-0 problem, and giving it a Day-0 mechanism would be a mistake.

A spoke is declared in the tenant's repository. ArgoCD reconciles the declaration, Crossplane and Cluster API provision the cluster, and the controllers that already run continuously do the work. Nothing is triggered, nothing is applied by an external runner, and a spoke that drifts is corrected by the same loop that created it.

**The Atlantis pattern is rejected here, and is correct there (ADR-071).** Atlantis exists because Terraform needs an apply that someone authorises, and a controller that watches pull requests is how that authority is expressed. This platform's provisioning is already Kubernetes-native and already reconciled: the thing that keeps a spoke in its desired state is the thing that creates it. Introducing a pull-request-driven applier would add a second, imperative provisioning path for a lifecycle that has a declarative one, and the two would then disagree about what a cluster is.

Running spokes through CI workflows is rejected for the same reason and more sharply. A workflow provisions once and then stops caring; a controller does not.

### The seed is the tenant's declaration

A tenant's cluster is bootstrapped by applying the Application its own repository declares, rather than one the CLI renders. The chart, the version and the values all come from the repository, which is what makes the repository authoritative rather than merely present.

This settles a disagreement that existed silently: the CLI's rendered seed and the scaffolded `bundle.yaml` install the same bundle and disagree about who owns the values. The repository owns them.

## Amendment 2026-11-09: a workload cluster's Applications, and what its own directory holds

ADR-045's amendment of the same date places the Applications a workload cluster
contributes — `<cluster>-infrastructure` and `<cluster>-generated` — in the MANAGEMENT
cluster's directory, since that is the only directory anything applies. This ADR owns
the question of what the cluster's own directory is for, and the answer narrows:

| `registry/clusters/<cluster>/` holds | Read by |
|---|---|
| `infrastructure/spokepool.yaml` | the `<cluster>-infrastructure` Application, in the management cluster's directory |
| `generated/` | the `<cluster>-generated` Application, likewise; `.gitkeep` until Day-0 or `app hosts aggregate` writes |
| `values.yaml` | the CLI and people — a RECORD of what the cluster was declared as |

`values.yaml` is not rendered by anything. The chart reads the management cluster's
values, applications declare themselves under `environments/<env>/<app>/` (ADR-082,
amendment 2026-11-07), and what the cluster actually runs is its claim. It is kept
because a cluster that is only a claim cannot say who owns it, which environment it
serves, or what it was sized as — and `add-cluster` holds all three at declaration
time. The per-value notes in the template say where each control is, so a reader does
not edit a record expecting an effect.

Two fields are withdrawn from it: `workloads` (BYOWR, read until 2026-09-05 and
withdrawn by ADR-073) and, from a MANAGEMENT cluster's values, `environmentSlug` — the
hub declares no workload environment (ADR-111 Revision 7), and under the
environment-driven deployment rule it would additionally have read as a claim on that
environment's applications.

## Ownership

This ADR owns the location of Day-0 execution and the mechanism of post-Day-0 cluster lifecycle. It does not change ADR-040's phase boundary, ADR-045's artifact registry, or ADR-062's repository separation; it makes each of them true in practice. For resource ownership, see ADR-039.

## Consequences

### Positive

ADR-065's central claim becomes testable. The platform holding no cloud, cluster or secret credential is verifiable when the bootstrap runs under the tenant's own credentials, and was not while an operator held all three.

A promotion changes something. The field ADR-064's proposal edits is the field the cluster reads, so a merged proposal reaches infrastructure and a declined one does not — which is the whole of the mechanism.

One provisioning path, not two. A spoke is created and kept by the same controllers, so there is no second mechanism to keep in agreement with the first.

The bootstrap becomes re-runnable by someone other than its author. A sequence that runs in a defined execution context, from a binary that carries what it needs, is one a tenant can run without the platform present.

### Negative

Cloud credentials move into the tenant's CI configuration, which is a place they can be misconfigured. The platform no longer holds them, and equally no longer sees when they are wrong.

A tenant that cannot run the execution context cannot bootstrap. The arrangement assumes a tenant with a git organisation and CI available to it, which the customer this platform is built for has, and which is nonetheless now a prerequisite rather than a convenience.

Day-0 failures move somewhere the platform cannot read directly. A failed bootstrap is a workflow run in the tenant's organisation, and diagnosing it depends on what the tenant reports or on ADR-067's telemetry rather than on a terminal the platform is watching.

The platform's own development path diverges from the tenant path unless it uses the same one. A laptop bootstrap kept for convenience would be a second Day-0 mechanism, and the one the platform exercises daily would not be the one tenants run.

## Impact

The seed the CLI renders is replaced by the Application the tenant's repository declares, which changes where a cluster reads its values from.

Day-0 gains a step: committing generated artifacts to the tenant's repository rather than to `zero-ops`.

The bootstrap's phases become individually invocable, so an execution context can run them as separate units and resume a partial run.

Nothing changes for a spoke. Its lifecycle is already declarative, and this ADR records that it stays that way.

## References

- ADR-039: Platform Ownership Model
- ADR-040: Day-0 vs Day-1 Lifecycle Boundary
- ADR-045: Bootstrap-Generated GitOps Artifacts
- ADR-062: Onboarding and Scaffolding a Tenant
- ADR-063: The Platform Bundle and Its Version
- ADR-064: Bundle Promotion and Cell-Scoped Policy
- ADR-065: The Control Plane Ships Into the Box
- ADR-067: Support Telemetry and the Basis of Maintenance
- ADR-071: How This Platform Differs From kubefirst
