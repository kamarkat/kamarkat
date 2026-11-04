#!/usr/bin/env python3
"""The at-rest encryption key is per cluster, so its delivery object must be too.

WHY THIS EXISTS

ADR-003 §6 adopts one encryption key per cluster, and ADR-076 holds it in the escrow
under that cluster's id. The key reaches a control plane as a Secret in
`platform-capi`, named by a ClusterClass's control-plane template -- and a
ClusterClass is shared by EVERY cluster of its class.

So a Secret name written literally into a template is one object serving all of
them. That shipped: both classes named `secret-encryption-config` flat. Every
cluster of a class would have read the same key, and the first cluster to rotate
would have left the others' etcd undecryptable by a key they still believed in.
Worse, the object was owned by an ExternalSecret, so deleting that ExternalSecret
garbage-collected the Secret and left a ClusterClass referencing nothing.

THE NAME IS BUILT TWICE, IN TWO LANGUAGES THAT CANNOT SEE EACH OTHER:

  Go      assets.EncryptionSecretName, for `kakt encryption enable` and the Day-0
          provisioner, which CREATE the Secret.
  CAPI    a `valueFrom.template` in each ClusterClass's `secretEncryptionConfig`
          patch, rendered from `{{ .builtin.cluster.name }}` at topology-reconcile
          time, which is what the control plane READS.

If those two disagree the Secret exists under one name and is referenced under
another. CAPI does not report that as a missing Secret: the control-plane node
simply never finishes bootstrapping, and the KCP says NodeProvisioning. The two
halves are checked against each other here because nothing else can.

FOUR THINGS ARE ASSERTED:

  agreement     every ClusterClass patch builds the same name the Go helper does.
  per-cluster   that name interpolates the cluster, rather than being a literal.
  completeness  a control-plane template that MOUNTS /etc/kubernetes/enc has a
                patch delivering the file into it. A mount with no file is the
                failure mode that reads as a missing file rather than a missing
                delivery -- and once the API server argument lands in v4, it is a
                control plane that does not start.
  producer-gap  every per-cluster Secret referenced via contentFrom.secret in any
                ClusterClass has a named producer helper in internal/assets/embed.go.

Emits tab-separated lines the shell wrapper classifies:
    OK<TAB><message>
    BAD<TAB><message>
    NONE
"""
import re
import sys
from pathlib import Path

HERE = Path(__file__).resolve()
ZERO_OPS = HERE.parents[3]

GO_HELPER = ZERO_OPS / "internal" / "assets" / "embed.go"
DAY0_TEMPLATE = ZERO_OPS / "internal" / "assets" / "manifests" / "secrets" / "secret-encryption-config.yaml"

# `internal/platform/embedded` is a GENERATED copy of `manifests`
# (scripts/package/embed-platform-assets.sh), so judging it would double findings at
# a path nobody edits.
TREES = [
    ZERO_OPS / "manifests" / "providers" / "hetzner",
    ZERO_OPS / "internal" / "assets" / "manifests" / "classes",
]

ENC_PATH = "/etc/kubernetes/enc"
PATCH_NAME = "secretEncryptionConfig"


def go_suffix():
    """The suffix assets.EncryptionSecretName appends, read from the source."""
    text = GO_HELPER.read_text(encoding="utf-8")
    m = re.search(r'func EncryptionSecretName\(cluster string\) string \{\s*\n\s*return cluster \+ "([^"]+)"', text)
    if not m:
        return None
    return m.group(1)


def go_secret_helpers():
    """Map of suffix -> function name for all cluster Secret helpers in embed.go."""
    text = GO_HELPER.read_text(encoding="utf-8")
    matches = re.findall(
        r'func\s+([A-Za-z0-9_]+SecretName)\(cluster string\)\s+string\s*\{\s*\n\s*return\s+cluster\s*\+\s*"([^"]+)"',
        text,
    )
    return {suffix: fn for fn, suffix in matches}


