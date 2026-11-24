# ADR-035: Enterprise PKI and Delegated Trust via Infisical OSS

*Amended by: ADR-117 (Self-Hosted Secret Management on OpenBao), 2026-11-24 — OpenBao replaces Infisical as the intermediate authority, and cert-manager's built-in Vault issuer replaces the Infisical issuer. Root custody moves from "offline / HSM required" to "outside every runtime and automation identity; escrow-held by default; offline or HSM optional". Issuance restrictions follow ADR-117 §6.*

**Date:** 2026-06-08 (rewritten)
**Status:** Accepted (rewritten — supersedes original ADR-035)

## Context

The platform requires a scalable PKI architecture capable of supporting hundreds of Spoke clusters. Historically, certificate issuance was performed on the Hub and distributed to Spokes through Crossplane Composition Functions and Kubernetes Secret traversal.

This approach created several problems:
1. Crossplane became a secret distribution plane.
2. Private key material traversed Hub control-plane components.
3. Certificate lifecycle logic became tightly coupled to provisioning.
4. Certificate distribution violated ownership boundaries.

The platform requires a model where:
- Only cert-manager may reconcile certificate lifecycle and perform certificate issuance and renewal operations.
- Spokes manage their own certificate lifecycle.
- PKI remains centrally governed.
- Certificate issuance follows the same lifecycle dependency model as provisioning and secret management.

This rewrite broadens the PKI ban from component-specific scopes to a universal architectural rule, adds a PKI Decision Hierarchy to disambiguate certificate authority delegation, and references the ownership and authority models defined in ADRs 039, 041, and 043.

## Decision

### Core Architectural Principle

> **"A mechanism that distributes authority is not itself the authority."**
> - `cert-manager` is the certificate lifecycle authority for platform-delegated X.509 certificates.
> - `CNPG` is the authority for CNPG-native internal database TLS.
> - `Infisical` is the authority for Infisical-managed platform secrets and intermediate PKI.
> - `ESO` is a delivery/synchronization mechanism, not a source of truth or issuance authority.
> - `ClusterResourceSet (CRS)` is a Day-0 distribution transport, not a certificate manager.
> - `RBAC aggregation` is a permission composition mechanism, not an authorization boundary.

### Universal Platform PKI Rule

**Only cert-manager may reconcile platform-delegated X.509 certificate lifecycle resources and perform platform certificate issuance and renewal operations.** This is a universal platform rule. No component — present or future — may perform PKI operations unless explicitly authorized by an amendment to this ADR.

#### Bounded Subsystem Exception (CNPG Native TLS)

Components that own a delegated subsystem's native internal PKI, such as CloudNativePG (CNPG) managing its internal PostgreSQL cluster TLS, MAY manage that subsystem-native PKI within their documented ownership boundary ([ADR-005](docs/adr/005-unified-abstraction-layers-crossplane.md), [ADR-041](docs/adr/041-controller-responsibility-matrix.md)). This bounded exception applies strictly to intra-subsystem communication managed natively by the subsystem operator and does not extend to platform-wide mTLS or external workload identities.

#### Forbidden from ALL PKI operations

The following components must never generate, sign, renew, manage CA lifecycle, or handle private key material for X.509 certificates:

- Hub Operator
- Spoke Operator (any spoke-local operator)
- CLI (post Day-0 bootstrap — see Bootstrap Exception)
- Crossplane and all Composition Functions
- Kube-SBT
- External Secrets Operator (ESO)
- ArgoCD
- Atlas Operator
- CNPG
- Billing Operator
- Kyverno

#### Allowed

The following components may create `Certificate`, `Issuer`, and `ClusterIssuer` Custom Resources, delegating actual PKI operations to cert-manager:

- ArgoCD (delivers Certificate manifests from Git)
- Hub Operator (if a Spoke requires a specific Certificate resource as part of provisioning)
- Any component that requires a certificate at runtime — by declaring a Certificate resource, not by generating one.

Creating a Certificate resource is not a PKI operation. It is a Kubernetes resource declaration. cert-manager processes the resource and performs the PKI operation. This distinction must be preserved.

### PKI Decision Hierarchy

```
  Root CA (offline, CLI-generated)
        │
        ▼
  Intermediate CA (Infisical PKI)
        │
        ▼
  Issuer / ClusterIssuer (cert-manager, referencing infisical-issuer)
        │
        ▼
  Certificate (cert-manager, requested by any authorized component)
```

