# Runbook: Backup-Credential Chain Recovery

**Applies to:** pool spokes (e.g., `spoke-pool-hybrid-dev-01`)
**Incident class:** missing CRS-delivered `infisical-auth` / ClusterIssuer,
broken ESO store, failing barman archiver
**Decision references:** ADR-046 §12, ADR-048 (CRS lifecycle injection)

## Defect map

| Layer | Failure mode | Defect |
|---|---|---|
| CRS delivery | deleted payloads are not restored | `Reconcile` re-applies only on payload/binding hash change — no live-state drift repair |
| ESO store/ES | conditions frozen after secret recreation | store/externalsecret controllers do not watch the source secret |
| CNPG | barman envs stale after secret change | instance manager resolves `s3Credentials` once at instance boot |
| Infisical identity | 401 lockout | repeated failed logins lock the auth method; every further probe extends the lock |
| Application deletion | tracked Namespace cascade deletes untracked payloads | requires `preserveResourcesOnDeletion: true` on the ApplicationSet (now codified) |

## Symbols

- `SPOKE` — spoke name, e.g. `spoke-pool-hybrid-dev-01`
- `HUB_KC` / `SPOKE_KC` — kubeconfigs at `${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/hub-hybrid-dev.kubeconfig`, `${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/<spoke>.kubeconfig`

## Recovery procedure (order matters)

### 1. Determine the defect scope

```bash
# spoke
kubectl --kubeconfig $SPOKE_KC get clustersecretstore infisical-backend \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
kubectl --kubeconfig $SPOKE_KC get externalsecrets -A \
  -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}:{range .status.conditions[*]}{.type}={.status} {end}{"\n"}{end}'
kubectl --kubeconfig $SPOKE_KC get secrets -A | grep infisical-auth
```

Missing `infisical-auth` + stale store conditions = incident class above.

### 2. Rotate the machine identity (sanctioned CRS re-render)

`rotationPolicy` (`interval: 60d`, `overlapPeriod: 24h`) is codified in
hub-operator; the SMI is owned by the SpokePool controller. Clear
`Status.NextRotation` to trigger immediate rotation:

```bash
# hub
kubectl --kubeconfig $HUB_KC patch spokemachineidentity $SPOKE -n platform-capi \
  --subresource=status --type=merge \
  -p '{"status":{"nextRotation":null}}'
```

A plain (non-`--subresource`) patch is a NO-OP. Do not re-trigger while the
previous rotation is in flight; observe the chain first.

**Verify the chain (expected outcomes):**

```bash
# hub — secret rvs must bump
kubectl --kubeconfig $HUB_KC get secret smi-$SPOKE-auth -n platform-capi -o jsonpath='{range .metadata}{.generation}{" "}{.resourceVersion}{"\n"}{end}'
kubectl --kubeconfig $HUB_KC get secret $SPOKE-machine-identity -n platform-capi -o jsonpath='{.metadata.resourceVersion}{"\n"}'
# hub — binding generation bump + new hash + applied:true
kubectl --kubeconfig $HUB_KC get clusterresourcesetbinding $SPOKE -n platform-capi \
  -o jsonpath='{.metadata.generation}{"\n"}{range .status.bindingDecision[*]}{.resource}{" "}{.applied}{" "}{.hash}{"\n"}{end}'
# spoke — both secrets re-delivered (age ~30s)
kubectl --kubeconfig $SPOKE_KC get secret infisical-auth -n platform-ops
kubectl --kubeconfig $SPOKE_KC get secret infisical-auth -n cert-manager
```

### 3. Nudge ESO revalidation (store, then each ExternalSecret)

Annotations are transient triggers — strip them once conditions are
healthy; do NOT add them to manifests.

