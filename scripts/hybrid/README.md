# Hybrid Home-Lab Operations & Runbook (ADR-046 §13)

## The Single Command

Home workers are **Hyper-V Gen2 VMs running immutable Flatcar Container Linux**,
provisioned 100% remotely from macOS via SSH. Provisioning, recovery, and
verification are handled by **one script**:

```bash
# Provision / (re)provision a node — prepares Windows (power/Hyper-V),
# builds the Flatcar base VHDX + Ignition ISO, creates the Gen2 VM, and
# joins the spoke via kubeadm.
./scripts/hybrid/provision-flatcar-worker.sh --cluster hub 
./scripts/hybrid/provision-flatcar-worker.sh --cluster spoke 

./scripts/hybrid/provision-flatcar-worker.sh --node 1

# Provision all registered nodes (home-lab.env)
./scripts/hybrid/provision-flatcar-worker.sh

# Verify Ready status of all registered nodes without changing anything
./scripts/hybrid/provision-flatcar-worker.sh --verify

# Autonomous Tailscale re-auth. Omit --ts-authkey and the key is read from
# k8-secrets/tailscale/authkey; pass it only to use a different one.
./scripts/hybrid/provision-flatcar-worker.sh --node 1 --ts-authkey tskey-...

# Re-running is RECOVERY-FIRST, not rebuild-first: an existing VM is started and
# given --recover-wait seconds (default 300) to rejoin before it is replaced.
./scripts/hybrid/provision-flatcar-worker.sh --node 1 --recover-wait 600

# Skip that gate and rebuild from the base image. This DESTROYS the VM's disk.
./scripts/hybrid/provision-flatcar-worker.sh --node 1 --force-rebuild
```

`--help` lists the rest: `--spoke`, `--env`, `--vswitch`, `--upgrade-flatcar`,
`--host-cpu-max`, and per-run capacity overrides (`--memory-gb`, `--min-memory-gb`,
`--max-memory-gb`, `--cpus`, `--disk-gb`) that win over the registry for that run.

> **The run is only a success when the node reports `Ready`** in the spoke
> cluster. The provisioner exits non-zero if any targeted node fails to
> become Ready.

### Phases run per node

| Phase | What it does |
|-------|--------------|
| [0/6] | Verify offline worker binary cache on Mac (kubelet, kubeadm, tailscale), then the **recovery gate**: an existing VM is started and given time to rejoin before it is replaced |
| [1/6] | SSH reachability gate to the Windows host |
| [2/6] | Windows host & Hyper-V preparation (power/lid, Hyper-V feature, directories) |
| [3/6] | Flatcar base VHDX & Ignition ISO bundle preparation |
| [4/6] | Provision Generation 2 Hyper-V VM with attached Ignition DVD ISO |
| [5/6] | Wait for Flatcar first-boot, Tailscale mesh, and kubeadm join |
| [6/6] | Verify node Ready — the success gate |

### How it works

1. **Ignition config** (`config.ign`, spec 3.4.0) declares hostname, Tailscale
   auth, kubelet, and a oneshot `kubeadm join` unit — no post-boot
   configuration needed. Join credentials (`<spoke>-home-worker-join` Secret,
   minted by hub-operator) are baked into the ISO.
2. **Native Tailscale daemon** joins the VM to the tailnet
   (`tailscale up --hostname=$VM_NAME`, which is the node's hostname from the
   registry); the node is then reachable on its tailnet address, which is what it
   advertises as its `InternalIP`.
3. **Native Linux cgroup v2 + eBPF** — standard Cilium CNI deployment, no
   cgroup mount hacks.

### Node registry

Nodes are declared in `scripts/hybrid/home-lab.env` (gitignored; see
`home-lab.env.example`). Each line is

```
<hostname>|<ssh-target>|<os-info>|<tailnet-host>|<box-tag>|<target-cluster>|<startupGB>|<minGB>|<maxGB>|<cpus>|<diskGB>
```