- **Above the Issuer boundary:** Only the CLI (Day-0) may operate. Root CA generation and Intermediate CA upload are immutable Day-0 operations.
- **At and below the Issuer boundary:** Only cert-manager may reconcile. No other component may manage `Issuer`, `ClusterIssuer`, `CertificateRequest`, or `Certificate` reconciliation.
- **Any component may create a Certificate CR.** The act of declaring intent (creating a Certificate resource) is distinct from the act of reconciliation (issuing the certificate), which only cert-manager performs.

### Trust Hierarchy

#### Offline Root CA

- **Generation & Key Management**: The Root CA private key SHALL be generated and retained using an approved offline key-management process. Hardware Security Module (HSM) backing or hardware-isolated key storage is required for enterprise production. No Kubernetes cluster, Infisical instance, CI/CD pipeline, operator, CLI cache, or application component may retain the Root CA private key.
- **Role**: Offline platform trust anchor. Signs the Fleet Intermediate CA during Day-0 bootstrap or formal CA rollover ceremonies.
- **Storage**: Stored strictly offline. Never connected to production systems or runtime environments.
- **Trust Distribution**: Public certificate distributed as a ConfigMap (not a Secret) to Hub and Spoke clusters.

#### Fleet Intermediate CA

- Hosted within Infisical PKI (`hub-platform` project, type `cert-manager`).
- Sole issuing authority for all platform certificates.
- Signs all infrastructure certificates and all Spoke-issued leaf certificates.
- Private key never leaves Infisical.

#### Leaf Certificates

Issued through cert-manager → infisical-issuer → Infisical PKI. All certificates chain through:

```
Offline Root CA → Fleet Intermediate CA → Leaf Certificate
```

### Certificate Issuance Model

Each Spoke cluster deploys cert-manager and infisical-issuer. Leaf certificates are requested locally:

```
Certificate CR → cert-manager → infisical-issuer → Infisical /api/v1/cert-manager/* API
```

The Fleet Intermediate CA signs the certificate. The private key is generated and stored locally in the Spoke cluster. Crossplane is never involved. No certificate material traverses the Hub.

This model depends on the Infisical certificate-management API family (`/api/v1/cert-manager/*`). The platform does not rely on deprecated `/api/v1/pki/*` endpoints.

### Certificate Profiles

All certificate issuance occurs through centrally managed certificate profiles. Direct issuance against Certificate Authorities by ID is not supported.

A certificate profile defines:
- `slug`: Human-readable identifier (survives backup/restore).
- `caId`: The issuing Certificate Authority (Fleet Intermediate CA).
- `enrollmentType`: Issuance method (`api`, `acme`, `scep`, `est`).
- `defaults`: Default TTL, key algorithm, subject fields.
- `issuerType`: `ca` or `self_signed`.

The issuance boundary is:

```
Machine Identity → Project-level PKI permissions → Certificate Profile → Fleet Intermediate CA
```

Profiles are identified by UUID internally and resolved by `slug` operationally.

### PKI Profile TTLs

| Profile | TTL |
|---|---|
| Infrastructure Services | 24h |
| Database Clients | 4h |
| Service Mesh Components | 1h |
| Human Access | 15m |
| Bootstrap Certificate | 72h |

TTLs are enforced server-side by the certificate profile configuration in Infisical.

These are the **default durations for workload and client identities**, and they
remain the target. They are not the profile ceiling: the `infrastructure-services`
profile also serves control-plane *serving* identities that cannot hot-reload a
rotated certificate, and its ceiling is **7 days** for the reasons set out in
addendum §5. A certificate requests the duration it needs; the profile refuses
anything above the ceiling.

### Bootstrap Certificate & Private-Key Transit Exception (Bounded Security Exemption)

The ArgoCD Agent must establish an mTLS connection before GitOps becomes available on a new Spoke. A bootstrap certificate is issued declaratively via `cert-manager` on the Hub — no custom operator code performs PKI operations.

The delivery of this bootstrap certificate requires private key transit across the Hub control plane via `ClusterResourceSet` (CRS). This is an **explicit, formally bounded architectural exception** to the principle that private keys must never leave their originating node or cluster.

**Lifecycle:**

