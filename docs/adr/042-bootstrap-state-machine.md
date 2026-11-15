# ADR-042: Bootstrap State Machine

**Date:** 2026-06-08
**Status:** Accepted

## Context

Bootstrap logic is currently distributed across ADRs 003 (secret injection), 021 (Day-0 CLI choreography), 025 (phased operator lifecycle), and 035 (PKI bootstrap exception). Each ADR describes a fragment of the bootstrap sequence but no single document defines the complete state machine, the owner of each state, the criteria for state transitions, or the recovery path for failures.

Without a state machine, bootstrap failures produce ambiguous diagnostics. When the platform is stuck between `SECRETS_READY` and `GITOPS_READY`, it is unclear which component is responsible for recovery or whether manual intervention is required.

This ADR consolidates bootstrap logic into a single state machine with explicit ownership, entry/exit criteria, and failure recovery instructions.

## Decision

### State Machine

```
  NEW
    │
    ▼
  SECRET_STORE_READY
    │
    ▼
  PKI_READY
    │
    ▼
  SECRETS_READY
    │
    ▼
  GITOPS_READY
    │
    ▼
  PLATFORM_READY
```

### State Definitions

#### NEW

The Hub cluster is provisioned (KinD or CAPH) but no platform components have been installed.

- **Owner:** CLI
- **Entry Criteria:** Hub cluster exists and `kubectl` is configured.
- **Exit Criteria:** `hub-platform` and `hub-secrets` Infisical projects exist. Machine Identity with admin access to both projects exists. Infisical access token is stored in Hub cluster.
- **Failure Recovery:** Re-run CLI bootstrap from start. State is fully idempotent.

#### SECRET_STORE_READY

Infisical projects and Machine Identities are created. Bootstrap secrets are generated and stored.

- **Owner:** CLI
- **Entry Criteria:** `NEW` exit criteria satisfied.
- **Operations:**
  - Generate bootstrap secrets (`platform-db-app`, `infisical-db-credentials`, `platform-db-ca`, `infisical-secrets`, `infisical-redis-credentials`).
  - Upload bootstrap secrets to Infisical `hub-secrets` project.
  - Create bootstrap secrets as Kubernetes Secrets in the Hub cluster.
- **Exit Criteria:** All bootstrap secrets exist in Infisical and as Kubernetes Secrets. Infisical API returns 200 for all secret paths.
- **Failure Recovery:** If Infisical is unreachable, retry with exponential backoff (max 5 minutes). If secret generation fails, re-run — generation is idempotent. If Kubernetes Secret creation fails, check RBAC permissions and re-run.

#### PKI_READY

The PKI trust hierarchy is established and the Fleet Intermediate CA is hosted in Infisical.

- **Owner:** CLI
- **Entry Criteria:** `SECRET_STORE_READY` exit criteria satisfied.
- **Operations:**
  - Generate Offline Root CA (outside Infisical, outside Kubernetes).
  - Upload Fleet Intermediate CA (signed by Offline Root CA) to Infisical `hub-platform` project.
  - Create certificate profiles for infrastructure services, database clients, service mesh, and human access.
  - Store Offline Root CA public certificate as a ConfigMap in the Hub cluster.
- **Exit Criteria:** Fleet Intermediate CA exists in Infisical PKI. Certificate profiles exist. Root CA ConfigMap exists in Hub cluster.
- **Failure Recovery:** If Root CA generation fails, re-generate — no downstream state depends on this operation. If Fleet Intermediate CA upload fails, verify Infisical Machine Identity PKI permissions and retry. Root CA private key must never be uploaded to Infisical or Kubernetes.

#### SECRETS_READY

All bootstrap secrets are synchronized to Kubernetes and infrastructure components can bootstrap.

- **Owner:** CLI (initiates), then ArgoCD (completes)
- **Entry Criteria:** `PKI_READY` exit criteria satisfied. `01-platform-infra` ArgoCD boundary is applied.
- **Operations:**
  - CLI applies `01-platform-infra` boundary and waits for Healthy/Synced.
  - CLI injects Secret Zero (trust anchors, bootstrap credentials).
  - CLI applies `02-platform-data`, `03-platform-services`, and `04-tenant-services` boundaries sequentially.
  - CNPG bootstraps using `platform-db-app` bootstrap secret.
  - ESO syncs application secrets from Infisical.
  - Crossplane provider-sql creates database roles from ESO-synced credentials (per ADR-023, ADR-039, ADR-043).
