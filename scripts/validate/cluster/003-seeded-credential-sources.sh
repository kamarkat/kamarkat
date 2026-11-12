#!/usr/bin/env bash
# ADR-003: a seeded credential's SOURCE Secret carries every key the uploader reads.
#
# Bootstrap Secrets are seeded once, at Day-0: hub-bootstrap step 6b writes
# k8-secrets/<subdir>/<file> into a cluster Secret, hub-operator's CLISecretMappings
# publishes that Secret to Infisical, and ESO delivers it back out. Only the last two
# hops reconcile. Nothing watches k8-secrets/ -- it is a directory on the operator's
# machine, reachable from no cluster -- so a credential added there AFTER the box was
# built never arrives, and the box keeps running on whatever it was seeded with.
#
# That drift is quiet. The uploader skips a source Secret that does not exist, so a
# credential absent from this box is not a failure; but a Secret that EXISTS and is
# missing a key fails every reconcile with "source secret missing key <k>", the
# summary drops to "17 of 19", and nothing else changes. Observed on the hub,
# 2026-10-30: platform-ops/grafana-cloud still held the retired Grafana Cloud metrics
# keys and neither IRM URL, two uploads had been failing on every reconcile for days,
# and alerting kept working because the values had reached Infisical by another route.
#
# The check is the same question the uploader asks, asked before anyone reads a log.
# Declarations come from the operator source, so a mapping added without its seed is
# caught by the same rule rather than by a list maintained here.

validate_adr003_seeded_credential_sources() {
    section "ADR-003: every seeded credential's source Secret carries its keys"

    local out line
    out=$(cd "$VALIDATE_ROOT" && HUB_KUBECONFIG="$HUB_KUBECONFIG" python3 - <<'PYCHECK'
import json, os, re, subprocess

SRC = "operators/hub-operator/internal/infisical/secret_mappings.go"
CONSTS = "operators/hub-operator/internal/infisical/constants.go"
kubeconfig = os.environ["HUB_KUBECONFIG"]

# Namespace constants, read from the source so a renamed namespace cannot make this
# check silently examine the wrong one.
ns_by_const = dict(re.findall(r'(Namespace\w+)\s*=\s*"([^"]+)"', open(CONSTS).read()))

src = open(SRC).read()
block = re.search(r'var CLISecretMappings = \[\]SecretMapping\{(.*?)\n\}\n', src, re.S)
if not block:
    print("FAIL CLISecretMappings not found in the operator source; this check cannot "
          "tell which Secrets are seeded")
    raise SystemExit

wanted = {}
# Commented-out lines are not declarations. The registry carries a worked Template
# block, and reading it as an entry invented a mapping for a Secret named
# "k8s-secret-name" that no box has or should have.
body = "\n".join(l for l in block.group(1).splitlines() if not l.lstrip().startswith("//"))
for entry in re.findall(r'\{(.*?)\n\t\}', body, re.S):
    ns = re.search(r'SourceNamespace:\s*(\w+)', entry)
    name = re.search(r'SourceName:\s*"([^"]+)"', entry)
    key = re.search(r'SourceKey:\s*"([^"]+)"', entry)
    ikey = re.search(r'InfisicalKey:\s*"([^"]+)"', entry)
    if not (ns and name and key):
        continue
    namespace = ns_by_const.get(ns.group(1), ns.group(1))
    wanted.setdefault((namespace, name.group(1)), []).append(
        (key.group(1), ikey.group(1) if ikey else "?"))

if not wanted:
    print("FAIL no source mappings parsed from CLISecretMappings; the check would pass "
          "vacuously, which is worse than failing")
    raise SystemExit

for (namespace, name), keys in sorted(wanted.items()):
    r = subprocess.run(["kubectl", "--kubeconfig", kubeconfig, "-n", namespace,
                        "get", "secret", name, "-o", "json"],
                       capture_output=True, text=True)
    if r.returncode != 0:
        # Absent is not a failure: the uploader skips it, and a box that does not use
        # a credential should not be made to carry one.
        print("NOTE %s/%s is not on this box; its %d mapping(s) are skipped by the "
              "uploader" % (namespace, name, len(keys)))
        continue
    data = json.loads(r.stdout).get("data") or {}
    missing = [k for k, _ in keys if k not in data]
    empty = [k for k, _ in keys if k in data and len(data[k]) == 0]
    if missing:
        print("FAIL %s/%s exists but is missing %s -- the uploader fails this on every "
              "reconcile with 'source secret missing key', and the Infisical value is "
              "whatever was seeded last. Re-apply it from the operator store (hub-bootstrap "
              "step 6b)" % (namespace, name, ", ".join(sorted(missing))))
    if empty:
        print("FAIL %s/%s has empty value(s) for %s -- the uploader rejects empty, so "
              "nothing is published and nothing says so"
              % (namespace, name, ", ".join(sorted(empty))))
    if not missing and not empty:
        print("PASS %s/%s carries all %d key(s) its mappings read"
              % (namespace, name, len(keys)))

# Keys the box carries that no mapping reads. Not a failure -- a Secret may serve
# more than the uploader -- but a retired integration's credentials linger this way,
# and a live vendor key nobody reads is worth naming.
for (namespace, name), keys in sorted(wanted.items()):
    r = subprocess.run(["kubectl", "--kubeconfig", kubeconfig, "-n", namespace,
                        "get", "secret", name, "-o", "json"],
                       capture_output=True, text=True)
    if r.returncode != 0:
        continue
    data = json.loads(r.stdout).get("data") or {}
    extra = sorted(set(data) - {k for k, _ in keys})
    if extra:
        print("NOTE %s/%s also carries %s, which no mapping reads"
              % (namespace, name, ", ".join(extra)))
PYCHECK
    )
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        case "$line" in
            PASS\ *) pass "${line#PASS }" ;;
            FAIL\ *) hard_fail "${line#FAIL }" ;;
            NOTE\ *) note "${line#NOTE }" ;;
        esac
    done <<< "$out"
}
