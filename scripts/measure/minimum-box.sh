#!/usr/bin/env bash
# Measure what a supported minimum box actually costs.
#
# ADR-070 states a target of EUR 150-400/month and is explicit that the number
# "is a constraint to validate, not an established fact... Until a box is built
# and measured it stays a constraint, and no other decision may rest on it as
# though it were measured."
#
# This is the instrument. It reads a running cluster and prices exactly the
# inventory ADR-070 names: cluster machinery, mandatory capabilities, storage,
# observability, artefact mirroring, and whatever HA the topology requires.
#
# PRICES ARE FETCHED LIVE, per location, from Hetzner's pricing API. A checked-in
# price list goes stale between the day it is written and the day the figure is
# quoted, and a stale number is exactly how a constraint becomes an "established
# fact" nobody rechecked. Object storage and artefact mirroring are not in that
# API and stay declared in prices.yaml; the run refuses to report a total while
# either is unset, because a box's cost is not the part of it that happened to
# be machine-readable.
#
# WHAT THIS DOES NOT DO. It does not decide the topology. ADR-070 settles that
# first, from the obligation ADR-066 and ADR-069 create, and measures the result
# -- so this prices what a box IS running. Pointing it at a box whose replicas
# were reduced to hit the number produces a figure that means nothing, which is
# the failure ADR-070's second clause refuses.
#
#   minimum-box.sh <kubeconfig>
#
# HCLOUD_TOKEN is read from the environment, or from this operator's store.
# The pricing endpoint is read-only and the token is never sent anywhere else.
set -euo pipefail

KUBECONFIG_PATH="${1:?usage: minimum-box.sh <kubeconfig>}"
export KUBECONFIG="$KUBECONFIG_PATH"

HERE="$(cd "$(dirname "$0")" && pwd)"
PRICES="$HERE/prices.yaml"
[ -f "$PRICES" ] || { echo "no price declaration at $PRICES" >&2; exit 1; }

command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

TOKEN="${HCLOUD_TOKEN:-}"
KAMARKAT_STORE="${KAMARKAT_HOME:-$HOME/.kamarkat}"
if [ -z "$TOKEN" ] && [ -f "$KAMARKAT_STORE/hetzner/token" ]; then
    TOKEN="$(cat "$KAMARKAT_STORE/hetzner/token")"
fi
[ -n "$TOKEN" ] || {
    echo "no HCLOUD_TOKEN: prices are fetched live and cannot be guessed" >&2
    exit 1
}

echo "measuring $(kubectl config current-context)"
echo

