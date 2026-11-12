#!/usr/bin/env bash
# End-to-end test of the RELEASED path, from this checkout, in one command.
#
# Publishes a prerelease bundle, builds a CLI that declares it, scaffolds a tenant
# repository against it and bootstraps a cluster from that repository -- which is
# the sequence a tenant's Day-0 performs, with the same artefacts and the same
# gates. See docs/runbooks/local-release-path-testing.md for what each phase does
# and why the released path needs testing separately at all (ADR-068 addendum 1).
#
#   scripts/dev/local-e2e.sh 0.1.16-rc.1                 # everything
#   scripts/dev/local-e2e.sh 0.1.16-rc.1 --dry           # package and gate only
#   scripts/dev/local-e2e.sh 0.1.16-rc.1 cli scaffold    # named phases only
#   scripts/dev/local-e2e.sh 0.1.16-rc.2 clean           # tear the last run down
#   scripts/dev/local-e2e.sh 0.1.16-rc.2 clean publish cli scaffold bootstrap
#
# Phases: clean  publish  cli  escrow  scaffold  bootstrap  workload  verify  adr
#         (default: all but clean. `clean` is opt-in: it destroys a running
#          cluster and deletes a GitHub repository, so it is never implied.)
#
# Credentials are read from the operator's store (~/.kamarkat/), one file per
# credential, never prompted and never echoed. That directory is gitignored and
# is the same place teardown reads from. A missing one is reported by path so it
# can be created once rather than retyped per run. This harness exports each
# credential under the name its consumer reads, because no consumer substitutes
# another: a local bootstrap without GITOPS_REPO_WRITE_TOKEN now fails where it
# once quietly borrowed the operator's own.
set -euo pipefail

cd "$(dirname "$0")/../.."
ROOT="$PWD"

# ── What is being built ─────────────────────────────────────────────────────
# Overridable from the environment; the defaults describe a throwaway dev box.
GIT_ORG="${GIT_ORG:-kamarkat}"
# MANDATORY. No default, and `acme.example` is not one.
#
# It used to default to acme.example, which is an RFC 2606 reserved TLD: it
# cannot resolve, for anyone, ever. A box scaffolded on it can never publish a
# DNS record, never obtain a certificate, and fails the public-endpoint gate by
# construction -- so the loop whose purpose is to exercise the whole path could
# not complete, and the reason was a default nobody chose.
#
# Worse, the default was silent. DOMAIN is read ONLY by the scaffold phase, so
# passing DOMAIN=example.org to a run without that phase set a variable that was
# never used, while the box went on publishing the domain it was scaffolded with.
# An input accepted and discarded is worse than one refused.
# It is mandatory for EVERY phase, not just scaffold, because the box's identity
# now derives from it -- see TENANT below. A phase that does not scaffold still
# has to know which box it is talking about, and the domain is the only input
# that says so.
DOMAIN="${DOMAIN:-}"
if [[ -z "$DOMAIN" ]]; then
    echo "local-e2e: DOMAIN is required." >&2
    echo "  It is the base domain the box publishes on, every public hostname derives" >&2
    echo "  from it (ADR-051), and it names the box: the tenant and cluster are taken" >&2
    echo "  from it. There is no default." >&2
    echo "    DOMAIN=example.org ./scripts/dev/local-e2e.sh $*" >&2
    exit 1
fi
case "$DOMAIN" in
    *.example|*.test|*.invalid|*.localhost)
        echo "local-e2e: DOMAIN=$DOMAIN is a reserved domain (RFC 2606) and cannot resolve." >&2
        echo "  Use a domain whose zone you control, or the box cannot publish." >&2
        exit 1 ;;
esac

# The tenant IS the domain. Derived, never given.
#
# TENANT and CLUSTER were two independent literals defaulting to acme and
# acme-hub, and CLUSTER did not even derive from TENANT -- setting TENANT=foo
# still produced acme-hub. So a run carrying DOMAIN=nutgraf.in scaffolded a box
# that published on nutgraf.in while every path, repository and cluster name in
# the run said acme. Three names for one box, agreeing only by coincidence, and
# the logs showed .local-e2e/acme-gitops for a domain nobody had called acme.
#
# One input, one identity. A name that cannot disagree with the domain cannot
# point a phase at the wrong box.
TENANT="${DOMAIN%%.*}"
if ! printf '%s' "$TENANT" | grep -Eq '^[a-z]([-a-z0-9]*[a-z0-9])?$'; then
    echo "local-e2e: DOMAIN=$DOMAIN yields tenant '$TENANT', which is not a DNS label." >&2
    echo "  The tenant names a repository and a cluster; it must be lowercase" >&2
    echo "  alphanumeric with hyphens, starting with a letter." >&2
    exit 1
fi
# Refused rather than ignored. An explicit TENANT that disagrees with the domain
# is the exact split this removes, and silently overriding it would restore it.
if [[ -n "${TENANT_OVERRIDE:-}" && "$TENANT_OVERRIDE" != "$TENANT" ]]; then
    echo "local-e2e: TENANT_OVERRIDE=$TENANT_OVERRIDE disagrees with DOMAIN=$DOMAIN (tenant '$TENANT')." >&2
    echo "  The tenant is derived from the domain. Change the domain." >&2
    exit 1
fi
# The box's names are MINTED by `kakt box init`, from the second it ran in, so
# this harness cannot derive them -- it reads them back from the declaration the
# box carries. Every phase resolves them the same way, which is also what lets
# one phase run on its own against the box a previous run created.
#
# box_dir   the checkout, found by the one glob a box of this tenant matches
# box_repo  its repository name
# box_hub   the management cluster the box declares
# Set by resolve_box, from the box's own declaration. Empty until then.
CLUSTER=""
BOX_DIR=""
REPO=""

box_dir() {
    local hits=("$WORKSPACE/$TENANT-gitops-"*)
    if [ ! -d "${hits[0]:-}" ]; then
        echo "local-e2e: no box checkout under $WORKSPACE for tenant $TENANT." >&2
        echo "  run the scaffold phase first, or point WORKSPACE at the run you mean." >&2
        return 1
    fi
    if [ "${#hits[@]}" -gt 1 ]; then
        echo "local-e2e: $WORKSPACE holds ${#hits[@]} boxes for tenant $TENANT:" >&2
        printf '    %s\n' "${hits[@]}" >&2
        echo "  a box's identity is minted per run; remove the ones you are done with." >&2
        return 1
    fi
    printf '%s' "${hits[0]}"
}

box_repo() { basename "$(box_dir)"; }

# resolve_box sets BOX_DIR, REPO and CLUSTER from the box this run operates on.
#
# Called by every phase that acts on one. CLUSTER is a global because the phases
# below pass it to `kakt` and to the validators, and it is no longer derivable
# from the tenant: the box minted it.
resolve_box() {
    BOX_DIR="$(box_dir)" || return 1
    REPO="$(basename "$BOX_DIR")"
    CLUSTER="$(box_hub)" || return 1
}

