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
# A package that does not exist reads the same as a private one: GHCR returns no
# token and 403 for both, and will not say which, so existence cannot be probed
# anonymously. Before the first publish a package therefore reads NOT PUBLIC,
# and the settings page it points at does not exist yet.
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

echo ""
echo "Checking package visibility..."
echo ""

PRIVATE=()

for entry in "${PACKAGES[@]}"; do
  IFS='|' read -r label repository url <<< "$entry"
  printf "  %-30s " "$label"

  if check_public "$repository"; then
    echo "PUBLIC"
  else
    echo "NOT PUBLIC"
    PRIVATE+=("$label|$url")
  fi
done

echo ""

if [ ${#PRIVATE[@]} -eq 0 ]; then
  echo "All packages are public!"
  echo ""
  exit 0
fi

echo "========================================="
echo "  ${#PRIVATE[@]} package(s) not publicly pullable"
echo "========================================="
echo ""
echo "A package nobody has published yet appears here too, with no settings"
echo "page to open. Publish it first."
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
