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

# THREE GATES, because a package has three independent states and only two of
# them can be read.
#
#   1 PULLABLE          public, so a tenant's cluster can pull it. ArgoCD holds
#                       no OCI credential and the nodes no imagePullSecret.
#                       Read by asking GHCR for an anonymous pull token.
#   2 CONNECTED         a repository is connected to the package, which grants
#                       that repository Actions access implicitly. Read from the
#                       package record's `repository`.
#   3 WRITE GRANTED     Manage Actions access names a repository with the Write
#                       role. NOT READABLE: GitHub documents nine endpoints for
#                       an organisation's packages and none of them exposes it,
#                       and actions-access, permissions, repositories and
#                       actions/permissions all return 404. So it is DECLARED,
#                       in actions-write-granted.txt, and read from there.
#
# CI can push when 2 or 3 holds. Gate 2 was read as though it were gate 3 and
# failed the release for it, on ten image packages whose Write role had already
# been granted by hand -- a check refusing a release that would have worked,
# because it was reading the field next to the one that mattered.
# Gate 2. Prints the connected repository, or nothing when the package has none.
#
# A FAILED CALL IS NOT AN ANSWER. This ended in `|| echo ""`, so a rate-limited
# or transient request was reported as "no repository connected" -- and this
# makes about fifty calls in a row, so that happened: a package whose link five
# consecutive direct queries confirm read as unconnected in one run and
# connected in the next. The same rule the already-published gate states:
# anything other than a clean answer is unknown, not absent.
linked_repo() {
  local org="${1%%/*}" rest="${1#*/}" out status
  out=$(gh api "/orgs/${org}/packages/container/${rest//\//%2F}" \
    --jq '.repository.full_name // ""' 2>&1)
  status=$?
  if [ "$status" -ne 0 ]; then
    LINK_QUERY_ERROR="$out"
    return 1
  fi
  printf '%s' "$out"
}

