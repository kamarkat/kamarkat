#!/usr/bin/env bash
# Publish a bundle version to the registry.
#
#   scripts/release.sh                       publish the next rc after the newest published
#   scripts/release.sh --version 0.1.17-rc.1 an explicit version
#   scripts/release.sh --dry-run             say what it would do, change nothing
#   scripts/release.sh --stable --version 0.2.8   publish a STABLE version by
#                                            creating its GitHub release, which
#                                            is what the publish workflow runs on
#   scripts/release.sh --check               rehearse the next version and check
#                                            every component behind latest has a reason
#
# NO BOX IS TOUCHED HERE. A release is the platform's act and it ends when the
# version exists in the registry; moving a box onto a version is that box
# owner's act, done with `kakt promote` using their own credentials in their own
# repository (ADR-065).
#
# The rewrite that moves a box between versions lives in `kakt promote`
# (internal/kakt-cli/tenant/promote.go): all references moved or none, no stale
# checkout, no .state/ staged.
set -euo pipefail

OWNER="${OWNER:-kamarkat}"
VERSION=""
DRY_RUN=0
CHECK=0
STABLE=0

while [ $# -gt 0 ]; do
  case "$1" in
    --version)      VERSION="${2:?--version needs a value}"; shift 2 ;;
    --dry-run)      DRY_RUN=1; shift ;;
    --check)        CHECK=1; shift ;;
    --stable)       STABLE=1; shift ;;
    -h|--help)      sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)              echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

# --check spends nothing and touches no tenant: every step that would change
# something is the rehearsal's instead.
if [ "$CHECK" = 1 ]; then DRY_RUN=1; fi

# A STABLE version is published by CI, from a GitHub Release, and never from a
# workstation. The publish workflow triggers on `release: types: [created]`
# (ADR-063: a version names a set that was tested together, and a release is a
# deliberate act with an author and a record). So --stable creates that release
# and follows the run; it does not package anything here.
#
# Without this flag the publish step runs `make publish-local`, which is right
# for a candidate and wrong for a stable version: it would push charts that no
# release names, so nothing would carry the tag, the CLI binaries would never be
# built, and the Homebrew tap would stay on the previous version.
if [ "$STABLE" = 1 ]; then
  case "$VERSION" in
    "") echo "--stable needs --version: a stable version is never incremented for you," >&2
        echo "  because whether the next one is a patch, a minor or a major says what the" >&2
        echo "  release means. Name it." >&2; exit 2 ;;
    *-rc.*) echo "--stable publishes a stable version, and $VERSION is a candidate." >&2
            echo "  Drop --stable to publish a candidate from here." >&2; exit 2 ;;
  esac
fi
# The same rule from the other direction. A named stable version with no
# --stable would be packaged and pushed from here, which is the one way to get
# charts in the registry that no release, tag, binary or tap entry agrees with.
#
# PUBLISHING ONLY. --check rehearses and pushes nothing, so a stable version is
# exactly what it should accept: rehearsing 0.3.0 is what you do BEFORE
# publishing 0.3.0, and refusing it left the one safe way to test a stable
# release unreachable. This guard was conditional on the publish path and lost
# that condition when the variable carrying it was removed.
if [ "$CHECK" = 0 ] && [ "$STABLE" = 0 ] && [ -n "$VERSION" ]; then
  case "$VERSION" in
    *-rc.*) ;;
    *) echo "$VERSION is a stable version, and a stable version is published by its" >&2
       echo "  GitHub Release rather than from here. Add --stable." >&2; exit 2 ;;
  esac
fi

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
say "1. Decide the version"
CURRENT="$(scripts/release/release-data.py newest --owner "$OWNER")" \
  || die "could not read the newest published version from ghcr.io/$OWNER (gh auth status)"
