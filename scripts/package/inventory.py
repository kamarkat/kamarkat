#!/usr/bin/env python3
"""The component inventory of one bundle version: what it installs, on which nodes, at what version.

    inventory.py <version> <published-chart-dir> <out.json> --expand <cache-dir>
        [--previous prev.json] [--previous-version V] [--helm helm]
        [--repo <checkout>] [--catalog <catalog.yaml>]

A version installs two kinds of cluster, and each has control plane nodes and
worker nodes:

    management   the cluster the control plane runs on
    managed      every cluster the management cluster provisions

plus the tools the soloz CLI runs during Day-0, which are installed on no cluster.
Every row says which cluster it belongs to and which node roles it runs on. The
applications a tenant deploys are not the platform's and are never listed.

What runs where is read, never assumed:

  - node packages from the active templates of each ClusterClass: the OS image,
    every versioned download and every apt package. A download with no version
    is listed as unpinned rather than omitted.
  - control-plane images as kubeadm lists them at the pinned Kubernetes
    version, minus kube-proxy where the ClusterClass deletes it.
  - container images from every pod the bundle renders: the platform's own
    objects, the upstream charts the cluster pulls at sync time, and the Cluster
    API provider manifests the operator fetches. A pod runs on control plane
    nodes when it selects them, on both roles when it tolerates their taint, and
    on worker nodes otherwise.

Only the hetzner provider is inventoried: it is supported in every environment.

Every artefact must match manifests/architecture/component-catalog.yaml, or the
release stops: a component is published only with a description, a licence and
a layer.
"""
from __future__ import annotations

import argparse
import glob
import hashlib
import json
import os
import re
import subprocess
import sys
import urllib.request
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]

GROUPS = [
    ("management", "Management cluster"),
    ("managed", "Managed clusters"),
    ("bootstrap", "Bootstrap tools"),
]
NODES = [("control-plane", "Control plane nodes"), ("worker", "Worker nodes")]
CP, WORKER = "control-plane", "worker"
CP_KEYS = {"node-role.kubernetes.io/control-plane", "node-role.kubernetes.io/master"}

# Charts delivered to managed clusters. Everything else in the platform chart
# runs on the management cluster.
#
# DESTINATION, not subject. platform-spoke-datasources is deliberately absent:
# it describes a spoke and is applied to the HUB, where Grafana runs, so listing
# it here would place it on the cluster it only names.
MANAGED_CHARTS = {
    "platform-spoke-catalog", "platform-spoke-bootstrap-templates", "platform-spoke-gateway",
    "universal-tenant", "platform-workload", "tenant-public-tls",
}

APPSETS = "manifests/argocd/environment-manager/templates/0*-*.yaml"
MANAGEMENT_CLASS = "internal/assets/manifests/classes/hetzner-mgmt-ubuntu-v1.yaml"
MANAGED_CLASS = "manifests/providers/hetzner/base/spokepool-clusterclass-v1.yaml"

# What the CLI applies to the hetzner management cluster itself, outside GitOps
# (internal/soloz-cli/bootstrap/driver_hetzner.go: CiliumAddonPath, the CCM
# addon, the CSI catalog entry, and the Cluster API operator).
MANAGEMENT_ADDONS = [
    "manifests/spoke/spoke-bootstrap/cilium-addon-template.yaml",
    "manifests/providers/hetzner/base/spoke-addons/ccm-addon-template.yaml",
    "internal/assets/catalog/cloud-providers/hetzner/csi/install.yaml",
    "internal/assets/manifests/core/capi-operator/install.yaml",
]

# The Cluster API providers the operator installs on the management cluster:
# (provider manifest, upstream repository, release asset). The CLI applies each
# manifest as written (internal/soloz-cli/capi/operator.go), so the version that
# runs is the manifest's spec.version -- not a Go constant, which has disagreed
# with it before (v0.1.15). The manifests set no fetchConfig, so the operator
# fetches each release asset from the upstream repository, as clusterctl's
# provider registry names it.
CAPI_PROVIDERS = [
    ("core-provider.yaml", "kubernetes-sigs/cluster-api", "core-components.yaml"),
    ("bootstrap-provider-kubeadm.yaml", "kubernetes-sigs/cluster-api", "bootstrap-components.yaml"),
    ("controlplane-provider-kubeadm.yaml", "kubernetes-sigs/cluster-api", "control-plane-components.yaml"),
    ("infrastructure-provider-hetzner.yaml", "syself/cluster-api-provider-hetzner", "infrastructure-components.yaml"),
]
CAPI_PROVIDER_DIR = "internal/assets/manifests/core/capi-operator/providers"

# Binaries the CLI downloads and runs during a hetzner Day-0, on the machine or CI
# that runs soloz; installed on no cluster. Each version is the constant the CLI's
# own download manager uses (internal/soloz-cli/binaries/<tool>.go). Packer is
# not listed: it builds only the hybrid provider's Flatcar image.
BOOTSTRAP_TOOLS = ["clusterctl"]

