# ADR Template

Follow the Keeling pattern. Structure:

```
# ADR-0XX: Title

**Date:** YYYY-MM-DD
**Status:** Proposed | Accepted | Deprecated | Superseded

## Context
Describe the forces at play and the problem being solved.
- No assumptions. Only state facts established by other ADRs or observed in the platform.
- Reference relevant ADRs by number.

## Decision
The decision and its rationale. Be precise. No config or code.
- Focus on the final decision, not the exploration process.
- If alternatives were considered and discussed, include them. Do not invent alternatives for completeness.

## Ownership
Mandatory section. Reference ADR-039 (Platform Ownership Model).

Format:

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| <resource> | <system_of_record> | <lifecycle_owner> | <reconciler> | <consumer> | Day-0 or Day-1+ |

If the ADR defines architectural constraints without owning resources:

This ADR defines [constraints/patterns/conventions] and does not own platform resources. For resource ownership, see ADR-039.

## Consequences
### Positive
### Negative

## Impact
Changes this ADR introduces to the existing platform. Reference ADRs that are amended or superseded.

## References
- ADR-039: Platform Ownership Model
- ADR-040: Day-0 vs Day-1 Lifecycle Boundary
- ADR-041: Controller Responsibility Matrix
- ADR-043: Control Plane Authority Model
```

## Standing Decisions

All ADRs must respect these architectural rules without restating them:

1. **System of Record (ADR-039):** Every resource has exactly one System of Record. No component may read state from or write state to a different store and treat it as authoritative.

2. **Universal PKI Rule (ADR-035, ADR-041):** Only cert-manager may issue, renew, revoke, or manage X.509 certificates. All other components are prohibited from PKI operations.

3. **Day-0 vs Day-1 (ADR-040):** Day-0 is CLI-only, exactly once. Day-1+ is controllers-only, continuous. Day-0 artifacts are immutable inputs for Day-1 controllers.

4. **Single Authority per Domain (ADR-043):** Exactly one authority exists per domain. Where two ADRs appear to conflict, ADR-043 controls.

5. **Secret Lifecycle (ADR-003, ADR-117):** All secrets follow: Generation → Storage (OpenBao) → Delivery (ESO) → Consumption → Rotation. Values flow one way; nothing is published into OpenBao from a cluster. No Kubernetes Secret may be a System of Record for secret material.

6. **No actor-focused language:** Describe what must be stored where and who owns the lifecycle. Do not describe "who performs the API call."

## Conventions
- Do not add a summary section.
- Do not include configuration or code in the body of the ADR.
- Keep it to the point.
- Superseded ADRs must be marked clearly at the top.