pricing=$(curl -sS --max-time 30 -H "Authorization: Bearer ${TOKEN}" \
    https://api.hetzner.cloud/v1/pricing)
# A pricing document that did not arrive must not read as an empty price list:
# every line would report "not priced" and the run would look like a config
# problem in prices.yaml rather than a failed request.
jq -e '.pricing.server_types | length > 0' >/dev/null <<<"$pricing" || {
    echo "the pricing API returned nothing usable:" >&2
    head -c 400 <<<"$pricing" >&2; echo >&2
    exit 1
}

nodes=$(kubectl get nodes -o json)
pvcs=$(kubectl get pvc -A -o json 2>/dev/null || echo '{"items":[]}')
svcs=$(kubectl get svc -A -o json 2>/dev/null || echo '{"items":[]}')

# python3 -c with the payload on stdin. `python3 - <<'PY' <<<"$json"` gives stdin
# to the LAST redirection, so python would read the JSON as its own program.
priced=$(cat <<'PY'
import json, sys, collections, datetime
import yaml

prices_path, = sys.argv[1:2]
blob = json.load(sys.stdin)
inv, pricing, cfg = blob["inventory"], blob["pricing"]["pricing"], yaml.safe_load(open(prices_path)) or {}

# `net` throughout. VAT is a property of the account, not of the architecture,
# and ADR-070's range is an architecture target -- mixing a tax rate into it
# would make the same box pass or fail depending on who bought it. The rate the
# API reported for this account is printed so the difference is visible.
def net(p):
    return float(p["net"])

def by_location(entries, name, location):
    """A price for one named thing in one location.

    Hetzner prices per location and the spread is real, so a measurement that
    took the first price in the list would report a box in Helsinki at Falkenstein
    rates. Returns None rather than a fallback: an unknown price is a hole in the
    measurement, and filling it with a plausible number is the failure ADR-070's
    third clause is about.
    """
    for e in entries:
        if e.get("name") != name and e.get("type") != name:
            continue
        for p in e.get("prices", []):
            if p.get("location") == location:
                return net(p["price_monthly"])
        return None
    return None

lines, missing, total, notes = [], [], 0.0, []

# ── Nodes ───────────────────────────────────────────────────────────────────
groups = collections.Counter()
onprem = 0
for n in inv["nodes"]["items"]:
    labels = (n.get("metadata") or {}).get("labels") or {}
    itype = labels.get("node.kubernetes.io/instance-type") or labels.get("beta.kubernetes.io/instance-type")
    region = labels.get("topology.kubernetes.io/region") or labels.get("failure-domain.beta.kubernetes.io/region")
    # A node the tenant owns is not a line in a Hetzner bill (ADR-075). Counted
    # and reported, never priced, and never treated as a missing price -- a
    # hybrid box would otherwise look unmeasurable rather than cheaper.
    if not itype or not region:
        onprem += 1
        continue
    groups[(itype, region)] += 1

for (itype, region), count in sorted(groups.items()):
    unit = by_location(pricing.get("server_types", []), itype, region)
    label = f"{count}x {itype} @ {region}"
    if unit is None:
        missing.append(f"server type {itype!r} in {region!r} is not in the pricing API")
        lines.append((label, None))
    else:
        total += unit * count
        lines.append((label, unit * count))

if onprem:
    notes.append(f"{onprem} node(s) carry no Hetzner instance type and are not "
                 f"priced: on-premises capacity is the tenant's own hardware (ADR-075).")

# ── Primary IPv4, one per cloud node ────────────────────────────────────────
cloud_nodes = sum(groups.values())
if cloud_nodes:
    region = sorted(groups)[0][1]
    ip = by_location(pricing.get("primary_ips", []), "ipv4", region)
    if ip is None:
        missing.append(f"primary ipv4 price in {region!r}")
        lines.append((f"{cloud_nodes}x primary IPv4", None))
    else:
        total += ip * cloud_nodes
        lines.append((f"{cloud_nodes}x primary IPv4 @ {region}", ip * cloud_nodes))

# ── Block storage ───────────────────────────────────────────────────────────
gb = 0
for p in inv["pvcs"]["items"]:
    cap = ((p.get("status") or {}).get("capacity") or {}).get("storage", "0")
    digits = "".join(c for c in cap if c.isdigit())
    n = int(digits or 0)
    gb += n * 1024 if cap.endswith("Ti") else n if cap.endswith("Gi") else n // 1024 if cap.endswith("Mi") else 0
per_gb = ((pricing.get("volume") or {}).get("price_per_gb_month") or {})
if per_gb:
    total += net(per_gb) * gb
    lines.append((f"{gb}Gi block storage", net(per_gb) * gb))
else:
    missing.append("volume price_per_gb_month")
    lines.append((f"{gb}Gi block storage", None))

# ── Load balancers ──────────────────────────────────────────────────────────
lbs = collections.Counter()
for s in inv["svcs"]["items"]:
    if (s.get("spec") or {}).get("type") != "LoadBalancer":
        continue
    ann = ((s.get("metadata") or {}).get("annotations") or {})
    lbs[(ann.get("load-balancer.hetzner.cloud/type", "lb11"),
         ann.get("load-balancer.hetzner.cloud/location")
         or (sorted(groups)[0][1] if groups else None))] += 1
for (lbtype, region), count in sorted(lbs.items(), key=lambda x: str(x[0])):
    unit = by_location(pricing.get("load_balancer_types", []), lbtype, region) if region else None
    label = f"{count}x load balancer {lbtype}" + (f" @ {region}" if region else "")
    if unit is None:
        missing.append(f"load balancer type {lbtype!r} in {region!r}")
        lines.append((label, None))
    else:
        total += unit * count
        lines.append((label, unit * count))
if not lbs:
    lines.append(("0x load balancer", 0.0))

# ── The two lines the API does not carry ────────────────────────────────────
off = cfg.get("off_api") or {}
for key, label in (("object_storage", "object storage (backups)"),
                   ("artefact_mirror", "artefact mirroring")):
    v = (off.get(key) or {}).get("price_month")
    if v is None:
        missing.append(f"off_api.{key}.price_month in {prices_path}")
        lines.append((label, None))
    else:
        total += float(v)
        lines.append((label, float(v)))

# ── Report ──────────────────────────────────────────────────────────────────
width = max(len(l) for l, _ in lines)
for label, cost in lines:
    print(f"  {label:<{width}}   " + ("? (not priced)" if cost is None else f"EUR {cost:8.2f}"))
print("  " + "-" * (width + 16))
print(f"  {'total':<{width}}   EUR {total:8.2f} / month")
print()
print(f"  prices: live from api.hetzner.cloud, {datetime.datetime.now(datetime.timezone.utc):%Y-%m-%d %H:%MZ}, "
      f"net of VAT (account rate {pricing.get('vat_rate')}%)")
for n in notes:
    print("  " + n)
print()

target = cfg.get("target") or {}
low, high = target.get("low", 150), target.get("high", 400)

if missing:
    print("NOT A MEASUREMENT. These lines are not priced:")
    for m in missing:
        print("  - " + m)
    print()
    print("ADR-070: the range stays a constraint until a box is measured, and a")
    print("total assembled from the lines that happened to be priced is not a")
    print("measurement. No decision may rest on the figure above.")
    sys.exit(1)

print(f"ADR-070 target: EUR {low}-{high}/month")
if total <= high:
    print(f"within the target, by EUR {high - total:.2f}/month.")
else:
    print(f"EXCEEDS the target by EUR {total - high:.2f}/month.")
    print()
    print("ADR-070: this is a business decision, not an architectural one. What")
    print("changes is who the platform is for, what it charges, or which")
    print("capabilities are mandatory -- not the supported topology, and not the")
    print("replica counts that make it supportable.")
PY
)

python3 -c "$priced" "$PRICES" <<<"$(jq -n \
    --argjson n "$nodes" --argjson p "$pvcs" --argjson s "$svcs" --argjson pr "$pricing" \
    '{inventory:{nodes:$n,pvcs:$p,svcs:$s}, pricing:$pr}')"