```text
SpokePool (cluster-scoped owner, platform-capi)
   │
   ├── creates Certificate CR (72h TTL, cert-manager.io/v1, platform-capi)
   │       │
   │       ▼
   │   cert-manager + infisical-issuer fulfill Certificate
   │       │
   │       ▼
   │   TLS Secret (platform-capi)
   │
   ├── hub-operator: ensureBootstrapCertCRSWrapper()
   │       │
   │       ├── reads TLS Secret (strictly scoped RBAC)
   │       ├── packages into CRS wrapper Secret (type: addons.cluster.x-k8s.io/resource-set)
   │       ├── sets ownerReferences → SpokePool (for GC)
   │       ↓
   │   CRS Wrapper Secret (immutable after creation, platform-capi)
   │
   ├── creates SpokeMachineIdentity CR (platform-capi)
   │       │
   │       ▼
   │   spoke-identity-operator reconciles
   │       │
   │       ├── creates Infisical Machine Identity
   │       ├── packages into identity CRS wrapper Secret
   │       ├── sets ownerReferences → SpokePool (for GC)
   │       ↓
   │   Identity CRS Wrapper Secret (immutable after creation, platform-capi)
   │
   └── ClusterResourceSet applies via ApplyOnce
           │
           ▼
       Spoke ArgoCD Agent starts with mTLS
           │
           ▼
       Spoke cert-manager takes over certificate lifecycle
           │
           ▼
       Bootstrap certificate expires at 72h
```

**Formal Exemption Controls & Security Invariants:**

1. **Only cert-manager issues certificates.** The hub-operator creates the `Certificate` CR — a declarative resource declaration, not a PKI operation (per ADR-035 Universal PKI Rule).

2. **Strictly Scoped RBAC for Transit Access.** The Hub Operator is granted read access *only* to the specific bootstrap TLS Secret in `platform-capi` by exact name/label convention. The Hub Operator SHALL NOT possess cluster-wide `get`, `list`, or `watch` permissions across Kubernetes Secrets.

3. **Zero Logging & Secret Exposure Prohibition.** Private key and certificate contents SHALL never be logged in controller stdout/stderr, written to Kubernetes Event messages, or exposed in Custom Resource `status` fields.

4. **CRS wrapper Secrets are immutable once created.** The hub-operator's `ensureBootstrapCertCRSWrapper()` and the spoke-identity-operator's identity wrapper reconciler both guard wrapper creation with:

   ```go
   if existing Secret exists AND type == "addons.cluster.x-k8s.io/resource-set":
       return nil  // Immutable — never regenerate after initial creation
   ```

   This prevents Certificate renewal from mutating the `ApplyOnce` bootstrap payload. Bootstrap material is disposable by design.

5. **Automatic garbage collection.** All bootstrap artifacts (Certificate CR, SpokeMachineIdentity CR, CRS wrapper Secrets) set `ownerReferences` pointing to the SpokePool. The implementation relies on Kubernetes support for cluster-scoped owners of namespaced resources. This behavior SHALL be validated during implementation before cert-operator removal. If GC does not cascade correctly, finalizer-based cleanup on SpokePool becomes the fallback mechanism.

6. **Namespace alignment (ADR-015).** All bootstrap artifacts live in `platform-capi`:
   - `Certificate` CR
   - `SpokeMachineIdentity` CR
   - CRS wrapper Secrets

7. **Rotation updates the authoritative Secret, not the bootstrap wrapper.** Machine Identity rotation (`spec.rotationPolicy`) updates the authoritative `smi-{spokeName}-auth` Secret used by Spoke ExternalSecrets (ESO) for Day-1 credential access. The identity CRS wrapper Secret (`{spokeName}-machine-identity`) is a bootstrap-only artifact — it is created once and never mutated. This distinction means:
   - **Day-0:** Bootstrap wrapper delivers initial identity credentials to the Spoke via CRS.
   - **Day-1:** Spoke ESO pulls rotated credentials from Infisical directly, bypassing the immutable wrapper.
   
   The bootstrap wrapper's immutability ensures CRS `ApplyOnce` semantics are never violated by later rotation events.

8. **Periodic Architectural Review.** This transit exemption exists solely to overcome the Day-0 mTLS cold-start chicken-and-egg dilemma. Platform engineering SHALL periodically evaluate mechanisms (e.g., node attestation, SPIFFE/SPIRE federation, TPM-backed identity) to deprecate and eliminate private-key transit entirely.

**This is the only platform exception to delegated issuer-based issuance.** All other certificates are issued directly on the Spoke by cert-manager + infisical-issuer.

### Failure Domain Analysis

The platform PKI architecture has three critical components. This section documents behavior when each is unavailable.

#### cert-manager unavailable