```bash
kubectl --kubeconfig $SPOKE_KC annotate clustersecretstore infisical-backend \
  "recovery.zeroops.io/rotation-triggered=$(date -Iseconds)" --overwrite
# repeat on each ExternalSecret that must revalidate, e.g.:
for es in $(kubectl --kubeconfig $SPOKE_KC get externalsecrets -A -o name); do
  kubectl --kubeconfig $SPOKE_KC annotate $es "recovery.zeroops.io/rotation-triggered=$(date -Iseconds)" --overwrite
done
kubectl --kubeconfig $SPOKE_KC get clustersecretstore infisical-backend \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

Cleanup (Git-state convergence):

```bash
kubectl --kubeconfig $SPOKE_KC annotate clustersecretstore infisical-backend recovery.zeroops.io/rotation-triggered-
# for each annotated ExternalSecret
kubectl --kubeconfig $SPOKE_KC annotate externalsecret -n <ns> <name> recovery.zeroops.io/rotation-triggered-
```

### 4. Restart the CNPG instance (stale barman envs)

```bash
kubectl --kubeconfig $SPOKE_KC delete pod shared-cnpg-1 -n platform-data --wait=false
```

Verify: pod Running/ready; then in the pod logs the first
`Archived WAL file` success (a failed attempt is expected before the
restart while envs were cached).

### 5. Verify the S3 credential classification

The failure signature decides the next step:

| S3 error | Meaning | Action |
|---|---|---|
| `InvalidAccessKeyId ... does not exist in our records` | key unknown (e.g., HCloud API token used as S3 key) | fix ES remoteRef keys (codified commit) |
| `AccessDenied` | key known, unauthorized | bucket policy / permissions |
| `NoSuchBucket` | bucket missing | create bucket |

Codified truth: the s3-credentials ExternalSecret reads
`S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY` from
`/spoke-pool/<spoke>/shared/` (hub-secrets), injected per-spoke by the
spoke-catalog ApplicationSet patch; `hcloud-token` is an API token, never
an S3 key. Hetzner Object Storage access keys start `0A…` (24 chars).

### 6. Trigger the immediate base backup (G1 gate)

`ScheduledBackup` fires only on schedule; a manual base backup is a
one-shot `Backup` CR (operator convention; not catalog state):

```bash
kubectl --kubeconfig $SPOKE_KC create -f - <<EOF
apiVersion: postgresql.cnpg.io/v1
kind: Backup
metadata:
  name: shared-cnpg-$(date -u +%Y%m%d%H%M%S)
  namespace: platform-data
spec:
  cluster:
    name: shared-cnpg
  method: barmanObjectStore
EOF
kubectl --kubeconfig $SPOKE_KC get backup -n platform-data \
  -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,ID:.status.backupId
kubectl --kubeconfig $SPOKE_KC get cluster shared-cnpg -n platform-data \
  -o jsonpath='{.status.continuousArchiving}{" "}{.status.lastSuccessfulBackup}{"\n"}'
```

G1 GREEN = `ContinuousArchiving: True` + backup `phase: completed`.

### 7. Restore a missing CRS-delivered ClusterIssuer

Applies to the same delete-trap: the binding shows `applied: true` but the
cluster-scoped object is gone (nothing re-applies it). Re-apply the
**authoritative wrapper payload** — operator-rendered only, never
hand-edited (per-spoke `clientId` must not be committed to shared Git):

```bash
# hub — extract
kubectl --kubeconfig $HUB_KC get secret $SPOKE-cluster-issuer -n platform-capi \
  -o jsonpath='{.data.cluster-issuer\.yaml}' | base64 -d | tee /tmp/cluster-issuer.yaml
# spoke — re-apply
kubectl --kubeconfig $SPOKE_KC apply -f /tmp/cluster-issuer.yaml
```

Validate: the infisical-issuer operator's `clusterissuer` controller logs
`Success` within a minute. Confirm the issuer exists:

```bash
kubectl --kubeconfig $SPOKE_KC get clusterissuer infisical-fleet-issuer
```

If certificates keep failing, first restart the operator to flush a stale
exponential-backoff retry queue before touching anything else:

```bash
kubectl --kubeconfig $SPOKE_KC rollout restart deploy/infisical-issuer -n cert-manager
kubectl --kubeconfig $SPOKE_KC rollout status deploy/infisical-issuer -n cert-manager --timeout=60s
```

### 8. Handle Infisical Universal-Auth lockout

Signatures (401):
- `This identity auth method is temporarily locked, please try again later`
- `This identity auth method is not allowed for the current project`

Recovery hierarchy (do NOT skip the operator path):

```text
normal   -> spoke-identity-operator self-heal (primary)
fallback -> human admin recovery via clear-lockouts API (emergency only)
```

#### 8a. Primary: operator self-heal

`spoke-identity-operator` (hub, `platform-security`) probes every
SpokeMachineIdentity on a bounded cadence (max 5min, independent of the
certificate rotation window) and:

- `temporarily locked` → clears the lockout and re-probes to verify.
  Operator log markers: `Identity auth method locked — clearing lockout`
  then `Identity lockout cleared and verified`.
- `Invalid credentials` → sets `Ready=False / AuthenticationDefect` and
  does NOT clear. This is a credential/secret defect, not a lockout:
  follow §2 (rotate the machine identity), never clear-lockouts.

Usually no action is needed; wait one reconcile window (≤5min) and verify
with a single probe:

```bash
CID=$(kubectl --kubeconfig $SPOKE_KC get secret infisical-auth -n platform-ops \
  -o jsonpath='{.data.client-id}' | base64 -d)