- **Exit Criteria:** All four ArgoCD boundaries (`01`, `02`, `03`, `04`) report `Healthy` and `Synced`. All ExternalSecrets report `SecretSynced`. CNPG clusters are ready. Hub Operator has completed initial reconciliation.
- **Failure Recovery:** If any ArgoCD boundary fails, inspect the specific Application health in ArgoCD. If ESO ExternalSecrets fail, verify Infisical connectivity and SecretStore configuration. If CNPG fails to bootstrap, verify `platform-db-app` secret content. If Crossplane provider-sql fails to create database roles, check provider-sql controller logs.

#### GITOPS_READY

ArgoCD is autonomously reconciling all boundaries. The CLI ownership handoff to Day-1 controllers is in progress.

- **Owner:** ArgoCD (platform-infra boundary), Hub Operator (Spoke orchestration), Crossplane (infrastructure)
- **Entry Criteria:** `SECRETS_READY` exit criteria satisfied.
- **Operations:**
  - All three ArgoCD boundaries continue reconciling autonomously.
  - Hub Operator begins SpokePool reconciliation.
  - Crossplane begins infrastructure XR reconciliation.
  - cert-manager begins certificate lifecycle.
  - Atlas Operator begins schema migration reconciliation.
- **Exit Criteria:** All Day-1 controllers have completed at least one successful reconciliation loop. The Hub Operator has reconciled all existing SpokePool CRs. Crossplane XRs report `Ready: True`. cert-manager Certificate resources report `Ready: True`.
- **Failure Recovery:** Controller-specific. Each controller must emit Kubernetes Events and status conditions. Operators diagnose failures using controller logs and Crossplane/ArgoCD status conditions. See ADR-040 for ownership handoff failure rules.

#### PLATFORM_READY

All Day-1 controllers are operating normally. The platform is fully provisioned and accepting tenant workloads.

- **Owner:** All Day-1 controllers (per ADR-039 ownership matrix)
- **Entry Criteria:** `GITOPS_READY` exit criteria satisfied. All Day-1 controllers have completed at least one successful reconciliation.
- **Exit Criteria:** Terminal state. No further transitions.
- **Failure Recovery:** Not applicable — this is the operational steady state. Failures are handled by individual controller reconciliation loops.

### State Transition Governance

1. A state transition may only be initiated by the state's designated Owner.
2. A state is entered only when all Entry Criteria are satisfied.
3. A state is exited only when all Exit Criteria are satisfied.
4. If a state's Exit Criteria cannot be satisfied after exhausting the documented Failure Recovery path, the bootstrap is blocked and requires manual intervention.
5. The CLI may initiate transitions through `SECRETS_READY`. After `SECRETS_READY` is achieved, the CLI exits permanently and controllers manage all subsequent transitions.

## Consequences

### Positive

- Bootstrap failures produce unambiguous diagnostics — the current state indicates which component is responsible.
- Consolidated bootstrap logic eliminates the need to cross-reference four separate ADRs to understand the bootstrap sequence.
- Explicit ownership per state prevents "who fixes this" ambiguity during bootstrap incidents.
- Failure recovery instructions per state reduce mean time to recovery.

### Negative

- The state machine is procedural; a state transition failure blocks all subsequent states. There is no partial progression.
- If a new infrastructure dependency is added to the bootstrap, this state machine must be updated.
- The state machine documents the ideal bootstrap path; edge cases (partial state, controller crashes during transition) require operator judgment beyond the documented recovery paths.

---

## Amendment 2026-06-09: Boundary-Sequenced State Machine

**Status:** Accepted

### Context

The original `SECRETS_READY` state contained a temporal paradox discovered during local kind bootstrap runs. The state required `02-platform-data` to be applied before `init-secrets` could run (because `init-secrets` needs the CNPG Cluster CR to exist), but the monolithic `SECRETS_READY` operations block applied all three boundaries simultaneously.

