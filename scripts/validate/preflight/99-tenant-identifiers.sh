#!/usr/bin/env bash
# ADR-047 addendum: a platform manifest or binary must not name a tenant.
#
# Tenant runtime state lives in fleet-registry (ADR-004), and the platform renders
# tenant resources parameterised by fleetId. A literal tenant name in platform code
# inverts that: the platform starts depending on something that only exists after
# onboarding.
#
# That is not cosmetic. waypoint-bff-client-secret is an ExternalSecret in
# platform-edge whose key is only produced once that tenant's OAuth client is
# registered — so on a fresh hub, where no tenant has onboarded, it can never
# resolve. It was the single ExternalSecret still failing at 21/22 after every other
# secret converged, and it fails that way on every rebuild. It was repeatedly
# mistaken for a seeding gap; the dependency simply should not exist yet.
#
# The known offenders are baselined so the rule is enforceable today without
# blocking on the refactor. Anything NEW fails: a tenth file, or a second tenant
# hardcoded the same way.
validate_tenant_identifiers() {
    section "No tenant identifiers in platform code (ADR-047 addendum)"

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 - <<'PY'
import os, re, subprocess

# Paths that constitute "the platform". Tenant material belongs under
# manifests/tenants/ and the fleet provisioning ApplicationSets.
PLATFORM = ["manifests/hub-core-services", "manifests/argocd", "manifests/spoke",
            "internal", "cmd", "operators"]

# Tenant identifiers observed in this repo. There is no canonical local list —
# tenants live in fleet-registry — so this enumerates the names that have actually
# leaked. The rule is "no tenant names at all"; this is how it is detected.
#
# `nutgraf` IS THE TENANT. `waypoint` and `oranger` are its APPLICATIONS (ADR-088
# keeps those axes separate), and for a long time this list held only the two
# application names -- so the check was called "tenant identifiers" while being
# unable to see a single tenant name. The rule applies to both axes for the same
# reason: the platform renders them parameterised, and naming either makes platform
# code depend on something that exists only after onboarding.
TENANTS = ["nutgraf", "waypoint", "oranger"]

# `nutgraf.in` IS NOT COUNTED, AND THAT IS A DEFERRAL, NOT AN EXEMPTION.
#
# It appears 345 times, as two different things: the DNS domain every environment
# is served on (api., auth., console., dev., stg.) and the API GROUP of every
# platform CRD -- ops.nutgraf.in, billing.nutgraf.in, compute.nutgraf.in,
# spokepools.nutgraf.in, tenantdatabases.nutgraf.in.
#
# The API groups are a real leak and a worse one than anything this check catches:
# the platform ships to a customer's own box (BYOC), so a SOLOZ platform installed
# for another customer would serve CRDs named after this tenant. But renaming an
# API group rewrites every CRD, every RBAC rule, every manifest that references
# one, and every object already stored in etcd under the old group. That is a
# migration with its own decision to make, not something a validation script
# should force by failing the build today.
#
# So it is excluded HERE and recorded as open. If it were simply matched, this
# check would fail 130 files and be switched off, which is the outcome that loses
# the rule entirely.
# Both spellings: controller-gen derives webhook paths from the API group, so
# `nutgraf.in` arrives as `nutgraf-in` in /mutate-nutgraf-in-v1alpha1-spokepool.
# Same leak, same deferral; subtracting only the dotted form missed it.
DOMAIN_SUFFIXES = ("nutgraf.in", "nutgraf-in")

# Files that already violate the rule, recorded so the debt is visible and any NEW
# leak still fails. Shrinks as the ADR-047 remediation lands.
#
# The auth-proxy coupling is CLOSED: the platform binary no longer registers a
# tenant's OAuth clients, and its Deployment no longer mounts a tenant secret.
# What remains is hostname-level.
# EMPTY as of 2026-09-02. Both former entries are remediated: d3b7303d moved
# AgentGateway to one instance per tenant configured from the fleet registry, so
# agentgateway-config.yaml no longer defines tenant routing, and the appset is
# fleet-parameterised. What each still contains is a tenant name in a COMMENT,
# which this check no longer counts.
BASELINE = set()

pattern = r"\b(" + "|".join(re.escape(t) for t in TENANTS) + r")\b"


def strip_comments(line, ext):
    """Remove the comment tail of a line, ignoring comment markers inside strings.

    Naive stripping is not safe here. A Go line may carry
    "postgresql://tenant:..." — a real violation whose "//" is part of a URL, not
    a comment — so the marker only counts when it falls OUTSIDE a quoted string.
    Conversely a tenant name in prose (an example query, an incident note, a
    comment explaining why a label IS fleet-agnostic) is documentation, and
    flagging it penalises writing down the reasoning.
    """
    # A YAML file may embed another language whose comment marker differs — the
    # Alloy/River config in grafana-alloy.yaml is written with "//" inside a YAML
    # block scalar. Honour both markers there rather than only the outer one.
    markers = ("//",) if ext == ".go" else ("#", "//")
    quote = None
    i = 0
    while i < len(line):
        c = line[i]
        if quote:
            if c == "\\" and ext == ".go":
                i += 2
                continue
            if c == quote:
                quote = None
        elif c == '"' or c == "'":
            # Deliberately not treating a Go raw-string delimiter as a quote: a
            # tenant name inside one is still hardcoded, and skipping it would
            # be the unsafe direction.
            quote = c
        elif any(line.startswith(m, i) for m in markers):
            m = next(m for m in markers if line.startswith(m, i))
            # '#' opens a comment only at line start or after space; '//' must not
            # swallow a URL scheme ("postgresql://tenant:..."), which is a real
            # violation rather than a comment.
            if m == "#" and i > 0 and line[i - 1] not in " \t":
                i += 1
                continue
            if m == "//" and i > 0 and line[i - 1] == ":":
                i += 2
                continue
            return line[:i]
        i += 1
    return line


def parameterised_literals(path):
    """Literals the nearest templated-fields.yaml declares a substitution for.

    THE MECHANISM MUST NOT BE PENALISED BY THE RULE IT IMPLEMENTS. A component's
    templated-fields.yaml is how a manifest carrying this box's names renders
    another box's at package time -- `from: /nutgraf-mgmt` /
    `with: /{{ .Values.global.tenantId }}-mgmt`. That file necessarily SPELLS the
    literal it replaces, and the manifest beside it necessarily contains it, or
    there would be nothing to substitute.

    Counting either one reports the remediation as the defect, and the only way to
    satisfy the check would be to delete the substitution -- which is the leak.
    So a literal declared here is subtracted from the files that file governs: the
    question is whether a name is PARAMETERISED, not whether it appears.

    Scoped to the directory tree the templated-fields.yaml sits in, walking up from
    the file, because a declaration in one component says nothing about another's.
    """
    lits = set()
    d = os.path.dirname(os.path.abspath(path))
    root = os.path.abspath(".")
    while d.startswith(root):
        tf = os.path.join(d, "templated-fields.yaml")
        if os.path.isfile(tf):
            try:
                with open(tf, errors="replace") as fh:
                    for line in fh:
                        m = re.match(r"\s*from:\s*(.+?)\s*$", line)
                        if m:
                            lits.add(m.group(1).strip("'\""))
            except OSError:
                pass
        if d == root:
            break
        d = os.path.dirname(d)
    # Longest first, so a shorter literal cannot eat the prefix of a longer one.
    return sorted(lits, key=len, reverse=True)


def count_code_hits(path):
    ext = os.path.splitext(path)[1]
    params = parameterised_literals(path)
    n = 0
    in_block = False
    try:
        with open(path, errors="replace") as fh:
            for line in fh:
                # A BLOCK COMMENT SPANS LINES AND strip_comments DOES NOT.
                # `{{- /* ... */ -}}` in a Helm .tpl carries pages of incident prose
                # whose continuation lines have no marker of their own, so every
                # cluster name written down in one was counted as code.
                if in_block:
                    end = line.find("*/")
                    if end < 0:
                        continue
                    line = line[end + 2:]
                    in_block = False
                while True:
                    start = line.find("/*")
                    if start < 0:
                        break
                    end = line.find("*/", start + 2)
                    if end < 0:
                        line = line[:start]
                        in_block = True
                        break
                    line = line[:start] + line[end + 2:]
                code = strip_comments(line, ext)
                # Remove the platform domain and API-group suffix before counting,
                # so `ops.nutgraf.in` does not read as a tenant reference while a
                # bare `nutgraf` still does. See DOMAIN_SUFFIX above for why this is
                # deferred rather than exempt.
                for suffix in DOMAIN_SUFFIXES:
                    code = code.replace(suffix, "")
                # A literal the component's own templated-fields.yaml declares a
                # substitution FOR is parameterised at package time, which is the
                # thing this rule asks for. See parameterised_literals.
                for lit in params:
                    code = code.replace(lit, "")
                n += len(re.findall(pattern, code))
    except OSError:
        return 0
    return n


hits = {}
for root in PLATFORM:
    if not os.path.isdir(root):
        continue
    r = subprocess.run(["grep", "-rlE", pattern, root], capture_output=True, text=True)
    for f in r.stdout.split("\n"):
        f = f.strip()
        if not f:
            continue
        # Vendored or generated trees are not authored platform code.
        #
        # internal/platform/embedded is the packaging tree: a COPY of manifests and
        # scripts already checked at the paths they are authored in. Counting the
        # copy duplicates every real finding and manufactures false ones -- this
        # very file is embedded, and its own pattern list contains the tenant names
        # it searches for, so it failed on a copy of itself. Fourteen of the twenty
        # preflight failures were that, including embedded copies of six other
        # checks whose subject is tenant naming.
        #
        # The authored originals are still checked. Nothing is skipped; the same
        # bytes are simply not counted twice.
        if ("/vendor/" in f or "/reference-projects/" in f
                or "internal/platform/embedded/" in f):
            continue
        # A TEST FIXTURE IS NOT A RUNTIME DEPENDENCY, and this rule is about
        # runtime dependencies. The header's own example says why: the
        # waypoint-bff-client-secret ExternalSecret "can never resolve" on a fresh
        # hub because the key does not exist until that tenant onboards. A table
        # test naming `oranger` has no such property -- it resolves nothing, waits
        # for nothing, and ships in no binary. Counting it blocked 13 files from the
        # application-identity work (ADR-094/095/097) for naming their own subject.
        if f.endswith("_test.go"):
            continue
        # Generated by controller-gen from the Go types. The group lives in
        # api/*/groupversion_info.go, which IS checked; failing the generated copy
        # as well reports one decision twice and sends someone to edit an artefact
        # that is overwritten on the next `make manifests`.
        if "/config/crd/bases/" in f:
            continue
        # Also controller-gen output, from the same API group in `+kubebuilder`
        # markers. Reporting it repeats the groupversion_info.go finding at a path
        # that is overwritten on the next `make manifests`.
        if f.endswith("/config/webhook/manifests.yaml"):
            continue
        n = count_code_hits(f)
        if n:
            hits[f] = n

new = {f: n for f, n in hits.items() if f not in BASELINE}
known = {f: n for f, n in hits.items() if f in BASELINE}

for f in sorted(new):
    print("\t".join(["BAD", f, str(new[f])]))
for f in sorted(known):
    print("\t".join(["DEBT", f, str(known[f])]))

# A baseline entry that no longer matches means the refactor landed — prompt for
# its removal so the list cannot rot into permanent noise.
for f in sorted(BASELINE):
    if f not in hits:
        print("\t".join(["CLEAN", f, "0"]))
PY
)

    local tag file count bad=0 debt=0
    while IFS=$'\t' read -r tag file count; do
        case "$tag" in
            BAD)
                bad=1
                hard_fail "$file names a tenant ($count occurrence(s)) — platform code must be fleet-parameterised"
                ;;
            DEBT) ((debt++)) ;;
            CLEAN)
                warn "$file is in the baseline but no longer names a tenant — remove it from BASELINE"
                ;;
        esac
    done <<< "$out"

    if (( ! bad )); then
        pass "no new tenant identifiers in platform code"
    fi
    if (( debt )); then
        note "$debt file(s) still name a tenant (ADR-047 remediation outstanding):"
        while IFS=$'\t' read -r tag file count; do
            [[ "$tag" == "DEBT" ]] || continue
            note "  $file ($count)"
        done <<< "$out"
    fi
}