IMAGE_LINE = re.compile(r"""(?:image|imageName):\s*["']?[a-z0-9]""")
PACKAGE_RE = re.compile(r"""^xpkg\.[a-z0-9.-]+/""")
TEMPLATED = "TEMPLATED"
UNCATALOGUED = 3

# Content rendered only for the hybrid provider: a -hybrid subchart, a
# hybrid-templated file, or a hybrid directory.
HYBRID = re.compile(r"(^|[/_-])hybrid([/_.-]|$)")

# kubeadm's image list, read from its constants at the pinned Kubernetes tag:
# the same answer as `kubeadm config images list`, from a binary that exists
# only for Linux.
KUBEADM_CONSTANTS = "https://raw.githubusercontent.com/kubernetes/kubernetes/v{v}/cmd/kubeadm/app/constants/constants.go"


def split_ref(ref: str) -> tuple[str, str, str]:
    """registry/repo:tag@digest -> (repo, tag, digest). Docker Hub refs are normalised."""
    digest = ""
    if "@" in ref:
        ref, digest = ref.split("@", 1)
    tag = ""
    if ":" in ref.rsplit("/", 1)[-1]:
        ref, tag = ref.rsplit(":", 1)
    first = ref.split("/", 1)[0]
    if "/" not in ref:
        ref = f"docker.io/library/{ref}"
    elif "." not in first and ":" not in first and first != "localhost":
        ref = f"docker.io/{ref}"
    return ref, tag, digest


def atomic_write(path: Path, data: bytes) -> None:
    """A cache file appears whole or not at all: a release and a backfill can
    share the cache, and a half-written file would be read back as a result."""
    tmp = path.with_name(f".{path.name}.{os.getpid()}")
    tmp.write_bytes(data)
    os.replace(tmp, path)


def one(repo: Path, pattern: str) -> Path:
    """The single file a pattern names; the CLI package was renamed once, so
    patterns span both names. More than one match is ambiguous and refused."""
    found = sorted(repo.glob(pattern))
    if len(found) != 1:
        sys.exit(f"inventory: expected one {pattern} in {repo}, found {len(found)}")
    return found[0]


# ── Rows ────────────────────────────────────────────────────────────────────


class Inventory:
    def __init__(self, catalog: list[dict]):
        self.catalog = catalog
        self.rows: dict[tuple, dict] = {}
        self.unmatched: set[str] = set()

    def match(self, key: str) -> dict | None:
        best = None
        for entry in self.catalog:
            for p in entry["match"]:
                hit = key == p or (p.endswith("*") and key.startswith(p[:-1]))
                if hit and (best is None or len(p) > len(best[0])):
                    best = (p, entry)
        return best[1] if best else None

    def add(self, *, kind: str, key: str, name: str, version: str, digest: str = "",
            cluster: str, nodes: set[str], where: str, boundary: str = "", via: str = "") -> None:
        entry = self.match(key)
        if entry is None:
            self.unmatched.add(f"{kind} {key}")
            return
        row = self.rows.setdefault((cluster, kind, name), {
            "name": name, "component": entry["name"], "description": entry["description"],
            "license": entry.get("license") or "", "layer": entry["layer"], "type": kind,
            "cluster": cluster, "source": key, "nodes": set(), "versions": {},
            "where": set(), "boundaries": set(), "via": set(),
        })
        row["nodes"] |= nodes
        row["versions"].setdefault(version, set())
        if digest:
            row["versions"][version].add(digest)
        if where:
            row["where"].add(where)
        if boundary:
            row["boundaries"].add(boundary)
        if via:
            row["via"].add(via)

    def image(self, ref: str, *, cluster: str, nodes: set[str], where: str, boundary: str = "",
              via: str = "") -> None:
        # A wildcard is a pattern an admission policy matches images against;
        # it pins nothing and runs nowhere.
        if any(ch in ref for ch in "*?"):
            return
        repo, tag, digest = split_ref(ref)
        kind = "package" if PACKAGE_RE.match(repo) else "container"
        self.add(kind=kind, key=repo, name=repo.rsplit("/", 1)[-1], version=tag or "digest-pinned",
                 digest=digest, cluster=cluster, nodes=nodes, where=where, boundary=boundary, via=via)


# ── Placement ───────────────────────────────────────────────────────────────


def tolerates_cp(spec: dict) -> bool:
    for t in spec.get("tolerations") or []:
        if not isinstance(t, dict):
            continue
        if t.get("operator") == "Exists" and not t.get("key"):
            return True
        if t.get("key") in CP_KEYS:
            return True
    return False


def selects_cp(spec: dict) -> bool:
    if isinstance(spec.get("nodeSelector"), dict) and set(spec["nodeSelector"]) & CP_KEYS:
        return True
    affinity = spec.get("affinity") if isinstance(spec.get("affinity"), dict) else {}
    req = ((affinity.get("nodeAffinity") or {}).get("requiredDuringSchedulingIgnoredDuringExecution") or {})
    for term in req.get("nodeSelectorTerms") or []:
        for e in term.get("matchExpressions") or []:
            if e.get("key") in CP_KEYS and e.get("operator") in ("In", "Exists"):
                return True
    return False