CSEC=$(kubectl --kubeconfig $SPOKE_KC get secret infisical-auth -n platform-ops \
  -o jsonpath='{.data.client-secret}' | base64 -d)
curl -sk -X POST "https://infisical.nutgraf.in/api/v1/auth/universal-auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"clientId\":\"$CID\",\"clientSecret\":\"$CSEC\"}"
```

Rules: do NOT probe repeatedly — every failed login extends the lock. 200 =
credentials valid; 401 `temporarily locked` = operator has not cleared yet
(check SMI `Ready` condition and operator logs); 401 `Invalid credentials` =
AuthenticationDefect, go to §2.

#### 8b. Emergency fallback: manual clear-lockouts API

Only when the operator is unable to act (operator down, CR missing, or the
lock is on a non-SMI identity). Uses the hub admin identity
(`platform-security/infisical-auth`, client-id/client-secret GitOps-rendered)
to call the same endpoint the operator uses:

```bash
HUB_ADMIN_CID=$(kubectl --kubeconfig $HUB_KC get secret infisical-auth -n platform-security \
  -o jsonpath='{.data.client-id}' | base64 -d)
HUB_ADMIN_CSEC=$(kubectl --kubeconfig $HUB_KC get secret infisical-auth -n platform-security \
  -o jsonpath='{.data.client-secret}' | base64 -d)
TOKEN=$(curl -sk -X POST "https://infisical.nutgraf.in/api/v1/auth/universal-auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"clientId\":\"$HUB_ADMIN_CID\",\"clientSecret\":\"$HUB_ADMIN_CSEC\"}" | jq -r .accessToken)
curl -sk -X POST "https://infisical.nutgraf.in/api/v1/auth/universal-auth/sign-in/clear-lockout" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d "{\"identityId\":\"<smi.status.identityID>\",\"clientId\":\"<smi.status.clientID>\",\"lockedOut\":false}"
```

The call is idempotent (400 on an already-unlocked identity is expected).
Then verify with a single probe as in 8a. Confirm the identity was NOT
locked for an auth defect first — see IdentityProvisioned / Ready
conditions; if `AuthenticationDefect` is present, run §2 and do not clear.

## ArgoCD operational notes (adjacent to this incident)

- Service: `svc/argocd-server` in `platform-ops`; HTTPS only (HTTP 307s to
  https). Port-forward: `kubectl --kubeconfig $HUB_KC port-forward -n platform-ops svc/argocd-server 18444:443`.
- Session: `POST https://127.0.0.1:18444/api/v1/session` with
  `admin` + password from `argocd-initial-admin-secret` → bearer token.
- A sync stuck in health-wait ("waiting for healthy state of ...") blocks
  new syncs: terminate the operation first
  (`DELETE /api/v1/applications/<app>/operation`, with
  `Content-Type: application/json`), then re-POST `/sync`; auto-sync
  (`selfHeal`) will race to claim the slot — that is expected.
- Hard refresh: `POST /api/v1/applications/<app>/refresh?refresh=hard`
  with `Content-Type: application/json` (415 without it).
- Do NOT delete or regenerate appset-managed Applications (ADR-046 §12).

## Reporting

After recovery, report at minimum: store + ESes condition, archiver state
(`ContinuousArchiving`, last WAL), last successful base backup, app sync
status, and the defect classifications observed (S3 error class, lockout
state). Any new adhoc step MUST be codified in this runbook (if
procedural), in manifests (if state), or in an ADR (if decision) before
the next checkpoint.