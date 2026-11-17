#!/usr/bin/env python3
"""Fail when a reusable workflow reads a secret it does not declare.

GitHub resolves `secrets.X` inside a reusable workflow to the EMPTY STRING when
X is not declared under `on.workflow_call.secrets` -- no warning, no failure, no
log line. The step runs with the variable unset.

That is how the tenant path broke. tenant-bootstrap.yml passed nine credentials
to the Day-0 step -- the registry pair, object storage, and five Grafana Cloud
values -- and declared none of them. A tenant who had set every one of those
repository secrets correctly got a box that could not pull their own images and
silently had no database backups, while the local loop worked because it reads
k8-secrets/ instead.

Three checks, because each fails differently:

  used but not declared      the value silently vanishes (the bug above)
  declared but not forwarded the same vanishing, one level up -- for EVERY
    by a caller              declared secret, optional ones included: an
                             optional credential a caller drops is a box that
                             quietly lacks a capability (the tenant template
                             never forwarded IDENTITY_PROVIDER_SMTP_URL, so no box it
                             bootstrapped could send mail)
  forwarded but not declared GitHub refuses the whole run

Usage: workflow-secrets.py [repo-root]
"""
import os
import pathlib
import re
import subprocess
import sys

try:
    import yaml
except ImportError:
    sys.exit("PyYAML required")

# Always available; never declared.
BUILTIN = {"GITHUB_TOKEN"}


def err(msg):
    if os.environ.get("GITHUB_ACTIONS"):
        print(f"::error::{msg}")
    else:
        print(f"error: {msg}", file=sys.stderr)


def load(path):
    with open(path) as fh:
        return yaml.safe_load(fh), open(path).read()


def main():
    root = sys.argv[1] if len(sys.argv) > 1 else "."
    failures = []

    reusable = {}  # path -> declared secret names
    for base, _, files in os.walk(os.path.join(root, ".github", "workflows")):
        for name in sorted(files):
            if not name.endswith((".yml", ".yaml")):
                continue
            path = os.path.join(base, name)
            doc, text = load(path)
            if not isinstance(doc, dict):
                continue
            # PyYAML parses the `on:` key as the boolean True.
            on = doc.get("on", doc.get(True)) or {}
            call = (on or {}).get("workflow_call") if isinstance(on, dict) else None
            if call is None:
                continue

            declared = set((call or {}).get("secrets") or {})
            reusable[os.path.relpath(path, root)] = declared

            used = set(re.findall(r"secrets\.([A-Z_][A-Z0-9_]*)", text)) - BUILTIN
            missing = sorted(used - declared)
            if missing:
                failures.append(
                    f"{os.path.relpath(path, root)} reads {', '.join(missing)} but "
                    f"declares them nowhere under workflow_call.secrets. GitHub "
                    f"resolves each to the empty string, silently.")

    # Callers: anything naming a reusable workflow must forward what it declares.
    # The repository's own files only, as git tracks them. A directory walk
    # tested ".git" as a substring, which also matched ".github", so no caller
    # was ever checked; and it would reach untracked checkouts of other
    # repositories (.local-e2e/), whose callers pin older releases.
    tracked = subprocess.run(["git", "-C", root, "ls-files", "-z", "*.yml", "*.yaml"],
                             capture_output=True, text=True, check=True).stdout.split("\0")
    for rel in sorted(p for p in tracked if p and not p.startswith("reference-projects/")):
        path = os.path.join(root, rel)
        with open(path) as fh:
            if "uses:" not in fh.read():
                continue  # only a job with `uses:` calls a reusable workflow
        try:
            doc, _ = load(path)
        except yaml.YAMLError:
            # Multi-document manifests and templated YAML are not workflows;
            # a broken workflow is yamllint's to report.
            continue
        if not isinstance(doc, dict) or "jobs" not in doc:
            continue
        for job in (doc.get("jobs") or {}).values():
            if not isinstance(job, dict):
                continue
            uses = str(job.get("uses") or "")
            if not uses:
                continue
            target = next((p for p in reusable if uses.split("@")[0].endswith(p)), None)
            if target is None:
                continue
            forwarded = job.get("secrets")
            if forwarded == "inherit":
                continue
            forwarded = set(forwarded or {})
            declared = reusable[target]
            caller = os.path.relpath(path, root)
            missing = sorted(declared - forwarded)
            if missing:
                failures.append(
                    f"{caller} calls {target} without forwarding "
                    f"{', '.join(missing)}: each arrives empty, silently.")
            extra = sorted(forwarded - declared)
            if extra:
                failures.append(
                    f"{caller} forwards {', '.join(extra)}, which {target} "
                    f"does not declare: GitHub refuses the run.")

    # ── 4. a credential the ESCROW holds is not also a repository secret ────
    #
    # This check was the other way round when it was written: "every credential
    # Day-0 reads from the environment must be forwarded by the tenant workflow".
    # That was true of the old design and enforced it -- it would have failed the
    # migration that replaced it, which is what a gate pointed at an abandoned
    # contract does.
    #
    # The contract now: `kakt box init` escrows the credentials a box is built
    # with, Day-0 reads them from there, and the repository holds the escrow's
    # four coordinates plus GITOPS_REPO_WRITE_TOKEN -- the one credential a
    # GitHub Actions build of the tenant's own needs, which cannot read an
    # escrow. A second copy of anything else here "only widens where it" leaks
    # (ADR-076), and an Actions secret cannot be read back, so the copy could
    # never be compared with the one in use.
    #
    # The authority is the CODE: the artefacts escrow.go declares box-scoped, and
    # the environment names components/secrets_platform.go no longer reads.
    allowed = {
        "ESCROW_PROVIDER_URL", "ESCROW_PROVIDER_PROJECT_ID",
        "ESCROW_PROVIDER_CLIENT_ID", "ESCROW_PROVIDER_CLIENT_SECRET",
        "GITOPS_REPO_WRITE_TOKEN",
    }
    workflow = pathlib.Path(root, "manifests", "tenants", "gitops-template",
                            ".github", "workflows", "bootstrap-cluster.yml")
    used = 0
    if workflow.is_file():
        referenced = set(re.findall(r"secrets\.([A-Z][A-Z0-9_]{3,})", workflow.read_text()))
        used = len(referenced)
        extra = sorted(referenced - allowed)
        if extra:
            failures.append(
                f"{os.path.relpath(workflow, root)} reads {', '.join(extra)} from repository "
                f"secrets, and a box's credentials are escrowed now (ADR-076's amendment).\n"
                f"    `kakt box init` enrols them and Day-0 reads the escrow; a copy here "
                f"cannot be read back, so it can never be compared with the copy in use. "
                f"Only {', '.join(sorted(allowed))} belong on the repository.")

    for f in failures:
        err(f)
    print(f"workflow-secrets: {len(reusable)} reusable workflow(s), "
          f"{used} repository secret(s) the tenant workflow reads of "
          f"{len(allowed)} allowed, {len(failures)} failure(s)")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
