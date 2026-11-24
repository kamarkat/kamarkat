# ADR-030: Autonomous Credential Rotation and Lifecycle Management

*Amended by: ADR-117 (Self-Hosted Secret Management on OpenBao), 2026-11-24 — The zero-downtime claim is withdrawn. Rotation converges eventually, may temporarily fail new authentications, and makes no unconditional zero-downtime guarantee (ADR-117 §5).*

## Status
Accepted

## Context
With the adoption of ADR-024 (Cryptographic Secret Generation via ESO), tenant database credentials are now securely and autonomously generated within the cluster. However, generating a secret is only half the lifecycle; we must define how these credentials are rotated, how the data plane (PostgreSQL) is updated, and how the app plane (Workloads) gracefully recovers without downtime.

## Decision
We mandate the following closed-loop rotation lifecycle for all autonomous credentials:

1. **Continuous Reconciliation:** ExternalSecrets tied to Generators MUST have a non-zero `refreshInterval` (e.g., `1h` or `24h`) to ensure drift detection, recovery from accidental deletion, and readiness for time-based rotation policies.
2. **Provider Reconciliation:** Infrastructure providers (e.g., Crossplane `provider-sql`) SHALL continuously monitor the resulting Kubernetes `Secret`. When the password changes, the provider MUST issue an `ALTER ROLE ... PASSWORD` without dropping the role.
3. **Application Rolling Updates:** Workloads consuming these secrets MUST NOT cache credentials indefinitely. Changes to the underlying Kubernetes `Secret` MUST trigger a rolling update of the consuming Pods. (This is typically achieved via `stakater/Reloader` annotations on the `Rollout` base).
4. **Full Entropy & Safe Encoding:** Password generators MUST NOT artificially weaken entropy (e.g., disabling symbols) to satisfy URI formatting. Generators SHALL output full-entropy strings. Consumers of these passwords (e.g., DSN builders) MUST use URL-encoding (e.g., Sprig's `{{ .password | urlquery }}`) during injection.
5. **Component Granularity:** Where a consumer requires a DSN URL, the Secret template MUST ALSO project the individual components (`host`, `port`, `username`, `password`, `dbname`) alongside the opaque URL to support sidecars, connection poolers, and audit tooling.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Application Secrets | Infisical | Infisical | ESO | Workloads, Crossplane | Day-1+ |
| Tenant Passwords | Infisical | Tenant Identity Service | ESO | Tenant Apps, provider-sql | Day-1+ |

See ADR-039 for the complete ownership matrix.

## Consequences
- **Positive:** True zero-downtime rotation is achieved. Secrets maintain maximum cryptographic entropy. Drift is automatically corrected.
- **Negative:** Workloads will experience a rolling restart whenever a credential rotates, which requires applications to handle SIGTERM gracefully to avoid dropping in-flight requests.