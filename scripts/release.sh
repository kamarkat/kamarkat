#!/usr/bin/env bash
# Publish the next bundle and, given a tenant's GitOps checkout, promote it.
#
#   scripts/release.sh                       publish the next rc after the newest published
#   scripts/release.sh --version 0.1.17-rc.1 an explicit version
#   scripts/release.sh --dry-run             say what it would do, change nothing
#   GITOPS_DIR=<checkout> scripts/release.sh publish, promote that tenant, sync its box
#   GITOPS_DIR=<checkout> scripts/release.sh --no-promote / --no-sync
#                                            stop after publishing / after promoting
#   GITOPS_DIR=<checkout> scripts/release.sh --promote-only --version 0.1.17
#                                            promote a version already published
#   scripts/release.sh --check               rehearse the next version and check
#                                            every component behind latest has a reason
#
# A release is the platform's, not a tenant's: its version follows the newest one
# in the registry, and publishing needs no tenant. Promoting and syncing are a
# tenant's, so they run only when GITOPS_DIR names its checkout.
#
# --promote-only is for a version this script did not publish: a stable release,
# which the GitHub Release workflow publishes. Without it the publish step finds
# the version in the registry and refuses, correctly, so a CI-published version
# could not be promoted at all. The version must be named, because "the next rc"
# is a version to create, never one to assume exists; and it must already be in
# the registry, because promoting first writes a reference to nothing.
#
# WHY ONE SCRIPT. Publishing and promoting are two halves of one intent, and
# doing them by hand has failed in three distinct ways that this sequence
# removes:
#
#   the version was bumped in values.yaml only, so the root Application synced
#   and every CHILD stayed pinned to the previous release -- reporting Synced
#   while shipping nothing (rc.111, rc.112);
#
#   the promotion commit was written against a stale checkout, so the push was
#   rejected and a later force would have discarded a CI bot's own commit;
#
#   .state/*.json was staged along with the version files, committing local
#   machine state into a tenant's repository.
#
# The ordering is deliberate and is the point: check and PULL the tenant, then
# publish, then promote. A tenant that cannot be promoted is refused before
# anything is published, so a failure never leaves a version published and
# unpromoted that could have been caught first. Promoting before publishing
# writes a reference to an artefact that does not exist, which ArgoCD reports as
# a chart pull failure naming a version nobody can find.
set -euo pipefail

GITOPS_DIR="${GITOPS_DIR:-}"
OWNER="${OWNER:-kamarkat}"
VERSION=""
DRY_RUN=0
PUBLISH=1
PROMOTE=1
SYNC=1
CHECK=0

while [ $# -gt 0 ]; do
  case "$1" in
    --version)      VERSION="${2:?--version needs a value}"; shift 2 ;;
    --dry-run)      DRY_RUN=1; shift ;;
    --promote-only) PUBLISH=0; shift ;;
    --no-promote)   PROMOTE=0; shift ;;
    --no-sync)      SYNC=0; shift ;;
    --check)        CHECK=1; shift ;;
    -h|--help)      sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)              echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

# --check spends nothing and touches no tenant: every step that would change
# something is the rehearsal's instead.
if [ "$CHECK" = 1 ]; then DRY_RUN=1; PROMOTE=0; fi

if [ "$PUBLISH" = 0 ]; then
  [ "$PROMOTE" = 1 ] || { echo "--promote-only and --no-promote together do nothing" >&2; exit 2; }
  [ -n "$VERSION" ] || { echo "--promote-only needs --version: name the published version to promote" >&2; exit 2; }
  [ -n "$GITOPS_DIR" ] || { echo "--promote-only needs GITOPS_DIR: the tenant checkout to promote" >&2; exit 2; }
fi
[ -n "$GITOPS_DIR" ] || PROMOTE=0

say()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }
die()  { printf '\n\033[31mFAILED\033[0m %s\n' "$*" >&2; exit 1; }
run()  { if [ "$DRY_RUN" = 1 ]; then note "would run: $*"; else "$@"; fi; }

# ---------------------------------------------------------------------------
# 1. Decide the version FROM THE REGISTRY.
#
# The newest version published is the one the next follows, whichever tenant
# runs what. It is also what the inventory's "changed since" compares against:
# the release notes say what changed since the last release, not since one box.
# ---------------------------------------------------------------------------
if [ "$PUBLISH" = 1 ]; then
  say "1. Decide the version"
  CURRENT="$(scripts/release/release-data.py newest --owner "$OWNER")" \
    || die "could not read the newest published version from ghcr.io/$OWNER (gh auth status)"
  note "newest published: $CURRENT"
  if [ -z "$VERSION" ]; then
    case "$CURRENT" in
      *-rc.*) VERSION="${CURRENT%-rc.*}-rc.$((${CURRENT##*-rc.} + 1))" ;;
      *) die "cannot increment $CURRENT automatically: it is not an -rc.N version. Pass --version." ;;
    esac
  fi
  note "next:             $VERSION"
