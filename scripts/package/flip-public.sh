#!/usr/bin/env bash
set -euo pipefail

# Packages that must be public for anonymous GHCR pull.
# Format: "label|check_type|check_arg|settings_url"
PKG="https://github.com/orgs/kamarkat/packages/container"
PACKAGES=(
  "environment-manager|helm|kamarkat/charts/environment-manager|$PKG/charts%2Fenvironment-manager/settings"
  "platform|helm|kamarkat/charts/platform|$PKG/charts%2Fplatform/settings"
  "universal-tenant|helm|kamarkat/charts/universal-tenant|$PKG/charts%2Funiversal-tenant/settings"
  "tenant-public-tls|helm|kamarkat/charts/tenant-public-tls|$PKG/charts%2Ftenant-public-tls/settings"
  "hub-operator|image|kamarkat/kamarkat/hub-operator|$PKG/kamarkat%2Fhub-operator/settings"
  "auth-proxy|image|kamarkat/kamarkat/auth-proxy|$PKG/kamarkat%2Fauth-proxy/settings"
  "kube-sbt|image|kamarkat/kamarkat/kube-sbt|$PKG/kamarkat%2Fkube-sbt/settings"
  "mcp-server|image|kamarkat/kamarkat/mcp-server|$PKG/kamarkat%2Fmcp-server/settings"
  "ephemeral-job-operator|image|kamarkat/kamarkat/ephemeral-job-operator|$PKG/ephemeral-job-operator/settings"
  "ephemeral-vm-provisioner|image|kamarkat/kamarkat/ephemeral-vm-provisioner|$PKG/ephemeral-vm-provisioner/settings"
  "spoke-identity-operator|image|kamarkat/kamarkat/spoke-identity-operator|$PKG/spoke-identity-operator/settings"
  "workspace-sync|image|kamarkat/kamarkat/workspace-sync|$PKG/workspace-sync/settings"
)

check_helm() {
  helm show chart "oci://ghcr.io/$1" --version 0.1.11 >/dev/null 2>&1
}

check_image() {
  local token http_code
  token=$(curl -s "https://ghcr.io/token?scope=repository:$1:pull" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('token',''))" 2>/dev/null || echo "")
  if [ -z "$token" ]; then
    return 1
  fi
  http_code=$(curl -s -o /dev/null -w "%{http_code}" \
    -H "Authorization: Bearer $token" \
    -H "Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json" \
    "https://ghcr.io/v2/$1/manifests/latest" 2>/dev/null || echo "000")
  [ "$http_code" = "200" ]
}

echo ""
echo "Checking package visibility..."
echo ""

PRIVATE=()

for entry in "${PACKAGES[@]}"; do
  IFS='|' read -r label type check_arg url <<< "$entry"
  printf "  %-30s " "$label"

  if [ "$type" = "helm" ]; then
    if check_helm "$check_arg"; then
      echo "PUBLIC"
    else
      echo "PRIVATE"
      PRIVATE+=("$label|$url")
    fi
  else
    if check_image "$check_arg"; then
      echo "PUBLIC"
    else
      echo "PRIVATE"
      PRIVATE+=("$label|$url")
    fi
  fi
done

echo ""

if [ ${#PRIVATE[@]} -eq 0 ]; then
  echo "All packages are public!"
  echo ""
  exit 0
fi

echo "========================================="
echo "  ${#PRIVATE[@]} package(s) still PRIVATE"
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