`<diskGB>` is optional and is the eleventh field; omitted, the per-cluster default
applies (100 GB hub, 60 GB spoke). `<os-info>` is a legacy column, informational
only — the shipped examples set it to `unused`.

**The hostname column is authoritative**, not informational: the provisioner
uses it directly (`VM_NAME="${_HOST}"`) and only falls back to the conventional
`flatcar-<cluster>-node-<idx>` when it is empty. `--node N` indexes this list in
order, counting every entry — including ones a `--cluster` filter skips — so
renumbering the file changes what `--node N` means.

Capacity fields are the per-node Hyper-V sizing, and `home-lab.env.example`
carries the full rationale for each. Two things are worth knowing before editing
them:

- **`cpus` overrides the provisioner's automatic division.** Left empty, the
  provisioner splits the host's threads across the VMs that will share it (floor
  of 2). Set, it wins — so a value copied between boxes can oversubscribe a host
  that cannot take it. Sum the column per `box-tag` and keep it at or near that
  host's logical CPU count: a 15 W part driven at 2:1 reaches the ACPI critical
  thermal trip and powers the box off mid-provision (ADR-046 §24.5). The
  provisioner warns when the total oversubscribes, and `--host-cpu-max` (default
  70) caps `PROCTHROTTLEMAX` on the host.
- **`minGB == startupGB == maxGB` means fixed memory.** The guest holds it for its
  whole life and returns none of it, so the host keeps only the remainder. A 16 GB
  box running a 13 GB hub node leaves about 3 GB for Windows and Hyper-V.

The same node list lives in the SpokePool claim annotation
(`home-workers`), which drives hub-operator's join-payload Secret via
`manifests/providers/hybrid/k8s/spokepool-hybrid-composition.yaml`.

---

## Support Scripts

| Script | Purpose |
|--------|---------|
| `provision-flatcar-worker.sh` | **THE entry point** — Hyper-V + Flatcar provisioning |
| `convert-to-external-switch.sh` | Convert a host from Internal+NetNat to an External switch (see below) |
| `render-home-workers.sh` | Render the SpokePool `home-workers` JSON annotation |
| `rejoin-tailnet.sh` | Re-join a node to the tailnet after its device was deleted (see below) |
| `free-spoke.sh` | Host disk sweep + 24-hour log bound for every box (see below) |
| `setup-hyper-v.sh` | One-time: install Hyper-V on a remote Windows **Home** host over SSH, and reboot it. Run before the first provision of a box; the provisioner's `[2/6]` enables the feature but cannot bring a host from no-Hyper-V to Hyper-V across a reboot |
| `sandbox-runtime-rollback.sh` | ADR-107: remove the sandboxed runtime layer from a node (Flatcar read-only config, or the managed block on Kind/Ubuntu) |
| `kubelet-host-disk.conf` | The kubelet disk bound every node receives (see below) |
| `hyper-v-install-guide.txt` | Reference only: where the Windows Home enablement technique came from, plus its BIOS virtualisation requirements. Nothing reads it — `setup-hyper-v.sh` generates its own batch file |
| `home-lab.env` | Node registry — gitignored, real values only |
| `home-lab.env.example` | Template for `home-lab.env`. Copy it, fill it in, and keep the eleven-field format above |

---

## Keeping the disks from filling

