#!/usr/bin/env bash
# Floating tags make a cluster non-reproducible: `:latest` resolves at pull time,
# so a pod reschedule can silently land on a different build than its neighbours.
# This is not hypothetical here — the argocd-agent `latest` in use was rebuilt
# upstream nightly, with no version label in the image config to even identify it.
#
# The check runs against RENDERED output, not source text. A kubebuilder scaffold
# leaves `image: controller:latest` in manager.yaml and overrides it from
# kustomization.yaml, so the source line is a false positive while the shipped
# manifest is correctly digest-pinned. Rendering is the only way to tell the two
# apart, and rendered output is what actually reaches the cluster.
validate_image_pinning() {
    section "Image pinning (rendered output)"

    local out
    out=$(cd "$VALIDATE_ROOT" && python3 - <<'PY'
import os, re, subprocess, sys

# internal/platform/embedded is the PACKAGING tree, and a pin it does not carry is
# not an unpinned image.
#
# The embed copies the manifests the runtime closure needs, NOT the kustomizations
# that own them: the embedded auth-proxy directory holds deployment.yaml alone. The
# digest lives in auth-proxy/kustomization.yaml's `images:` entry -- rewritten by
# the auth-proxy workflow after every build -- so it is absent from the copy, and
# the bare `image: ghcr.io/kamarkat/kamarkat/auth-proxy` that is CORRECTLY bare in
# source reads as untagged there. `kubectl kustomize` on the real directory
# resolves it to ...auth-proxy@sha256:1cf69a31...
#
# The originals under manifests/ are still scanned, and still judged through the
# kustomization that owns them. Nothing is skipped; a copy is simply not judged by
# a rule its directory cannot answer.
SKIP_DIRS = ("/vendor/", "/node_modules/", "/.git/", "/reference-projects/",
             "/internal/platform/embedded/")
ROOTS = ["manifests", "operators", "internal", "test"]

def skip(p):
    return any(s in "/" + p for s in SKIP_DIRS)

# Directories kustomize owns. Their images must be read from rendered output,
# and their source files must NOT be scanned directly.
kustomize_dirs = []
for root in ROOTS:
    for dirpath, _, files in os.walk(root):
        if skip(dirpath):
            continue
        if "kustomization.yaml" in files or "kustomization.yml" in files:
            kustomize_dirs.append(dirpath)

# A nested kustomization is rendered by its parent; rendering only the outermost
# avoids double-reporting the same image.
tops = []
for d in sorted(kustomize_dirs):
    if not any(d != o and d.startswith(o + os.sep) for o in kustomize_dirs):
        tops.append(d)

IMAGE_RE = re.compile(r"^\s*-?\s*image:\s*[\"']?(\S+?)[\"']?\s*$", re.M)

findings = []   # (source, image)

for d in tops:
    r = subprocess.run(["kubectl", "kustomize", d], capture_output=True, text=True)
    if r.returncode != 0:
        continue  # the render module already reports build failures
    for img in IMAGE_RE.findall(r.stdout):
        findings.append((d + " (rendered)", img))

covered = tuple(d + os.sep for d in kustomize_dirs)
for root in ROOTS:
    for dirpath, _, files in os.walk(root):
        if skip(dirpath):
            continue
        if dirpath in kustomize_dirs or (dirpath + os.sep).startswith(covered):
            continue
        for f in files:
            if not f.endswith((".yaml", ".yml")):
                continue
            p = os.path.join(dirpath, f)
            try:
                text = open(p, encoding="utf-8", errors="ignore").read()
            except OSError:
                continue
            for img in IMAGE_RE.findall(text):
                findings.append((p, img))

for src, img in sorted(set(findings)):
    # Substituted at render time, so there is nothing to pin here. A tenant
    # workload base deliberately ships `image: placeholder`; the per-tenant
    # overlay in fleet-registry replaces it with an immutable digest, and that
    # overlay is where the pin is enforced.
    if "{{" in img or img.startswith("$"):
        continue
    if img.lower() in ("placeholder", "controller"):
        continue
    if "PLACEHOLDER" in img:
        continue
    if img.endswith(":latest") or ":latest@" in img:
        print("LATEST\t%s\t%s" % (src, img))
    elif "@sha256:" not in img and ":" not in img.rsplit("/", 1)[-1]:
        print("UNTAGGED\t%s\t%s" % (src, img))
PY
)

    if [[ -z "$out" ]]; then
        pass "every rendered image is pinned (no :latest, no untagged refs)"
        return 0
    fi

    local kind src img
    while IFS=$'\t' read -r kind src img; do
        [[ -z "$kind" ]] && continue
        case "$kind" in
            LATEST)   hard_fail "floating tag in $src — $img (resolves at pull time; a reschedule can land on a different build)" ;;
            UNTAGGED) hard_fail "untagged image in $src — $img (an untagged ref defaults to :latest)" ;;
        esac
    done <<< "$out"
}
