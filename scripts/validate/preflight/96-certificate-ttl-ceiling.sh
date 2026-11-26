#!/usr/bin/env bash
# A certificate profile's TTL is a server-side CEILING. Requesting more than the
# ceiling does not fail when the Certificate is applied — cert-manager accepts it,
# the issuer refuses or truncates at signing time, and the damage appears at the
# FIRST RENEWAL, one full certificate lifetime later. For a 7-day certificate that
# is a week; for the 90-day ones this replaced, it was a quarter.
#
# That delay is the whole problem: whoever changes a duration or a ceiling gets no
# feedback, and the failure surfaces as unexplained mTLS breakage long after.
#
# So: every Certificate issued through an OpenBao issuer must request no more than
# its profile allows. Ceilings are read from internal/pki, the single authoritative
# registry (ADR-035 addendum §2), and the issuer→profile mapping from the
# issuers' pki_int/sign/<profile> paths, so this check follows both as they change.
validate_certificate_ttl_ceiling() {
    section "Certificate durations fit their profile ceiling (ADR-035 §5)"

    local out rc=0
    out=$(cd "$VALIDATE_ROOT" && python3 - <<'PY'
import glob, os, re, yaml

reg = "internal/pki/profiles.go"
if not os.path.exists(reg):
    print("SKIP\tinternal/pki not found")
    raise SystemExit

# slug -> ceiling in days
ceilings = {m.group(1): int(m.group(2)) for m in
            re.finditer(r'\{Slug:\s*"([^"]+)",\s*TTLDays:\s*(\d+)\}', open(reg).read())}
if not ceilings:
    print("SKIP\tno profiles parsed from internal/pki")
    raise SystemExit

# issuer name -> profile slug, from the ClusterIssuer manifests
issuer_profile = {}
for f in glob.glob("manifests/**/*.yaml", recursive=True):
    try:
        docs = [d for d in yaml.safe_load_all(open(f)) if d]
    except Exception:
        continue
    for d in docs:
        # A top-level LIST document (templated-fields.yaml and friends) is not a
        # resource. Calling .get() on one raised AttributeError, python exited
        # non-zero, and the empty stdout that produced was read as "no findings".
        if not isinstance(d, dict):
            continue
        if d.get("kind") not in ("ClusterIssuer", "Issuer"):
            continue
        path = ((d.get("spec") or {}).get("vault") or {}).get("path", "")
        if path.startswith("pki_int/sign/"):
            issuer_profile[d["metadata"]["name"]] = path[len("pki_int/sign/"):]

# The hub's two Vault ClusterIssuers are rendered by a Helm range over a dict of
# names and profile values, which no YAML decoder sees; read both halves.
tpl = "manifests/hub-core-services/security/templates/openbao-issuers.yaml"
vals = "manifests/hub-core-services/security/values.yaml"
if os.path.exists(tpl) and os.path.exists(vals):
    profiles = (yaml.safe_load(open(vals)) or {}).get("certificateProfile") or {}
    for name, key in re.findall(r'"(openbao-[a-z-]+)" \.Values\.certificateProfile\.([A-Za-z]+)', open(tpl).read()):
        if profiles.get(key):
            issuer_profile[name] = profiles[key]

# Only profiles internal/pki declares carry a ceiling here; per-spoke roles are
# hub-operator's and capped by it.
issuer_profile = {k: v for k, v in issuer_profile.items() if v in ceilings}
if not issuer_profile:
    print("BAD\t-\tno OpenBao issuer maps to an internal/pki profile; this check would inspect nothing")

def hours(v):
    if not isinstance(v, str):
        return None
    m = re.fullmatch(r"(\d+)([hmd])", v.strip())
    if not m:
        return None
    n, unit = int(m.group(1)), m.group(2)
    return n * {"h": 1, "m": 1 / 60, "d": 24}[unit]

for f in glob.glob("manifests/**/*.yaml", recursive=True):
    try:
        docs = [d for d in yaml.safe_load_all(open(f)) if d]
    except Exception:
        continue
    for d in docs:
        # A top-level LIST document (templated-fields.yaml and friends) is not a
        # resource. Calling .get() on one raised AttributeError, python exited
        # non-zero, and the empty stdout that produced was read as "no findings".
        if not isinstance(d, dict):
            continue
        if d.get("kind") != "Certificate":
            continue
        spec = d.get("spec") or {}
        ref = (spec.get("issuerRef") or {}).get("name", "")
        slug = issuer_profile.get(ref)
        if slug is None:
            continue                      # not an infisical-backed issuer
        ceiling = ceilings.get(slug)
        if ceiling is None:
            print("\t".join(["BAD", d["metadata"]["name"],
                             f"issuer {ref} names profile {slug!r}, absent from internal/pki"]))
            continue
        h = hours(spec.get("duration"))
        if h is None:
            continue                      # no explicit duration: cert-manager default
        if h > ceiling * 24:
            print("\t".join(["BAD", d["metadata"]["name"],
                             f"requests {spec['duration']} through {slug} (ceiling {ceiling}d) "
                             f"— renewal would break {h/24:.0f} days from issuance, not at apply time"]))
PY
) || rc=$?

    if grep -q $'^SKIP\t' <<< "$out" 2>/dev/null; then
        warn "could not read the profile registry — TTL ceilings unverified"
        return 0
    fi

    # rc is captured because an EMPTY result and a CRASHED checker are the same
    # string, and `-z "$out"` below cannot tell them apart. A checker that could
    # not run has not found nothing.
    if (( rc != 0 )); then
        hard_fail "the checker exited ${rc} — it inspected nothing, and an empty result from a crashed checker is not a passing one"
        return 1
    fi

    if [[ -z "$out" ]]; then
        pass "every OpenBao-issued Certificate fits its profile ceiling"
        return 0
    fi

    local tag name msg
    while IFS=$'\t' read -r tag name msg; do
        [[ "$tag" == "BAD" ]] || continue
        hard_fail "Certificate $name — $msg"
    done <<< "$out"
}
