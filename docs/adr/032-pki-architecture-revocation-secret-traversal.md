# ADR 032: PKI Architecture, Revocation, and Secret Traversal

*Amended by: ADR-117 (Self-Hosted Secret Management on OpenBao), 2026-11-24 — The issuing authority is OpenBao's intermediate, signed by an escrow-held root (ADR-117 §6).*

## Status

Superseded in part by ADR-035, 2026-08-31. The Delegated Spoke Issuance and
Revocation Strategy decisions below no longer describe the platform; ADR-035
governs both. The Secret Traversal Constraint remains binding and is depended
upon by ADR-003.

## Supersedes

Centralized Leaf Issuance

## Context

Centralized Hub leaf certificate issuance creates a massive reconciliation bottleneck at scale. Furthermore, allowing raw secret material to traverse Crossplane Composition Functions risks security exposure. The platform requires a scalable issuance and revocation model.

## Decision

**Delegated Spoke Issuance:**
The Hub cluster will solely generate and securely distribute Intermediate CA certificates to Spoke clusters. Spoke clusters will run local `cert-manager` instances to mint, rotate, and manage their own local leaf certificates for workloads (e.g., SPIRE, PostgREST).

**Revocation Strategy:**
Leaf certificates are strictly bounded to a 1-hour Time-To-Live (TTL). No Certificate Revocation Lists (CRLs) or OCSP responders will be maintained for leaf certificates. A compromised Spoke cluster requires manual intervention to replace and redistribute trust anchors for the Hub-issued Intermediate CA specific to that Spoke, isolating the blast radius to a single cluster.

**Secret Traversal Constraint:**
Crossplane Composition Functions are explicitly prohibited from parsing, manipulating, or transporting application-level secret material. Function payload traversal is strictly limited to Day-0 bootstrap trust anchors (Intermediate CAs). All application secrets must utilize the Infisical-to-ESO runtime resolution pattern.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Certificates | Kubernetes API | cert-manager | cert-manager | Workloads, ArgoCD Agent | Day-1+ |

See ADR-039 for the complete ownership matrix.

## Consequences

### Positive
- Eliminates Hub-side certificate issuance bottleneck.
- Short-lived leaf certificates (1h TTL) eliminate need for CRLs or OCSP.
- Secret traversal constraint prevents exposure in Composition Functions.

### Negative
- Compromised Spoke requires manual Intermediate CA trust anchor replacement.

## Amendment (2026-08-31): the issuance model recorded here was never built

ADR-035 replaced this ADR's issuance and revocation decisions. That was not
recorded here, and the text below has since been read as current. Correcting it,
against the deployed fleet:

**There is no per-Spoke Intermediate CA.** A single authority signs every
certificate in the fleet. ADR-035 states this directly — the Fleet Intermediate
CA is the "sole issuing authority for all platform certificates" and signs "all
Spoke-issued leaf certificates" — and the fleet holds exactly one CA. What is
delegated to a Spoke is private-key generation and the request path, not signing
authority.

**The blast-radius isolation claimed below therefore does not exist.** This ADR
says a compromised Spoke is contained by replacing the trust anchor "specific to
that Spoke". There is no such anchor to replace. Compromise of the CA, or of the
credentials that authorise issuance against it, is fleet-wide. ADR-035's
Compromise Response Matrix is the accurate account.

**The 1-hour leaf TTL is not the policy.** ADR-035 addendum §5 sets 24 hours as
the default for workload and client identities and 7 days as the ceiling for
serving identities that cannot reload a certificate without restarting. The
deployed certificates request 24 hours and 7 days accordingly. The 1-hour figure
was never reconciled with the components that must restart on rotation.

The absence of "no CRL, no OCSP" from this correction is deliberate: that part
holds, and ADR-035 restates it. Short lifetimes remain the only containment
mechanism, which is what makes the concentration of signing authority above
consequential rather than academic.

### Why this was corrected now

A certificate authority can exist, be discoverable by name, accept the creation
of profiles against it, and still refuse every issuance request. The fleet's CA
was left in that state by a rebuild, and because signing authority is central,
the effect appeared days later and on a different cluster: three Spoke
certificates and one tenant workload certificate expired together with no
renewal available, and the Spoke's catalog reconciliation stalled waiting for
them.

Under the architecture recorded below — a Spoke holding its own intermediate and
issuing locally — a Hub-side authority fault could not expire a Spoke's leaf
certificates. Under the architecture that exists, it does. That difference is
the reason this correction is worth making rather than deleting.

This amendment records the gap. It does not decide whether to close it: whether
the fleet should move to per-Spoke intermediates, or accept central issuance and
defend it with monitoring, is a decision for ADR-035 to make or amend.