`free-spoke.sh` is the maintenance half of the 2026-09-25 and 2026-10-11 outages (the full
causal chain is in the script's header): `C:\` on the Dell box reached 0 bytes
free, Hyper-V paused both spoke VMs with a `Disk Full` notification, and kubelet
raised no pressure event at all — it measures the guest's 60 GB disk, which was
two thirds empty, and the host that backs it is not its metric. The host is
nobody's metric.

To stop this from recurring, four coordinated guards bound host and guest disk growth (ADR-046):

1. **Guest Kubelet Bound (`kubelet-host-disk.conf`):** Every node receives
   `/etc/kubernetes/kubelet.conf.d/50-host-disk.conf` (applied via `--config-dir`).
   It configures early image garbage collection (`imageGCHighThresholdPercent: 50`,
   `imageGCLowThresholdPercent: 40`) and aggressive hard eviction thresholds
   (`nodefs.available: "40%"`, `imagefs.available: "40%"`, `nodefs.inodesFree: "5%"`,
   `memory.available: "100Mi"`). This bounds guest disk usage to ~60% of virtual disk
   size (~31 GB on a 60 GB spoke, ~52 GB on the 100 GB hub node).
2. **Provisioner Headroom Guard:** `provision-flatcar-worker.sh` checks the host's
   free space on `C:\` before copying/resizing the VHDX. It calculates the bounded
   growth (60%) of this VM and all peer VMs; if projected headroom falls below a
   20 GB reserve, provisioning aborts immediately with `HOST-DISK=FAIL`.
3. **Maintenance Sweep (`free-spoke.sh`):** Applies the same kubelet disk bound to
   existing nodes during Phase 1 (restarting kubelet if changed) before image pruning
   and `fstrim`. In Phase 2, it compacts claimed VHDXes and vacuums caches.
4. **Cluster Validation (`046-home-host-disk-headroom.sh`):** Continuously monitors
   live Windows host `C:\` free space via SSH, hard-failing if any host drops below
   20 GB free.

```bash
# Every couple of days. ~1 minute. No options.
./scripts/hybrid/free-spoke.sh
```

Per host it deletes VHDX files that no VM on that host references — by any
disk path `Get-VMHardDiskDrive` declares or any matching VM name, whatever
state the VM is in (never `flatcar-base.vhdx`, the pristine image) — plus
ignition artefacts of VMs that no longer exist on that box, and the Windows
temp/update caches. Every deletion prints as a `DEL` line; every refusal as a
`SKIP` line, so the run is auditable. Guests are reached on the provisioner's own
port numbering — SSH on `2220 + registry index` — so a guest that is absent from
`home-lab.env`, or renumbered in it, is simply skipped. Per guest that
answers it writes the 24-hour log bound
(`/etc/systemd/journald.conf.d/24h.conf` — `MaxRetentionSec=1day`,
`SystemMaxUse=500M`, the same file new provisions get from Ignition), vacuums
the journal to a day, removes rotated pod logs older than a day, prunes unused
container images, applies the kubelet disk bound, and runs `fstrim`: without
TRIM the blocks a guest frees never come back to the VHDX, so host free space
would not move however much was deleted inside.

The exit status is the point:

| exit | meaning |
|------|---------|
| `0` | every reachable host ended above 20 GB free |
| `1` | a host was unreachable, or one ended below 20 GB — run it again soon, or move data off |

Spoken-node VHDXes grow ~2–3 GB/day per node (metrics PVC, CNPG data and
container images — logs are the bounded share), so a two-day cadence spends
about 6 GB against the ~47 GB the first run reclaims.

The 24-hour bound is on the GUEST's journal, not on the platform's logs. Alloy
ships pod logs to Loki (`loki.platform-observability.svc:3100`), which stores them
in object storage with `limits_config.retention_period: 168h` — seven days — in
`manifests/hub-core-services/loki/loki.yaml` and its spoke counterpart. So
vacuuming a guest journal to a day costs nothing that Grafana reads: the logs are
already off the disk this section is trying to protect. Metrics keep their 15 days
(`retentionPeriod: "15d"` on both `VMSingle`s in `victoriametrics/stores.yaml`).

---

## Recovering a node's tailnet membership

Use `rejoin-tailnet.sh` when a node's **Tailscale device was deleted or expired**
— usually by clearing out stale devices in the admin console and catching a live
one by mistake. Deletion revokes the node key permanently; there is no undelete,
so the node must re-register.

```bash
# Interactive: prints a login URL, you authenticate in a browser
./scripts/hybrid/rejoin-tailnet.sh --node hub-hybrid-dev-h2vst-pll5z

# Cluster is auto-detected; pass it explicitly to skip the lookup
./scripts/hybrid/rejoin-tailnet.sh --node flatcar-spoke-node-1 --cluster spoke

# Non-interactive, and a no-op dry run that changes nothing
./scripts/hybrid/rejoin-tailnet.sh --node <n> --authkey-file k8-secrets/tailscale/authkey
./scripts/hybrid/rejoin-tailnet.sh --node <n> --dry-run
```

### How it presents

The same trap as the NetNat defect below: **every node reports `Ready`
throughout**. Kubelet's outbound path to the API server runs over the *public*
LB endpoint and never touches the tailnet, so `Ready` stays green while the
return path is dead (ADR-046 §22).

The tell is asymmetry — the API server can reach kubelet on nodes that still
have the tailnet, and hangs on the ones that don't:

```
kubectl logs <pod on the affected node>   -> hangs, exit 124
kubectl logs <pod on a healthy node>      -> instant
```

`exec` and `port-forward` fail the same way. A whole-cluster sweep localises it:

```bash
for N in $(kubectl get nodes -o name | cut -d/ -f2); do
  P=$(kubectl get pods -n kube-system --field-selector spec.nodeName=$N \
        -o jsonpath='{.items[0].metadata.name}')
  timeout 20 kubectl logs -n kube-system "$P" --tail=1 >/dev/null 2>&1
  printf '%-38s rc=%s\n' "$N" "$?"
done
```

### Two things that mislead during diagnosis

**`BackendState: Running` does not prove the device is still authorised.** A
deleted device keeps serving from its cached netmap — interface up, old IP
still held, state still `Running` — until tailscaled next reaches the
coordination server and is rejected. If that sync is failing the node may never
find out. The script therefore requires `Self.Online` as well, prints any
`Health` warnings, and re-authenticates when the two disagree.

**A revoked node cannot route to *any* peer.** So one broken node looks like
several broken peers, and it is easy to blame the wrong end. Confirm against the
admin console, which is the only authoritative view of what is still registered.

### What it does

Delivery is through the Kubernetes API, not SSH: CAPI-provisioned control planes
have port 22 closed, and a node that has lost the tailnet often has no other
route in. The script runs a privileged `hostPID` pod pinned to the node and
`nsenter`s into PID 1 to re-run that node's own bootstrap sequence. It prompts
before doing so.

It is node-class aware, because the two classes differ:

| | CAPI Ubuntu CP | Flatcar home worker |
|---|---|---|
| `tailscale` binary | on `PATH` | `/opt/bin/tailscale` |
| node-ip helper | `/usr/local/bin/dynamic-node-ip.sh` | `/opt/bin/dynamic-node-ip.sh` |
| `/etc/tailscale-authkey` | present | absent — interactive login only |

Both are probed, never assumed.

### The control-plane caveat

Re-registration usually yields a **new** tailnet IP. The node still advertises
the old one as its `InternalIP`, so kubelet must restart for
`dynamic-node-ip.sh` to re-derive it. The script detects the change and asks
first — and **on a control-plane node that restart bounces the static pods**, so
the API server and etcd go with it. On a single-replica control plane that is a
short outage. Take it deliberately.

If the address comes back unchanged, no restart is needed and the script says so.

---

## Guest networking: why Internal + NetNat breaks a two-box cluster

*Findings from the 2026-08-26 investigation. Read this before changing anything
about the guest network.*

### The defect

`provision-flatcar-worker.sh` created the guest network as an **Internal**
switch plus `New-NetNat 172.30.0.0/24`. An Internal switch has no physical
uplink, so every guest sits behind a **second NAT** — Hyper-V NetNat, then the
home router. That is correct while one box hosts the whole cluster, and silently
wrong as soon as a cluster spans two boxes:

- Each box independently creates **the same** `172.30.0.0/24` island, each
  owning `172.30.0.1`. Guests on different boxes cannot ARP each other, and
  cannot be routed to each other either — the peer address is *local* on both
  hosts. `ip neigh` on the guest shows the peer as `FAILED`.
- Double NAT defeats Tailscale's UDP hole-punching, so peers fall back to a DERP
  relay.
- Worse: tailscaled advertises **every** local address as a candidate endpoint,
  including Cilium's `cilium_host` (`10.244.x.x`). That address *is* reachable —
  through the VXLAN tunnel, which itself rides Tailscale. Peers select it and the
  "direct" path becomes **Tailscale → VXLAN → Tailscale**.

Observed endpoint set from the Tailscale admin console, with the junk candidate
first:

```
10.244.2.237:37397     <- cilium_host: circular, and the only "reachable" one
110.226.113.35:14986   <- public
172.30.0.13:37397      <- NAT island, unreachable from the other box
```

### How it presents

Nothing names the network. Symptoms land far from the cause:

| Symptom | Actually caused by |
|---|---|
| `upstream connect error` / 503 from agentgateway | backend Service resolves, path doesn't carry |
| PostgREST `EAI_AGAIN` storms, then recovery | DNS crossing the relayed path |
| TCP handshakes taking ~5 s, some timing out | double encapsulation |
| Postgres client hangs with **zero** sessions on the server | TLS handshake never completes |
| ArgoCD PreSync migration Job hits `activeDeadlineSeconds` | it never reached the DB |

`kubectl get nodes` reports every node `Ready` throughout — kubelet's *outbound*
path to the API server is fine. Node `Ready` does not cover this, the same way
it does not cover kubelet inbound reachability (ADR-046 §22).

### The fix

Bind the switch to a real adapter so the inner NAT disappears:

```bash
./scripts/hybrid/convert-to-external-switch.sh --node 3 --dry-run
./scripts/hybrid/convert-to-external-switch.sh --node 3
./scripts/hybrid/convert-to-external-switch.sh --node 3 --revert   # back to Internal
```

**Re-provisioning does not undo the conversion.** The provisioner creates an
Internal switch and its `New-NetNat` only when the switch is missing, and skips the
gateway and NAT setup entirely when the switch it finds is not Internal — so a
converted host stays converted across any number of provisioning runs. It also
prints, on creating an Internal one, that the layout is host-local and breaks
across two boxes, and points back at this file.

**`-AllowManagementOS $true` is mandatory on Wi-Fi.** With `$false` the adapter
is taken away from the host's WLAN service — which is what maintains the 802.11
association — so it drops to `Status: Disconnected` and the guest sits at
`State: no-carrier (configuring)` forever. This looks exactly like "Hyper-V
External switches don't work over Wi-Fi", which is the wrong conclusion.

Verified working on a **Realtek 8821CU USB** adapter (the least favourable case):
guest received `192.168.1.24` by DHCP, reached the gateway at ~5 ms, and reached
the *other box's* host over TCP.

After converting, guests still hold the old static `172.30.0.x` address —
`10-static.network` is `DHCP=no`. Re-provision the node, or switch that unit to
`DHCP=yes`, before the guest can use the LAN.

### Verifying — and two test methods that lie

A created switch is **not** proof of a working bridge. The Wi-Fi failure mode is
"switch exists, adapter disconnected, guest has no carrier". Assert the adapter
stayed `Up` *and* the guest obtained an address.

Both of these produced confident, wrong answers during the investigation:

- **`/dev/tcp/host/port`** — Flatcar's bash does not support it. It reports
  failure for *every* target, including ones that are reachable. Control it
  against `localhost:22` first, or use `nc -z`.
- **`ping` from inside the `cilium-agent` container** — ICMP is unavailable
  there, so every target "fails", including same-node ones.

Any connectivity claim needs a positive control that is known to work.

### The invariant worth enforcing

> A home worker must never select an endpoint inside the cluster pod CIDR for
> another home worker.

That single check names this failure in seconds. It holds regardless of which
switch layout is in use, and it catches the condition while Kubernetes still
reports everything healthy.