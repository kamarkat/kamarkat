#!/usr/bin/env bash
#
# Seed the externally-sourced Infisical keys — the ones no controller can
# regenerate — from this operator's own store (~/.kamarkat/).
#
# WHY THIS EXISTS
#
#   Most platform secrets are produced by the hub-operator: it generates them,
#   writes them to Infisical, and ESO delivers them. A handful cannot be, because
#   they are issued by third parties — Hetzner, the IRM, GitHub. When
#   Infisical is reinitialised those keys are simply gone, every ExternalSecret
#   that reads one reports "could not get secret data from provider", and nothing
#   in the platform can recover them. They live in the operator's store and have to be put
#   back by hand. This script is that by-hand step, made repeatable.
#
#   The key -> file mapping is the same one recorded in
#   scripts/validate/preflight/95-infisical-key-producers.sh (its EXTERNAL map),
#   which is what stops the validator failing on these. Keep the two in step: if
#   you add a key here, declare it there, or preflight will call it unproduced.
#
# USAGE
#   INFISICAL_BASE_URL=https://infisical.<box-domain> \
#   KUBECONFIG=<hub kubeconfig> \
#     scripts/secret/seed-external-secrets.sh <cell-id> [--dry-run]
#
#   e.g. INFISICAL_BASE_URL=https://infisical.dev.nutgraf.in \
#        KUBECONFIG=.local-e2e/nutgraf-gitops/k8-secrets/kubeconfig/nutgraf-hub.kubeconfig \
#          scripts/secret/seed-external-secrets.sh nutgraf-01 --dry-run
#
#   Run from the repository root. Dry-run first: it lists every key, its source
#   file and its Infisical path, and prints the command that writes them.
#
# AUTH
#   Reads INFISICAL_CLIENT_ID / INFISICAL_CLIENT_SECRET from the environment, or
#   from the live cluster (platform-security/infisical-auth) when a kubeconfig is
#   available. Never prints a secret value — only key names and outcomes.
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# The operator's own store, not a checkout: these credentials outlive every clone
# (internal/kakt-cli/opstore). SECRETS_DIR still overrides.
SECRETS_DIR="${SECRETS_DIR:-${KAMARKAT_HOME:-$HOME/.kamarkat}}"
# No default. The one that was here pointed at the PLATFORM's Infisical, so a
# tenant running this seeded the platform's secret store instead of their own
# (ADR-065). It is this box's instance -- infisical.<the box's domain> -- or a
# port-forward to it.
INFISICAL_URL="${INFISICAL_BASE_URL:-}"

# Every refusal below names what is missing and then prints the whole command,
# so the next attempt is a copy, not a reconstruction.
usage() {
    printf 'seed-external-secrets: %s\n\n' "$1" >&2
    sed -n '/^# USAGE/,/^# AUTH/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2
    exit 2
}
[[ -z "$INFISICAL_URL" ]] && usage "INFISICAL_BASE_URL is not set. It names THIS box's Infisical; there is no default."
PROJECT_SLUG="${INFISICAL_PROJECT_SLUG:-hub-secrets}"
ENVIRONMENT="${INFISICAL_ENVIRONMENT:-dev}"

CELL_ID="${1:-}"
DRY_RUN=0
[[ "${2:-}" == "--dry-run" ]] && DRY_RUN=1

[[ -z "$CELL_ID" || "$CELL_ID" == -* ]] && usage "no <cluster-name>. It is the cluster whose /spoke-pool/<cluster-name>/shared folder receives the shared keys."
[[ -n "${2:-}" && "${2:-}" != "--dry-run" ]] && usage "unknown argument '${2}'."

# ── key : source file : infisical path ──────────────────────────────────────
# An empty path means the Infisical root, which is where the root-scoped
# consumers (cert-manager's DNS solver, the GHCR pull secret) read from.
SHARED="/spoke-pool/${CELL_ID}/shared"
MAPPINGS=(
  "S3_ACCESS_KEY_ID:s3/access-key-id:${SHARED}"
  "S3_SECRET_ACCESS_KEY:s3/secret-access-key:${SHARED}"
  # ADR-078 add.5 §5: one IRM heartbeat per cluster. This cell's own, from its
  # own file; the hub's is at the root, below. The IRM alert URL and the mail
  # password reach each cell through hub-operator's fleet copy.
  "GRAFANA_IRM_HEARTBEAT_URL:grafana-cloud/irm-heartbeat-url-${CELL_ID}:${SHARED}"
  # The hub's alert delivery, at the root.
  "GRAFANA_IRM_ALERTMANAGER_URL:grafana-cloud/irm-alertmanager-url:/"
  "GRAFANA_IRM_HEARTBEAT_URL:grafana-cloud/irm-heartbeat-url:/"
  "hcloud-token:hetzner/token:/"
)