This produced an unrecoverable error:
```
Error: failed to install infisical secrets: CNPG Cluster CR not found
```

The root cause: `init-secrets` was called as a separate CLI command (`hub init-secrets`) from the shell bootstrap script, executing after `hub bootstrap` had already applied all three boundaries. When boundary sync was slow (CNPG operator still starting, CR not yet created), the one-shot CNPG CR check in `InstallInfisicalSecrets` failed fast.

Additionally, the monolithic `platform-deploy` phase was a single checkpoint with no ability to resume at a sub-phase boundary. A failure in `init-secrets` (between B02 and B03) required re-running the entire `platform-deploy` phase, which was idempotent but wasteful.

A secondary issue: `ingress-nginx` controller and its `Ingress` resources were deployed in the same ArgoCD boundary (`01-platform-infra`). The ingress-nginx validating webhook rejected `Ingress` resources until the controller pod was ready, causing a sync failure in `platform-argocd` Application.

### Decision

Restructure the `SECRETS_READY` platform state into four sequential sub-states, each gated by the orchestrator using boundary-specific Helm deploy flags:

```
SECRETS_READY
    │
    ├── SECRETS_GENERATED  (PhaseBoundary01 + PhaseBoundary02)
    │
    ├── SECRET_STORE_READY    (init-secrets: encryption keys, CNPG CA, DB credentials)
    │
    ├── BOUNDARY_03_READY  (PhaseBoundary03: Secret Providers)
    │
    ├── SECRETS_SYNCED     (ESO syncs from Infisical)
    │
    └── BOUNDARY_04_READY  (PhaseBoundary04: Secret Consumers)
```

#### Boundary Deployment Order (ADR-021 §2 divergence)

ADR-021 defined the boundary list as:
1. `01-platform-infra`: Core controllers, Operators (Crossplane, CNPG, Atlas), CRDs
2. `02-platform-data`: Stateful workloads (CNPG Clusters, NATS, Redis)
3. `03-platform-services`: Stateless applications and Control Planes

This amendment adds a **boundary deployment order** that the CLI enforces during Day-0:

| Order | Phase | Helm `deploy.*` flags | Operator Action |
|-------|-------|----------------------|-----------------|
| 1 | `PhaseBoundary01` | `b01=true, b02=false, b03=false, b04=false` | Install ArgoCD, apply `01-platform-infra` ApplicationSet, wait for webhooks/CRDs/pods |
| 2 | `PhaseBoundary02` | `b01=true, b02=true, b03=false, b04=false` | Apply `02-platform-data` ApplicationSet, wait for CNPG Cluster CR |
| 3 | `PhaseInitSecrets` | (no helm deploy) | `RunInitSecrets()` internal Go call — generates encryption keys, bootstraps Infisical, stores credentials |
| 4 | `PhaseBoundary03` | `b01=true, b02=true, b03=true, b04=false` | Apply `03-platform-services` ApplicationSet |
| 5 | `PhaseBoundary04` | `b01=true, b02=true, b03=true, b04=true` | Apply `04-tenant-services` ApplicationSet |

This is implemented via `deploy.boundary01/02/03/04` boolean Helm values. The CLI orchestrator sets these flags incrementally. In steady-state Day-1 operation, all four flags are always `true`.

#### Ingress NGINX Race Fix

The `ingress-nginx` controller and `ingress-config` (containing `Ingress` resources with `ingressClassName: nginx`) were both in `01-platform-infra`. The ingress-nginx validating webhook rejects Ingresses until the controller is running.

**Fix:** Moved `ingress-nginx` controller Helm chart + `ingress-config` Application from `01-platform-infra` to `03-platform-services`. The ArgoCD `server.ingress` config was also moved to `03` (disabled in `01`). This guarantees the ingress-nginx webhook is up before any Ingress resource is applied.

### State Changes

#### `SECRETS_READY` (amended)

