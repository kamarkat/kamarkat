#!/usr/bin/env bash
# The release body: a pointer to the version's page in the documentation.
#
# ONE LINK, AND NOTHING ELSE. This used to write the disclosure into the release
# itself -- the versions it spans, the security fixes among them, whether the
# predecessor is restorable, the minimum version it may be taken from, the
# support window -- as two tables appended to whatever a maintainer had typed.
# ADR-064 revision 2 moves it: the version page carries all of it, rendered from
# the same inventory the release publishes, and the release says where that page
# is. One place holds the answer rather than two that can disagree, and the page
# can be corrected after a release while a release body is what it was.
#
# NOTHING IS EXPECTED FROM WHOEVER CREATES THE RELEASE. `gh release create` is
# run with no --notes: this supplies the body, so a release never depends on a
# maintainer remembering to describe it.
set -euo pipefail

VERSION="${1:?usage: release-metadata.sh <version>}"

# WHERE THE DOCUMENTATION IS SERVED. Declared, never guessed: the host is set in
# the documentation platform rather than in this repository, so there is nothing
# here to derive it from, and a wrong default would put a dead link in every
# release body and report success doing it.
if [[ -z "${DOCS_BASE_URL:-}" ]]; then
    echo "release-metadata: DOCS_BASE_URL is not set, so the release body would" >&2
    echo "  carry no link to the version page -- which is the whole body." >&2
    echo "" >&2
    echo "  Set it once as a repository variable, and the release workflow passes it:" >&2
    echo "    gh variable set DOCS_BASE_URL --body https://<your documentation host>" >&2
    exit 1
fi

# The page's own slug, as scripts/release/release-pages.py names the file it
# writes: the version with every character that is not a letter or a digit
# turned into a hyphen.
slug=$(printf '%s' "$VERSION" | tr -c '[:alnum:]' '-' | tr -s '-' | sed 's/-$//')

printf '%s/versions/releases/%s\n' "${DOCS_BASE_URL%/}" "$slug"
