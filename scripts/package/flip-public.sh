#!/usr/bin/env bash
set -euo pipefail

# Packages that must be public for anonymous GHCR pull.
# Format: "label|repository|settings_url"
PKG="https://github.com/orgs/kamarkat/packages/container"
PACKAGES=(
  "environment-manager|kamarkat/charts/environment-manager|$PKG/charts%2Fenvironment-manager/settings"
  "platform|kamarkat/charts/platform|$PKG/charts%2Fplatform/settings"
  "universal-tenant|kamarkat/charts/universal-tenant|$PKG/charts%2Funiversal-tenant/settings"
  "tenant-public-tls|kamarkat/charts/tenant-public-tls|$PKG/charts%2Ftenant-public-tls/settings"
  "hub-operator|kamarkat/kamarkat/hub-operator|$PKG/kamarkat%2Fhub-operator/settings"
  "auth-proxy|kamarkat/kamarkat/auth-proxy|$PKG/kamarkat%2Fauth-proxy/settings"
  "kube-sbt|kamarkat/kamarkat/kube-sbt|$PKG/kamarkat%2Fkube-sbt/settings"
  "mcp-server|kamarkat/kamarkat/mcp-server|$PKG/kamarkat%2Fmcp-server/settings"
  "ephemeral-job-operator|kamarkat/kamarkat/ephemeral-job-operator|$PKG/kamarkat%2Fephemeral-job-operator/settings"
  "ephemeral-vm-provisioner|kamarkat/kamarkat/ephemeral-vm-provisioner|$PKG/kamarkat%2Fephemeral-vm-provisioner/settings"
  "spoke-identity-operator|kamarkat/kamarkat/spoke-identity-operator|$PKG/kamarkat%2Fspoke-identity-operator/settings"
  "workspace-sync|kamarkat/kamarkat/workspace-sync|$PKG/kamarkat%2Fworkspace-sync/settings"
  # Pulled with no imagePullSecret -- support-agent by the hub and every spoke,
  # postgres-partman by the billing cluster (CNPG pulls it on each node), so
  # both must be anonymously pullable and neither was listed.
  "support-agent|kamarkat/kamarkat/support-agent|$PKG/kamarkat%2Fsupport-agent/settings"
  "postgres-partman|kamarkat/kamarkat/postgres-partman|$PKG/kamarkat%2Fpostgres-partman/settings"
)

# One check for charts and images alike: ask GHCR for an anonymous pull token and
# list the repository's tags. Anonymous pull is the thing being tested, so it is
# tested directly.
#
# Version-free on purpose. Charts were checked by asking for version 0.1.11 by
# name, so every chart in an organisation that had never published that version
# read as not public whatever its real visibility was. Images were checked
# against the `latest` tag, which the chart releases do not carry.
#
# A package that does not exist reads the same as a private one HERE: GHCR
# returns no token and 403 for both and will not say which. That is a property
# of the anonymous endpoint, not of the registry -- `exists` below asks the
# authenticated API, which does distinguish them, so a package nobody has
# published is reported as absent instead of sending someone to a settings page
# that 404s.
# Whether the package exists at all. Only consulted for one that is not publicly
# pullable, to say WHY. Authenticated, because that is the only way to tell an
# absent package from a private one; `gh` is required rather than optional, since
# guessing here is what produced the 404.
exists() {
  local org="${1%%/*}" rest="${1#*/}"
  gh api "/orgs/${org}/packages/container/${rest//\//%2F}" >/dev/null 2>&1
}

check_public() {
  local token http_code
  token=$(curl -s "https://ghcr.io/token?scope=repository:$1:pull" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('token',''))" 2>/dev/null || echo "")
  [ -n "$token" ] || return 1
  http_code=$(curl -s -o /dev/null -w "%{http_code}" \
    -H "Authorization: Bearer $token" \
    "https://ghcr.io/v2/$1/tags/list?n=1" 2>/dev/null || echo "000")
  [ "$http_code" = "200" ]
}

command -v gh >/dev/null || {
  echo "flip-public: gh is required -- it is what tells an absent package from a" >&2
  echo "             private one. Install it, or read the visibility column in" >&2
  echo "             https://github.com/orgs/kamarkat/packages" >&2
  exit 1
}
gh auth status >/dev/null 2>&1 || {
  echo "flip-public: gh is not authenticated; run 'gh auth login'" >&2
  exit 1
}

echo ""
echo "Checking package visibility..."
echo ""

PRIVATE=()
ABSENT=()

for entry in "${PACKAGES[@]}"; do
  IFS='|' read -r label repository url <<< "$entry"
  printf "  %-30s " "$label"

  if check_public "$repository"; then
    echo "PUBLIC"
  elif exists "$repository"; then
    echo "PRIVATE"
    PRIVATE+=("$label|$url")
  else
    echo "NOT PUBLISHED"
    ABSENT+=("$label")
  fi
done

if [ ${#ABSENT[@]} -gt 0 ]; then
  echo ""
  echo "${#ABSENT[@]} not published yet: ${ABSENT[*]}"
  echo "Visibility is set after the first publish; there is no settings page before it."
fi

echo ""

if [ ${#PRIVATE[@]} -eq 0 ]; then
  echo "All packages are public!"
  echo ""
  exit 0
fi

echo "========================================="
echo "  ${#PRIVATE[@]} package(s) PRIVATE"
echo "========================================="
echo ""
echo "For each package:"
echo "  1. Click Package settings (right sidebar)"
echo "  2. Scroll to Danger Zone > Change visibility"
echo "  3. Select Public > confirm"
echo "  4. Press Enter here to continue"
echo ""
echo "========================================="
echo ""

TOTAL=${#PRIVATE[@]}

for i in "${!PRIVATE[@]}"; do
  IFS='|' read -r label url <<< "${PRIVATE[$i]}"
  idx=$((i + 1))

  echo "[$idx/$TOTAL] $label"
  echo "    $url"
  echo ""

  open "$url"

  if [ "$idx" -lt "$TOTAL" ]; then
    read -rp "Press Enter to continue to next package... "
    echo ""
  fi
done

echo ""
echo "========================================="
echo "  Done! Re-run to verify all are public"
echo "========================================="
echo ""