- **Impact:** All Certificate CR reconciliation stops. New CertificateRequests are not processed. Existing certificates continue operating until expiry. Renewals fail.
- **Bootstrap:** New Spokes cannot bootstrap — bootstrap Certificate CRs are never fulfilled.
- **Spoke operations:** Existing Spokes are unaffected (their local cert-manager continues operating independently).

#### Infisical unavailable

- **Impact:** Certificate issuance and renewal fail. `infisical-issuer` cannot sign CertificateRequests. Existing certificates continue operating until TTL expiry.
- **Bootstrap:** Bootstrap Certificate CRs remain in `Ready=False` state until Infisical is restored.
- **Machine Identity:** `spoke-identity-operator` cannot create, rotate, or revoke Machine Identities. Existing identities continue functioning (access tokens cached with Infisical-configured TTL).

#### hub-operator unavailable

- **Impact:** No new SpokePool bootstrap state declared. No new Certificate or SpokeMachineIdentity CRs created. No new CRS wrapper Secrets packaged.
- **Bootstrap:** Existing bootstrap artifacts already delivered to Spokes are unaffected.
- **Existing Spokes:** Unaffected. The spoke-identity-operator and spoke-local cert-manager continue operating independently.

#### spoke-identity-operator unavailable

- **Impact:** Machine Identity rotation stops. New Machine Identities for new Spokes are not created. Identity CRS wrappers are not created.
- **Bootstrap:** SpokeMachineIdentity CRs remain in `Ready=False` until the operator is restored.
- **Existing Spokes:** Identity rotation resumes once the operator is restored. Existing identities continue functioning.

### CNPG Native TLS & Trust-Material Distribution Pattern

#### CNPG Native TLS Boundary

CNPG clusters manage their own internal TLS PKI natively. Zero-Ops does not provision or manage CNPG internal CAs. Consumers requiring database trust material SHALL obtain the CA certificate from the CNPG-generated `<cluster-name>-ca` Secret (e.g., `platform-db-ca` for the `platform-db` cluster, containing key `ca.crt`).

This is consistent with ADR-005 (domain-bounded controllers), ADR-039 (platform ownership model), and ADR-041 (controller responsibility matrix): CNPG owns its database lifecycle, including internal TLS. No platform component — CLI, Hub Operator, or custom operator — generates or manages CNPG certificate material.

The CNPG-generated CA Secret is a Kubernetes resource. It must be included in Kubernetes resource backups (e.g., Velero) for disaster recovery, as it is not contained within PostgreSQL data directory backups.

#### Trust-Material Distribution Pattern (Public CA Projection vs. PKI Issuance)

**Trust material distribution is not certificate issuance.** Consuming and projecting a public CA certificate across namespaces or subsystems according to documented ownership rules does not grant or require PKI issuance authority.

Approved trust-distribution architecture:

```text
CNPG-owned CA Secret (e.g., platform-db-ca)
        │
        ▼
Platform-approved read-only projection (e.g., ESO / Reflector / ConfigMap)
        │
        ▼
Tenant-local Secret / Trust Bundle (read-only consumer)
        │
        ▼
Consuming Client / Workload
```

Platform components MAY distribute public trust anchors (CA certificates/bundles) to enable client verification. This does not violate the Universal Platform PKI Rule because no private key material is generated, signed, renewed, or transferred. This prevents the mistaken assumption that because cert-manager owns PKI, all CA certificates must be routed through cert-manager or Infisical.

### Operator Responsibilities

#### Spoke Identity Operator

The Spoke Identity Operator manages Machine Identity lifecycle for Spoke clusters:
- Watch `SpokeMachineIdentity` CRs in `platform-capi`.
- Create Machine Identities in Infisical.
- Package client secrets into Identity CRS wrapper Secret (type `addons.cluster.x-k8s.io/resource-set`).
- Guard wrapper creation as immutable — if wrapper Secret exists, return nil.
- Set `ownerReferences` on wrapper Secret → SpokePool (for automatic GC).
- Reconcile wrapper existence (drift detection).
- Rotate client secrets per `spec.rotationPolicy`.
- Revoke identities on CR deletion per `spec.revocationPolicy`.

The Spoke Identity Operator must never: issue, renew, or manage certificates. PKI operations are cert-manager's exclusive domain.

#### Hub Operator

