#!/usr/bin/env bash
set -euo pipefail

# --check reports and changes nothing: no settings page is opened and nothing
# waits on a keypress, so `kakt release check` can run it. It exits non-zero when
# a package is unreachable in either direction, because both directions break a
# release and neither is visible until it does.
CHECK=0
[ "${1:-}" = "--check" ] && CHECK=1

# Packages that must be public for anonymous GHCR pull.
# Format: "label|repository|settings_url"
PKG="https://github.com/orgs/kamarkat/packages/container"
PACKAGES=(
  "environment-manager|kamarkat/charts/environment-manager|$PKG/charts%2Fenvironment-manager/settings"
  "platform|kamarkat/charts/platform|$PKG/charts%2Fplatform/settings"
  "universal-tenant|kamarkat/charts/universal-tenant|$PKG/charts%2Funiversal-tenant/settings"
  "tenant-public-tls|kamarkat/charts/tenant-public-tls|$PKG/charts%2Ftenant-public-tls/settings"
  # The three spoke/workload charts, added 2026-11-14. They were published
  # private, which a tenant cannot pull: ArgoCD holds no OCI credential and the
  # nodes hold no imagePullSecret. platform-spoke-gateway is referenced four
  # times by the released management bundle. The release failed on a 403 from
  # the already-published check, not here, because they were not listed.
  "platform-spoke-gateway|kamarkat/charts/platform-spoke-gateway|$PKG/charts%2Fplatform-spoke-gateway/settings"
  "platform-spoke-datasources|kamarkat/charts/platform-spoke-datasources|$PKG/charts%2Fplatform-spoke-datasources/settings"
  "platform-workload|kamarkat/charts/platform-workload|$PKG/charts%2Fplatform-workload/settings"
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

# WHICH REPOSITORY MAY PUSH THIS PACKAGE. Anonymous pull above is what a cluster
# needs; this is what CI needs, and the two are independent -- every package here
# was public and none could be written to.
#
# An org package is linked to a repository when it is created by that
# repository's GITHUB_TOKEN, or when the pushed artefact carries
# org.opencontainers.image.source. A package with no link grants Actions access
# to NO repository, so a workflow is refused write -- "403 Forbidden" on
# POST .../blobs/uploads/ -- however it declares `packages: write`. The
# 2026-11-04 rename to this org re-created all 17 packages by hand, so none was
# linked and the first CI release after it could not write a single blob, while
# every package still read PUBLIC here.
#
# Reported from the record exists() already fetches, because that state has no
# other way to be seen: GitHub exposes a package's Actions access only in the UI,
# and `repository` being null is the one part of it the API will admit to.
# Fixing it is UI work on the same settings page this script already opens.
linked_repo() {
  local org="${1%%/*}" rest="${1#*/}"
  gh api "/orgs/${org}/packages/container/${rest//\//%2F}" \
    --jq '.repository.full_name // ""' 2>/dev/null || echo ""
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
UNLINKED=()

for entry in "${PACKAGES[@]}"; do
  IFS='|' read -r label repository url <<< "$entry"
  printf "  %-30s " "$label"

  if check_public "$repository"; then
    if [ -n "$(linked_repo "$repository")" ]; then
      echo "PUBLIC"
    else
      echo "PUBLIC, NOT LINKED"
      UNLINKED+=("$label|$url")
    fi
  elif exists "$repository"; then
    echo "PRIVATE"
    PRIVATE+=("$label|$url")
  else
    echo "NOT PUBLISHED"
    ABSENT+=("$label")
  fi
done

if [ ${#UNLINKED[@]} -gt 0 ]; then
  echo ""
  echo "${#UNLINKED[@]} package(s) are pullable but NOT LINKED to a repository, so CI"
  echo "cannot push them -- a release fails with 403 Forbidden on blob upload:"
  for entry in "${UNLINKED[@]}"; do
    IFS='|' read -r label url <<< "$entry"
    echo "  $label"
    echo "    $url"
  done
  echo ""
  echo "On each settings page: Manage Actions access > Add repository >"
  echo "kamarkat/kamarkat > role Write. (Connect repository, where offered, does"
  echo "the same by inheritance.) New packages link themselves from now on: the"
  echo "charts carry sources[0] and the images an image.source label."
fi

if [ ${#ABSENT[@]} -gt 0 ]; then
  echo ""
  echo "${#ABSENT[@]} not published yet: ${ABSENT[*]}"
  echo "Visibility is set after the first publish; there is no settings page before it."
fi

echo ""

if [ "$CHECK" = 1 ]; then
  # Absent is not a failure: GHCR has no settings page for a package before its
  # first publish, so a new artefact is legitimately absent here. It will come
  # out private, which is why the note above says to flip it afterwards.
  if [ ${#PRIVATE[@]} -eq 0 ] && [ ${#UNLINKED[@]} -eq 0 ]; then
    echo "every package is publicly pullable and linked to a repository"
    exit 0
  fi
  echo "release would fail: ${#PRIVATE[@]} package(s) not publicly pullable," >&2
  echo "${#UNLINKED[@]} that CI cannot push to. Both are fixed in the GitHub UI --" >&2
  echo "run 'bash scripts/package/flip-public.sh' to be walked through them." >&2
  exit 1
fi

if [ ${#PRIVATE[@]} -eq 0 ]; then
  if [ ${#UNLINKED[@]} -eq 0 ]; then
    echo "All packages are public and linked!"
    echo ""
    exit 0
  fi
  # Non-zero: every package is pullable, and a release would still fail on the
  # first push. Reporting that as success is how this went unnoticed until a
  # release hit it.
  echo "All packages are public, but ${#UNLINKED[@]} cannot be pushed to by CI."
  echo ""
  exit 1
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