def placement(spec: dict) -> set[str]:
    """A pod that selects control plane nodes runs only there; one that tolerates
    their taint can run on either role; any other pod cannot run on them."""
    if selects_cp(spec):
        return {CP}
    if tolerates_cp(spec):
        return {CP, WORKER}
    return {WORKER}


def walk_images(obj):
    """Yield (image, nodes) for every pod spec and every image a custom resource names.

    A pod spec's placement is its own. An image a custom resource names (a
    database image, a package) runs in pods its operator creates from that
    resource, so it takes the placement that resource's spec declares."""
    if isinstance(obj, dict):
        if obj.get("kind") == "CustomResourceDefinition":
            return
        if isinstance(obj.get("containers"), list):
            nodes = placement(obj)
            for key in ("initContainers", "containers", "ephemeralContainers"):
                for c in obj.get(key) or []:
                    if isinstance(c, dict) and isinstance(c.get("image"), str):
                        yield c["image"], nodes
            for k, v in obj.items():
                if k not in ("initContainers", "containers", "ephemeralContainers"):
                    yield from walk_images(v)
            return
        for key in ("imageName", "image", "package"):
            v = obj.get(key)
            if isinstance(v, str) and "/" in v and " " not in v:
                yield v, placement(obj)
            elif key == "image" and isinstance(v, dict) and isinstance(v.get("repository"), str) \
                    and "/" in v["repository"] and (v.get("tag") or v.get("digest")):
                yield v["repository"] + (f":{v['tag']}" if v.get("tag") else "") \
                    + (f"@{v['digest']}" if v.get("digest") else ""), placement(obj)
        for v in obj.values():
            yield from walk_images(v)
    elif isinstance(obj, list):
        for v in obj:
            yield from walk_images(v)
    elif isinstance(obj, str) and "apiVersion:" in obj and re.search(r"^\s*kind:", obj, re.M):
        # A manifest carried as a string -- a ClusterResourceSet Secret, a
        # Crossplane Object -- is applied as written, so its pods are listed too.
        for doc in load_docs(obj, "an embedded manifest"):
            yield from walk_images(doc)


def load_docs(text: str, origin: str) -> list:
    """Every YAML document in a file. A Helm template is parsed with each
    expression replaced by a marker and each directive line dropped; a file that
    still does not parse but names an image is refused, never skipped."""
    try:
        return [d for d in yaml.safe_load_all(text) if d is not None]
    except yaml.YAMLError:
        pass
    text = re.sub(r"\{\{-?\s*/\*.*?\*/\s*-?\}\}", "", text, flags=re.S)
    text = re.sub(r"\{\{.*?\}\}", TEMPLATED, text, flags=re.S)
    lines = [l for l in text.splitlines() if l.strip() not in (TEMPLATED, f"- {TEMPLATED}")]
    try:
        return [d for d in yaml.safe_load_all("\n".join(lines)) if d is not None]
    except yaml.YAMLError as e:
        if IMAGE_LINE.search(text):
            sys.exit(f"inventory: {origin} names an image but cannot be read: {e}")
        return []


# ── Sources ─────────────────────────────────────────────────────────────────


def boundaries_by_app(repo: Path) -> dict[str, str]:
    out: dict[str, str] = {}
    for f in glob.glob(str(repo / "manifests/argocd/components/*/*.yaml")):
        text = Path(f).read_text()
        m = re.search(r"^appName:\s*(\S+)", text, re.M)
        b = re.search(r"^boundary:\s*'?(\d+)", text, re.M)
        if m and b:
            out[m.group(1).strip("'\"")] = b.group(1)
    for f in glob.glob(str(repo / APPSETS)):
        for name in re.findall(r"appName:\s*'?([a-z0-9-]+)", Path(f).read_text()):
            out.setdefault(name, Path(f).name[:2])
    return out


def subchart_of(path: Path, charts: Path) -> str:
    parts = path.relative_to(charts).parts
    return parts[2] if len(parts) > 2 and parts[1] == "charts" else parts[0]


# Resources an operator turns into pods whose image the resource names only in
# part, or not at all. The operator fills the rest from defaults compiled into
# it, so each default below was read from the operator's source at the version
# named, and a bundle running any other version is refused until they are read
# again at that version.
#   grafana-operator v5.24.0  controllers/config/operator_constants.go
#   VictoriaMetrics operator v0.75.0  internal/config/config.go (images),
#     internal/controller/operator/factory/build/defaults.go (config-reloader)
OPERATED = {
    "Grafana": {"operator": "Grafana operator", "verified": "v5.24.0",
                "image": "docker.io/grafana/grafana", "tag": "13.0.1", "reloader": False},
    "VMSingle": {"operator": "VictoriaMetrics operator", "verified": "v0.75.0",
                 "image": "victoriametrics/victoria-metrics", "tag": "", "reloader": False},
    "VMAlert": {"operator": "VictoriaMetrics operator", "verified": "v0.75.0",
                "image": "victoriametrics/vmalert", "tag": "", "reloader": True},
    "VMAuth": {"operator": "VictoriaMetrics operator", "verified": "v0.75.0",
               "image": "victoriametrics/vmauth", "tag": "", "reloader": True},
    "VMAlertmanager": {"operator": "VictoriaMetrics operator", "verified": "v0.75.0",
                       "image": "prom/alertmanager", "tag": "v0.34.1", "reloader": True},
}
VM_CONFIG_RELOADER = "victoriametrics/operator:config-reloader-{v}"