The Hub Operator declares desired state and owns Spoke lifecycle orchestration:
- Creates `Certificate` CRs for SpokePool bootstrap certificates (declarative intent) in `platform-capi`.
- Creates `SpokeMachineIdentity` CRs declaring desired Spoke identity state in `platform-capi`.
- Reads TLS Secret (cert-manager-issued) and packages into Bootstrap Cert CRS wrapper Secret.
- Guards wrapper creation as immutable — if wrapper Secret exists and type is `addons.cluster.x-k8s.io/resource-set`, return nil.
- Sets `ownerReferences` on wrapper → SpokePool (for automatic GC).
- Uses explicit `Watches()` with `EnqueueRequestsFromMapFunc` (not `Owns()`) for `Certificate` and `SpokeMachineIdentity` → `SpokePool` reconciliation.

The Hub Operator must never: issue, renew, or manage certificates; call the Infisical PKI API directly; or mutate a CRS wrapper Secret after initial creation.

### SpokePool Deletion Sequence

When a SpokePool is deleted, the following sequence executes via Kubernetes garbage collection (ownerReferences) and operator finalizers:

```text
1. SpokePool deleted (kubectl delete spokepool <name>)
2. Kubernetes GC cascades deletion to all namespaced dependents:
      ↓
   Certificate CR deleted (platform-capi)
      ↓
   SpokeMachineIdentity CR deleted (platform-capi)
      ↓
   Bootstrap CRS wrapper Secret deleted (platform-capi)
      ↓
   Identity CRS wrapper Secret deleted (platform-capi)
3. spoke-identity-operator finalizer runs on SpokeMachineIdentity:
      ↓
   Machine Identity revoked in Infisical (if revokeOnDelete: true)
      ↓
   All client secrets revoked
      ↓
   Infisical project permissions removed
      ↓
   Finalizer removed → CR fully deleted
4. CAPI deletes the spoke Cluster
```

If Kubernetes GC does not cascade correctly (see invariant 3), the hub-operator's SpokePool finalizer SHALL enumerate and delete all bootstrap artifacts explicitly.

### Infisical Project Separation

The platform maintains two Infisical projects:
- **`hub-platform`** (type `cert-manager`): Fleet Intermediate CA, certificate profiles, PKI infrastructure.
- **`hub-secrets`** (type `secret-manager`): Application secrets, database credentials, infrastructure tokens.

Machine Identity permissions are project scoped in Infisical OSS. Separation into two projects ensures PKI Machine Identities cannot read application secrets and ESO only references the secrets project.

### Compromise Response Matrix

The platform deliberately avoids online CRL/OCSP infrastructure to prevent distributed runtime availability dependencies on revocation responders. Mitigation and incident response are strictly partitioned by credential tier:

| Compromise Domain | Primary Mitigation & Containment | Operational Invariant & Recovery Action |
|---|---|---|
| **Leaf Private Key** | Short certificate TTL (1h – 24h; 7d ceiling for serving identities). | Delete compromised Secret; trigger `cert-manager` re-issuance; restart consuming workload via Reloader. Certificate expires naturally. |
| **Machine Identity** | Scoped Infisical permissions (`hub-platform`). | Immediate revocation and rotation of client secret via `spoke-identity-operator` / Infisical API. Access token invalidated on expiry. |
| **Fleet Intermediate CA** | Project-scoped isolation in Infisical. | Immediate CA replacement ceremony: sign new Intermediate via Offline Root CA, update Infisical profiles, and trigger fleet-wide re-issuance. |
| **Root CA** | Offline, HSM/hardware-isolated storage. | Full trust-anchor replacement: rebootstrap trust bundle ConfigMaps across all Hub and Spoke clusters via out-of-band management. |

### CA Rotation and Trust-Anchor Rollover Procedure

CA rotation (whether planned lifecycle retirement or emergency replacement) is fundamentally distinct from leaf certificate renewal. CA rotation requires a strict, multi-phase **dual-trust overlap** to prevent fleet-wide mTLS partition:

```text
       Existing State: [Old CA] trusted, [Old CA] signs
                              │
                              ▼
 Phase 1: Publish Dual-Trust  [Old CA + New CA] published to Trust Bundle ConfigMaps
                              │
                              ▼
 Phase 2: Roll Consumers      Workloads reload Trust Bundle (trust both Old and New)
                              │
                              ▼
 Phase 3: Switch Issuance     Infisical profiles updated; [New CA] signs all new certs
                              │
                              ▼
 Phase 4: Fleet Convergence   Verify 100% of active leaf certificates chain to [New CA]
                              │
                              ▼
 Phase 5: Remove Old CA       [Old CA] stripped from Trust Bundle ConfigMaps; reload
```