log()  { printf '%s\n' "$*"; }
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# ── Credentials ─────────────────────────────────────────────────────────────
if [[ -z "${INFISICAL_CLIENT_ID:-}" || -z "${INFISICAL_CLIENT_SECRET:-}" ]]; then
  if command -v kubectl >/dev/null && kubectl -n platform-security get secret infisical-auth >/dev/null 2>&1; then
    log "Reading Infisical credentials from platform-security/infisical-auth"
    INFISICAL_CLIENT_ID=$(kubectl -n platform-security get secret infisical-auth -o jsonpath='{.data.client-id}' | base64 -d)
    INFISICAL_CLIENT_SECRET=$(kubectl -n platform-security get secret infisical-auth -o jsonpath='{.data.client-secret}' | base64 -d)
  else
    usage "no Infisical credentials: set INFISICAL_CLIENT_ID and INFISICAL_CLIENT_SECRET, or point KUBECONFIG at the hub (it reads platform-security/infisical-auth)."
  fi
fi

log "=== Seeding external secrets ==="
log "    Infisical:  ${INFISICAL_URL}"
log "    Project:    ${PROJECT_SLUG}  (env: ${ENVIRONMENT})"
log "    Cell:       ${CELL_ID}"
log "    Source:     ${SECRETS_DIR}"
[[ $DRY_RUN -eq 1 ]] && log "    MODE:       dry run — nothing will be written"
log ""

TOKEN=$(curl -sS -X POST "${INFISICAL_URL}/api/v1/auth/universal-auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"clientId\":\"${INFISICAL_CLIENT_ID}\",\"clientSecret\":\"${INFISICAL_CLIENT_SECRET}\"}" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin).get("accessToken",""))' 2>/dev/null || true)
[[ -z "$TOKEN" ]] && fail "Infisical login failed — credentials rejected"

WORKSPACE_ID=$(curl -sS "${INFISICAL_URL}/api/v1/workspace" -H "Authorization: Bearer ${TOKEN}" \
  | python3 -c "
import json,sys
for w in json.load(sys.stdin).get('workspaces',[]):
    if w.get('slug')=='${PROJECT_SLUG}': print(w['id']); break
" 2>/dev/null || true)
[[ -z "$WORKSPACE_ID" ]] && fail "no project with slug '${PROJECT_SLUG}'"

seeded=0; skipped=0; missing=0

for entry in "${MAPPINGS[@]}"; do
  KEY="${entry%%:*}"; rest="${entry#*:}"
  FILE="${rest%%:*}"; PATH_="${rest#*:}"
  SRC="${SECRETS_DIR}/${FILE}"

  if [[ ! -f "$SRC" ]]; then
    log "  MISSING  ${KEY}  (no ${FILE} in ${SECRETS_DIR})"
    missing=$((missing+1)); continue
  fi

  # Trailing newlines are the classic cause of a credential that looks correct
  # and authenticates nowhere.
  VALUE=$(tr -d '\r\n' < "$SRC")
  [[ -z "$VALUE" ]] && { log "  MISSING  ${KEY}  (${FILE} is empty)"; missing=$((missing+1)); continue; }

  if [[ $DRY_RUN -eq 1 ]]; then
    log "  WOULD    ${KEY}  <- ${FILE}  -> ${PATH_}"
    seeded=$((seeded+1)); continue
  fi

  body=$(python3 -c "
import json,sys
print(json.dumps({'workspaceId':sys.argv[1],'environment':sys.argv[2],'secretPath':sys.argv[3],
                  'secretValue':sys.argv[4],'type':'shared'}))" "$WORKSPACE_ID" "$ENVIRONMENT" "$PATH_" "$VALUE")

  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "${INFISICAL_URL}/api/v3/secrets/raw/${KEY}" \
    -H 'Content-Type: application/json' -H "Authorization: Bearer ${TOKEN}" -d "$body")

  # 409 means it already exists; update instead of failing the run.
  if [[ "$code" == "409" || "$code" == "400" ]]; then
    code=$(curl -sS -o /dev/null -w '%{http_code}' -X PATCH "${INFISICAL_URL}/api/v3/secrets/raw/${KEY}" \
      -H 'Content-Type: application/json' -H "Authorization: Bearer ${TOKEN}" -d "$body")
    [[ "$code" =~ ^2 ]] && { log "  UPDATED  ${KEY}  -> ${PATH_}"; skipped=$((skipped+1)); continue; }
  fi

  if [[ "$code" =~ ^2 ]]; then
    log "  SEEDED   ${KEY}  -> ${PATH_}"; seeded=$((seeded+1))
  else
    log "  FAILED   ${KEY}  (HTTP ${code})"; missing=$((missing+1))
  fi
done

log ""
log "seeded=${seeded} updated=${skipped} missing=${missing}"
[[ $missing -gt 0 ]] && log "NOTE: missing entries have no source file — supply them in ${SECRETS_DIR} and re-run."
if [[ $DRY_RUN -eq 1 ]]; then
  log ""
  log "To write these, run:"
  log "  INFISICAL_BASE_URL=${INFISICAL_URL}${KUBECONFIG:+ KUBECONFIG=${KUBECONFIG}} $0 ${CELL_ID}"
fi
exit 0