def operated_images(doc: dict) -> list[tuple[str, str]]:
    """(image, operator) for each pod image the operator creates from this resource."""
    d = OPERATED[doc["kind"]]
    spec = doc.get("spec") or {}
    if doc["kind"] == "Grafana":
        # spec.version is a tag or a whole image reference; absent, the default.
        v = str(spec.get("version") or "")
        return [(v if "/" in v else f"{d['image']}:{v or d['tag']}", d["operator"])]
    img = spec.get("image") if isinstance(spec.get("image"), dict) else {}
    tag = str(img.get("tag") or d["tag"])
    if not tag:
        # Its default is the operator's own release version, which the operator's
        # deployment does not state; a resource that leaves it out cannot be listed.
        sys.exit(f"inventory: {doc['kind']} {doc.get('metadata', {}).get('name')} sets no image tag")
    out = [(f"{img.get('repository') or d['image']}:{tag}", d["operator"])]
    if d["reloader"]:
        out.append((spec.get("configReloaderImage") or VM_CONFIG_RELOADER, d["operator"]))
    return out


def scan_operated(inv: Inventory, found: list[tuple[dict, str, str, str]]) -> None:
    """Pods operators create from the bundle's resources, at the operator version
    the bundle deploys."""
    deployed = {}
    for row in inv.rows.values():
        if row["component"] in {d["operator"] for d in OPERATED.values()}:
            deployed.setdefault(row["component"], set()).update(row["versions"])
    for doc, cluster, sub, boundary in found:
        d = OPERATED[doc["kind"]]
        versions = deployed.get(d["operator"], set())
        if versions != {d["verified"]}:
            sys.exit(f"inventory: {doc['kind']} is reconciled by {d['operator']} "
                     f"{', '.join(sorted(versions)) or '(not deployed)'}, but its defaults were read at "
                     f"{d['verified']}; read them at the deployed version and update OPERATED")
        nodes = placement(doc.get("spec") or {})
        for ref, operator in operated_images(doc):
            inv.image(ref.replace("{v}", d["verified"]), cluster=cluster, nodes=nodes, where=sub,
                      boundary=boundary, via=f"{operator} {d['verified']}")


def scan_bundle(inv: Inventory, charts: Path, bmap: dict[str, str]) -> list[tuple[dict, str, str, str]]:
    """Every pod the platform's own charts render, and images assembled from values.
    Returns the resources an operator turns into pods, for scan_operated."""
    templated_nodes: dict[str, set[str]] = {}
    operated: list[tuple[dict, str, str, str]] = []
    for f in sorted(charts.rglob("*.yaml")):
        s = str(f)
        if ("/templates/" not in s and "/files/" not in s) or f.parent.name == "descriptors" \
                or re.search(r"(platform-bundle|environment-manager)/templates", s):
            continue
        if HYBRID.search(str(f.relative_to(charts))):
            continue
        sub = subchart_of(f, charts)
        cluster = "managed" if sub in MANAGED_CHARTS else "management"
        for doc in load_docs(f.read_text(errors="replace"), s):
            if isinstance(doc, dict) and doc.get("kind") in OPERATED:
                operated.append((doc, cluster, sub, bmap.get(sub, "")))
            for ref, nodes in walk_images(doc):
                if TEMPLATED in ref:
                    templated_nodes.setdefault(sub, set()).update(nodes)
                    continue
                inv.image(ref, cluster=cluster, nodes=nodes, where=sub, boundary=bmap.get(sub, ""))
    for f in sorted(charts.rglob("values.yaml")):
        if HYBRID.search(str(f.relative_to(charts))):
            continue
        sub = subchart_of(f, charts)
        try:
            data = yaml.safe_load(f.read_text())
        except yaml.YAMLError:
            continue
        refs: list[str] = []

        def walk(o):
            if isinstance(o, dict):
                r = o.get("repository")
                if isinstance(r, str) and "/" in r and (o.get("digest") or o.get("tag")):
                    refs.append(r + (f":{o['tag']}" if o.get("tag") else "")
                                + (f"@{o['digest']}" if o.get("digest") else ""))
                for v in o.values():
                    walk(v)
            elif isinstance(o, list):
                for v in o:
                    walk(v)

        walk(data)
        if refs and sub not in templated_nodes:
            sys.exit(f"inventory: {f} sets images, but no pod in {sub} takes its image from values")
        for ref in refs:
            inv.image(ref, cluster="managed" if sub in MANAGED_CHARTS else "management",
                      nodes=templated_nodes[sub], where=sub, boundary=bmap.get(sub, ""))
    return operated


