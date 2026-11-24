#!/usr/bin/env python3
"""ADR-076's escrow list and the implementation must name the same artefacts.

WHY THIS EXISTS

The escrow holds what a box cannot be rebuilt without. ADR-076 enumerates it, and
that enumeration is the only place the set is written down -- the implementation
names each artefact at the call site that backs it up, so the set exists nowhere as
a set.

Which makes the failure shape unusually bad. An artefact that BELONGS in the escrow
and is not backed up produces no error, no warning and no difference in behaviour.
A box bootstraps, runs and behaves identically for its whole life. The absence
surfaces on the day the cluster is gone -- and on that day the escrow cannot be
added retroactively, which is the exact reasoning ADR-076 gives for the escrow
being mandatory rather than opt-in.

So the list is checked against the code instead of trusted. Both directions:

  in the ADR, not in the code     an artefact the ADR promises and nothing backs up.
                                  This is the dangerous direction. zitadel-masterkey
                                  is this today, deliberately and recorded as such.

  in the code, not in the ADR     an artefact being escrowed that the list does not
                                  mention. Less dangerous and still wrong: a
                                  recovery runs from the list, so an artefact
                                  nobody knows exists is one nobody restores.

Emits tab-separated lines the shell wrapper classifies:
    OK<TAB><message>
    BAD<TAB><message>
    WARN<TAB><message>
    NONE
"""
import re
import sys
from pathlib import Path

HERE = Path(__file__).resolve()
ZERO_OPS = HERE.parents[3]
ADR = ZERO_OPS / "docs/adr/076-reaching-a-box-you-own.md"
# THE PACKAGE, not one file of it. This read escrow.go alone, and the package has
# eight files: infisical-admin-credential is declared in admin_credential.go, so
# the check called it documented-but-unimplemented for as long as it has existed.
ESCROW_PKG = ZERO_OPS / "internal/platform/escrow"
# OpenBao's artefacts (ADR-117 §6) are named by the CLI package that writes them.
OPENBAO_PKG = ZERO_OPS / "internal/kakt-cli/openbao"

# Artefacts the ADR lists as NOT YET IMPLEMENTED. Each needs that status written
# beside it in the ADR, so the exemption cannot be silent -- it is a promise with a
# date attached, not a gap that stopped being mentioned.
PENDING_MARKER = "NOT YET IMPLEMENTED"


def adr_artefacts() -> dict:
    """Artefact name -> whether the ADR marks it pending.

    Read from the normative table under "What the escrow holds", by its backticked
    first column. Parsed from that section only: the ADR mentions the master keys
    and the kubeconfig in prose elsewhere, and prose is not the list.
    """
    text = ADR.read_text()
    # The section heading is matched, and a rename must FAIL rather than silently
    # find nothing: this check is the only thing asserting the list and the code
    # agree, so a parser that quietly matched an empty section would report green
    # for a repository that no longer enumerates the escrow.
    try:
        start = text.index("### The recovery contract")
        end = text.index("### The escrow account is the tenant's", start)
    except ValueError:
        print("BAD\tADR-076 no longer contains a '### The recovery contract' section; "
              "the normative escrow list cannot be read")
        return {}
    section = text[start:end]

    # The first column, backticked. Rows whose artefact is not a single backticked
    # token -- the "ordinary application secrets" row, which names a class rather
    # than an artefact -- are not escrow artefacts and are skipped by this pattern.
    names = re.findall(r"^\|\s*`([a-z0-9-]+)`\s*\|", section, re.M)
    pending = set()
    for name in names:
        # The pending note names the artefact and the marker in one paragraph.
        for para in section.split("\n\n"):
            if f"`{name}`" in para and PENDING_MARKER in para:
                pending.add(name)
    return {n: (n in pending) for n in names}


def code_artefacts() -> set:
    """Artefact names the escrow implementation can store, from its constants.

    Matched on string literals assigned to a const whose name says artefact or
    secret name. Deliberately not on every string in the file -- a path fragment or
    a log message is not an artefact.
    """
    if not ESCROW_PKG.is_dir():
        return set()
    names: set = set()
    for path in sorted(ESCROW_PKG.glob("*.go")) + sorted(OPENBAO_PKG.glob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        names.update(
            re.findall(
                r"(?:const\s+|^\s+)[A-Za-z]*[Aa]rtifact[A-Za-z]*\s*=\s*\"([a-z0-9-]+)\"",
                path.read_text(), re.M,
            )
        )
    return names


def main() -> int:
    if not ADR.exists():
        # NOT a skip. ADR-076 is this repository's own file and the only enumeration
        # of what a box can be rebuilt from. Its absence means the list is gone, which
        # is strictly worse than the list being wrong -- a skip here would report a
        # clean build for a repository that no longer says what the escrow holds.
        print("BAD\tADR-076 is missing, so nothing states what the escrow must hold")
        return 0

    listed = adr_artefacts()
    if not listed:
        print("BAD\tADR-076 names no escrow artefacts; the normative list is missing or its table changed shape")
        return 0

    implemented = code_artefacts()

    for name, pending in sorted(listed.items()):
        if name in implemented:
            if pending:
                print(
                    f"BAD\t{name} is implemented but ADR-076 still marks it {PENDING_MARKER}; "
                    f"the list says a box cannot be recovered when it can"
                )
            else:
                print(f"OK\t{name} is listed in ADR-076 and named by the escrow implementation")
            continue
        if pending:
            # The known gap. A warning, not a failure: the ADR records it on
            # purpose, and failing here would block every build until it is closed.
            print(
                f"WARN\t{name} is listed in ADR-076 as {PENDING_MARKER} and is escrowed nowhere — "
                f"a box that loses its secret store cannot recover it"
            )
            continue
        print(
            f"BAD\t{name} is listed in ADR-076 as escrowed but the implementation names no such "
            f"artefact; nothing backs it up and nothing will report that until a recovery"
        )

    for name in sorted(implemented - set(listed)):
        print(
            f"BAD\t{name} is escrowed by the implementation but absent from ADR-076's list; "
            f"a recovery runs from that list, so an unlisted artefact is one nobody restores"
        )

    return 0


if __name__ == "__main__":
    sys.exit(main())