# Gate 3, from the record beside this script. The label, as PACKAGES prints it.
WRITE_GRANTED_FILE="$(dirname "${BASH_SOURCE[0]}")/actions-write-granted.txt"
write_granted() {
  [ -f "$WRITE_GRANTED_FILE" ] || return 1
  grep -qxF "$1" <(grep -v "^#" "$WRITE_GRANTED_FILE" | grep -v "^[[:space:]]*$")
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
echo "  PULLABLE and CONNECTED are read from GitHub. WRITE-RECORDED is read from"
echo "  $(basename "$WRITE_GRANTED_FILE"), because GitHub exposes Manage Actions"
echo "  access through no API -- 'no' there means not recorded, not that the role"
echo "  is missing. OK needs all three: a package CI can push but nobody can trace"
echo "  back to this repository is not a package this release ships."
echo ""

printf "  %-30s %-9s %-10s %-15s %s\n" PACKAGE PULLABLE CONNECTED WRITE-RECORDED OK
printf "  %-30s %-9s %-10s %-15s %s\n" "------------------------------" -------- --------- -------------- --------

PRIVATE=()      # gate 1 failed: a tenant's cluster cannot pull it
WORK=()         # any gate failed, with the gates named
ABSENT=()       # never published, so it has no settings page yet
UNREADABLE=()   # the link could not be read, so this run established nothing

# ALL THREE GATES, FOR EVERY PACKAGE. Each is a separate fact about a separate
# control, so each is evaluated and printed for each package. They were an
# if/elif chain -- connected, else write-declared, else refuse -- which meant a
# connected package never had its Write grant looked at and a write-granted one
# never had its connection reported. One column each, so the row says which
# controls are set rather than which branch was taken.
#
# PUSHABLE is the conclusion, not a fourth gate: connecting a repository grants
# Actions access implicitly, so either of gates 2 and 3 is enough on its own.
for entry in "${PACKAGES[@]}"; do
  IFS='|' read -r label repository url <<< "$entry"

  if ! exists "$repository"; then
    printf "  %-30s %s\n" "$label" "NOT PUBLISHED"
    ABSENT+=("$label")
    continue
  fi

  if check_public "$repository"; then pullable=yes; else pullable=no; fi

  # Unknown is its own value, and it is never read as "no": a run that could not
  # ask has established nothing, and says so rather than reporting the absence of
  # an answer as the absence of a link.
  LINK_QUERY_ERROR=""
  if connected="$(linked_repo "$repository")"; then
    if [ -n "$connected" ]; then connected_s=yes; else connected_s=no; fi
  else
    connected_s="UNKNOWN"
    UNREADABLE+=("$label|$url|${LINK_QUERY_ERROR}")
  fi

  if write_granted "$label"; then declared=yes; else declared=no; fi

  # ALL THREE, each required on its own. Connecting a repository is not merely
  # another way to grant Actions access: it is what gives the package a readme,
  # its discussions and its contributors on the package page, so a package CI can
  # push and nobody can trace is still wrong. A Write role does not excuse a
  # missing connection and a connection does not excuse an unrecorded role.
  if [ "$connected_s" = "UNKNOWN" ]; then
    ok="UNKNOWN"
  elif [ "$pullable" = yes ] && [ "$connected_s" = yes ] && [ "$declared" = yes ]; then
    ok=yes
  else
    ok=no
  fi

  printf "  %-30s %-9s %-10s %-15s %s\n" \
    "$label" "$pullable" "$connected_s" "$declared" "$ok"

  # One entry per package, naming every gate it fails, so a package with two
  # problems is one visit to one page rather than two.
  if [ "$ok" = no ]; then
    needs=""
    [ "$pullable" = no ]     && needs="${needs}${needs:+,}pullable"
    [ "$connected_s" = no ]  && needs="${needs}${needs:+,}connect"
    [ "$declared" = no ]     && needs="${needs}${needs:+,}write"
    WORK+=("$label|$url|$needs|$repository")
  fi
  true
done

if [ ${#UNREADABLE[@]} -gt 0 ]; then
  echo ""
  echo "${#UNREADABLE[@]} package(s) could not be read, so this run established nothing" >&2
  echo "about them. Run it again -- about fifty calls go out in a row and GitHub" >&2
  echo "rate-limits in bursts:" >&2
  for entry in ${UNREADABLE[@]+"${UNREADABLE[@]}"}; do
    IFS='|' read -r label url err <<< "$entry"
    echo "  $label: ${err:-no detail}" >&2
  done
  exit 1
fi

if [ ${#ABSENT[@]} -gt 0 ]; then
  echo ""
  echo "${#ABSENT[@]} not published yet: ${ABSENT[*]:-}"
  echo "A package has no settings page before its first publish."
fi

echo ""

if [ ${#WORK[@]} -eq 0 ]; then
  echo "Every package is pullable, connected to this repository, and recorded as"
  echo "granting it Write."
  echo ""
  exit 0
fi

if [ "$CHECK" = 1 ]; then
  echo "release would fail: ${#WORK[@]} package(s) do not pass all three gates:" >&2
  for entry in ${WORK[@]+"${WORK[@]}"}; do
    IFS='|' read -r label url needs repository <<< "$entry"
    echo "  $label: ${needs//,/, }" >&2
  done
  echo "Fix in the GitHub UI: run 'bash scripts/package/flip-public.sh'." >&2
  exit 1
fi

echo "========================================="
echo "  ${#WORK[@]} package(s) need a visit"
echo "========================================="
echo ""

TOTAL=${#WORK[@]}

for i in "${!WORK[@]}"; do
  IFS='|' read -r label url needs repository <<< "${WORK[$i]}"
  idx=$((i + 1))
  org="${repository%%/*}"
  name="${repository#*/}"
  # The package's own page, where the "Link this package to a repository" prompt
  # is. The settings page below holds visibility and Actions access.
  page="https://github.com/orgs/${org}/packages/container/package/${name//\//%2F}"

  echo "[$idx/$TOTAL] $label"
  case ",$needs," in
    *,pullable,*)
      echo "    NOT PULLABLE -- a tenant's cluster cannot pull it."
      echo "      $url"
      echo "      Danger Zone > Change visibility > Public > confirm"
      ;;
  esac
  case ",$needs," in
    *,connect,*)
      echo "    NOT CONNECTED -- nothing ties this package to its source. Connecting it"
      echo "    adds the readme, the discussions and the contributors to its page, and"
      echo "    grants this repository Actions access."
      echo "      $page"
      echo "      Connect repository > kamarkat/kamarkat"
      ;;
  esac
  case ",$needs," in
    *,write,*)
      echo "    WRITE NOT RECORDED -- grant it, then write it down, because GitHub"
      echo "    exposes Manage Actions access through no API."
      echo "      $url"
      echo "      Manage Actions access > Add repository > kamarkat/kamarkat > Write"
      echo "      then add '$label' to $(basename "$WRITE_GRANTED_FILE")"
      ;;
  esac
  echo ""

  case ",$needs," in
    *,connect,*) open "$page" ;;
    *)           open "$url" ;;
  esac

  if [ "$idx" -lt "$TOTAL" ]; then
    read -rp "Press Enter to continue to next package... "
    echo ""
  fi
done

echo ""
echo "========================================="
echo "  Done! Re-run to verify all three gates"
echo "  pass for every package."
echo "========================================="
echo ""