- **Owner:** CLI (initiates), then ArgoCD (completes)
- **Entry Criteria:** `PKI_READY` exit criteria satisfied. Management cluster kubeconfig is available.
- **Operations:**
  1. CLI installs ArgoCD via Helm (if not already installed).
  2. CLI applies `01-platform-infra` boundary (B01 only), waits for operator webhooks, CRDs, and operator pods.
  3. CLI applies `02-platform-data` boundary (B01+B02), which deploys CNPG Cluster, Redis, NATS.
  4. CLI calls `RunInitSecrets()` internally — generates encryption keys, DB passwords, waits for CNPG Cluster to be Ready, reads `platform-db-ca` certificate, bootstraps Infisical Org/Project/Machine Identity, stores credentials in Infisical via API.
  5. CLI applies `03-platform-services` boundary (B01+B02+B03), which deploys ingress-nginx, SPIRE, Infisical, and API Gateway.
  6. CLI applies `04-tenant-services` boundary (B01+B02+B03+B04), which deploys Identity (Ory), Billing (OpenMeter), and Tenant proxies.
- **Exit Criteria:** All four ApplicationSets applied. CNPG Cluster Ready. Infisical healthy with Machine Identity. `infisical-auth` Secret exists in `platform-ops`. `hub-bootstrap-config` ConfigMap has OrgID/ProjectID.
- **Failure Recovery:**
  - If B01 operator webhooks don't appear, check ArgoCD Application status and operator Helm releases.
  - If B02 CNPG Cluster CR doesn't appear, check CNPG operator logs and ArgoCD sync status of `platform-database` Application.
  - If `RunInitSecrets` fails, check CNPG Cluster `Ready` condition and Infisical pod logs. Re-running the bootstrap orchestrator resumes from the failed phase.
  - If B03 sync fails, check ingress-nginx webhook is up and ArgoCD can reach the Git repository.

### Implementation Details

1. **Phase enum** (`internal/hub-cli/state/manager.go`): Added `PhaseBoundary01`, `PhaseBoundary02`, `PhaseInitSecrets`, `PhaseBoundary03`, `PhaseBoundary04`. `PhasePlatformDeploy` retained as deprecated alias for backward state compatibility.

2. **Helm values** (`manifests/argocd/environment-manager/values.yaml`): Added `deploy.boundary01`, `deploy.boundary02`, `deploy.boundary03`, `deploy.boundary04` boolean fields, defaulting to `true`.

3. **ApplicationSet templates** (`manifests/argocd/environment-manager/templates/*.yaml`): Each wrapped in `{{ if .Values.deploy.<boundary> }}` / `{{ end }}` for conditional rendering.

4. **Orchestrator** (`internal/hub-cli/bootstrap/orchestrator.go`): Replaced `deployPlatform` with `deployBoundary01/02/03`. Added `renderAndApplyBoundaries` helper that passes `deploy.*` flags to `helm template`. `RunInitSecrets` is called as an internal Go function (not a subprocess) between B02 and B03.

5. **CNPG CR polling** (`internal/hub-cli/components/secrets_infisical_impl.go`): Replaced one-shot `kubectl get` with `wait.PollImmediateUntilWithContext` loop (10s interval, respects context cancellation).

6. **Shell bootstrap** (`scripts/hub-bootstrap.sh`): Removed `step5_init_secrets` (replaced by orchestrator-internal call). Removed all fixed `sleep` intervals between steps. Added stale `ReplicaSet` cleanup after GitHub config step.

7. **Inline ingress-nginx** (`03-platform-services-appset.yaml`): Added `ingress-nginx-controller` Helm chart as an inline Application within the services boundary, replacing the separate `ingress-nginx` ApplicationSet.

### Consequences

#### Positive

- The temporal paradox is resolved: CNPG Cluster CR is guaranteed to exist before `RunInitSecrets` is called.
- Sub-phase checkpoints allow resuming from the exact failure point instead of re-running the entire deploy.
- ingress-nginx webhook race is eliminated by moving the controller to the services boundary.
- No process-fork or exec overhead for init-secrets — it's an internal Go function call sharing the orchestrator's authenticated k8s client.
- Backward compatible: existing state files with `PhasePlatformDeploy` are handled correctly (new phases run idempotently).

#### Negative

- Four new state transitions within `SECRETS_READY` increase the number of checkpoint writes during bootstrap.
- The sequential boundary order adds latency: B03 cannot start until B02's CNPG is ready and init-secrets completes. Previous monolithic deployment started all three simultaneously.
- Helm template is called three times during bootstrap instead of once (minor overhead).