fi

# ---------------------------------------------------------------------------
# 2. Pull the tenant repository, BEFORE publishing.
#
# Before any edit, because a rebase with local changes in the tree is a conflict
# resolved under time pressure -- and the only safe resolution of a conflict on
# a version file is to start again. Before publishing, because a tenant that
# cannot be promoted should stop the release while nothing is spent.
# ---------------------------------------------------------------------------
if [ "$PROMOTE" = 1 ]; then
  say "2. Pull the tenant repository"
  [ -d "$GITOPS_DIR/.git" ] || die "no tenant GitOps checkout at $GITOPS_DIR"
  cd "$GITOPS_DIR"
  git diff --quiet -- registry/ environments/ || die "uncommitted changes under registry/ or environments/ in $GITOPS_DIR
   This script rewrites version references. Commit or discard them first;
   it will not rebase on top of work it did not make."
  if [ "$DRY_RUN" = 1 ]; then
    note "would: git pull --rebase"
  else
    # Autostash covers .state/*.json, which this box writes and never commits.
    git -c rebase.autoStash=true pull --rebase -q || die "could not update $GITOPS_DIR -- resolve it there, then re-run"
  fi
  note "at $(git log --oneline -1)"

  # The version the tenant runs, which promotion rewrites -- every cluster's
  # environmentRevision, de-duplicated.
  #
  # `grep -m1` over several files matches once PER FILE, not once in total, so the
  # naive read returns one line per cluster and the "current version" becomes two
  # versions joined by a newline. Sorted unique instead, and a disagreement is a
  # refusal rather than a guess: clusters on different versions is a real state --
  # a promotion that half-applied -- and picking either one silently completes it
  # in a direction nobody chose.
  RUNNING="$(awk '/^environmentRevision:/ {print $2}' registry/clusters/*/values.yaml 2>/dev/null | sort -u)"
  [ -n "$RUNNING" ] || die "no environmentRevision found under $GITOPS_DIR/registry/clusters/*/values.yaml"
  if [ "$(printf '%s\n' "$RUNNING" | wc -l | tr -d ' ')" != "1" ]; then
    die "clusters are on different versions, so there is no single current version:
$(grep -H '^environmentRevision:' registry/clusters/*/values.yaml)
   Bring them to one version first."
  fi
  note "tenant runs: $RUNNING"

  # Every cluster must pull the bundle from the registry this release publishes
  # to. Promotion rewrites the version and nothing else, so a bundle.yaml still
  # naming another registry is promoted to a version that registry does not hold:
  # ArgoCD cannot pull it and every Application stays on the old version. That
  # shipped twice (rc.189, rc.190) after the platform's registry moved.
  WANT_REGISTRY="ghcr.io/${OWNER}/charts"
  WRONG=""
  for f in registry/clusters/*/bundle.yaml; do
    got="$(awk '/repoURL:/ {u=$NF} /chart: environment-manager/ {print u; exit}' "$f")"
    [ "$got" = "$WANT_REGISTRY" ] || WRONG="$WRONG
     $f: ${got:-no environment-manager chart source}"
  done
  [ -z "$WRONG" ] || die "these clusters do not pull the bundle from $WANT_REGISTRY, where $VERSION is published:$WRONG
   Set their environment-manager repoURL to $WANT_REGISTRY, commit and push, then re-run.
   Nothing has been published."
  note "every cluster pulls from $WANT_REGISTRY"

  # Refuse to reuse a version. A republished tag with different content is the one
  # failure GitOps cannot diagnose: every reference still resolves, and the box
  # runs something other than what the reference names.
  EVERY_REF="$(grep -rn -- "$VERSION" registry/ --include='*.yaml' 2>/dev/null || true)"
  [ -z "$EVERY_REF" ] || die "$VERSION is already referenced in $GITOPS_DIR/registry/ -- a version is published once
$EVERY_REF"
  cd - >/dev/null
fi

# ---------------------------------------------------------------------------
# --check: rehearse this version and check it can be explained.
#
# Every packaging step and gate runs and nothing is pushed, so the version stays
# free. Then release-data.py -- the same lookups the release pages are generated
# from -- names every component behind its latest release today with no `behind`
# reason in the catalog. A published version that cannot be explained cannot be
# documented, and finding that after publishing is a release page nobody can write.
# ---------------------------------------------------------------------------
if [ "$CHECK" = 1 ]; then
  say "3. Rehearse $VERSION (nothing is pushed)"
  PREVIOUS_VERSION="$CURRENT" SKIP_PUSH=1 make publish-local VERSION="$VERSION" OWNER="$OWNER" \
    || die "the rehearsal failed a gate -- fix the cause on main before releasing"
  say "4. Every component behind its latest release has a reason"
  # Exits 4 and 5 are failed lookups, not unexplained versions: the catalog
  # advice below would send someone to edit a reason that is not the problem.
  set +e
  scripts/release/release-data.py check dist/charts/platform-bundle/components.json
  reasons_rc=$?
  set -e
  if [ "$reasons_rc" = 4 ]; then
    die "the reasons check could not reach an upstream API (above). Nothing is wrong with the
   catalog -- run the same command again."
  elif [ "$reasons_rc" = 5 ]; then
    die "the reasons check looked up something that does not exist (above). Either a catalog
   entry names the wrong repo or package, or a platform image has not been published under
   its current name -- run its build workflow on main, then run this again."
  elif [ "$reasons_rc" != 0 ]; then
    die "the versions above are behind their latest release and not explained: no reason, or a
   reason whose target is this release. Fix the \`behind\` item for each in
   manifests/architecture/component-catalog.yaml (format in its header): land the change, or
   give the real status and a later target."
  fi
  say "Ready. $VERSION can be released."
  exit 0
fi

# ---------------------------------------------------------------------------
# 3. Publish. The existing script, unchanged -- one packaging path, one gate set.
# ---------------------------------------------------------------------------
if [ "$PUBLISH" = 0 ]; then
  # Read, not assumed: the chart a box pulls must resolve before anything names it.
  say "3. Confirm $VERSION is published"
  helm show chart "oci://ghcr.io/${OWNER}/charts/environment-manager" --version "$VERSION" >/dev/null 2>&1 \
    || die "could not confirm $VERSION in oci://ghcr.io/${OWNER}/charts -- not published, or the registry
   could not be read (helm registry login ghcr.io). Nothing has been promoted."
  note "oci://ghcr.io/${OWNER}/charts/environment-manager:$VERSION resolves"
else
say "3. Publish $VERSION"
# The inventory compares this version against the newest published.
export PREVIOUS_VERSION="$CURRENT"
run make publish-local VERSION="$VERSION" OWNER="$OWNER" \
  || die "publish failed -- nothing has been promoted, so the box is untouched"
fi

if [ "$PROMOTE" = 0 ]; then
  if [ "$DRY_RUN" = 1 ]; then
    say "Would publish $VERSION. No tenant promoted."
  elif [ -n "$GITOPS_DIR" ]; then
    say "Published $VERSION. Not promoted (--no-promote)."
  else
    say "Published $VERSION. No tenant promoted: pass a GitOps checkout to promote one."
  fi
  exit 0
fi

# ---------------------------------------------------------------------------
# 4. Promote EVERY reference, and verify none of the old one survives.
#
# Not just environmentRevision. A bundle.yaml carries the version twice -- once
# as the root Application's targetRevision and once as the value the generated
# child Applications are rendered with -- and moving only the first leaves the
# root Synced while every child stays on the previous release. That is not a
# theoretical ordering problem: it shipped twice.
# ---------------------------------------------------------------------------
say "4. Promote $RUNNING -> $VERSION"
cd "$GITOPS_DIR"
FILES="$(grep -rl -- "$RUNNING" registry/ --include='*.yaml' 2>/dev/null || true)"
[ -n "$FILES" ] || die "no file under registry/ references $RUNNING"
note "rewriting: $(echo "$FILES" | tr '\n' ' ')"

if [ "$DRY_RUN" = 1 ]; then
  # Falls THROUGH to step 5 rather than exiting. A dry run that stops before the
  # last step cannot answer the question a dry run is for -- what will this do to
  # the box -- and the sync is the step with the consequences.
  note "would rewrite $(echo "$FILES" | wc -l | tr -d ' ') file(s), commit, rebase and push"
  cd - >/dev/null
  DRY_PROMOTED=1
fi
if [ "${DRY_PROMOTED:-0}" != 1 ]; then

for f in $FILES; do
  # In place, both GNU and BSD sed.
  sed -i.bak "s|${RUNNING}|${VERSION}|g" "$f" && rm -f "$f.bak"
done

LEFT="$(grep -rn -- "$RUNNING" registry/ --include='*.yaml' 2>/dev/null || true)"
[ -z "$LEFT" ] || die "after rewriting, $RUNNING still appears -- promotion is incomplete and has NOT been pushed
$LEFT"
note "$(grep -rc -- "$VERSION" registry/ --include='*.yaml' 2>/dev/null | grep -v ':0$' | wc -l | tr -d ' ') file(s) now reference $VERSION"

# Staged BY NAME. `git add -A` here commits .state/*.json, which is this box's
# local bootstrap state and belongs to no tenant's repository.
git add $FILES
STAGED="$(git diff --cached --name-only)"
echo "$STAGED" | grep -q '^\.state/' && die "refusing to commit: .state/ is staged"

git commit -q -m "chore(promote): ${RUNNING} -> ${VERSION}

Every reference under registry/, not only environmentRevision: a bundle carries
the version twice, and moving one leaves the children pinned while the root
reports Synced."

# Rebase again before pushing. The publish above takes minutes, and a CI bot
# writing a chart bump in that window is ordinary -- the first push of this
# promotion was rejected for exactly that. Never force: the rejection is the
# other party's commit, and discarding it is the failure, not the rejection.
git -c rebase.autoStash=true pull --rebase -q || die "the remote moved and the rebase did not apply cleanly.
   $VERSION IS PUBLISHED. Resolve in $GITOPS_DIR and push; do not re-publish."
git push -q || die "push rejected. $VERSION IS PUBLISHED but not promoted -- resolve in $GITOPS_DIR and push"
note "pushed $(git log --oneline -1)"
cd - >/dev/null
say "Promoted to $VERSION"
fi

# ---------------------------------------------------------------------------
# 5. Sync, and VERIFY the box reached the version.
#
# Not an opt-in step, and not a line of advice printed at the end. A promotion
# that is written to git and never confirmed on the box is the failure this
# whole script exists to prevent: the repository says rc.N, every child stays on
# rc.N-1, and nothing reports it. Printing "Next: run this" leaves exactly that
# gap, and leaves it at the moment the operator has most reason to believe the
# work is done.
#
# The hard-sync is what turns "written" into "running": it drives the root, waits
# for each child to reach the version, and exits non-zero naming any that did
# not. This script's exit code is therefore the answer to "is the box on the new
# version", which is the only question worth asking after a release.
# ---------------------------------------------------------------------------
if [ "$SYNC" = 0 ]; then
  echo
  note "Not synced (--no-sync). The repository says $VERSION; the box does not yet."
  note "  KUBECONFIG=$GITOPS_DIR/k8-secrets/kubeconfig/<hub>.kubeconfig \\"
  note "    scripts/argocd-hard-sync.sh --promote $VERSION"
  exit 0
fi

say "5. Sync the box and verify it reached $VERSION"
if [ -z "${KUBECONFIG:-}" ]; then
  # Discovered, not assumed. Exactly one match or it asks: guessing which cluster
  # to drive is not a thing a release script may do quietly.
  # Plain globbing, not mapfile: the system bash on macOS is 3.2, where mapfile
  # does not exist and the script would die with "command not found" at the last
  # step of a release that had already published and promoted.
  HUBS=""
  for k in "$GITOPS_DIR"/k8-secrets/kubeconfig/*hub*.kubeconfig; do
    [ -f "$k" ] && HUBS="$HUBS$k"$'\n'
  done
  HUB_COUNT=$(printf '%s' "$HUBS" | grep -c . || true)
  case "$HUB_COUNT" in
    1) KUBECONFIG="$(printf '%s' "$HUBS" | head -1)"; export KUBECONFIG; note "using $KUBECONFIG" ;;
    0) die "no hub kubeconfig under $GITOPS_DIR/k8-secrets/kubeconfig/ -- set KUBECONFIG" ;;
    *) die "several hub kubeconfigs found; set KUBECONFIG to the one to drive:
$(printf '%s' "$HUBS" | sed 's/^/     /')" ;;
  esac
fi

if [ "$DRY_RUN" = 1 ]; then
  note "would run: scripts/argocd-hard-sync.sh --promote $VERSION"
  exit 0
fi

scripts/argocd-hard-sync.sh --promote "$VERSION" || die "$VERSION is published and promoted in git, but the box did NOT converge.
   The Applications named above are still on the previous version.
   Fix them and re-run the sync; do not re-publish."

say "Done. $VERSION is published, promoted and running."