box_hub() {
    local dir; dir="$(box_dir)" || return 1
    local name
    name="$(sed -n 's/^managementClusterName:[[:space:]]*//p' "$dir/registry/box.yaml" | tr -d '"')"
    if [ -z "$name" ]; then
        echo "local-e2e: $dir/registry/box.yaml declares no managementClusterName." >&2
        return 1
    fi
    printf '%s' "$name"
}
# The stage the WORKLOAD cluster this run declares will serve. Not the hub's: the
# management cluster declares no environment (ADR-111 Revision 7), so this reaches
# `tenant add-cluster` and nothing else.
ENVIRONMENT="${ENVIRONMENT:-dev}"
# The DNS label this box sits under, declared rather than derived (ADR-051
# amendment 2026-09-18). "dev" because that is what every run has published on --
# dev.nutgraf.in -- and a scaffold that quietly moved to the apex would take every
# hostname, certificate and DNS record with it.
#
# A LITERAL, not $ENVIRONMENT. Defaulting it to the workload environment is the
# derivation ADR-051's amendment removed, and it would have moved the hub's whole
# hostname set the first time someone ran this with ENVIRONMENT=stg.
#
# Pass SUBDOMAIN= explicitly to publish on the apex.
SUBDOMAIN="${SUBDOMAIN-dev}"
PROVIDER="${PROVIDER:-hetzner}"
REGION="${REGION:-hel1}"
# The GHCR namespace. kamarkat rather than a personal one because visibility is a
# property of the package, not the version: those charts are already public, so a
# prerelease pushed there is pullable immediately, while a new namespace creates
# private packages that ArgoCD cannot read until each is flipped by hand
# (scripts/package/flip-public.sh).
OWNER="${OWNER:-kamarkat}"
# Where the tenant clone is made. Outside the checkout: it is a separate
# repository with its own remote, and `git add -A` here has swept one in before.
WORKSPACE="${WORKSPACE:-$ROOT/.local-e2e}"

# The operator's own store. SECRETS still overrides, for a developer who keeps a
# separate set for local runs.
SECRETS="${SECRETS:-${KAMARKAT_HOME:-$HOME/.kamarkat}}"

usage() { sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-1}"; }

VERSION="${1:-}"
[ -n "$VERSION" ] || usage 1
shift

DRY=""
phases=()
for arg in "$@"; do
    case "$arg" in
        --dry|--skip-push) DRY=1 ;;
        clean|publish|cli|escrow|scaffold|bootstrap|workload|verify|adr) phases+=("$arg") ;;
        -h|--help) usage 0 ;;
        *) echo "unknown argument: $arg" >&2; usage 1 ;;
    esac