def upstream_charts(repo: Path) -> list[dict]:
    """Upstream charts the management cluster pulls at sync time, with their values."""
    found = []
    for f in glob.glob(str(repo / "manifests/argocd/components/*/*.yaml")):
        d = yaml.safe_load(Path(f).read_text()) or {}
        if d.get("chart") and str(d.get("repoURL", "")).startswith("http"):
            found.append({"app": d["appName"], "repo": d["repoURL"], "chart": d["chart"],
                          "version": str(d["targetRevision"]), "boundary": str(d["boundary"]),
                          "values": d.get("helmValues") or ""})
    for f in glob.glob(str(repo / APPSETS)):
        for chunk in re.split(r"^\s+- appName:", Path(f).read_text(), flags=re.M)[1:]:
            app = re.match(r"\s*'?([a-z0-9-]+)", chunk)
            url = re.search(r"^\s+repoURL:\s*'(https?://[^']+)'", chunk, re.M)
            rev = re.search(r"^\s+targetRevision:\s*'([^'{]+)'", chunk, re.M)
            chart = re.search(r"^\s+chart:\s*'([^'{]+)'", chunk, re.M)
            if not (app and url and rev and chart):
                continue
            block = re.search(r"^(\s+)helmValues:\s*\|\n((?:\1\s+.*\n|\s*\n)*)", chunk, re.M)
            values = "\n".join(l for l in block.group(2).splitlines() if "{{" not in l) if block else ""
            # An element can name a values file in this repository instead; a
            # released bundle carries the same file, so it is what the cluster renders.
            vfile = re.search(r"^\s+valueFilesPath:\s*'([^'{]+)'", chunk, re.M)
            if vfile:
                path = repo / vfile.group(1)
                if not path.exists():
                    sys.exit(f"inventory: {app.group(1)} names values file {vfile.group(1)}, missing from {repo}")
                values = path.read_text() + ("\n" + values if values else "")
            found.append({"app": app.group(1), "repo": url.group(1), "chart": chart.group(1),
                          "version": rev.group(1), "boundary": Path(f).name[:2], "values": values})
    seen, unique = set(), []
    for c in found:
        k = (c["repo"], c["chart"], c["version"])
        if k not in seen:
            seen.add(k)
            unique.append(c)
    return unique


def scan_upstream_charts(inv: Inventory, repo: Path, cache: Path, helm: str) -> dict[str, str]:
    """Render each upstream chart as the cluster does and list the pods it runs.

    Rendered against the management cluster's Kubernetes version: helm otherwise
    checks a chart's kubeVersion against its own built-in default, which is older
    than any cluster the platform creates."""
    kube = kubernetes_version(repo / MANAGEMENT_CLASS)
    versions = {}
    for c in upstream_charts(repo):
        versions[c["chart"]] = c["version"]
        values = cache / f"{c['app']}-values.yaml"
        # Lines carrying a bundle template expression are per-cluster values
        # (a hostname) or hybrid-only placement, which renders nothing on hetzner.
        text = "\n".join(l for l in c["values"].splitlines() if "{{" not in l)
        try:
            yaml.safe_load(text)
        except yaml.YAMLError as e:
            sys.exit(f"inventory: the values {c['app']} renders {c['chart']} with do not parse "
                     f"once template lines are removed: {e}")
        values.write_text(text)
        # A render is a function of the chart version, its values and the
        # Kubernetes version, so an unchanged one is read back rather than
        # downloaded and rendered again. Only a successful render is kept.
        key = hashlib.sha256("\0".join(
            [c["repo"], c["chart"], c["version"], text, kube]).encode()).hexdigest()[:16]
        rendered = cache / f"render-{c['chart']}-{c['version']}-{key}.yaml"
        if not rendered.exists():
            out = subprocess.run(
                [helm, "template", c["app"], c["chart"], "--repo", c["repo"], "--version", c["version"],
                 "--values", str(values), "--namespace", "platform-ops", "--kube-version", f"v{kube}"],
                capture_output=True, text=True,
            )
            if out.returncode != 0:
                sys.exit(f"inventory: cannot render {c['chart']} {c['version']}: {out.stderr.strip()}")
            atomic_write(rendered, out.stdout.encode())
        found = 0
        for doc in load_docs(rendered.read_text(), f"{c['chart']} {c['version']}"):
            for ref, nodes in walk_images(doc):
                found += 1
                inv.image(ref, cluster="management", nodes=nodes, where=c["app"],
                          boundary=c["boundary"], via=f"chart {c['chart']} {c['version']}")
        if not found:
            sys.exit(f"inventory: {c['chart']} {c['version']} rendered no pods")
    return versions


def scan_management_addons(inv: Inventory, repo: Path) -> None:
    for rel in MANAGEMENT_ADDONS:
        path = repo / rel
        if not path.exists():
            sys.exit(f"inventory: {rel} is applied by the CLI but missing from {repo}")
        for doc in load_docs(path.read_text(), rel):
            for ref, nodes in walk_images(doc):
                if TEMPLATED not in ref:
                    inv.image(ref, cluster="management", nodes=nodes, where="installed by the soloz CLI")