**CA Rollover Operational Invariants:**
1. **Mandatory Overlap Period**: In normal planned rotations, both Old and New CAs MUST remain present in all trust bundles for a duration exceeding the longest active leaf certificate TTL plus renewal buffer (minimum 10 days).
2. **Atomic Verification Gate**: The Old CA SHALL NOT be removed until fleet metrics verify that zero active mTLS connections or unrenewed certificates reference the retired authority.
3. **Emergency Fast-Path**: In an active CA compromise scenario, Phase 1 through Phase 3 execute concurrently with an immediate fleet-wide certificate re-issuance and pod restart directive.
4. **Audit & Observability**: Every CA generation, profile repointing, and trust bundle mutation MUST generate immutable audit log events in Infisical and Kubernetes API audit sinks.

### Infisical OSS Authorization Limitation

Machine Identity permissions are project scoped in Infisical OSS. CA-level authorization, Intermediate-specific authorization, and CA-ID-based permission constraints are not available.

Therefore:
- PKI authorization boundaries cannot be enforced per Intermediate CA.
- Security isolation relies on project boundaries, Machine Identity protection, short certificate TTLs, and SPIRE workload identity separation.

This limitation is accepted by the platform architecture.

## Consequences

### Positive

- Universal PKI rule eliminates ambiguity: no future discussion about "can component X generate a certificate" is necessary.
- PKI Decision Hierarchy disambiguates certificate authority delegation — any component may declare a Certificate, only cert-manager may reconcile one.
- Certificate lifecycle ownership moves to Spokes. Bootstrap certificate private keys traverse the Hub through ClusterResourceSet (documented in invariant 5). All non-bootstrap certificates are issued locally on Spokes — their private keys never leave the Spoke cluster.
- Centralized issuance audit trail via Infisical PKI.
- Consistent lifecycle dependency model with ADR-034 (Runtime vs Lifecycle Autonomy).

### Negative

- Certificate issuance depends on Infisical availability.
- Lifecycle autonomy is not provided for certificate issuance.
- Machine Identity PKI permissions remain project scoped in Infisical OSS.
- A compromised Machine Identity could issue additional certificates within the same Infisical project.

This trade-off is accepted because it aligns with ADR-034's distinction between Runtime Autonomy and Lifecycle Autonomy.

## Impact

This rewrite supersedes the original ADR-035. The broadened PKI ban, PKI Decision Hierarchy, and explicit component prohibition list replace the original component-specific scope. All component-level PKI prohibitions in other ADRs must reference this ADR as the single authority for PKI boundaries.

## Addendum (2026-08-22): profile registry corrections

Bootstrapping a hub against a rebuilt Infisical exposed three things this ADR did
not state. Recorded here because the profile table above is the authority the
implementation is meant to derive from, and it had silently stopped being that.

### 1. `signing-keys` is a required profile and was undocumented

The table above lists five profiles. A sixth exists and is load-bearing:
`signing-keys`, named by the `infisical-signing-issuer` ClusterIssuer and used by
the `argocd-agent-jwt` Certificate — a long-lived **asymmetric JWT signing key**,
not an mTLS transport certificate, which is why no existing profile fits it. It was
referenced only by a comment in `manifests/argocd-principal/certificates.yaml`
citing an "ADR-035 PKI review" whose conclusion never reached this document.

The rule this implies, now enforced by test: **every profile named by an
infisical-issuer ClusterIssuer must exist in the registry**, or certificates using
that issuer cannot be issued at all. Today that is `infrastructure-services`
(fleet issuer, hub and spoke) and `signing-keys`.

### 2. The profile list had two divergent implementations

ADR-042 makes PKI_READY CLI-owned, and the hub-operator added a self-healing
reconcile of the same profiles. Both hardcoded their own list, under an operator
comment claiming they matched. They shared exactly one entry:

| CLI | hub-operator |
|---|---|
| argocd-bootstrap | infrastructure-services |
| infrastructure-services | argocd-principals |
| database-clients | argocd-agents |
| service-mesh | |
| human-access | |
| signing-keys | |

`argocd-principals` and `argocd-agents` are referenced by no issuer, no Certificate
and no ADR — the operator was provisioning profiles nothing consumes. More
seriously the operator omitted `signing-keys`, so on any cluster where the CLI's
PKI phase did not complete, nothing ever created it and nothing self-healed it. The
observable failure named neither PKI nor the operator: `argocd-agent-principal`
sat in `ContainerCreating` on a missing Secret, and only the cert-manager
CertificateRequest carried the cause — *"Certificate template with name
signing-keys not found"*.

