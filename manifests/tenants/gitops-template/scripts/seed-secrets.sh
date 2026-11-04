#!/usr/bin/env bash
# Supply an application's secrets from a local .env file, by KEY=VALUE.
#
#   ./scripts/seed-secrets.sh <app> <environment> [--confirm]
#
# THE FILES
#
#   environments/<env>/<app>/.env.example   committed: every key this app
#                                           declares, empty, with the capability
#                                           each serves. No values, ever.
#   environments/<env>/<app>/.env           yours: a copy of the example with
#                                           the values filled in. Gitignored.
#
# WHAT IT DOES
#
#   1. Checks .env.example still lists exactly what the app declares. If the
#      declaration changed, it stops and says how to regenerate the example.
#   2. Without .env, copies .env.example to .env (mode 600) and stops, so you
#      can fill it in.
#   3. Shows which keys would be written, and writes nothing.
#   4. With --confirm, writes them.
#
# An entry left empty is never written: it would replace a supplied secret
# with nothing. Re-running with a new value for a key rotates it. For one key,
# `kakt fleet secrets set <env> <app> <KEY>` prompts with echo disabled and
# touches no file at all.
#
# WHAT IT DOES NOT DO
#
# Feed the whole file. Only keys declared under `secrets:` in
# environments/<env>/<app>/values.yaml are written; configuration belongs in
# `config:` there, in git, where it can be reviewed.
#
# The filtering and the example's content are not implemented here. Both come
# from `kakt fleet secrets`, the same command `status` and `set` use, so there
# is ONE answer to "which keys does this app declare" -- a second
# implementation in a script is a second answer, and the two disagree the
# moment either changes.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

usage() {
  cat >&2 <<EOF
Usage: $(basename "$0") <app> <environment> [--confirm]

  app           names environments/<env>/<app>/
  environment   names environments/<env>/ in this repository
  --confirm     actually write; without it, nothing is written

Requires: kakt on PATH, and KUBECONFIG pointing at this box's hub.
EOF
  exit 1
}

[[ $# -lt 2 || $# -gt 3 ]] && usage
APP="$1"
ENVIRONMENT="$2"
CONFIRM=""
if [[ $# -eq 3 ]]; then
  [[ "$3" == "--confirm" ]] || usage
  CONFIRM="--confirm"
fi

APP_DIR="${REPO_ROOT}/environments/${ENVIRONMENT}/${APP}"
# What to call the files when talking to a person: they are standing in the
# repository root, not holding an absolute path.
APP_DIR_REL="environments/${ENVIRONMENT}/${APP}"
ENV_FILE="${APP_DIR}/.env"
EXAMPLE_FILE="${APP_DIR}/.env.example"

[[ -d "$APP_DIR" ]] || {
  echo "No application at ${APP_DIR_REL}/." >&2
  echo "The environment and app name a directory in this repository." >&2
  exit 1
}
command -v kakt >/dev/null || {
  echo "kakt is not on PATH. It is the same binary that scaffolded this" >&2
  echo "repository, and it holds the credential for this box's secret store." >&2
  exit 1
}
# Plain test rather than ${KUBECONFIG:?...}: inside that expansion bash parses
# an apostrophe in the message as an opening quote, even within double quotes,
# and the script fails to parse rather than printing the message.
if [[ -z "${KUBECONFIG:-}" ]]; then
  echo "KUBECONFIG is not set. It must point at this box's hub: the credential" >&2
  echo "for the secret store is held in the cluster, not on this machine." >&2
  exit 1
fi

# The example is generated, never hand-written: transcribing the declaration
# by hand is where a typo becomes a key the import silently skips. A stale
# example is refused rather than used, because a key missing from it is a key
# nobody is asked for.
REGENERATE="kakt fleet secrets template ${ENVIRONMENT} ${APP} --repo . > ${APP_DIR_REL}/.env.example"
if [[ ! -f "$EXAMPLE_FILE" ]]; then
  echo "${APP_DIR_REL}/.env.example does not exist. Generate and commit it:" >&2
  echo "  ${REGENERATE}" >&2
  exit 1
fi
# The keys are compared, not the comments: the guidance text may differ by
# kakt version, the set of keys may not.
keys() { grep -E '^[A-Za-z_][A-Za-z0-9_]*=' | cut -d= -f1; }
if ! diff -q <(kakt fleet secrets template "$ENVIRONMENT" "$APP" --repo "$REPO_ROOT" | keys) <(keys < "$EXAMPLE_FILE") >/dev/null; then
  echo "${APP_DIR_REL}/.env.example no longer matches the secrets ${APP} declares." >&2
  echo "Regenerate and commit it, then copy any new keys into your .env:" >&2
  echo "  ${REGENERATE}" >&2
  exit 1
fi

if [[ ! -f "$ENV_FILE" ]]; then
  cp "$EXAMPLE_FILE" "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  echo "Created ${APP_DIR_REL}/.env from .env.example."
  echo ""
  echo "Fill in the values, then run this again."
  echo "It is gitignored; it must never be committed."
  exit 0
fi

kakt fleet secrets import "$ENVIRONMENT" "$APP" \
  --from-env "$ENV_FILE" \
  --repo "$REPO_ROOT" \
  --kubeconfig "$KUBECONFIG" \
  $CONFIRM

if [[ -n "$CONFIRM" ]]; then
  echo ""
  echo "Verify, and see anything still outstanding:"
  echo "  kakt fleet secrets status ${ENVIRONMENT} ${APP} --repo . --kubeconfig \$KUBECONFIG"
  echo ""
  echo "Then delete ${APP_DIR_REL}/.env -- its values are now held where the"
  echo "app's declaration says they are, and a copy here is one nothing rotates."
fi