def scan_capi_providers(inv: Inventory, repo: Path, cache: Path) -> None:
    for manifest, project, asset in CAPI_PROVIDERS:
        path = repo / CAPI_PROVIDER_DIR / manifest
        if not path.exists():
            sys.exit(f"inventory: {CAPI_PROVIDER_DIR}/{manifest} is applied by the CLI but missing from {repo}")
        version = ((yaml.safe_load(path.read_text()) or {}).get("spec") or {}).get("version")
        if not version:
            sys.exit(f"inventory: {CAPI_PROVIDER_DIR}/{manifest} declares no spec.version")
        target = cache / f"{project.replace('/', '_')}-{version}-{asset}"
        if not target.exists():
            url = f"https://github.com/{project}/releases/download/{version}/{asset}"
            try:
                with urllib.request.urlopen(url, timeout=60) as r:
                    atomic_write(target, r.read())
            except OSError as e:
                sys.exit(f"inventory: cannot fetch {url}: {e}")
        # Release manifests carry clusterctl variables; they are values, not structure.
        text = re.sub(r"\$\{[^}]+\}", TEMPLATED, target.read_text())
        found = 0
        for doc in load_docs(text, str(target)):
            for ref, nodes in walk_images(doc):
                found += 1
                inv.image(ref, cluster="management", nodes=nodes, where="Cluster API",
                          via=f"{asset.replace('-components.yaml', '')} provider {version}")
        if not found:
            sys.exit(f"inventory: {project} {version} {asset} names no image")


def class_templates(path: Path):
    """The control plane template and each worker class's bootstrap template that
    a ClusterClass actually references, with their machine templates."""
    docs = [d for d in yaml.safe_load_all(path.read_text()) if d]
    by = {(d["kind"], d["metadata"]["name"]): d for d in docs}
    cc = [d for d in docs if d["kind"] == "ClusterClass"]
    if len(cc) != 1:
        sys.exit(f"inventory: {path} must hold exactly one ClusterClass")
    spec = cc[0]["spec"]
    cp = by[("KubeadmControlPlaneTemplate", spec["controlPlane"]["ref"]["name"])]
    cp_machine = by[("HCloudMachineTemplate", spec["controlPlane"]["machineInfrastructure"]["ref"]["name"])]
    workers = []
    for md in spec["workers"]["machineDeployments"]:
        workers.append((md["class"],
                        by[("KubeadmConfigTemplate", md["template"]["bootstrap"]["ref"]["name"])],
                        by[("HCloudMachineTemplate", md["template"]["infrastructure"]["ref"]["name"])]))
    return cp, cp_machine, workers


def node_commands(cfg: dict) -> list[str]:
    return [c for key in ("preKubeadmCommands", "postKubeadmCommands") for c in (cfg.get(key) or [])
            if isinstance(c, str)]


def scan_node_packages(inv: Inventory, cluster: str, commands: list[str], nodes: set[str], where: str) -> None:
    """Every package a node installs: versioned downloads, apt packages, and
    downloads with no version, which are listed as unpinned."""
    env = dict(re.findall(r"export ([A-Z_]+)=(\S+)", "\n".join(commands)))
    for cmd in commands:
        for url in re.findall(r"https?://[^\s'\"|>]+", cmd):
            if "169.254.169.254" in url or url.endswith((".key", "sha256sum")) or ".sha256" in url:
                continue
            used = [v for v in re.findall(r"\$\{?([A-Z_]+)", url) if v in env and not v.startswith("TRIMMED")]
            if used:
                name = used[0].lower()
                inv.add(kind="package", key=f"pkg:{name}", name=name, version=env[used[0]],
                        cluster=cluster, nodes=nodes, where=where)
            elif "$" not in url:
                host = re.sub(r"^https?://", "", url).split("/", 1)[0]
                base = url.rstrip("/").rsplit("/", 1)[-1]
                # An install script is named after what it installs, which is its host.
                name = host.split(".")[-2] if base in ("install.sh", "install") else base
                inv.add(kind="package", key="url:" + re.sub(r"^https?://", "", url),
                        name=name, version="unpinned", cluster=cluster, nodes=nodes, where=where)
        m = re.search(r"apt-get (?:-y )?install (?:-y )?(.+?)(?:&&|;|$)", cmd)
        if m:
            for token in m.group(1).split():
                if token.startswith("-"):
                    continue
                pkg, _, ver = token.partition("=")
                ver = ver.strip("\"'")
                for var in re.findall(r"\$\{?([A-Z_]+)\}?", ver):
                    ver = re.sub(r"\$\{?" + var + r"\}?", env.get(var, f"${var}"), ver)
                ver = ver.replace("-*", "") if ver else "unpinned"
                inv.add(kind="package", key=f"pkg:{pkg}", name=pkg, version=ver,
                        cluster=cluster, nodes=nodes, where=where)