note "newest published: $CURRENT"
CHOSE_VERSION=0
if [ -z "$VERSION" ]; then
  CHOSE_VERSION=1
  case "$CURRENT" in
    *-rc.*) VERSION="${CURRENT%-rc.*}-rc.$((${CURRENT##*-rc.} + 1))" ;;
    # A STABLE VERSION IS NOT INCREMENTED FOR A PUBLISH. Whether the next one
    # is a patch, a minor or a major says what the release means, and a version
    # is consumed the moment it is published (ADR-063) -- so a guess there
    # spends a number that was meant for something else.
    #
    # A REHEARSAL IS DIFFERENT, because it consumes nothing: --check runs with
    # SKIP_PUSH=1 and pushes no artefact, so the number is still free when it
    # finishes. Refusing to rehearse was stricter than the reason for the rule,
    # and it made the cheapest step the one with the most friction. It takes the
    # patch candidate and says which one it took, so a rehearsal of the wrong
    # number is visible rather than assumed.
    *) if [ "$CHECK" = 1 ]; then
         VERSION="${CURRENT%.*}.$((${CURRENT##*.} + 1))"
       else
         die "$CURRENT is a stable version, and the next one says what this release means:
   a patch, a minor or a major. Name it:

     kakt release check --version ${CURRENT%.*}.$((${CURRENT##*.} + 1))   # patch
     kakt release check --version <X.Y.Z>

   Only an -rc.N version is incremented automatically."
       fi ;;
  esac
fi
# Said only when the rehearsal CHOSE the version. Printed unconditionally it
# told an operator who had just passed --version that publishing "requires
# --version", which reads as a contradiction of what they typed.
if [ "$CHECK" = 1 ] && [ "$CHOSE_VERSION" = 1 ]; then
  note "rehearsing:       $VERSION (chosen; publishing a stable version requires --version)"
fi
note "next:             $VERSION"

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
  # REHEARSE FROM THE STATE CI STARTS IN. The embedded platform tree is build
  # output and git-ignored, so a CI run begins without it and a workstation
  # begins with whatever the last build left. That difference is not academic:
  # 0.3.0 passed this rehearsal and then failed its own closure gate in CI with
  # all 111 scripts reported missing, because the gate ran before anything
  # generated the tree. Locally the tree was already there, so the one condition
  # that mattered was the one condition a rehearsal could not see.
  #
  # Removing it is safe and is the point: it is generated, ignored by git, and
  # regenerated by publish.sh in the next breath.
  say "3. Rehearse $VERSION from a clean embed (nothing is pushed)"
  rm -rf internal/platform/embedded/manifests internal/platform/embedded/scripts
  PREVIOUS_VERSION="$CURRENT" SKIP_PUSH=1 make publish-local VERSION="$VERSION" OWNER="$OWNER" \
    || die "the rehearsal failed a gate -- fix the cause on main before releasing"
  # Both directions a package can be unreachable, checked before a release finds
  # out. PULLABLE is what a tenant needs: ArgoCD holds no OCI credential and the
  # nodes no imagePullSecret, so a private package is unreachable from a box.
  # PUSHABLE is what CI needs: GHCR grants a workflow write only through a
  # package's link to a repository, so an unlinked one refuses GITHUB_TOKEN with
  # a 403 on blob upload whatever `packages: write` says.
  #
  # Here because neither state is in this repository and nothing else looks. The
  # 2026-11-04 org rename left all 17 packages public and unlinked, and the
  # release that followed reported every gate green and then failed on its first
  # push. A rehearsal that cannot see that is rehearsing the wrong thing.
  say "4. Every package is publicly pullable and pushable by CI"
  scripts/package/flip-public.sh --check \
    || die "fix the packages above in the GitHub UI, then run this again"
  # THE BINARY A TENANT INSTALLS, built and exercised here rather than only in
  # CI after the charts are pushed.
  #
  # This gate lives in the release workflow, which runs it AFTER publish.sh has
  # consumed the version -- so everything it catches is caught too late to fix
  # cheaply. It caught a renamed command once and a renamed flag twice, each
  # time on a published version that then could not be released. A rehearsal
  # that does not build the thing a tenant runs is not rehearsing the release.
  #
  # TARGETS=host: one binary for this machine, because what is being checked is
  # whether a released build resolves its platform content outside a checkout,
  # and that answer does not differ per architecture.
  # EVERY VALUE THE RELEASE NEEDS, BEFORE THE VERSION IS SPENT. DOCS_BASE_URL is
  # read in the second-to-last step of the release workflow, which runs long
  # after publish.sh has pushed the charts -- so an unset variable consumed
  # 0.2.6 and left it with no CLI attached, no disclosure and no support-state
  # report. A requirement that can only be met before publishing has no business
  # being discovered after it.
  say "5. Every value the release workflow needs is set"
  missing=""
  for v in DOCS_BASE_URL; do
    # This checkout's own repository, resolved by gh. Composed from $OWNER it
    # would be "$OWNER/$OWNER", which is right here only because the
    # organisation and the repository happen to share a name.
    gh variable list --json name -q '.[].name' 2>/dev/null \
      | grep -qx "$v" || missing="$missing $v"
  done
  if [ -n "$missing" ]; then
    die "the release workflow reads these repository variables and they are not set:$missing
   Set each once, and every release after this one has it:
     gh variable set DOCS_BASE_URL --body https://<your documentation host>
   Nothing has been published."
  fi
  note "every required repository variable is set"

  say "6. The released CLI bootstraps from its binary alone"
  TARGETS=host ./scripts/package/build-cli.sh "$VERSION" dist/bin >/dev/null \
    || die "the release CLI did not build"
  ./scripts/validate/released-cli-standalone.sh "$VERSION" \
    || die "the released binary cannot bootstrap a box outside a checkout (ADR-063).
   Nothing has been published."

  say "7. Every component behind its latest release has a reason"
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
if [ "$STABLE" = 1 ]; then
  say "3. Publish $VERSION by creating its GitHub release"

  # The repository is READ from the one declaration that already names it, not
  # assembled from $OWNER. "$OWNER/$OWNER" happens to be right today only
  # because the organisation and the repository share a name.
  . scripts/lib/platform-source-url.sh
  REPO="$(platform_source_url)" || die "could not read the platform repository URL"
  REPO="${REPO#*://github.com/}"; REPO="${REPO%.git}"; REPO="${REPO%/}"
  note "repository:       $REPO"

  command -v gh >/dev/null 2>&1 || die "gh is required to create a release"
  gh auth status >/dev/null 2>&1 || die "gh is not authenticated (gh auth login)"

  # WHAT CI BUILDS IS A COMMIT, NOT THIS WORKING TREE.
  #
  # `kakt release check` rehearses the tree you have, and a candidate publishes
  # that same tree -- `make publish-local` packages it directly. A stable
  # version does neither: the run that publishes it checks out the commit the
  # release names. Targeting a branch would publish whatever that branch held at
  # the time, so the thing rehearsed and the thing published could differ with
  # nothing saying so.
  #
  # So the release names a commit, and the checkout has to BE that commit.
  DIRTY="$(git status --porcelain)"
  [ -z "$DIRTY" ] || die "the platform checkout has uncommitted changes, and a stable release
   publishes a COMMIT -- so these would be rehearsed here and absent from what CI builds:

$(printf '%s\n' "$DIRTY" | sed 's/^/     /')

   Commit and push them, or stash them, then run this again."

  SHA="$(git rev-parse HEAD)"
  git fetch -q origin || die "could not reach the remote to confirm HEAD is published"
  [ -n "$(git branch -r --contains "$SHA" 2>/dev/null)" ] \
    || die "HEAD ($SHA) is not on the remote, so there is nothing for the release to name.
   Push this branch and run this again."
  note "commit:           $SHA"

  # A version is published once (ADR-063). A re-created release with different
  # content is the one failure GitOps cannot diagnose, so an existing one stops
  # this before anything is promoted.
  if gh release view "v$VERSION" --repo "$REPO" >/dev/null 2>&1; then
    die "v$VERSION already has a release on $REPO. A version is published once --
   take the next one, or move a box onto this one with: kakt promote --version $VERSION."
  fi

  # NO --draft, and no --notes.
  #
  # --draft: the workflow triggers on `release: types: [created]`, which does
  # NOT fire for a draft, nor for publishing an existing draft. A draft here
  # would create a release, publish nothing, and look like it worked.
  #
  # --target is the SHA checked above, not a branch, so the version names the
  # exact tree that was rehearsed -- which is what ADR-063 asks of a version.
  #
  # --notes: the workflow writes the body itself at the end, pointing at the
  # version page, and a gate refuses a release that does not. Notes given here
  # are overwritten, so this passes the shortest placeholder that keeps
  # `gh release create` non-interactive.
  run gh release create "v$VERSION" --repo "$REPO" \
    --target "$SHA" --title "v$VERSION" --notes "Publishing." \
    || die "could not create the release -- nothing was published or promoted"

  if [ "$DRY_RUN" = 1 ]; then
    note "would follow the publish run and wait for it to finish"
  else
    # THE RUN FOR THIS COMMIT, matched on the SHA.
    #
    # "The newest release run" is wrong whenever one already exists: a previous
    # attempt at the same version had left a failed run, the new one had not
    # registered yet, and this reported that old failure as the outcome of a
    # publish that was in fact still running -- "has already completed with
    # 'failure'", seconds after creating the release.
    #
    # headSha is exact and is a field gh has always returned, and the release
    # was created at this very commit, so there is nothing to infer.
    say "3b. Follow the publish run"
    RUN_ID=""
    for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do
      RUN_ID="$(gh run list --repo "$REPO" --workflow publish-platform-charts.yml \
        --event release --limit 20 --json databaseId,headSha \
        -q "[.[] | select(.headSha == \"$SHA\")] | .[0].databaseId" 2>/dev/null || true)"
      [ -n "$RUN_ID" ] && [ "$RUN_ID" != "null" ] && break
      sleep 5
    done
    [ -n "$RUN_ID" ] && [ "$RUN_ID" != "null" ] \
      || die "the release v$VERSION exists but no publish run appeared. Check
   https://github.com/$REPO/actions -- the release must NOT be promoted until it
   has published, because promoting first writes a reference to nothing."
    note "run:              https://github.com/$REPO/actions/runs/$RUN_ID"
    gh run watch "$RUN_ID" --repo "$REPO" --exit-status \
      || die "the publish run failed. v$VERSION has a release but no charts, so
   nothing has been promoted. Fix the run and re-run it; do not create the
   release again."
    note "published by CI"
  fi
else
say "3. Publish $VERSION"
# The inventory compares this version against the newest published.
export PREVIOUS_VERSION="$CURRENT"
run make publish-local VERSION="$VERSION" OWNER="$OWNER" \
  || die "publish failed -- nothing has been promoted, so the box is untouched"
fi

say "Published $VERSION."
note "No box is touched by a release. Its owner moves it with: kakt promote --version $VERSION"
