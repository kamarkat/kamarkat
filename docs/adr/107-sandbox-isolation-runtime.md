# ADR-107: Untrusted workloads run under a sandboxed runtime, offered node by node

**Date:** 2026-10-20
**Status:** Accepted. Implemented and awaiting deploy; the rollout is per node
(see Decision §4).
**Relates to:** ADR-052 (sandbox compute), ADR-106 (sandbox containment baseline),
ADR-108 (untrusted preview origin)

## Context

A sandbox can run an untrusted browser, or a renderer loading a session's app.
Network policy (ADR-106) bounds where that code can connect. It does nothing about
the kernel: a container shares the node's kernel, so a kernel exploit from inside
it reaches the node and every other tenant's pods on it.

Verified on nutgraf-01 on 2026-10-20:

- no RuntimeClass exists;
- sandboxes land only on the two hybrid Flatcar workers (containerd 2.2.5, kernel
  6.12). The third node is the tainted control plane;
- those workers are Hyper-V guests.

## Decision

### 1. One capability, named for what it gives

- **The RuntimeClass is `sandboxed`**, with handler `runsc`: gVisor on its systrap
  platform. Systrap needs no hardware virtualisation. Kata was ruled out because
  it does need it, and on Hyper-V guests that means nested virtualisation.
- **Jobs ask by capability:** `EphemeralJob.spec.isolation: Sandboxed`. A session
  peer (ADR-106 §4) always runs `sandboxed`, whatever it asks for.
- **No fallback.** A pod that asked for isolation runs with it or doesn't run.
  The RuntimeClass carries a node selector, so with no offering node the pod stays
  Pending and the scheduler says why.

### 2. Offered per node, by one label

`sandbox.kamarkat.io/runtime=sandboxed` on a node is the single statement that the
node offers the runtime. It drives three things:

- **The installer** (`spoke-catalog/infra/sandbox-runtime/`, a DaemonSet).
  It runs on labelled nodes and:
  - installs a pinned, checksummed gVisor release (20261012) as released, a whole
    tree in `/opt/gvisor/<release>-<arch>`, because runsc loads helper binaries
    (`gvisor-bin/`) from beside itself. The architecture is the node kernel's
    (x86_64 or aarch64, each checksum-pinned); gVisor doesn't run under binary
    translation, so a mismatched release is never installed;
  - registers the `runsc` handler in the containerd config the running daemon
    actually loads, read from its command line;
  - restarts containerd once;
  - stays Ready only while the binaries are present.
- **The Cilium bypass** (a CiliumNodeConfig). See §3.
- **Scheduling** through the RuntimeClass node selector.

### 3. Cilium's socket load balancer is bypassed on offering nodes

Under kube-proxy replacement, Cilium resolves Service addresses in the `connect()`
hook. A gVisor pod's network stack runs inside the sandbox, so its `connect()`
never reaches the host. Without the bypass its Service traffic, DNS included,
leaves unresolved. Cilium's remedy is `bpf-lb-sock-hostns-only: true`, which moves
the lookup for pods to the pod's interface (v1.17.18,
`Documentation/network/kubernetes/kubeproxy-free.rst:793-799`).

The flag applies to every pod on the node. This repo records a policy that broke on
the hub when Service translation moved (`hub-core-services/database/network-policy.yaml`).
That risk is why the flag is per node and why §4 rolls out one node at a time.

### 4. Rollout

1. Label one worker, then delete its cilium pod: a CiliumNodeConfig applies only
   when the agent restarts.
2. Run `scripts/validate/cluster/107-sandbox-runtime.sh` (R1–R3) and the tenant
   smoke checks for workloads on that node.
3. Only then label the next node.

To revert a node, remove the label and restart its cilium pod. The installed
binaries are inert without the label.

### 5. Burst nodes

Burst workers (Hetzner cloud, ClusterClass `spokepool-v1`) are not in dev. When
they are, they need the same three things. Because ClusterClass templates are
immutable, that means new template names (`spokepool-burst-bootstrap-v7`), not an
in-place edit.

### 6. Local Kind is a supported target

The same component runs on Kind (`scripts/local/kind/runtime`), installed with
`scripts/local/kind-profile.sh`. The script:

1. applies the runtime;
2. labels every Kind node;
3. waits for the installer;
4. runs one pod under `sandboxed` and requires gVisor's banner in its `dmesg`;
5. fails, naming the step, if any of that doesn't hold.

There's no Cilium bypass on Kind: kube-proxy resolves Services per packet there.

The installer asks a node for only `curl`, `systemctl` and `crictl`. Checksum
verification and extraction run in its own pinned container, because node images
differ: the Kind node image has no `bzip2`.

The operator for Kind is `scripts/local/kind/operator`: `config/default` with
`--network-policy=none`. The flag is required on every install and never detected
(ADR-106), and Kind enforces no network policy.

## Acceptance criteria

1. R1–R3 pass on every labelled node.
2. A `Sandboxed` pod on a labelled node reports `runsc` (`dmesg` inside it shows
   the gVisor banner). It resolves and reaches a Service and an external name its
   policy allows.
3. Headless Chromium starts and renders a page inside a `Sandboxed` pod, with the
   workspace-sync sidecar syncing.
4. With no labelled node, a `Sandboxed` pod is Pending with a scheduling message
   naming the RuntimeClass's node selector.
5. Tenant workloads on a labelled node pass their smoke checks after the Cilium
   restart.

## Consequences

- Each sandboxed pod runs a gVisor sentry, which costs memory and syscall-heavy
  CPU. The ADR-106 limits apply to the workload, not to that overhead. If the
  overhead shows up in measurements, it is added as RuntimeClass `overhead`.
- An offering node's Service handling changes for all of its pods. That is the
  price of the bypass, and it is paid one node at a time.

## References

- `manifests/spoke/spoke-catalog/infra/sandbox-runtime/ (+ sandbox-runtime-cilium.yaml)`
- `operators/ephemeral-job-operator/api/v1alpha1/ephemeraljob_types.go`
  (`Isolation`, `Peer`), `internal/controller/session_peer.go` (`runtimeClassFor`)
- `scripts/validate/cluster/107-sandbox-runtime.sh`