done
[ ${#phases[@]} -gt 0 ] || phases=(publish cli escrow scaffold bootstrap workload verify adr)

wants() { printf '%s\n' "${phases[@]}" | grep -qx "$1"; }
say()   { printf '\n\033[1m[local-e2e] %s\033[0m\n' "$*"; }

# ── Credentials ─────────────────────────────────────────────────────────────

# read_secret <file> <description> [default]
#
# Returns the file's contents, or the default when one is given and the file is
# absent. Fails by naming the path, because the fix is to create that file once.
read_secret() {
    local path="$1" what="$2" default="${3-}"
    if [ -r "$path" ]; then
        local v; v="$(tr -d '\r' < "$path" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
        if [ -n "$v" ]; then printf '%s' "$v"; return 0; fi
    fi
    if [ -n "$default" ]; then printf '%s' "$default"; return 0; fi
    echo "local-e2e: missing ${what}." >&2
    echo "  expected it in: ${path}" >&2
    return 1
}

load_credentials() {
    HCLOUD_TOKEN="$(read_secret "$SECRETS/hetzner/token" "the Hetzner API token")"
    GITOPS_REPO_WRITE_TOKEN="$(read_secret "$SECRETS/github/github-pat-token" \
        "a GitHub token that can create the tenant repository and let Day-0 commit to it")"
    # Two jobs, one token here, which is the only reason this is a single file.
    # Scaffolding needs a credential to CREATE the repository, which it reads
    # from GITHUB_ORG_SETUP_TOKEN, while Day-0 commits and ArgoCD reads with
    # GITOPS_REPO_WRITE_TOKEN. In a real onboarding they are two credentials with
    # two owners; in a local run one GitHub account owns both sides, so the same
    # value plays both roles -- EXPORTED UNDER BOTH NAMES, because the code reads
    # each one by name and falls back to neither.
    export GITHUB_ORG_SETUP_TOKEN="$GITOPS_REPO_WRITE_TOKEN"
    export GITOPS_REPO_WRITE_TOKEN

    # ADR-076 makes the escrow mandatory: RequireEscrow refuses a box that would
    # have nowhere to keep its master keys. All four or none -- three of four
    # produces an operator that attempts a backup every reconcile and fails.
    ESCROW_URL="$(read_secret "$SECRETS/infisical/INFISICAL_ESCROW_URL" \
        "the escrow Infisical URL" "https://app.infisical.com")"
    # Read AFTER the escrow phase has had its chance to create it, because that
    # phase is what produces this file. Demanded up front it would refuse every
    # first run of a new tenant -- the value does not exist until the project is
    # created, and creating it is the point of the phase.
    #
    # Still required: load_credentials is called again below, and a run reaching
    # scaffold without it stops there.
    ESCROW_PROJECT_ID="$(read_secret "$SECRETS/infisical/INFISICAL_ESCROW_PROJECT_ID" \
        "the escrow project id" "${ESCROW_PROJECT_ID_OPTIONAL:-}")"
    ESCROW_CLIENT_ID="$(read_secret "$SECRETS/infisical/INFISICAL_ESCROW_CLIENT_ID" \
        "the escrow machine identity client id")"
    ESCROW_CLIENT_SECRET="$(read_secret "$SECRETS/infisical/INFISICAL_ESCROW_CLIENT_SECRET" \
        "the escrow machine identity client secret")"

    # The box's own Infisical administrator. Required: the defaults these replace
    # were a personal email and the literal "Password@123", identical on every box.
    INFISICAL_ADMIN_EMAIL="$(read_secret "$SECRETS/infisical/INFISICAL_ADMIN_EMAIL" \
        "the Infisical administrator's email")"
    INFISICAL_ADMIN_PASSWORD="$(read_secret "$SECRETS/infisical/INFISICAL_ADMIN_PASSWORD" \
        "the Infisical administrator's password")"
    # Exported, not passed as flags. fillFromEnvironment reads them by the same
    # name scaffolding stores on the repository and Day-0 reads at bootstrap --
    # one spelling for all three, which is what the credential registry is for.
    export INFISICAL_ADMIN_EMAIL INFISICAL_ADMIN_PASSWORD

    # Exported, under the names Day-0 reads.
    #
    # These four were collected for `tenant scaffold`, which takes them as FLAGS
    # and satisfies RequireEscrow with them. Day-0 reads them from the
    # ENVIRONMENT, and nothing put them there -- so every box built by this
    # script passed the escrow gate during scaffolding and was then bootstrapped
    # without one. It printed "no escrow configured" and carried on.
    #
    # The dispatch path never had the bug: scaffolding writes these as repository
    # secrets and the workflow passes them to the bootstrap step as env. Only the
    # local path collected them and dropped them, which is why the platform's own
    # runs hid it.
    export INFISICAL_ESCROW_URL="$ESCROW_URL"
    export INFISICAL_ESCROW_PROJECT_ID="$ESCROW_PROJECT_ID"
    export INFISICAL_ESCROW_CLIENT_ID="$ESCROW_CLIENT_ID"
    export INFISICAL_ESCROW_CLIENT_SECRET="$ESCROW_CLIENT_SECRET"

    # The tenant's registry credential (ADR-066: workloads are theirs).
    #
    # Mandatory, and it used to surface three seconds INTO bootstrap -- after the
    # cloud cluster existed -- because nothing loaded it here. A credential the
    # run cannot finish without belongs in this function, where a missing one
    # stops the run before it spends a version or provisions anything.
    #
    # The store's ghcr/ pair if present. Otherwise the GitHub credential already
    # loaded above,
    # which carries write:packages and therefore read, with the login taken from
    # gh. That is a convenience for THIS loop only: a tenant supplies a token
    # scoped to pulling their own images, and scaffolding collects it.
    # Read directly: these two are OPTIONAL inputs with a fallback, and
    # read_secret is for required ones -- it reports a missing file, which is
    # exactly what an absent optional credential is not.
    GHCR_USERNAME=""; GHCR_TOKEN=""
    [ -r "$SECRETS/ghcr/username" ] && GHCR_USERNAME="$(tr -d '\r\n' < "$SECRETS/ghcr/username")"
    [ -r "$SECRETS/ghcr/token" ]    && GHCR_TOKEN="$(tr -d '\r\n' < "$SECRETS/ghcr/token")"
    if [ -z "$GHCR_USERNAME" ]; then
        GHCR_USERNAME="$(env -u GITHUB_ORG_SETUP_TOKEN -u GITHUB_TOKEN -u GH_TOKEN gh api user --jq .login 2>/dev/null || true)"
    fi
    [ -n "$GHCR_TOKEN" ] || GHCR_TOKEN="$GITOPS_REPO_WRITE_TOKEN"
    if [ -z "$GHCR_USERNAME" ] || [ -z "$GHCR_TOKEN" ]; then
        echo "local-e2e: no registry credential for pulling this tenant's private images." >&2
        echo "  put a username and token under ${SECRETS}/ghcr/, or run: gh auth login" >&2
        return 1
    fi
    export GHCR_USERNAME GHCR_TOKEN

    # Destination credentials: where this box sends backups and telemetry.
    #
    # These are tierDestination, not tierCapability -- a box without them
    # bootstraps and runs, it just has nowhere to put backups or metrics. That is
    # the right behaviour and it is also why their absence is SILENT: the CLI
    # reports it in one line among many and the run continues. Loading them here
    # makes the run say up front what it will and will not configure, instead of
    # leaving an operator to notice afterwards that this box has no backups.
    #
    # Exported rather than left to the CLI's own lookup, so the run states what it
    # is passing instead of depending on where it happens to be run from -- the
    # same implicitness that let the registry credential go unnoticed until it
    # failed mid-bootstrap.
    local d
    # The prefix is GRAFANA, not GRAFANA_CLOUD: the files left in that directory are
    # the IRM integration URLs, and hub-bootstrap's SEED_SPECS reads them as
    # GRAFANA_IRM_*. With the old prefix this exported GRAFANA_CLOUD_IRM_HEARTBEAT_URL,
    # which nothing reads -- so a local box seeded no alert destination and said
    # nothing about it. The stack's own metrics credentials are gone with Grafana
    # Cloud (ADR-078 add.5): the stores run on the box now.
    for d in "$SECRETS/s3:S3" "$SECRETS/grafana-cloud:GRAFANA"; do
        local dir="${d%%:*}" prefix="${d##*:}" f key
        if [ ! -d "$dir" ]; then
            say "no ${dir}: this box will bootstrap without $( [ "$prefix" = S3 ] \
                && echo "database backups" || echo "an alert destination" )"
            continue
        fi
        for f in "$dir"/*; do
            [ -f "$f" ] || continue
            # access-key-id -> S3_ACCESS_KEY_ID, irm-heartbeat-url -> GRAFANA_IRM_HEARTBEAT_URL
            key="${prefix}_$(basename "$f" | tr 'a-z-' 'A-Z_')"
            export "$key=$(tr -d '\r\n' < "$f")"
        done
    done

    # Hybrid only. ADR-046 invariant 6 wants a tailnet IP on every node carrying
    # pod traffic; nothing reads this under any other provider.
    TAILSCALE_AUTHKEY=""
    if [ "$PROVIDER" = "hybrid" ]; then
        TAILSCALE_AUTHKEY="$(read_secret "$SECRETS/tailscale/authkey" "a Tailscale auth key")"
    fi
    # Exported, not only passed to scaffold.
    #
    # bootstrap runs from the TENANT CLONE, and readTailscaleAuthkey once read a
    # path relative to the working directory -- which is the clone, not the
    # operator's store the key actually sits in. The file lookup missed it, the
    # control plane was staged with an EMPTY authkey, and
    # the ClusterClass's `if [ -s /etc/tailscale-hostname ]` guard skipped
    # `tailscale up` entirely. Both nodes came up Ready and pod traffic between
    # them died in one direction (ADR-046 invariant 6); the visible symptom was
    # cert-manager's webhook timing out from the API server.
    #
    # The env var is read before the file, so exporting it here makes the key
    # reach bootstrap wherever it runs from.
    [ -n "$TAILSCALE_AUTHKEY" ] && export TS_AUTHKEY="$TAILSCALE_AUTHKEY"
    export HCLOUD_TOKEN
}

# ── Toolchain ───────────────────────────────────────────────────────────────

# Helm 3, shared with publish.sh so the local loop and a release package
# through the same toolchain.
# shellcheck source=scripts/dev/helm3.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/helm3.sh"

require_ghcr_login() {
    local scopes
    scopes="$(gh auth status 2>&1 | sed -n 's/.*Token scopes: //p')"
    case "$scopes" in
        *write:packages*) ;;
        *) echo "local-e2e: the gh token cannot write packages (scopes: ${scopes:-unknown})." >&2
           echo "  run: gh auth refresh -s write:packages,read:packages" >&2
           return 1 ;;
    esac
    gh auth token | helm registry login ghcr.io -u "$(gh api user --jq .login)" --password-stdin >/dev/null
}

# ── Phases ──────────────────────────────────────────────────────────────────

# Remove everything the previous run of THIS loop created.
#
# Never in the default set, and never implied: it destroys a running cluster and
# deletes a GitHub repository, and neither is recoverable. Naming the phase is
# the confirmation -- `make e2e` alone cannot reach it.
#
# Scoped to the names this script manages. It will not take a cluster or a
# repository it was not given in the same variables it would have created them
# from, so a stray CLUSTER= in the environment cannot point it at something real.
do_clean() {
    # Resolved from the checkout, because the box's names are minted and the
    # checkout is the only local record of them. Without one this phase cannot
    # know which repository it would be deleting, and guessing is how a loop
    # deletes a box somebody else is using.
    if ! resolve_box; then
        echo "  Nothing local names a box to remove. If a repository is left behind:" >&2
        echo "    gh repo list $GIT_ORG --json name --jq '.[].name | select(startswith(\"$TENANT-gitops-\"))'" >&2
        echo "  then delete the one you mean by name." >&2
        return 1
    fi
    local repo="$REPO"
    say "removing what the last run left: cluster $CLUSTER, $GIT_ORG/$repo"

    # The cloud cluster first: teardown needs the kubeconfig the clone holds, so
    # removing the clone before it would strand the servers with nothing left
    # that knows their names.
    # NOT `|| true`. The exit code was discarded, which threw away the one warning
    # that says records were orphaned -- at the exact moment the evidence is being
    # deleted, because the lines below remove the workspace and the kubeconfig with
    # it. A teardown that could not reach the cluster cannot read the box's DNS
    # ownership, and external-dns ignores a record whose owner does not match, so
    # an orphan is permanent: no later box can claim it.
    #
    # Reported and continued rather than fatal: `clean` exists to get back to a
    # buildable state, and a box that is already half-gone must not become
    # unremovable. But it says so, loudly, while the zone can still be checked.
    # --spoke is what this box DECLARES, and it is passed because the hub is the
    # thing being destroyed. teardown asks the hub which spokes it owns, which is
    # the right source right up until the hub is unreachable -- and then it
    # returns nothing, the spoke servers go unclaimed, and they are left running
    # and billing. That is the orphan that had to be deleted by hand after every
    # run. The declaration answers the same question and survives the cluster.
    local pool=""
    pool=$(spoke_pool_for) || {
        echo "local-e2e: cannot name the declared SpokePool; teardown will not be" >&2
        echo "  able to reclaim spoke servers if the hub is already gone." >&2
    }

    # THE WORKLOAD CLUSTER FIRST, IN THE SAME RUN. `kakt teardown` refuses to
    # destroy a management cluster while a cluster it manages is still there: the
    # hub holds that cluster's CAPI objects and the only controller that can
    # deprovision its load balancers, so a hub taken first strands the rest.
    # --destroy-managed deletes the pool's Applications on the hub, which is what
    # destroys it, and the hub goes only once that has finished.
    #
    # --tenant IS GONE from the CLI, and was still being passed here, so every
    # `clean` failed on an unknown flag before it deleted anything. It named the
    # box's prefix, and because a box's clusters share that prefix by construction
    # it made a hub teardown sweep the workload cluster's servers too.
    if ! "$ROOT/bin/kakt" teardown --name "$CLUSTER" --force --confirm \
             --gitops-dir "$WORKSPACE/$repo" \
             ${pool:+--destroy-managed "$pool"} 2>&1 | sed 's/^/  /'; then
        echo "local-e2e: teardown did not complete cleanly." >&2
        echo "  Records this box published may be left in the zone. They cannot be" >&2
        echo "  reclaimed by a later box -- check now, and release them with:" >&2
        echo "    kakt teardown --name $CLUSTER --confirm --dns-only \\" >&2
        echo "      --dns-owner <owner> --dns-zone <zone>" >&2
        echo "  Servers may also be left running. Check and remove with:" >&2
        echo "    kakt teardown --name $CLUSTER --force --confirm \\" >&2
        echo "      --gitops-dir $WORKSPACE/$repo${pool:+ --destroy-managed $pool}" >&2
    fi

    # The ephemeral bootstrap cluster. Left behind, `kind create cluster`
    # short-circuits on the next run and writes no kubeconfig, and every kubectl
    # call afterwards resolves nothing -- which surfaced as a connection refused
    # to localhost:8080 three phases later, naming neither cause.
    if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
        kind delete cluster --name "$CLUSTER" 2>&1 | sed 's/^/  /'
    else
        echo "  no kind cluster $CLUSTER"
    fi

    if [ -e "$WORKSPACE/$repo" ]; then
        rm -rf "$WORKSPACE/$repo"
        echo "  removed $WORKSPACE/$repo"
    fi

    if ! env -u GITHUB_ORG_SETUP_TOKEN -u GITHUB_TOKEN -u GH_TOKEN gh repo view "$GIT_ORG/$repo" --json name >/dev/null 2>&1; then
        echo "  no repository $GIT_ORG/$repo"
        return 0
    fi
    # Reported, not fatal. Everything above has already happened, and failing the
    # phase here would leave the caller believing none of it did -- while the
    # only thing left is a repository name that the next scaffold will collide
    # with, which it says so about clearly.
    # With no credential of ours in the environment, deliberately.
    #
    # load_credentials exports it from the operator's store so scaffolding can
    # create the
    # repository, and gh prefers that variable over its own keyring token. The
    # PAT carries repo/workflow/write:packages and not delete_repo, so every
    # delete here failed with "Must have admin rights" while the same command in
    # a plain shell succeeded -- the message named a scope the operator had
    # already added, to a token gh was not using.
    local out
    if out=$(env -u GITHUB_ORG_SETUP_TOKEN -u GITHUB_TOKEN -u GH_TOKEN gh repo delete "$GIT_ORG/$repo" --yes 2>&1); then
        echo "  deleted $GIT_ORG/$repo"
        return 0
    fi
    # gh's own words, not a guess at them. Discarding this and printing an
    # assumed cause said "gh needs the delete_repo scope" to someone who already
    # had it, which sent them to fix something that was not broken.
    echo "  could NOT delete $GIT_ORG/$repo -- everything else was removed." >&2
    printf '  %s\n' "$out" >&2
    echo "  If that is a missing scope: gh auth refresh -h github.com -s delete_repo" >&2
    echo "  Otherwise delete it in the browser." >&2

    # Fatal when scaffold is coming, because scaffold cannot succeed against a
    # repository that already has commits (ADR-062) and we already know it is
    # still there. Reporting and carrying on rebuilt the CLI, then refused --
    # work done after the run was already decided.
    if wants scaffold; then
        echo "  Stopping here: scaffold would refuse this repository anyway." >&2
        return 1
    fi
    echo "  Nothing else needs it removed, so continuing." >&2
}

do_publish() {
    require_helm3
    if [ -n "$DRY" ]; then
        say "packaging and gating $VERSION (nothing will be pushed)"
        SKIP_PUSH=1 ./scripts/package/publish.sh "$VERSION" "$OWNER"
        return
    fi
    require_ghcr_login
    say "publishing $VERSION to oci://ghcr.io/$OWNER/charts"
    ./scripts/package/publish.sh "$VERSION" "$OWNER"
}

do_cli() {
    say "building a CLI that declares $VERSION"
    TARGETS=host ./scripts/package/build-cli.sh "$VERSION" "$ROOT/bin"
    cp "$ROOT/bin/kakt-$(go env GOOS)-$(go env GOARCH)" "$ROOT/bin/kakt"

    # The one check worth making twice. A binary reporting `development` reads the
    # working tree and renders a git-shaped bundle, and every phase below would
    # succeed while testing the path this script exists to avoid.
    local got; got="$("$ROOT/bin/kakt" bundle-version)"
    [ "$got" = "$VERSION" ] || {
        echo "local-e2e: the built CLI declares '${got}', expected '${VERSION}'" >&2
        return 1
    }
    echo "bin/kakt declares $VERSION"
}

# Scaffolding refuses to clone over an existing directory, deliberately: a stale
# clone would be bootstrapped instead of the repository just created, and the
# difference is invisible in the output.
# Both halves of "already scaffolded": the local clone and the repository itself.
#
# Only the clone was checked, and the two do not go together -- a run that got as
# far as creating the repository and then failed leaves the repository without
# the clone. Preflight passed, scaffold ran, and the CLI refused on its own guard
# ("already has commits") after the cli phase had rebuilt for nothing.
workspace_is_clear() {
    local bad=""

    # ASSERTS THE ABSENCE OF A BOX, which is the opposite of resolve_box. A box
    # mints its own names, so this cannot check one name: it checks that this
    # tenant has no box here at all. Two boxes for one tenant are legitimate for
    # a real replacement (ADR-111 Revision 9); in this loop one is a leftover.
    local here=("$WORKSPACE/$TENANT-gitops-"*)
    if [ -d "${here[0]:-}" ]; then
        echo "local-e2e: $WORKSPACE already holds a box for $TENANT:" >&2
        printf '    %s\n' "${here[@]}" >&2
        bad=1
    fi
    local remote
    remote="$(gh repo list "$GIT_ORG" --limit 200 --json name --jq \
        ".[].name | select(startswith(\"$TENANT-gitops-\"))" 2>/dev/null || true)"
    if [ -n "$remote" ]; then
        echo "local-e2e: $GIT_ORG already holds a box repository for $TENANT:" >&2
        printf '    %s\n' $remote >&2
        echo "  A box is a starting state, not a fork (ADR-062), and this loop runs one" >&2
        echo "  box at a time." >&2
        bad=1
    fi
    [ -n "$bad" ] || return 0

    local suggested="${phases[*]}"
    wants clean || suggested="clean $suggested"
    echo "  Clear both and re-run:" >&2
    echo "    ./scripts/dev/local-e2e.sh $VERSION $suggested" >&2
    return 1
}

# The escrow project this tenant's boxes back up to.
#
# Before scaffold, because scaffold verifies the escrow and refuses a box whose
# escrow does not work -- and the thing it verifies is what this creates.
#
# Idempotent by the id file, which is the record of what exists. An Infisical
# project is not a box: it belongs to the tenant, outlives every cluster built
# against it, and holds the master keys of any box still running. Creating a
# second one per run would leave those keys reachable by nothing, which is why
# `kakt escrow init` refuses an id that is already recorded. This skips instead,
# the same way a completed bootstrap phase does.
#
# What stays manual is the Infisical ACCOUNT and its first machine identity. That
# cannot be otherwise: the escrow account is the tenant's and the platform holds
# no credential to it (ADR-076). It is one-time per tenant, not per box.
do_escrow() {
    local id_file="$SECRETS/infisical/INFISICAL_ESCROW_PROJECT_ID"

    if [ -s "$id_file" ]; then
        echo "[local-e2e] escrow project already recorded: $(cat "$id_file")"
        return 0
    fi

    say "creating the escrow project for $TENANT"
    "$ROOT/bin/kakt" escrow init \
        --url "$(read_secret "$SECRETS/infisical/INFISICAL_ESCROW_URL" \
                   "the escrow Infisical URL" "https://app.infisical.com")" \
        --client-id "$(read_secret "$SECRETS/infisical/INFISICAL_ESCROW_CLIENT_ID" \
                         "the escrow machine identity client id")" \
        --client-secret "$(read_secret "$SECRETS/infisical/INFISICAL_ESCROW_CLIENT_SECRET" \
                             "the escrow machine identity client secret")" \
        --name "soloz-escrow-$TENANT" \
        --owner-email "$(read_secret "$SECRETS/infisical/INFISICAL_ESCROW_OWNER_EMAIL" \
                           "the Infisical account that should own the escrow project")" \
        --out "$SECRETS/infisical"
}

do_scaffold() {
    mkdir -p "$WORKSPACE"
    workspace_is_clear || return 1

    # Two commands, because a box and its clusters are two things (ADR-111 §1).
    # `box init` creates the repository with NO clusters and leaves a checkout;
    # `tenant add-management` declares the first management cluster inside it.
    say "creating a box for $TENANT in $GIT_ORG against $VERSION"
    ( cd "$WORKSPACE" && "$ROOT/bin/kakt" box init --no-prompt \
        --tenant "$TENANT" --box "$GIT_ORG" --domain "$DOMAIN" \
        --provider "$PROVIDER" \
        --subdomain "$SUBDOMAIN" \
        --region "$REGION" \
        --bundle-version "$VERSION" \
        --bundle-registry "ghcr.io/$OWNER/charts" \
        --provider-token "$HCLOUD_TOKEN" \
        --gitops-token "$GITOPS_REPO_WRITE_TOKEN" \
        --escrow-url "$ESCROW_URL" \
        --escrow-project-id "$ESCROW_PROJECT_ID" \
        --escrow-client-id "$ESCROW_CLIENT_ID" \
        --escrow-client-secret "$ESCROW_CLIENT_SECRET" \
        ${TAILSCALE_AUTHKEY:+--tailscale-authkey "$TAILSCALE_AUTHKEY"} )

    # Read back, not derived: the box minted its own names a moment ago.
    local repo hub dir
    dir="$(box_dir)" || return 1
    repo="$(basename "$dir")"
    hub="$(box_hub)" || return 1
    say "box $repo created; declaring management cluster $hub"

    # No --name: a box's first management cluster takes the name the box
    # declares, so the three minted names cannot drift apart here.
    ( cd "$dir" && "$ROOT/bin/kakt" tenant add-management )

    # Committed and pushed here rather than by the CLI: a declaration is the
    # tenant's commit, and the bootstrap below reads the pushed tree.
    ( cd "$dir" \
        && git -c user.name=local-e2e -c user.email=local-e2e@local add . \
        && git -c user.name=local-e2e -c user.email=local-e2e@local \
             commit -qm "Declare management cluster $hub" \
        && git push -q \
             "https://x-access-token:${GITOPS_REPO_WRITE_TOKEN}@github.com/$GIT_ORG/$repo.git" main )

    # The assertion the whole exercise is for. A development build renders
    # `repoURL: https://github.com/...` with a `path:`; a released one renders the
    # OCI chart. Checked here rather than left to the eye, because the bootstrap
    # that follows takes twenty minutes to tell you the same thing.
    local bundle="$dir/registry/clusters/$hub/bundle.yaml"
    if ! grep -q "ghcr.io/$OWNER/charts" "$bundle"; then
        echo "local-e2e: $bundle does not name the published registry." >&2
        echo "  this is the development shape, not the released one -- re-run the cli phase." >&2
        grep -A3 'repoURL' "$bundle" | sed 's/^/    /' >&2
        return 1
    fi
    if ! grep -q 'bundleVersion' "$bundle"; then
        echo "local-e2e: $bundle names a chart but sets no bundleVersion parameter." >&2
        echo "  the boundaries would render 0 Applications (ADR-068 addendum 1)." >&2
        return 1
    fi
    echo "bundle names oci://ghcr.io/$OWNER/charts at $VERSION"

    # The workload cluster, declared by the box rather than shipped in the bundle.
    #
    # Scaffolding writes the MANAGEMENT cluster; a repository holds one of those
    # and as many workload clusters as it declares, each hydrated from
    # templates/workload-cluster (kubefirst's layout). The bundle used to ship a
    # SpokePool whose name was a literal, so every box provisioned its workload
    # cluster under the same name -- and the CNPG archive prefix derived from that
    # name collided across boxes, which is why barman refused every WAL with
    # "Expected empty archive" and no backup ever completed anywhere.
    #
    # Named from the cell, not the provider: CELL is the box's to choose and is
    # what the fleet ApplicationSets select on (ADR-047).
    local cell="${CELL:-$TENANT-01}"
    say "declaring workload cluster $cell"
    "$ROOT/bin/kakt" tenant add-cluster \
        --gitops-dir "$WORKSPACE/$repo" \
        --mgmt-cluster "$CLUSTER" --name "$cell" \
        --provider "$PROVIDER" --region "$REGION" --environment "$ENVIRONMENT" \
        --tenant "$TENANT" --bundle-version "$VERSION" \
        --bundle-registry "ghcr.io/$OWNER/charts" \
        --gitops-repo-url "https://github.com/$GIT_ORG/$repo" \
        || return 1

    ( cd "$WORKSPACE/$repo" \
      && git add registry/clusters \
      && git commit -q -m "feat: declare workload cluster $cell" \
      && git push -q ) || {
        echo "local-e2e: could not commit the workload cluster declaration." >&2
        echo "  The management cluster reads it from git; an uncommitted claim" >&2
        echo "  provisions nothing." >&2
        return 1
    }
    echo "workload cluster $cell declared and pushed"
}

# The bundle names charts by version and ArgoCD pulls them itself. If they are not
# in the registry, every boundary renders nothing and bootstrap reports "no
# ApplicationSet targets project boundary-01" -- after ten minutes, with a control
# plane and two workers already billing. The registry answers in two seconds.
#
# This exists because a --dry run published nothing and the phases after it went
# ahead regardless, which is exactly the failure it now prevents.
require_published() {
    local missing=""
    require_helm3
    # The four names scripts/package/flip-public.sh calls authoritative. NOT
    # "platform-bundle": that is a directory name in dist/, and the Chart.yaml
    # inside it still reads `name: environment-manager` (bundle-chart.sh:17-18),
    # so it packages and pushes under that name. Checking for it would refuse
    # every bootstrap, for a chart that is never published under that name.
    for c in environment-manager platform universal-tenant tenant-public-tls; do
        helm show chart "oci://ghcr.io/$OWNER/charts/$c" --version "$VERSION" >/dev/null 2>&1 \
            || missing="${missing}  ${c}"$'\n'
    done
    [ -z "$missing" ] && return 0

    cat >&2 <<EOF
local-e2e: ${VERSION} is not published to oci://ghcr.io/${OWNER}/charts.

Missing:
${missing}
The cluster pulls these itself, so bootstrapping now would build three servers
and then sit for ten minutes on "no ApplicationSet targets project boundary-01".

Publish first -- note that --dry deliberately pushes nothing:

  ./scripts/dev/local-e2e.sh ${VERSION} publish
EOF
    return 1
}

do_bootstrap() {
    resolve_box || return 1
    local repo="$REPO"
    [ -d "$WORKSPACE/$repo" ] || {
        echo "local-e2e: $WORKSPACE/$repo does not exist; run the scaffold phase first" >&2
        return 1
    }
    require_published
    say "bootstrapping $CLUSTER from $WORKSPACE/$repo"
    # From the clone, exactly as the tenant workflow does. Correct only because
    # the binary carries its platform content; a development build would resolve
    # manifests/ against this directory and find nothing.
    # --on-prem without a tailnet is refused by the CLI (ADR-046 invariant 6), so
    # pass the name alongside the flag rather than leaving it for the operator to
    # discover. TAILNET_NAME overrides; home-lab.env is where a developer's
    # tailnet is already recorded, so it is read rather than restated here.
    # The SpokePool this box declares. hub-bootstrap.sh refuses to continue
    # without one -- main() error_exits immediately before step10_wait_spokepool
    # -- and this was the only phase that never supplied it. The run therefore
    # completed every step up to the spoke wait and then died on an unset
    # variable, unless the caller happened to have SPOKEPOOL_NAME exported.
    local pool; pool=$(spoke_pool_for)

    local on_prem_flag=""
    if [ "$PROVIDER" = "hybrid" ]; then
        on_prem_flag="--on-prem"
        local tailnet="${TAILNET_NAME:-}"
        if [ -z "$tailnet" ] && [ -r "$ROOT/scripts/hybrid/home-lab.env" ]; then
            tailnet=$(. "$ROOT/scripts/hybrid/home-lab.env" >/dev/null 2>&1; printf '%s' "${TAILNET_NAME:-}")
        fi
        [ -n "$tailnet" ] && on_prem_flag="$on_prem_flag --tailnet-name $tailnet"
    fi
    # hub-bootstrap.sh, not the CLI directly.
    #
    # The CLI builds the management cluster and stops there. The ten steps that
    # decide whether the box is usable -- secrets present, Infisical reachable,
    # ESO wired, the database provisioned, the workload cluster actually up --
    # live in that script, with the state tracking and the error_exit gates that
    # make a failure stop the run.
    #
    # Calling the CLI here skipped all of it. That is why on 2026-09-15 a
    # workload cluster whose control plane never started sat Ready=False for
    # sixteen hours behind a bootstrap that reported success: nothing was
    # waiting on it, because the thing that waits was never invoked.
    #
    # The script runs the CLI itself, as its first step.
    ( cd "$WORKSPACE/$repo" \
      && ZERO_OPS_DIR="$WORKSPACE/$repo" KAKT_BINARY="$ROOT/bin/kakt" \
         bash "$ROOT/scripts/hub-bootstrap.sh" \
           --name "$CLUSTER" --provider "$PROVIDER" --region "$REGION" \
           --gitops-dir . \
           --spoke "$pool" \
           $on_prem_flag )
}

# The workload cluster alone, against a management cluster that already exists.
#
# Calls hub-bootstrap.sh's own step functions rather than reimplementing them:
# the script is sourced, which loads the functions without running the ten-step
# sequence, and the two workload gates are invoked directly. Those gates are
# where a workload cluster that never came up is caught -- on 2026-09-15 one sat
# Ready=False for sixteen hours because nothing invoked them.
#
# Separate from do_bootstrap because it is separately re-runnable: a workload
# cluster that failed can be retried without rebuilding the management cluster,
# which is thirty minutes and a fresh set of servers.
do_workload() {
    resolve_box || return 1
    local repo="$REPO"
    local ws="$WORKSPACE/$repo"
    local kc="${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/$CLUSTER.kubeconfig"
    [ -r "$kc" ] || {
        echo "local-e2e: no kubeconfig at $kc; the management cluster must exist first" >&2
        return 1
    }

    # Resolved the same way every other phase resolves it: from the manifests
    # that declare it. This read the live cluster instead, which is a second
    # answer to one question -- and the two could disagree, with `bootstrap`
    # having provisioned the pool the manifests name while `workload` waited on
    # whichever one the cluster happened to list first.
    local pool; pool=$(spoke_pool_for)

    say "workload cluster: $pool"
    CLUSTER_NAME="$CLUSTER" \
    PROVIDER="$PROVIDER" \
    SPOKEPOOL_NAME="$pool" \
    ZERO_OPS_DIR="$ws" \
    KAKT_BINARY="$ROOT/bin/kakt" \
    bash -c '
        source "'"$ROOT"'/scripts/hub-bootstrap.sh"
        read_kubeconfig_from_state
        export KUBECONFIG="$KUBECONFIG_PATH"
        step10_wait_spokepool || exit 1
        # WORKLOAD_SKIP_NODES=1 gates on the cluster without joining its nodes.
        # The node gate is live state, not a one-shot fact (ADR-046 §24.2), so
        # skipping it is a deliberate narrowing rather than a shortcut.
        [ -n "${WORKLOAD_SKIP_NODES:-}" ] || step10e_spoke_home_worker "$SPOKEPOOL_NAME"
        # The database gate follows the workers, never precedes them: it needs a
        # schedulable node, and 10e is what provides one. Skipped along with the
        # nodes under WORKLOAD_SKIP_NODES, since a cluster deliberately left
        # without workers has nowhere to run a database either.
        [ -n "${WORKLOAD_SKIP_NODES:-}" ] || step10f_spoke_database
    '
}

# Bootstrap returning success means every phase completed, not that the platform
# is serving: ArgoCD is still pulling charts and ESO is still waiting on a secret
# store another controller is writing. Bounded rather than open-ended, so "not
# converged" is an answer -- and long enough that normal settling is not reported
# as failure. VERIFY_DEADLINE overrides it.
do_verify() {
    resolve_box || return 1
    local repo="$REPO"
    local kc="${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/$CLUSTER.kubeconfig"
    [ -r "$kc" ] || {
        echo "local-e2e: no kubeconfig at $kc; bootstrap has not produced one" >&2
        return 1
    }
    say "verifying $CLUSTER converges"
    # The same command the tenant's workflow runs, not a local re-implementation.
    # A second implementation would be a second thing to keep true, and the whole
    # point of this loop is that local and CI take one path.
    "$ROOT/bin/kakt" verify --kubeconfig "$kc" --timeout "${VERIFY_DEADLINE:-45m}" || return 1

    # Then the component checks, which converging does not cover.
    #
    # `kakt verify` watches ArgoCD Applications reach Synced+Healthy -- a statement
    # about reconciliation, not about the platform working. external-dns crash-looped
    # 108 times on an invalid --provider while its Application sat Synced, so no
    # hostname for the box was ever published and every hub URL was unreachable,
    # with nothing reporting it.
    #
    # post-bootstrap-validate.sh checks that per component. hub-bootstrap.sh ran it
    # as a FATAL gate, for the reason stated in its own source: "swallowing this
    # into a warning is how a bootstrap 'succeeds' while leaving a platform that
    # does not work."
    #
    # Run from HERE and not from the CLI, deliberately. ADR-063 fails a release that
    # references the platform's repository at runtime, and these scripts live in it,
    # so a tenant's released `kakt bootstrap-mgmt` cannot call them (ADR-072 moved Day-0
    # into the tenant's own repository). This script IS a platform checkout, so the
    # check runs where it is runnable without putting a repository reference into
    # the artefact. Carrying these checks to tenants is separate work: they would
    # have to travel inside the binary, as manifests/ already do.
    # Through validate-all.sh, not post-bootstrap-validate.sh directly. It runs
    # the same validator; what it adds is the record -- .state/validation.json,
    # one entry per phase with its status, counts and log. Without it a verify
    # that passed and a verify that was never reached look identical afterwards,
    # which is the question this loop exists to answer.
    local validator="$ROOT/scripts/validate-all.sh"
    [ -r "$validator" ] || {
        echo "local-e2e: $validator is missing; component validation cannot run" >&2
        return 1
    }
    say "validating the platform"
    # ZERO_OPS_DIR is the WORKSPACE, not the platform checkout. The report and its
    # logs belong beside the box's own state, with everything else Day-0 wrote
    # about it -- writing them into the platform repository put one box's record
    # in a tree shared by every run.
    ZERO_OPS_DIR="$WORKSPACE/$repo" KUBECONFIG="$kc" \
        CLUSTER_NAME="$CLUSTER" \
        SPOKEPOOL_NAME="$(spoke_pool_for)" \
        bash "$validator" --only=platform
}

# The spoke this environment+provider provisions, mirroring the burstSpokePool
# table in environment-manager values.yaml. Wrong here means the validator checks
# a spoke this box never creates, and reports a failure that is not one.
# The workload clusters THIS BOX declares, newline-separated.
#
# Read from the tenant's repository, which is the only place they exist. They
# used to be read from manifests/spoke/spoke-pools/<env>/<provider> in the
# platform tree -- a SpokePool claim with a literal name, packaged into the
# published bundle, so every box that pulled it provisioned a workload cluster
# under the same name and every identity derived from that name collided across
# boxes.
#
# A repository holds one management cluster and as many workload clusters as it
# declares, each at clusters/<name>/infrastructure/spokepool.yaml (kubefirst's
# layout; `kakt tenant add-cluster` writes them). So this returns a LIST, and
# callers that can only act on one say which.
#
# Unknown is an error rather than a guess. A name this script invents is one the
# platform never created, and every check downstream of it fails against the
# wrong object.
workload_clusters() {
    resolve_box || return 1
    local repo="$BOX_DIR"
    local names=""

    if [ -d "$repo/registry/clusters" ]; then
        names=$(yq eval 'select(.kind == "SpokePool") | .metadata.name' \
                   "$repo"/registry/clusters/*/infrastructure/spokepool.yaml 2>/dev/null \
                 | grep -vx 'null' || true)
    fi

    if [ -z "$names" ]; then
        echo "local-e2e: $repo declares no workload cluster." >&2
        echo "  A management cluster provisions the workload clusters its repository" >&2
        echo "  declares, and this one declares none. Add one:" >&2
        echo "    $ROOT/bin/kakt tenant add-cluster --gitops-dir $repo \\" >&2
        echo "      --mgmt-cluster $CLUSTER --name <cell> --provider $PROVIDER \\" >&2
        echo "      --environment $ENVIRONMENT --tenant $TENANT \\" >&2
        echo "      --bundle-version $VERSION --gitops-repo-url <url>" >&2
        return 1
    fi
    printf '%s\n' "$names"
}

# The first workload cluster, for the steps that can only address one.
#
# hub-bootstrap.sh takes a single --spoke and waits on it; teardown reclaims one
# at a time. Naming the first is honest about that limit -- it is not a claim
# that a box has only one.
spoke_pool_for() {
    workload_clusters | head -1
}

# The acceptance pass. `verify` answers "did this converge"; this answers "does
# what each decision promised actually hold" -- which is a different question, and
# the one a run exists to settle. A box can converge perfectly while the bundle
# names an unpublished chart or a capability switched off is still running.
do_adr() {
    resolve_box || return 1
    local repo="$REPO"
    local kc="${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/$CLUSTER.kubeconfig"
    [ -r "$kc" ] || {
        echo "local-e2e: no kubeconfig at $kc; bootstrap has not produced one" >&2
        return 1
    }
    say "asserting ADR-062 through ADR-070 against $CLUSTER"
    ( cd "$ROOT" && PATH="$ROOT/bin:$PATH" \
        ./scripts/dev/adr-acceptance.sh "$VERSION" "$WORKSPACE/$repo" "$kc" \
            "ghcr.io/$OWNER/charts" )
}

# ── Run ─────────────────────────────────────────────────────────────────────

if ! printf '%s' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
    echo "local-e2e: '$VERSION' is not semver" >&2; exit 1
fi

# --dry publishes nothing, so anything downstream of publish would run against
# charts that do not exist. Refused here rather than discovered later.
if [ -n "$DRY" ] && { wants scaffold || wants bootstrap; }; then
    echo "local-e2e: --dry pushes nothing, so scaffold and bootstrap cannot follow it." >&2
    echo "  to check packaging only:   ./scripts/dev/local-e2e.sh $VERSION publish --dry" >&2
    echo "  to run the whole loop:     ./scripts/dev/local-e2e.sh $VERSION" >&2
    exit 1
fi

# Loaded once, up front, so a missing credential stops the run before anything is
# published rather than after -- a version spent on a run that could not finish is
# a version that cannot be reused (ADR-063).
if wants scaffold || wants bootstrap || wants clean; then
    # The escrow project id is the one value a run may legitimately not have yet:
    # the escrow phase creates it. Tolerated here so that phase can run, and
    # demanded again after it, where its absence is a real failure.
    if wants escrow; then
        ESCROW_PROJECT_ID_OPTIONAL="pending"
    fi
    load_credentials
    unset ESCROW_PROJECT_ID_OPTIONAL
fi

# Every requested phase's preconditions, checked before the first one runs.
#
# Each phase used to check its own on the way in, which is too late: the phases
# run in order, so a scaffold that could not start was discovered after publish
# had already pushed. ADR-063 consumes a version by publishing it, so that cost a
# version -- twice -- for a leftover directory a one-second test would have found.
preflight() {
    # DOMAIN is present and resolvable by the time anything runs -- it is checked
    # where it is read, because the box's identity derives from it. What is left
    # for preflight is the question only a scaffolded box can answer: does this
    # domain match the box the run is about to touch?
    if ! wants scaffold; then
        # Read from `domain`, not `hubDomain`. A box now records the zone and the
        # label under it as two fields and the chart joins them (ADR-051 amendment
        # 2026-09-18), so the composed value is no longer in values.yaml at all --
        # this grep silently matched nothing, and a check that matches nothing
        # passes, which is how the mismatch it exists to catch would return.
        #
        # `domain` is the right half to compare anyway: DOMAIN names the zone.
        local declared=""
        local box_values="$BOX_DIR/registry/clusters/$CLUSTER/values.yaml"
        if [[ -r "$box_values" ]]; then
            declared=$(grep -m1 '^domain:' "$box_values" 2>/dev/null | awk '{print $2}')
            # A box scaffolded before the split records only the joined value.
            [[ -z "$declared" ]] && declared=$(grep -m1 '^hubDomain:' "$box_values" 2>/dev/null | awk '{print $2}')
        fi
        # This cost a full bootstrap once: the box kept dev.acme.example while
        # every command carried DOMAIN=<something else>, and nothing said so
        # until the public-endpoint gate failed on eleven hostnames twenty
        # minutes in.
        if [[ -n "$declared" && "$declared" != *"$DOMAIN"* ]]; then
            echo "local-e2e: DOMAIN=$DOMAIN does not match the box at $BOX_DIR." >&2
            echo "  $CLUSTER publishes on $declared, fixed when it was scaffolded." >&2
            echo "  A box's domain cannot be changed afterwards: every certificate and" >&2
            echo "  DNS record derives from it." >&2
            echo "  Pass the domain this box was built on, or rebuild: clean scaffold ..." >&2
            return 1
        fi
    fi

    if wants publish; then
        require_helm3
        if [ -z "$DRY" ]; then
            require_ghcr_login
            # Probed before packaging rather than after. publish.sh refuses a
            # version already in the registry, correctly, but only once the three
            # minutes of packaging are done.
            if helm show chart "oci://ghcr.io/$OWNER/charts/environment-manager" \
                 --version "$VERSION" >/dev/null 2>&1; then
                echo "local-e2e: $VERSION is already published to ghcr.io/$OWNER/charts." >&2
                echo "  A version is consumed by publishing it (ADR-063); take the next one." >&2
                echo "  To reuse what is already there, skip the publish phase:" >&2
                echo "    ./scripts/dev/local-e2e.sh $VERSION cli scaffold bootstrap" >&2
                return 1
            fi
        fi
    fi

    # Only when scaffold will not create it: scaffold's own precondition is that
    # it does NOT exist, and bootstrap's is that it does.
    # Two preconditions that are each other's opposite, so neither may be applied
    # to a state an earlier phase in THIS run is about to produce.
    #
    # Scaffold needs the clone absent -- unless clean runs first, whose whole job
    # is to remove it. Bootstrap needs it present -- unless scaffold runs first,
    # which creates it. Written as an if/elif, the clean case fell through to the
    # bootstrap arm and demanded a clone that the very same run was about to make.
    if wants scaffold; then
        wants clean || workspace_is_clear
    elif wants bootstrap || wants verify || wants adr; then
        resolve_box || {
            echo "local-e2e: no box to act on; run the scaffold phase first" >&2
            return 1
        }
    fi

    # The charts bootstrap needs, unless this run is about to publish them.
    if wants bootstrap && ! wants publish; then
        require_published
    fi
}

preflight

for p in clean publish cli escrow scaffold bootstrap workload verify adr; do
    wants "$p" && "do_$p"
    # The escrow phase writes the project id that scaffold is about to use, so the
    # credentials are re-read once it has run. Without this the run would carry
    # the placeholder it was started with and hand a box an escrow id of "pending".
    if [ "$p" = escrow ] && wants escrow && { wants scaffold || wants bootstrap; }; then
        load_credentials
    fi
done

say "done: $VERSION"
if wants bootstrap; then
    cat <<EOF

Tear down when finished. The management cluster goes last and refuses while a
cluster it manages is still there, so name them:

  $ROOT/bin/kakt teardown --name <management cluster> --force --confirm --gitops-dir <checkout> \
      --destroy-managed <workload cluster>
  rm -rf $WORKSPACE/$TENANT-gitops-*
  gh repo delete $GIT_ORG/<repository> --yes
EOF
fi