def kubernetes_version(class_path: Path) -> str:
    """The Kubernetes version a ClusterClass's control plane nodes install."""
    cp, _, _ = class_templates(class_path)
    env = dict(re.findall(r"export ([A-Z_]+)=(\S+)",
                          "\n".join(node_commands(cp["spec"]["template"]["spec"]["kubeadmConfigSpec"]))))
    if "KUBERNETES_VERSION" not in env:
        sys.exit(f"inventory: {class_path} sets no KUBERNETES_VERSION for its control plane")
    return env["KUBERNETES_VERSION"]


def kubeadm_images(version: str, cache: Path) -> list[str]:
    target = cache / f"kubeadm-constants-{version}.go"
    if not target.exists():
        url = KUBEADM_CONSTANTS.format(v=version)
        try:
            with urllib.request.urlopen(url, timeout=60) as r:
                target.write_bytes(r.read())
        except OSError as e:
            sys.exit(f"inventory: cannot fetch {url}: {e}")
    text = target.read_text()
    coredns = re.search(r'CoreDNSVersion\s*=\s*"([^"]+)"', text)
    pause = re.search(r'PauseVersion\s*=\s*"([^"]+)"', text)
    # DefaultEtcdVersion, not the SupportedEtcdVersion map.
    #
    # Through 1.34 that map was keyed by literal minor (`31: "3.5.15-0"`), so the
    # entry for the version being scanned could be read straight out of it. From
    # 1.35 the keys are computed -- `uint8(getSkewedKubernetesVersion(0).Minor())`
    # -- so nothing literal matches and the lookup silently found nothing.
    #
    # DefaultEtcdVersion is the version for the release the file belongs to, which
    # is exactly the one wanted here: the constants are fetched at the tag the
    # ClusterClass installs. The map's own comment states the coupling -- "
    # DefaultEtcdVersion: with etcd version used for the current k8s release (0)".
    etcd = re.search(r'DefaultEtcdVersion\s*=\s*"([^"]+)"', text)
    if not (coredns and pause and etcd):
        sys.exit(f"inventory: kubeadm {version} constants name no CoreDNS, pause or "
                 f"etcd version; its constants.go may have been restructured")
    images = [f"registry.k8s.io/{c}:v{version}" for c in
              ("kube-apiserver", "kube-controller-manager", "kube-scheduler", "kube-proxy")]
    return images + [f"registry.k8s.io/coredns/coredns:{coredns.group(1)}",
                     f"registry.k8s.io/pause:{pause.group(1)}",
                     f"registry.k8s.io/etcd:{etcd.group(1)}"]


def scan_nodes(inv: Inventory, repo: Path, cache: Path) -> None:
    for cluster, rel in (("management", MANAGEMENT_CLASS), ("managed", MANAGED_CLASS)):
        cp, cp_machine, workers = class_templates(repo / rel)
        cp_cmds = node_commands(cp["spec"]["template"]["spec"]["kubeadmConfigSpec"])
        scan_node_packages(inv, cluster, cp_cmds, {CP}, "control plane")
        machines = [(cp_machine, {CP}, "control plane")]
        for cls, tpl, machine in workers:
            scan_node_packages(inv, cluster, node_commands(tpl["spec"]["template"]["spec"]), {WORKER}, cls)
            machines.append((machine, {WORKER}, cls))
        for machine, nodes, where in machines:
            image = machine["spec"]["template"]["spec"]["imageName"]
            os_name, _, os_version = image.partition("-")
            inv.add(kind="os", key=f"os:{os_name}", name=os_name, version=os_version or image,
                    cluster=cluster, nodes=nodes, where=where)

        version = kubernetes_version(repo / rel)
        no_proxy = any("delete daemonset -n kube-system kube-proxy" in c for c in cp_cmds)
        for ref in kubeadm_images(version, cache):
            name = split_ref(ref)[0].rsplit("/", 1)[-1]
            if name == "kube-proxy" and no_proxy:
                continue
            # Static pods run on control plane nodes. kube-proxy and the pause
            # sandbox image are on every node, and kubeadm deploys CoreDNS
            # tolerating the control plane taint.
            nodes = {CP} if name in ("kube-apiserver", "kube-controller-manager", "kube-scheduler", "etcd") \
                else {CP, WORKER}
            inv.image(ref, cluster=cluster, nodes=nodes, where="kubeadm", via=f"kubeadm v{version}")


def read_consts(repo: Path) -> dict[str, str]:
    text = one(repo, "internal/*/versions/versions.go").read_text()
    return dict(re.findall(r"^\s*([A-Z][A-Za-z]+Version)\s*=\s*\"([^\"]+)\"", text, re.M))


def check_seed_parity(inv: Inventory, consts: dict[str, str], chart_versions: dict[str, str]) -> None:
    """Day-0 seeds cert-manager and the GitOps engine, which GitOps then adopts.
    The seed is not listed on its own because it must be the version GitOps runs."""
    if chart_versions.get("argo-cd") != consts["ArgoCDChartVersion"]:
        sys.exit(f"inventory: Day-0 seeds argo-cd {consts['ArgoCDChartVersion']} but the bundle "
                 f"reconciles {chart_versions.get('argo-cd')}")
    cm = {v for (c, _, n), r in inv.rows.items() if c == "management" and n == "cert-manager-controller"
          for v in r["versions"]}
    if consts["CertManagerVersion"] not in cm:
        sys.exit(f"inventory: Day-0 seeds cert-manager {consts['CertManagerVersion']} but the management "
                 f"cluster runs {sorted(cm)}")


