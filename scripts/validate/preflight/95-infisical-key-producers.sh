#!/usr/bin/env bash
# Every ExternalSecret backed by Infisical names a key that something must put
# there. When nothing does, the failure is mute in a very specific way: the
# ExternalSecret reports "could not get secret data from provider" — which never
# says WHERE it looked or WHO was supposed to write it — and the consuming pod
# reports only CreateContainerConfigError. Neither names the missing key's owner.
#
# It has cost a full bootstrap four times: agentgateway-oidc-cookie-secret,
# waypoint-bff-client-secret, the Grafana Cloud set and the S3 backup pair all
# reached a rebuilt Infisical with no producer. The first was a genuine omission —
# its siblings hub-kratos-ui-cookie-secret and hub-hydra-system-secret were in the
# operator's registry and it simply was not.
#
# So: cross-check the keys the manifests CONSUME against the keys the hub-operator
# PRODUCES, and require that anything left over be declared here deliberately, with
# the reason it cannot be generated. Producers are read from the operator source, so
# adding a registry entry is enough — this check needs no edit to stay correct.
validate_infisical_key_producers() {
    section "Every Infisical-backed ExternalSecret key has a producer"

    local out rc=0
    out=$(cd "$VALIDATE_ROOT" && python3 - <<'PY'
import glob, os, re, yaml

# Credentials issued by systems OUTSIDE the platform, so nothing here can generate
# them. They are no longer hand-seeded: hub-bootstrap.sh step 6b builds a Secret
# from k8-secrets/<dir>/ and the operator's CLISecretMappings upload it. They stay
# listed so every run prints the set that needs real values on disk — an empty file
# is skipped rather than uploaded blank, and that skip is easy to miss.
EXTERNAL = {
    "S3_ACCESS_KEY_ID":              "k8-secrets/s3/access-key-id — Hetzner Object Storage key (starts 0A…, NOT the hcloud-token)",
    "S3_SECRET_ACCESS_KEY":          "k8-secrets/s3/secret-access-key — Hetzner Object Storage secret (CNPG Barman backups)",
    "GRAFANA_IRM_ALERTMANAGER_URL":  "k8-secrets/grafana-cloud/irm-alertmanager-url — IRM Alertmanager integration URL",
    "GRAFANA_IRM_HEARTBEAT_URL":     "k8-secrets/grafana-cloud/irm-heartbeat-url (hub) and irm-heartbeat-url-<cell> (each spoke) — one IRM heartbeat per cluster",
    # Uploaded from the zitadel-smtp Secret as the URL's PASSWORD, at the root,
    # for every cluster's Alertmanager; hub-operator copies it into each cell.
    "ALERT_SMTP_PASSWORD":           "k8-secrets/smtp/resend (or ZITADEL_SMTP_URL) — the same SMTP URL; its password",
    # Uploaded from the zitadel-smtp Secret as the URL's PASSWORD only, to the
    # owner's /<tenant>-mgmt folder (CLISecretMappings, TransformURLPassword).
    "zitadel_smtp_password":         "k8-secrets/smtp/resend (or ZITADEL_SMTP_URL) — the Resend SMTP URL, smtps://resend:<api-key>@smtp.resend.com:465",
}

# Known-missing producers awaiting a decision, reported as a WARNING rather than a
# failure so an open design question does not block a bootstrap. Anything not listed
# here and not produced is a hard failure — that is the point of the check.
# Removing an entry here is the last step of closing its gap.
PENDING = {
    "HCLOUD_TOKEN": (
        "ephemeral-vm-provisioner (ADR-052 §13, dev/hybrid only). The token exists "
        "— hub-cli reads it from the environment at bootstrap — but has never been "
        "materialised into a cell path, and doing so is a PRIVILEGE decision, not a "
        "list edit: it is the fleet-wide Hetzner API token, so every cell that can "
        "read it can create and delete servers across the whole fleet. A "
        "cell-scoped token would be the right answer and does not exist yet."
    ),
    "EPHEMERAL_JOBS_DATABASE_URL": (
        "ephemeral-vm-provisioner (ADR-052 §13, dev/hybrid only). The provisioner "
        "reads the ephemeral_jobs queue directly, so it needs a DSN for the tenant "
        "database rather than Kubernetes RBAC. Whether the platform mints a "
        "least-privilege role for it, or the tenant supplies the DSN, is open."
    ),
}

SRC = glob.glob("operators/hub-operator/internal/infisical/*.go") + \
      glob.glob("operators/hub-operator/internal/database/*.go") + \
      glob.glob("operators/hub-operator/internal/secrets/*.go")

# Keys produced INTO a cluster path (/spoke-pool/<clusterName>/...), as opposed to the
# Infisical root. This distinction is the whole point of the check: a spoke's
# SecretStore is authorised for its own prefix ONLY (ADR-031), so a producer that
# writes S3_ACCESS_KEY_ID to the root does NOT satisfy a spoke consumer reading
# /spoke-pool/<cell>/shared/S3_ACCESS_KEY_ID. Matching on key NAME alone treated
# those as satisfied and reported OK while every spoke ExternalSecret failed.
#
# Derived from the operator source rather than restated here, so deleting a
# producer breaks this check instead of silently narrowing it.
CELL_SRC = "operators/hub-operator/internal/secrets/cell_credentials.go"
cell_produced = {"infisical-credentials"}  # EnsureInfisicalCredentials, shared path
if os.path.exists(CELL_SRC):
    csrc = open(CELL_SRC).read()
    # Copied from the root into each cell by EnsureFleetCredentialsMaterialised.
    block = re.search(r'FleetCredentialKeys\s*=\s*\[\]string\{(.*?)\}', csrc, re.S)
    if block:
        cell_produced |= set(re.findall(r'"([^"]+)"', block.group(1)))

# Externally provisioned PER CELL: each cell has its own value, issued outside the
# platform, so no producer here writes it and the hub's must never be copied in
# (ADR-078 add.5 §5). Declared in the operator source next to the keys that ARE
# copied, so a consumer added without a seeding contract still fails this check.
#
# Being declared is not evidence the value exists. These are reported, and presence
# is asserted live per cell by cluster/078-alert-credentials.sh -- a static
# allowlist would turn a missing credential into a permanent pass.
external_cell = set()
if os.path.exists(CELL_SRC):
    block = re.search(r'ExternalPerCellCredentialKeys\s*=\s*\[\]string\{(.*?)\}',
                      open(CELL_SRC).read(), re.S)
    if block:
        external_cell = set(re.findall(r'"([^"]+)"', block.group(1)))

# GENERATED into each cell by the application-secret uploader. These are produced,
# not copied, so they are declared alongside the other generated secrets: any
# mapping carrying CellScopedKey is written once per SpokePool into that cell's
# shared path. Derived from the source so deleting the field breaks this check.
MAP_SRC = "operators/hub-operator/internal/infisical/secret_mappings.go"
if os.path.exists(MAP_SRC):
    cell_produced |= set(re.findall(r'CellScopedKey:\s*"([^"]+)"', open(MAP_SRC).read()))
if not SRC:
    print("SKIP\thub-operator source not found")
    raise SystemExit

produced = set()
for f in SRC:
    src = open(f).read()
    produced |= set(re.findall(r'InfisicalKey:\s*"([^"]+)"', src))
    produced |= set(re.findall(r'PasswordKey:\s*"([^"]+)"', src))
    produced |= {k for k in re.findall(r'UsernameKey:\s*"([^"]+)"', src) if k}
    # Key pairs: PrivateKeyKey / PublicKeyKey (KeyType != "")
    produced |= set(re.findall(r'PrivateKeyKey:\s*"([^"]+)"', src))
    produced |= set(re.findall(r'PublicKeyKey:\s*"([^"]+)"', src))
    # Key constants, consumed by the DB-role and uploader paths.
    #
    # A DECLARED constant is not a producer. hub-centralized-db-username and its
    # password were declared and referenced nowhere — no mapping entry, no uploader
    # path — so nothing ever wrote them, while this check reported them produced and
    # stayed green. The gap was invisible for as long as the values happened to
    # survive in Infisical from an earlier bootstrap; once that store was
    # reinitialised they were gone, one ExternalSecret could not resolve, and every
    # hub database role failed to provision behind it.
    #
    # Count a constant only when its identifier is used outside its own declaration.
    for ident, key in re.findall(r'\b(Key[A-Za-z0-9_]*)\s*=\s*"([^"]+)"', src):
        if any(re.search(r'\b' + re.escape(ident) + r'\b', open(f).read())
               for f in SRC if not f.endswith("constants.go")):
            produced.add(key)
    # Values written inline, e.g. the Svix JWT derived from its signing secret.
    produced |= set(re.findall(r'CreateOrUpdateSecretRaw\([^)]*?"([A-Za-z0-9_.\-]+)"', src, re.S))

wanted = {}
wanted_cell = {}
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
        if d.get("kind") != "ExternalSecret":
            continue
        store = (d.get("spec", {}).get("secretStoreRef") or {}).get("name", "")
        if "infisical" not in store:
            continue
        for item in d["spec"].get("data") or []:
            k = (item.get("remoteRef") or {}).get("key")
            if not k:
                continue
            # A cell-scoped key names its path; PLACEHOLDER is substituted per
            # spoke at render time. Previously any key containing PLACEHOLDER was
            # skipped outright, which excluded EVERY spoke ExternalSecret from the
            # check — the exact set most likely to reference an unproduced path.
            # Evaluate the leaf against the cell producers instead of skipping.
            if k.startswith("/spoke-pool/"):
                wanted_cell.setdefault(k.rsplit("/", 1)[-1], (k, f))
            elif "PLACEHOLDER" not in k:
                wanted.setdefault(k, f)

# Producers whose key names are BUILT from declared input and so cannot be found by
# scanning for literals: one OAuth client credential per client a fleet declares
# (ADR-053), named for the client. Each entry pairs the shape of the keys with a
# source fragment that must still exist, so deleting the producer breaks this check
# rather than silently widening it — the same principle as deriving the literal
# producers from source instead of restating them here.
DYNAMIC = [
    (re.compile(r"^OAUTH_[A-Z0-9_]+_CLIENT_(ID|SECRET)$"), "func InfisicalOAuthClientIDKey("),
]

all_src = "\n".join(open(f).read() for f in SRC)
for pattern, sentinel in DYNAMIC:
    if sentinel not in all_src:
        print("\t".join(["BAD", pattern.pattern,
                          "declared as a dynamic producer but '%s' is gone from the operator source"
                          % sentinel]))

def is_produced(key):
    if key in produced or key in EXTERNAL:
        return True
    return any(p.match(key) and sentinel in all_src for p, sentinel in DYNAMIC)


for k in sorted(wanted):
    if is_produced(k):
        continue
    if k in PENDING:
        print("\t".join(["PENDING", k, PENDING[k]]))
        continue
    print("\t".join(["BAD", k, os.path.relpath(wanted[k])]))

for leaf in sorted(wanted_cell):
    full, f = wanted_cell[leaf]
    if leaf in cell_produced or any(p.match(leaf) and sentinel in all_src for p, sentinel in DYNAMIC):
        continue
    # PENDING applies to a cell path too. It is keyed by the leaf because that is
    # the name a producer would create; the cell prefix is per-spoke.
    if leaf in external_cell:
        print("\t".join(["EXTCELL", full,
                          "externally provisioned per cell; presence is verified live, "
                          "not by this check (cluster/078-alert-credentials.sh)"]))
        continue
    if leaf in PENDING:
        print("\t".join(["PENDING", full, PENDING[leaf]]))
        continue
    where = "produced at the ROOT only" if (leaf in produced or leaf in EXTERNAL) else "not produced anywhere"
    print("\t".join(["BAD", full,
                     "%s — consumed from a cell path but %s; a cell SecretStore cannot read the root (ADR-031)"
                     % (os.path.relpath(f), where)]))

for k in sorted(EXTERNAL):
    if k in wanted or k in wanted_cell:
        print("\t".join(["SEED", k, EXTERNAL[k]]))
PY
) || rc=$?

    # rc is captured because an EMPTY result and a CRASHED checker are the same
    # string, and `-z "$out"` below cannot tell them apart. A checker that could
    # not run has not found nothing.
    if (( rc != 0 )); then
        hard_fail "the checker exited ${rc} — it inspected nothing, and an empty result from a crashed checker is not a passing one"
        return 1
    fi

    if [[ -z "$out" ]]; then
        pass "no Infisical-backed ExternalSecrets found to check"
        return 0
    fi

    if grep -q $'^SKIP\t' <<< "$out"; then
        warn "hub-operator source not available — cannot verify key producers"
        return 0
    fi

    local tag a b bad=0
    while IFS=$'\t' read -r tag a b; do
        [[ "$tag" == "BAD" ]] || continue
        bad=1
        hard_fail "Infisical key '$a' has no producer — nothing generates or uploads it ($b)"
    done <<< "$out"

    (( bad )) || pass "every consumed Infisical key is produced by the hub-operator, or declared external"

    while IFS=$'\t' read -r tag a b; do
        [[ "$tag" == "PENDING" ]] || continue
        warn "Infisical key '$a' has no producer (tracked gap, not yet closed)"
        note "$b"
    done <<< "$out"

    while IFS=$'\t' read -r tag a b; do
        [[ "$tag" == "EXTCELL" ]] || continue
        note "per-cell external credential '$a' — $b"
    done <<< "$out"

    note "externally issued — seeded by hub-bootstrap step 6b from k8-secrets/:"
    while IFS=$'\t' read -r tag a b; do
        [[ "$tag" == "SEED" ]] || continue
        note "  $a — $b"
    done <<< "$out"
}