Both now derive from a single definition, `internal/pki.RequiredProfiles`. They keep
their own Infisical clients; only the data is shared, which is the seam that was
missing.

### 3. Conflict — the Infrastructure Services TTL (resolved in §5)

The table above caps Infrastructure Services at **24h**. The three argocd-principal
certificates request `duration: 2160h` (**90 days**) through that profile, and are
live on the hub today holding a full 90-day validity window — so the profile as
actually provisioned carries the operator's 90-day cap, not this ADR's 24h.

`internal/pki` therefore records **90**, because that is what keeps the platform
working: the TTL is a server-side cap, and lowering it to 24h would not fail at
apply time — it would break renewal of three healthy certificates, silently, up to
90 days later.

**Resolved in §5.** Neither branch was correct as posed: 24h is right for the
workload identities it was written for, 90 days was never justified for anything,
and the profile TTL turned out to be a ceiling rather than a mandate — so both
classes can be served by one profile without weakening either.

### 4. Renewal is cert-manager's; reload is the consumer's problem

Ownership of the certificate lifecycle splits cleanly, and it is worth stating
because the hub-operator's involvement is easy to overestimate:

```
hub-operator     ensures the PKI template exists          (and nothing more)
cert-manager     issues, schedules renewal, rewrites the Secret
the consumer     must actually pick the new material up
```

The hub-operator never issues or renews a certificate. It cannot compensate for a
missing template either — which is why the drift in §2 was fatal rather than
cosmetic.

Renewal itself is automatic and needs no operator action: cert-manager schedules
it at `notAfter - renewBefore` (for the argocd-principal certificates, ~83 days
into a 90-day lifetime) and rewrites the Secret in place.

**The last step is not automatic.** A Secret being rewritten does not mean the
process using it has reloaded. For `argocd-agent-principal` it demonstrably does
not: the upstream source reads the Secret once during option assembly, memoises
the resulting `tls.Config`, never invalidates it, installs no `GetCertificate`
callback, and watches no Secret. Upstream's own documentation says to restart the
component after rotating certificates.

Left alone this produces a failure that actively misleads: the rotation succeeds
at day 83, the process keeps serving the *previous* certificate, and that
certificate stays valid for another 7 days — so the outage surfaces a week later
with nothing pointing back at the rotation.

The restart is therefore declared, via Stakater Reloader, on the consuming
Deployment. Two details are load-bearing:

- the **explicit** `secret.reloader.stakater.com/reload` list is required, not
  `reloader.stakater.com/auto`. Only `argocd-agent-jwt` is mounted as a volume;
  the TLS secrets are referenced by name through `principal-params-cm` and never
  mounted, so `auto` would track the one secret that matters least and ignore the
  three that rotate;
- Reloader keys on Secret **data**, not metadata, so it is inert for annotation-
  only changes — which is correct, and worth knowing when testing it.

**Rule for any new certificate consumer:** establish whether the process reloads
its own certificate material. Native reload is preferable — restarting a
security-sensitive control-plane component because a certificate changed is
operationally heavier. Only when the consumer cannot reload does the declared
restart apply.

### 5. Resolution — `infrastructure-services` ceiling is 7 days

**Decision.** The `infrastructure-services` profile TTL is **7 days**. The table
above stands unchanged as the default duration for workload and client identities;
7 days is the *ceiling* the profile enforces, not a duration anything is obliged to
use.

**Why the original question had no good answer.** It was posed as 24h *or* 90d, and
both are wrong platform-wide, because one profile serves two classes of consumer:

| Class | Example | Reloads? | Duration |
|---|---|---|---|
| Workload / client identity | spoke `argocd-agent-client-cert`, `alloy-client-cert`, `nats-leafnode-client-cert` | restarted cheaply | **24h** (unchanged) |
| Control-plane serving identity | `argocd-agent-principal-tls`, `argocd-principal-internal-tls`, `argocd-agent-resource-proxy-tls`, `nats-leafnode-server-cert` | **no** (§4) | **7d** |

The profile TTL is a **server-side maximum**. Once that is recognised the conflict
dissolves: the spoke certificates keep requesting 24h and are unaffected, and the
ceiling only has to be as high as the longest legitimate need.

**Why 7 days, specifically.** Three constraints pull in opposite directions and 7
days is where they balance:

1. **There is no revocation.** This ADR implements no CRL and no OCSP —
   *"compromised certificates expire naturally."* The TTL **is** the containment
   mechanism. At 90 days a stolen principal key, which every agent in the fleet
   trusts, stayed valid for a quarter with no way to withdraw it. That was the
   strongest argument against the status quo and nothing justified it.
2. **The consumers cannot hot-reload** (§4), so every rotation costs a process
   restart. A 24h ceiling would restart a control-plane component roughly 1.5×
   per day, which trades a small compromise window for continuous availability
   churn — worse, not better.
3. **Infisical dependency and the outage tolerance window.** Per this ADR's failure domain analysis, if Infisical becomes unavailable, *"certificate issuance and renewal fail; existing certificates continue operating until TTL expiry."* The selected maximum TTL ceiling SHALL account for the documented control-plane dependency's Recovery Time Objective (RTO) and maximum tolerated outage window under disaster recovery scenarios. An excessively short ceiling would convert a transient intermediate PKI outage into an immediate fleet-wide mTLS partition.

7 days with `renewBefore: 48h` cuts the unrevocable compromise window by roughly
**13×** versus 90 days, still tolerates about **5 days** of Infisical unavailability
before anything expires, and restarts the affected components weekly rather than
daily.

**Consequences.**

- `argocd-agent-principal-tls`, `argocd-principal-internal-tls` and
  `argocd-agent-resource-proxy-tls`: `2160h → 168h`, `renewBefore 168h → 48h`.
- `nats-leafnode-server-cert`: `720h → 168h`, `renewBefore 24h → 48h`. It exceeded
  the new ceiling and would otherwise have failed to renew.
- NATS gains the same declared restart as the principal (§4); it mounts its
  certificate and has no reload path wired.
- Spoke client certificates are unchanged.

**Ordering, which is load-bearing.** A ceiling below a certificate's requested
duration does **not** fail when applied. The issuer refuses or truncates at signing
time, so the breakage appears at the *first renewal* — one full lifetime later.
Always reduce the Certificates first, then the ceiling.
`scripts/validate/preflight/96-certificate-ttl-ceiling.sh` enforces the invariant
before a cluster is built, reading ceilings from `internal/pki` and the
issuer→profile mapping from the ClusterIssuer manifests so it follows both.

**Revisit if** revocation becomes available (CRL/OCSP, or SPIFFE/SPIRE short-lived
identities), or Infisical stops being a single point of failure. Either would remove
one of the two forces holding the ceiling above 24h, and the workload default of
24h should then extend to serving identities as well.

## Addendum (2026-09-12): "platform" here means the tenant's platform

This ADR predates ADR-065, and its vocabulary has not aged well. It was written
when there was one fleet — one hub and its spokes — so "platform" and "tenant"
named the same estate and the distinction never had to be made.

Under ADR-065 each tenant runs its own box, its own Infisical, and therefore its
own PKI. **Every occurrence of "platform" in this ADR means the tenant's
platform.** In particular:

- the **offline Root CA** is the tenant's, generated and held by them, and SOLOZ
  is not in any tenant's trust chain and holds no issuing authority over it;
- the **Fleet Intermediate CA** is one per box, not one across the estate, and
  "fleet" means that box's hub and spokes;
- **"sole issuing authority for all platform certificates"** is sole within one
  box.

This matters beyond wording. ADR-077 had to determine whether SOLOZ could
validate a Support Agent's certificate by chaining to a root it already held. It
cannot: it holds nothing, which is why enrolment there registers the tenant's CA
explicitly. A reader who took "platform trust anchor" to mean SOLOZ's would have
designed a support channel that silently depended on the platform being inside
every tenant's trust chain — the opposite of what ADR-065 and ADR-076 promise.

The blast-radius arithmetic follows the same correction: compromise of a Fleet
Intermediate CA is fleet-wide **within one tenant**, not across the estate, which
is what makes the central-signing concentration ADR-032's amendment flags
tolerable rather than systemic.

No decision in this ADR changes. What changes is that a reader after ADR-065 can
tell whose PKI it is describing.

## References

- ADR-015: Namespace Alignment
- ADR-032: PKI Architecture, Revocation, and Secret Traversal
- ADR-034: Control Plane Failure Domains and Data Plane Autonomy
- ADR-039: Platform Ownership Model
- ADR-040: Day-0 vs Day-1 Lifecycle Boundary
- ADR-041: Controller Responsibility Matrix
- ADR-043: Control Plane Authority Model
- ADR-045: Bootstrap-Generated GitOps Artifacts