def scan_bootstrap(inv: Inventory, repo: Path) -> None:
    for name in BOOTSTRAP_TOOLS:
        text = one(repo, f"internal/*/binaries/{name}.go").read_text()
        m = re.search(rf'{name}Version\s*=\s*"([^"]+)"', text)
        if not m:
            sys.exit(f"inventory: the CLI's {name} download names no {name}Version")
        inv.add(kind="binary", key=f"tool:{name}", name=name, version=m.group(1),
                cluster="bootstrap", nodes=set(), where="soloz CLI")


# ── Output ──────────────────────────────────────────────────────────────────


def finish(inv: Inventory) -> list[dict]:
    rows = []
    for row in inv.rows.values():
        versions = sorted(row.pop("versions").items())
        row["version"] = ", ".join(v for v, _ in versions)
        row["digests"] = sorted({d for _, ds in versions for d in ds})
        for k in ("nodes", "where", "boundaries", "via"):
            row[k] = sorted(row[k])
        rows.append(row)
    order = {k: i for i, (k, _) in enumerate(GROUPS)}
    rows.sort(key=lambda r: (order[r["cluster"]], r["component"].lower(), r["name"]))
    return rows


def compare(rows: list[dict], previous: dict) -> list[dict]:
    """Mark each row against the predecessor's inventory; a removed row is kept."""
    def key(r):
        return (r["cluster"], r["type"], r["name"])

    def identity(r):
        return (r["version"], tuple(r["digests"]))

    def short(r):
        return r["digests"][0][7:19] if r["digests"] else r["version"]

    before = {key(r): r for r in previous.get("components", [])}
    changes = []
    for r in rows:
        p = before.pop(key(r), None)
        base = {"name": r["name"], "cluster": r["cluster"], "type": r["type"], "nodes": r["nodes"]}
        if p is None:
            changes.append(base | {"kind": "added", "from": "", "to": r["version"]})
        elif identity(p) != identity(r):
            same = p["version"] == r["version"]
            changes.append(base | {"kind": "changed", "from": short(p) if same else p["version"],
                                   "to": short(r) if same else r["version"]})
    for p in before.values():
        changes.append({"name": p["name"], "cluster": p["cluster"], "type": p["type"],
                        "nodes": p["nodes"], "kind": "removed", "from": p["version"], "to": ""})
    return changes


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("version")
    ap.add_argument("charts")
    ap.add_argument("out")
    ap.add_argument("--expand", required=True, help="cache for rendered charts and fetched manifests")
    ap.add_argument("--previous")
    ap.add_argument("--previous-version", default="")
    ap.add_argument("--helm", default="helm")
    ap.add_argument("--repo", default=str(ROOT))
    ap.add_argument("--catalog", default="")
    a = ap.parse_args()

    repo, charts, cache = Path(a.repo), Path(a.charts), Path(a.expand)
    cache.mkdir(parents=True, exist_ok=True)
    catalog_file = Path(a.catalog) if a.catalog else repo / "manifests/architecture/component-catalog.yaml"
    inv = Inventory(yaml.safe_load(catalog_file.read_text())["components"])
    consts = read_consts(repo)

    scan_bootstrap(inv, repo)
    scan_nodes(inv, repo, cache)
    scan_management_addons(inv, repo)
    scan_capi_providers(inv, repo, cache)
    operated = scan_bundle(inv, charts, boundaries_by_app(repo))
    chart_versions = scan_upstream_charts(inv, repo, cache, a.helm)
    scan_operated(inv, operated)

    if inv.unmatched:
        print("inventory: these artefacts ship in the bundle but have no catalog entry:", file=sys.stderr)
        for u in sorted(inv.unmatched):
            print(f"  {u}", file=sys.stderr)
        print("Add each to manifests/architecture/component-catalog.yaml.", file=sys.stderr)
        # A distinct status, so a backfill can collect every version's gaps in one pass.
        sys.exit(UNCATALOGUED)
    check_seed_parity(inv, consts, chart_versions)

    rows = finish(inv)
    doc = {
        "version": a.version,
        "groups": [{"id": k, "title": t} for k, t in GROUPS],
        "nodes": [{"id": k, "title": t} for k, t in NODES],
        "provider": "hetzner",
        "components": rows,
    }
    if a.previous_version:
        doc["previous"] = a.previous_version
    if a.previous:
        prev = json.loads(Path(a.previous).read_text())
        doc["previous"] = prev["version"]
        doc["changes"] = compare(rows, prev)
    Path(a.out).write_text(json.dumps(doc, indent=2) + "\n")
    print(f"inventory: {len(rows)} components in {a.version}"
          + (f", {len(doc['changes'])} changed since {doc['previous']}" if "changes" in doc else ""))


if __name__ == "__main__":
    main()