def main():
    suffix = go_suffix()
    known_helpers = go_secret_helpers()
    if suffix is None:
        print("BAD\tinternal/assets/embed.go no longer defines EncryptionSecretName as "
              "`cluster + \"<suffix>\"`; this check cannot read the Go half of the name, and the "
              "two halves can now drift without anything noticing")
        return 0

    findings = []

    # The Day-0 template must take the name rather than spell one.
    if DAY0_TEMPLATE.exists():
        t = DAY0_TEMPLATE.read_text(encoding="utf-8")
        m = re.search(r"^metadata:\n\s*name:[ \t]*(.+?)[ \t]*$", t, re.M)
        if not m:
            findings.append("BAD\tthe Day-0 encryption Secret template has no metadata.name")
        elif m.group(1) != "{{ .SecretName }}":
            findings.append(
                f"BAD\t{DAY0_TEMPLATE.relative_to(ZERO_OPS)} names the Secret {m.group(1)!r} "
                f"instead of taking {{{{ .SecretName }}}}; a name written here is one object for "
                f"every cluster the management plane builds"
            )
    else:
        findings.append(f"BAD\t{DAY0_TEMPLATE.relative_to(ZERO_OPS)} is missing")

    classes = 0
    for tree in TREES:
        if not tree.exists():
            findings.append(
                f"BAD\t{tree.relative_to(ZERO_OPS)} does not exist; this check is looking in the "
                f"wrong place and would pass a class with no delivery at all"
            )
            continue
        for path in sorted(tree.rglob("*.yaml")):
            text = path.read_text(encoding="utf-8")
            rel = path.relative_to(ZERO_OPS)

            if "kind: ClusterClass" not in text:
                continue
            classes += 1

            # Completeness: a mount with no delivery.
            mounts = f"mountPath: {ENC_PATH}" in text
            if not mounts:
                # Check referenced control-plane template in the same tree
                cp_refs = re.findall(r'controlPlane:\s*\n\s*ref:\s*\n(?:\s*apiVersion:.*\n)?(?:\s*kind:.*\n)?\s*name:\s*(\S+)', text)
                for cp_name in cp_refs:
                    for sibling in tree.rglob("*.yaml"):
                        stext = sibling.read_text(encoding="utf-8")
                        if f"name: {cp_name}" in stext and f"mountPath: {ENC_PATH}" in stext:
                            mounts = True
                            break
                    if mounts:
                        break
            has_patch = f"name: {PATCH_NAME}" in text
            if mounts and not has_patch:
                findings.append(
                    f"BAD\t{rel} mounts {ENC_PATH} into the API server but has no "
                    f"{PATCH_NAME} patch delivering enc.yaml into it; the mount is an empty "
                    f"directory, which reads as a missing file rather than a missing delivery"
                )
            if not mounts and has_patch:
                findings.append(
                    f"BAD\t{rel} delivers enc.yaml via {PATCH_NAME} but does not mount "
                    f"{ENC_PATH} into the API server; the API server runs as a static pod and "
                    f"cannot see a path kubeadm does not mount"
                )

            # Agreement and per-cluster-ness.
            names = re.findall(r'"name":\s*"([^"]*encryption-config[^"]*)"', text)
            literal = re.findall(
                r"^\s*name:\s*([a-z0-9][a-z0-9-]*-encryption-config)\s*$", text, re.M
            )
            if has_patch and not names:
                findings.append(
                    f"BAD\t{rel} has a {PATCH_NAME} patch but no Secret name in it matching "
                    f"*{suffix}; the patch is not delivering what this check can verify"
                )
            for n in names:
                if not n.endswith(suffix):
                    findings.append(
                        f"BAD\t{rel} builds the Secret name {n!r}, which does not end in "
                        f"{suffix!r} as assets.EncryptionSecretName does; the Secret would be "
                        f"created under one name and read under another, and CAPI reports that as "
                        f"a node that never finishes bootstrapping"
                    )
                elif "{{ .builtin.cluster.name }}" not in n:
                    findings.append(
                        f"BAD\t{rel} builds the Secret name {n!r} without "
                        f"{{{{ .builtin.cluster.name }}}}; a ClusterClass is shared by every "
                        f"cluster of its class, so this is one key for all of them and the first "
                        f"rotation makes the rest undecryptable (ADR-003 §6)"
                    )
            for n in literal:
                findings.append(
                    f"BAD\t{rel} names a Secret {n!r} as a YAML literal; the encryption Secret is "
                    f"per cluster and must come from the {PATCH_NAME} patch, not from a template "
                    f"body that every cluster of the class shares"
                )

            # Producer gap: every per-cluster Secret referenced via contentFrom.secret
            # in any patch must have a corresponding named producer helper in internal/assets/embed.go.
            secret_content_refs = re.findall(r'"secret":\s*\{\s*"name":\s*"([^"]+)"', text)
            secret_content_refs += re.findall(r'secret:\s*\n\s*name:\s*([^\s\n]+)', text)
            for s in secret_content_refs:
                if "{{ .builtin.cluster.name }}" in s:
                    s_suffix = s.replace("{{ .builtin.cluster.name }}", "").strip()
                    if s_suffix not in known_helpers:
                        findings.append(
                            f"BAD\t{rel} references per-cluster Secret {s!r}, but suffix {s_suffix!r} "
                            f"has no producer helper in internal/assets/embed.go"
                        )

    if classes == 0:
        print("NONE")
        return 0

    for line in findings:
        print(line)
    if not findings:
        print(
            f"OK\t{classes} ClusterClass(es) deliver a per-cluster encryption Secret, named "
            f"<cluster>{suffix} on both the Go and the CAPI side, with "
            f"Secret producer coverage verified"
        )
    return 0


if __name__ == "__main__":
    sys.exit(main())
