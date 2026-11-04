# Runbook: encrypting Kubernetes Secrets at rest

**Applies to:** ADR-003 §6 (Protection at rest), ADR-076 (`secret-encryption-key`)
**Status:** required once per cluster. A cluster is not covered until step 9 has
passed on it.

**IT IS TWO ROLLS, NOT ONE, AND TWO RELEASES.** The control-plane template is
adopted in two phases: **v3** puts the key file on every control-plane node and
nothing reads it, then **v4** adds the API server argument that does. The reason is
in "Why it is two phases" below. Budget for both before starting; stopping after v3
leaves a cluster that is not encrypted and does not claim to be.

**This document covers WORKLOAD CLUSTERS only.** The management cluster is encrypted
by a different mechanism and is not reachable from here — see "The management cluster"
below before assuming the fleet is covered.
**ROTATION IS NOT YET SUPPORTED, and this document is not it.** An earlier version
said rotating was "this document again from step 4". It is not, and following that
would make every existing Secret unreadable.

Rotating `secretbox` needs TWO keys in the provider list at once — the new one first
so writes use it, the old one second so everything already written still reads —
then a roll, then a rewrite of every Secret, then removal of the old key and another
roll. `secret-encryption-config.yaml` renders exactly one key (`key1`), so there is
no configuration in which both are present, and replacing the single key is
precisely the "new key against existing data" failure the escrow exists to prevent.

Closing it means a two-key template and a procedure of its own. ADR-100 (KMS v2)
changes the mechanism anyway, which is why this has not been built: under KMS the
key identifier is a version and rotation is the plugin's concern, not a file's.
Until one of those lands, **the key for a cluster is set once** and a new key means
a new cluster.

**The concrete path, when it is wanted:** a two-key provider configuration plus
`--encryption-provider-config-automatic-reload=true` on the API server. Without that
flag the API server reads the file once at start, which is why every change here costs
a control-plane roll; with it, a rotation becomes "rewrite the Secret, rewrite every
Secret, drop the old key" and no node is replaced. It is not set today, and turning it
on is itself a template change and a roll.

## Why enabling it is not finishing it

Encryption applies to subsequent writes. Turning the provider on leaves every
existing Secret stored exactly as it is now — which is the failure this runbook
exists to prevent: a control enabled, reported as complete, and protecting nothing
already written.

The provider list is ordered. `secretbox` is first so every WRITE is encrypted;
`identity` is second so every existing plaintext READ still succeeds. That ordering
is what makes step 6 safe and what makes step 9 necessary — while `identity`
remains, a plaintext Secret is still readable and the control is partial.

## Why it is two phases

`encryption-provider-config` is what makes the API server REQUIRE the file. A path
it cannot read is a control plane that does not start — and on a workload cluster
that is a cluster nobody can log in to repair, while on the management cluster it is
the thing every other cluster is repaired from.

The file arrives through a CAPI ClusterClass patch that renders its Secret name from
`{{ .builtin.cluster.name }}`. That render cannot be verified from a laptop: it
happens inside CAPI at topology-reconcile time, and the only honest proof is a real
node. Shipping the argument and the file together makes the first evidence of a
correct render "the API server came up" — or did not.

So v3 delivers the mount and the file and nothing that reads them. A node that comes
up with the file missing or misnamed is an ordinary healthy node, and step 3 is `ls`
on it. v4 adds one line, after that evidence exists.

Each phase is a template SPEC change, so each needs a new template NAME and each
rolls the control plane. Each also needs a release: `kakt encryption enable` applies
the ClusterClass from the CLI's own embedded assets, so the CLI binary has to carry
the phase you are applying.

## Reading the configuration back: fingerprints, never the value

At several points below it is tempting to `kubectl get secret <cluster>-encryption-config
-o yaml` and look. **The `secret:` field in that object is the key to every Secret in
the cluster**, and anything it is pasted into keeps it: a terminal scrollback, a
transcript, a ticket, a chat message, a CI log.

It happened on 2026-10-05. `nutgraf-01`'s key was read back in full while verifying
step 9 and ended up in a session transcript. The key alone is useless without an etcd
snapshot of that cluster, but anyone holding both reads every credential in it.

> **CORRECTION, 2026-10-07.** This paragraph used to end *"rotation is not supported …
> an exposed key is not a thing that gets fixed; it gets lived with, or the cluster gets
> rebuilt."* **That is no longer true and following it would be wrong.**
> `kakt encryption rotate` performs a staged two-key rotation: the new key becomes the
> write key while the previous one is RETAINED for reads, so nothing becomes unreadable,
> and the old key is removed only after every Secret is verified rewritten. A disclosed
> key is now an incident with a procedure, and ADR-100 classes it as one — it is replaced
> now, not at the next cadence window.
>
> The sentence survived because the two-key path was built after this runbook was
> written. An operator reading the old text while holding a disclosed key would have
> concluded the only remedy was rebuilding the cluster.

So compare fingerprints:

```bash
# Does the cluster's Secret hold the key the escrow holds? Neither is printed.
kakt escrow verify     --cluster <cluster> --workload   # what the escrow holds
kakt encryption rotate --cluster <cluster> --dry-run    # what the cluster uses
```

The two `sha256:` values for the same generation **must match**.

> **These were not comparable until 2026-10-07**, and the failure is worth knowing because
> it recurs whenever a digest is taken of an encoding rather than a value. The escrow
> stores the key hex-encoded, the provider configuration holds it base64-encoded, and a
> freshly generated key is raw bytes — three representations of the same 32 bytes, three
> different digests, none of them equal. The commands invited a comparison that could not
> succeed, and a mismatch reads as *the escrow holds the wrong key*: the one conclusion
> that would stop a rotation that needed to happen.
>
> Both now reduce to the raw key before hashing. **A digest of the YAML document is still
> not comparable to either** — `shasum` of `enc.yaml`, which this runbook used to suggest,
> changes whenever the file's formatting or its second key does, so it answers a different
> question than the one being asked.

and to check the PROVIDERS without the key, read the structure and not the value:

```bash
kubectl --kubeconfig <HUB> -n platform-capi get secret <cluster>-encryption-config \
  -o jsonpath='{.data.enc\.yaml}' | base64 -d | grep -E 'secretbox|identity|- name:'
```

`kakt escrow verify` prints `sha256:` fingerprints only, for the same reason, and is
the right way to ask whether the escrow and the cluster agree.

On the HUB this matters more than anywhere: its etcd holds the secret store's own
credentials, the identity provider's signing keys, and every tenant's provisioning
material.

## What a disclosed key actually exposes — and what it does not

Get this wrong in either direction and the remediation is wrong. It was got wrong once, on
`nutgraf-01`'s rotation: eleven CloudNativePG base backups were classed as confidentially
compromised. They were not, and the thing that *was* exposed was not on the list.

**The key decrypts Kubernetes Secrets in etcd. Nothing else.**

| | compromised by the key? | why |
|---|---|---|
| **etcd snapshots** taken while the key was in force | **YES** | this is the matching half. Key plus snapshot reads every Secret in the cluster |
| **control-plane disk or volume snapshots** | **YES** | they contain etcd's data directory |
| **CloudNativePG base backups and WAL** | **NO** | they hold PostgreSQL pages, written by Postgres to object storage. `secretbox` never touched them, and the key does not decrypt a single byte of them |
| **application backups generally** | **NO**, for the same reason | unless the application itself stored the disclosed material |

**But there is a transitive path, and it is the one that actually needs closing.** An
attacker with the key and an etcd snapshot reads every Secret — including the object-store
credentials for the backup bucket. They cannot decrypt a CNPG backup with the key; they can
**fetch** it with the credentials the key exposed.

That changes the remediation completely:

| conclusion | action it leads to | correct? |
|---|---|---|
| "the CNPG backups are compromised" | re-take the backups | **no** — the old ones are still readable by anyone holding the credentials |
| "the credentials reachable from a compromised etcd snapshot are exposed" | **rotate every credential the etcd snapshot contained**, starting with object-store access | **yes** — this is what closes it |

So after a key disclosure, enumerate what the etcd snapshots held, not what the backup
system contains. The list is every Secret in the cluster at that time: object-store keys,
registry pull credentials, database passwords, the secret store's own machine identity,
webhook tokens. Rewriting Secrets under a new key does **not** rotate any of them — it only
stops *future* snapshots from disclosing them.

**Rotating the encryption key is therefore the first step of the response and not the
whole of it.** The rotation closes the window; it does not un-disclose what was already
readable.

## 0. Before you start

The escrow must be reachable. The key is the one value that cannot be regenerated —
etcd **and every etcd backup** are encrypted with it — so a cluster encrypting with a
key held nowhere else has traded disclosure for total loss (ADR-076
`secret-encryption-key`).

`kakt encryption enable` refuses outright without an escrow rather than generating a
key with nowhere to put it, so this check is a courtesy, not a gate:

```bash
# All four or none. A partial set produces a client that authenticates and fails.
env | grep -c '^INFISICAL_ESCROW_' ; echo "expect 4"

# What the escrow already holds for this cluster. Empty is fine on a first run —
# the key is generated and escrowed. A value means a previous cluster used it, and
# that is the value the new control plane must use.
kakt escrow --help >/dev/null && echo "escrow CLI available"
```

Then confirm the cluster you are about to change is the one you mean, because this
rolls its control plane:

```bash
kubectl --kubeconfig <path> config current-context
kubectl --kubeconfig <path> get kubeadmcontrolplane -A
```

Do not continue on a cluster mid-rollout, or whose control-plane replicas are not all
Ready. A roll started on top of a roll leaves members disagreeing about the write
provider, and a Secret written by one may be unreadable by another.

## 1. Adopt the control-plane template (phase 1, v3)

**THE CLASS ARRIVES BY SYNC. THE SECRET DOES NOT.** Get this the right way round,
because two earlier versions of this step had it backwards in both directions.

ArgoCD's `infrastructure-provider` ApplicationSet (boundary 03) syncs
`manifests/providers/<provider>`, whose kustomization pulls in `base/` and with it
`spokepool-clusterclass-v1.yaml`. So **publishing and promoting a bundle puts the new
control-plane template on the box by itself** — 0.1.16-rc.142 is how
`spokepool-control-plane-v3` arrived, with nobody running a command. Check before you
apply anything:

```bash
kubectl --kubeconfig <HUB> -n platform-capi get clusterclass spokepool-v1 \
  -o jsonpath='{.spec.controlPlane.ref.name}{"\n"}'
```

The **Secret** is the part nothing reconciles. It is deliberately absent from that
kustomization because it carries key material, per cluster, sourced from the escrow
(see the comment in `base/kustomization.yaml`). That is what the command below
writes, and it is all it writes.

**Run it against the MANAGEMENT cluster, including for a workload cluster.**
`platform-capi` and every CAPI object live on the hub; `--cluster` names whose key it
is, `--kubeconfig` says where CAPI lives. Pointing it at the spoke fails with
`namespaces "platform-capi" not found`.

Confirm first that the CLI you are about to run carries v3 and not the argument:

```bash
kakt encryption enable --cluster <cell> \
  --class manifests/providers/hetzner/base/spokepool-clusterclass-v1.yaml \
  --kubeconfig <path> --dry-run | grep -E 'control-plane-v|encryption-provider-config'
```

Expect `spokepool-control-plane-v3` and **no** `encryption-provider-config`. If the
argument is there, this CLI carries v4 and you are skipping the gate that makes the
roll safe.

```bash
# a workload cluster FIRST — see "Which cluster first" below
kakt encryption enable --cluster <cell> \
  --class manifests/providers/hetzner/base/spokepool-clusterclass-v1.yaml \
  --kubeconfig <path> --dry-run

kakt encryption enable --cluster <cell> \
  --class manifests/providers/hetzner/base/spokepool-clusterclass-v1.yaml \
  --kubeconfig <path>
```

It restores the key from the escrow or generates and escrows one, applies the provider
configuration Secret, then applies the ClusterClass. The Secret goes first, because a
control plane whose Secret does not exist cannot start and the template is what makes a
node ask for it.

**The Secret is named `<cluster>-encryption-config`, one per cluster.** It was briefly
one flat `secret-encryption-config` in `platform-capi` for every cluster of a class,
which is one key for all of them — and the first cluster to rotate would have left the
others' etcd undecryptable by a key they still believed in. The name is built by
`assets.EncryptionSecretName` on the create side and by the ClusterClass's
`secretEncryptionConfig` patch on the read side; preflight 003-encryption-secret-is-per-cluster pins the two together,
because a disagreement is not reported as a missing Secret, it is a node that never
finishes bootstrapping.

For the management cluster, omit `--class` — it defaults to that cluster's.

## Which cluster first

**A workload cluster, not the management cluster.**

If rolling a workload cluster's control plane goes wrong, tenant workloads are affected
and the management plane — CAPI, ArgoCD, the secret store, the identity provider — stays
up to repair it. Rolling the management cluster first risks losing the thing that would
repair it.

It also proves the whole chain once on a cluster where being wrong is recoverable: key
escrowed, Secret assembled, file on the node, provider active, encrypted prefix in etcd.
Do the management cluster second, with the procedure already exercised.

## 2. Roll the control plane (first roll)

An API server flag takes effect only on a process start, so each control-plane node
is replaced. CAPI does this when the template changes; it is not instant and it is
not free.

**On a spoke, check `providerID` before trusting a stall.** CAPI matches Machines to
Nodes by `.spec.providerID` and nothing else, so a node that is Ready and serving
traffic but carries none is, to CAPI, a Machine that never joined: empty `NODENAME`,
and a roll that waits reporting `NodeHealthy=False/NodeProvisioning` — which names a
provisioning step rather than a missing field. The spoke control-plane template
omitted the injection block until v3 for exactly this reason. Preflight 046-node-bootstrap-invariants now fails
a template that ships `dynamic-node-ip.sh` without it.

```bash
kubectl -n platform-capi get kubeadmcontrolplane -w
```

Wait for `UPDATED` to equal `REPLICAS` and all nodes `Ready`. Do not proceed while
a node is mid-replacement: the cluster then has members disagreeing about the write
provider, and a Secret written by one may be unreadable by another.

## 3. Verify the file landed, and is the right file

**This is the gate the two-phase split exists to create.** Nothing reads the file
yet, so being wrong here costs nothing — which is the only moment at which that is
true.

```bash
NODE=$(kubectl get nodes -l node-role.kubernetes.io/control-plane \
  -o jsonpath='{.items[0].metadata.name}')

# The file exists, is root-owned and 0600.
kubectl debug node/$NODE -it --image=busybox -- \
  ls -l /host/etc/kubernetes/enc/enc.yaml

# It is THIS cluster's key, and the provider order is secretbox then identity.
kubectl debug node/$NODE -it --image=busybox -- \
  grep -E 'secretbox|identity|name:' /host/etc/kubernetes/enc/enc.yaml
```

Also confirm the Secret CAPI read it from is named for this cluster:

```bash
kubectl -n platform-capi get secret <cluster>-encryption-config
```

If the file is absent, the patch did not render — **stop here**. The cluster is
healthy and unencrypted, which is the state v3 is designed to fail into. A missing
`<cluster>-encryption-config` Secret with the file absent means the name disagreed;
a present Secret with the file absent means the patch did not apply to the
control-plane template.

Do not proceed to v4 until this passes on a replaced node. A node that predates the
roll may hold a file written by an earlier procedure, so check one that CAPI
actually replaced.

## 4. Adopt the argument (phase 2, v4)

**Written and in the tree as of 2026-10-05.** Both classes carry
`spokepool-control-plane-v4` / `hetzner-mgmt-control-plane-v4` with the one line v3
withheld:

```yaml
apiServer:
  extraArgs:
    encryption-provider-config: /etc/kubernetes/enc/enc.yaml
```

A new template NAME, not an edit: the spec of a template a Cluster references is
immutable or effectively so, and an edit the webhook rejects leaves ArgoCD wedged on a
resource it can neither apply nor drop.

`cmd/kakt/encryption_test.go` used to assert the argument was ABSENT. Flipping that
assertion was the recorded phase boundary, deliberately a code change so the boundary
could not move by accident. It now requires the argument, the mount and the
per-cluster Secret name together — the argument without the mount is an API server
that cannot see a file that exists, and the argument without the Secret is a node that
never finishes bootstrapping.

**How it reaches each cluster differs, and this is where the two classes part ways:**

| | delivery | what adopts v4 |
|---|---|---|
| workload cluster | `manifests/providers/<provider>`, synced by ArgoCD | **publishing and promoting a bundle**; the roll starts on sync |
| management cluster | the CLI's embedded assets, synced by nothing | `kakt encryption enable --cluster <hub> --kubeconfig <HUB> --apply-class` |

So a spoke adopts v4 by release. The hub's class is reconciled by nothing — it is
applied once at Day-0 and never again — so `--apply-class` is the only way it ever
changes, and its live state can differ from the repository with nothing reporting it.

**Do the spoke first and finish step 6 on it before touching the hub.** A failed roll
on a workload cluster leaves the management plane up to repair it; the reverse does
not hold.

## 5. Roll the control plane (second roll)

As step 2. After this the provider is in force.

If the API server does not come up, the file is what to look at first — step 3 proved
it was there under v3, so a failure here is the argument's path or permissions, not
the render.

## 6. Verify NEW Secrets are encrypted

The check that matters is the stored form, read from etcd. **An API read shows
plaintext either way** — the API server decrypts on the way out — so `kubectl get
secret` proves nothing here.

```bash
kubectl -n default create secret generic enc-probe --from-literal=k=v

kubectl -n kube-system exec etcd-$(kubectl get nodes \
  -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[0].metadata.name}') -- \
  sh -c 'ETCDCTL_API=3 etcdctl \
    --cacert /etc/kubernetes/pki/etcd/ca.crt \
    --cert /etc/kubernetes/pki/etcd/server.crt \
    --key /etc/kubernetes/pki/etcd/server.key \
    get /registry/secrets/default/enc-probe' | hexdump -C | head -3
```

Expect the value to begin `k8s:enc:secretbox:v1:key1:`. If it begins `k8s` followed
by readable YAML, the provider is not in force — **stop and fix step 5** rather than
proceeding, because step 7 would rewrite every Secret into plaintext again.

```bash
kubectl -n default delete secret enc-probe
```

## 7. Rewrite every existing Secret

This is what actually encrypts the data already there. Each Secret is read and
written back unchanged, so the write goes through `secretbox`.

**First, find the Secrets this cannot rewrite.** `kubectl replace` FAILS on a Secret with
`immutable: true`, and the failure is quiet in a bulk pipe: the object stays sealed under
the previous key, step 8's count catches it, and anyone treating a small non-zero count as
"close enough" then makes those objects permanently unreadable at the finalize step.

```bash
kubectl --kubeconfig <SPOKE> get secrets -A -o json \
  | jq -r '.items[] | select(.immutable == true) | "\(.metadata.namespace)/\(.metadata.name)"'
```

Empty output is the pass. Anything listed must be **deleted and recreated**, not replaced,
and that is a change to whatever owns it — so it is a decision before the rotation, not a
surprise during it.

```bash
kubectl get secrets --all-namespaces -o json \
  | kubectl replace -f -
```

Per namespace instead, when the fleet is large enough that one transaction is
unwise:

```bash
for ns in $(kubectl get ns -o jsonpath='{.items[*].metadata.name}'); do
  echo "== $ns"
  kubectl -n "$ns" get secrets -o json | kubectl replace -f - || echo "FAILED: $ns"
done
```

**Service account token Secrets and Helm release Secrets are included and should
be** — they hold credentials and release data like anything else. A `replace` that
fails on an immutable Secret is reported and skipped; note it, because an immutable
Secret cannot be rewritten this way and must be recreated to become encrypted.

## 8. Verify EVERY Secret is encrypted

Not a sample. The point of the rewrite is that nothing is left behind, and a sample
cannot show that.

This counts both and NAMES anything still in plaintext, which a pair of counts
cannot. A difference tells you how many credentials the audit finding still covers;
only the key tells you which.

```bash
NODE=$(kubectl get nodes -l node-role.kubernetes.io/control-plane \
  -o jsonpath='{.items[0].metadata.name}')

kubectl -n kube-system exec etcd-$NODE -- sh -c '
  export ETCDCTL_API=3
  C="--cacert /etc/kubernetes/pki/etcd/ca.crt
     --cert /etc/kubernetes/pki/etcd/server.crt
     --key /etc/kubernetes/pki/etcd/server.key"

  # ENUMERATE FIRST, AND REFUSE TO PROCEED IF THAT FAILED.
  #
  # This is the difference between "no ciphertext exists" and "verification could not
  # run", and collapsing the two is how this step reports a catastrophe about a healthy
  # cluster. See below.
  KEYS="$(etcdctl $C get /registry/secrets/ --prefix --keys-only)" || {
    echo "ERROR could not list Secrets in etcd"; exit 2; }
  KEYS="$(echo "$KEYS" | grep .)" || {
    echo "ERROR etcd returned no Secret keys at all, which no live cluster does"; exit 2; }

  total=0; enc=0; plain=0
  for k in $KEYS; do
    total=$((total+1))
    V="$(etcdctl $C get "$k")" || { echo "ERROR could not read $k"; exit 2; }
    case "$V" in
      *k8s:enc:secretbox:v1:key1:*) enc=$((enc+1)) ;;
      *) plain=$((plain+1)); echo "PLAINTEXT $k" ;;
    esac
  done

  echo "total=$total encrypted=$enc plaintext=$plain"
  # EXIT CODES ARE DISTINCT, so anything wrapping this cannot read FAIL as success.
  # An earlier draft printed FAIL and exited 0, which means a script around it would
  # treat plaintext Secrets as a pass -- the same collapse as ERROR into 0 encrypted,
  # one level up.
  if [ "$total" -gt 0 ] && [ "$plain" -eq 0 ]; then echo PASS; exit 0; fi
  echo FAIL; exit 1
'
echo "verifier exit: $?"   # 0 PASS, 1 FAIL, 2 ERROR
```

**Expect `PASS`, `total` equal to `enc`, `plaintext=0`, and exit 0.**

#### Three outcomes, and ERROR is never 0 encrypted

| | means |
|---|---|
| `PASS` | ciphertext found for every Secret, carrying the expected prefix |
| `FAIL`, exit 1 | at least one Secret is plaintext or in an unexpected encoding — each named |
| `ERROR`, exit 2 | **verification could not inspect etcd.** Says nothing about the data |

The exit codes are distinct for the same reason the outcomes are: automation that wraps
this must not be able to read `FAIL` as success, which an earlier draft permitted by
printing `FAIL` and exiting 0.

**This distinction is the whole reason the block above is shaped the way it is**, and it
was learned the hard way: three separate attempts at this verification each returned an
empty string, and an empty string in the old version read as `total=0 encrypted=0` — a
catastrophic claim about clusters that were fully encrypted.

| what went wrong | what it looked like |
|---|---|
| `etcdctl` run on the node, where it does not exist | `0 encrypted` |
| `sh -c` against an etcd image with no shell | `0 encrypted` |
| binary piped through BSD `grep -c` / `tr` | `0 encrypted` |

A verification step whose failure mode is an empty result **will report the absence of
the thing it cannot measure.** That matters most during a Kubernetes upgrade, when the
etcd image changes underneath this command — so the enumeration is checked before any
counting begins, and a read failure aborts rather than counting as plaintext.

Three things about that command are deliberate, because the obvious version of it
lies:

**`case`, not `grep -c`.** Two earlier versions of this step piped etcd's output
through `grep -c 'k8s:enc:secretbox:v1:key1:'`. Secret values are CIPHERTEXT, so the
stream carries NUL bytes, and grep's behaviour on binary input is implementation
dependent. Measured on three encrypted records:

    BSD grep (macOS, where this step is actually run)   no output at all, exit 1
    busybox grep (alpine)                               3, exit 0

So the count is not portable, and on the machine this procedure is run from it
produces NOTHING rather than a number — which in a comparison against the total
reads as "no Secrets are encrypted" on a cluster where all of them are. `case` is a
shell builtin doing a glob on a string: command substitution drops the NULs and the
ASCII prefix survives, which is all that is being looked for.

**The loop runs INSIDE the pod.** Dumping `get --prefix` to the machine you are
sitting at would write every Secret still in plaintext to a local file, in the clear,
as a step in the procedure for protecting them. Nothing leaves the node here.

**No `grep` or `tr` inside the pod either.** The etcd image is minimal and what it
ships is not guaranteed; `etcdctl` plus shell builtins is.

#### It depends on `sh` in the etcd image, and that will not always be there

The loop above is `sh -c`, so it needs a shell in the etcd container. **On Kubernetes
1.31.6 — what both clusters run — there is one, and this is how 87/87 and 118/118 were
verified.** Newer etcd images are distroless and have none. Seen on 2026-10-05 in the
ADR-100 gate environment, whose kind node ships a later etcd:

```
exec: "sh": executable file not found in $PATH
```

**That failure is dangerous because of its shape.** `kubectl exec` returns nothing, the
command substitution is empty, and the step reports `total=0 encrypted=0` — or, with
the earlier phrasing, "etcd does not hold the prefix" — about a cluster that is fully
encrypted. It reads as a catastrophic finding rather than a missing binary, during the
procedure whose whole purpose is deciding whether credentials are protected.

**It will break here when the Kubernetes upgrade in ADR-052 §20 lands.** When it does,
drive the loop from the HOST instead, which needs no shell in the pod — one exec for
the key list, one per key:

```bash
E="kubectl -n kube-system exec etcd-$NODE -- etcdctl
   --cacert /etc/kubernetes/pki/etcd/ca.crt
   --cert /etc/kubernetes/pki/etcd/server.crt
   --key /etc/kubernetes/pki/etcd/server.key"
total=0; enc=0
for k in $($E get /registry/secrets/ --prefix --keys-only | grep .); do
  total=$((total+1))
  if $E get "$k" | LC_ALL=C grep -aq 'k8s:enc:secretbox:v1:key1:'; then
    enc=$((enc+1))
  else
    echo "PLAINTEXT $k"
  fi
done
echo "total=$total encrypted=$enc"
```

Slower — one round trip per Secret — and it moves the ciphertext across the API server
rather than keeping it on the node, so prefer the in-pod loop while a shell exists.
`LC_ALL=C` is required on the host side: BSD `grep` and `tr` abort with `Illegal byte
sequence` on ciphertext, which is another empty result that reads as a finding.

`ETCDCTL_API=3` is omitted above because setting it needs the shell this fallback
exists to avoid, and etcdctl v3.5+ defaults to the v3 API anyway.

**THE TRAILING SLASH ON `/registry/secrets/` IS LOAD-BEARING.** Without it the prefix
also matches every other etcd key beginning with that string, and on this platform
that includes `/registry/secrets.crossplane.io/storeconfigs/default` — a Crossplane
CRD, not a core Secret. It reported `total=88 encrypted=87` with one eternal
`PLAINTEXT` line, so step 8 could never pass and step 9 would be blocked forever by
an object that is correctly not encrypted.

An earlier version of this step had `etcdctl ...` with the certificate flags elided,
which is not a command anyone can run.

## 9. Remove the `identity` fallback

Only now, and **only after step 8 showed `total` equal to `enc` with no `PLAINTEXT`
lines**. While `identity` is in the list a missed Secret is still readable and the
control is merely partial; once it is gone that same Secret is **unreadable**, and
putting the fallback back cannot decrypt what was never encrypted. This step is not
reversible by re-running it.

```bash
./bin/kakt encryption enable --cluster <cell> --kubeconfig <HUB> \
  --no-plaintext-fallback
```

It restores the same key from the escrow and re-renders the Secret with `secretbox`
alone. It prints what it is about to do, because the consequence is not undoable:

```
[encryption] rendering WITHOUT the identity provider; any Secret still in plaintext
             will become unreadable once the control plane rolls
```

**It is a flag and not an edit, deliberately.** This used to say "edit
`internal/assets/manifests/secrets/secret-encryption-config.yaml` and re-run", which
changes the template for every cluster the CLI will ever render — including a cluster
part-way through step 7, where dropping the fallback loses data. The template renders
the fallback by DEFAULT and omits it only when asked, so the dangerous direction needs
saying out loud. The caller knows which cluster it is on; the template does not.

Nothing reconciles the Secret, which is why this is an explicit step and not something
that happens on a sync.

**Then roll the control plane again** — the API server reads the provider
configuration once at start, and nothing re-reads it
(`--encryption-provider-config-automatic-reload` is not set; see the rotation note
near the top). Until that roll completes, the API server is still using the
configuration it started with, fallback included.

#### Triggering it, and why it is not urgent

**Nothing rolls by itself here.** Only the Secret changed, and CAPI has nothing to
reconcile for a Secret — the ClusterClass is untouched. That is a gap in this
procedure and it is stated rather than papered over.

**Do not force it by deleting the Machine or the node.** On a single-replica control
plane that removes the only API server before a replacement exists, which is the one
thing the surge-based roll is careful never to do.

The options, honestly:

| | |
|---|---|
| let it ride with the next control-plane template change | no extra roll, no risk; the Secret is already correct and the next roll for any reason picks it up |
| `KubeadmControlPlane.spec.rolloutAfter` | **VERIFIED 2026-10-07, and now the recommendation for a deliberate roll.** It was recorded here as unverified on a topology-managed cluster; it was then used twice on `nutgraf-01` — once for each stage of the key rotation — and the topology controller preserved the hand-set field both times. A surge roll followed in each case and the API server stayed available throughout. `kubectl -n platform-capi patch kcp <name> --type=merge -p '{"spec":{"rolloutAfter":"<RFC3339 now>"}}'` |
| a no-op template rename purely to force a roll | works, and spends a control-plane replacement on a version bump that changes nothing |

> **The recommendation changed on 2026-10-07.** "Let it ride" was the advice while
> `rolloutAfter` was unverified. It is verified now, so a deliberate roll is available and
> should be used when the roll matters — which it does for a key rotation, because
> `identity` cannot be removed until the new configuration is actually on a node.
>
> One observation from both rolls, recorded because it will recur: each left a **stale
> `Node` object** for the replaced machine, and the `KubeadmControlPlane` reported
> `EtcdClusterHealthy: False` until it was deleted. The Machine was gone and the Node was
> not. Deleting the stale Node restored health and the roll completed. That is not part of
> the rotation and it blocks it, so check for it before concluding a roll has stalled.

**For an incidental configuration change the first is still reasonable, because a pending
roll is hygiene and not an open hole.** With step 8 showing every Secret encrypted, `identity` is a READ
fallback with nothing left to read: it never fires. `secretbox` is first, so every
write is encrypted regardless. The only way plaintext reappears is an etcd restore
from a pre-encryption backup — which is precisely the case where the fallback is what
you want.

So the substantive security outcome is reached at step 8. Removing the fallback is
defence in depth, and the honest state to record is below.

Then re-run step 8. It must still show `total` equal to `enc` — **if any Secret was
missed it is now unreadable**, which is why step 8 is run twice and why the fallback
is removed last.

## 10. Record it

There are TWO states worth recording separately, because conflating them either
overclaims or undersells what was done:

| state | reached when | what it means |
|---|---|---|
| **encrypted and verified** | the provider is active and step 8 shows `total` equal to `enc` | every credential in this cluster's etcd is ciphertext. This is the audit finding's substance |
| **fallback removed and verified** | `identity` is gone from the running configuration and step 8 passes again after that roll | a plaintext object could not be read even if one appeared |

The first is the security outcome. The second is defence in depth and may legitimately
wait for the next control-plane roll rather than buying one.

Record the date of each against the cluster, and record them as different things. A
cluster rebuilt from an older template is not covered by either.

### Fleet state

| cluster | encrypted and verified | fallback removed and verified |
|---|---|---|
| `nutgraf-01` (spoke) | **2026-10-05** — `total=87 encrypted=87` | config written without `identity`; **pending a roll** |
| `nutgraf-hub` | **2026-10-05** — `total=118 encrypted=118` | not started; the fallback is still in force |

Both clusters took `v4` on 2026-10-05 and both reached every-Secret-encrypted the same
day. The spoke's fallback removal is written into its Secret and takes effect on its
next control-plane roll; the hub's has not been started, and must not be until its
step 8 has been re-run after any further rewrite.

Neither pending removal is an open hole. With every Secret encrypted, `identity` is a
READ fallback with nothing left to read, and `secretbox` is first so every write is
encrypted regardless. See "Triggering it, and why it is not urgent" above. Note the date against
the cluster; a cluster rebuilt from an older template is not covered.

## The management cluster

Step 1 differs for it; steps 2 to 10 are the same, both rolls included.

It is created from a temporary bootstrap cluster and CAPI is then pivoted onto it, so
it ends up managing itself. That gives three cases, and only the last needs anything
special:

| | where the provider configuration is read from | circular? |
|---|---|---|
| first build | the bootstrap cluster | no — different cluster |
| node replacement | its own etcd, through a live API server that already holds the file on disk | no |
| full rebuild | a new bootstrap cluster, with no previous cluster to read from | **the key must come from the escrow** |

An earlier version of this runbook claimed the management cluster could not use this
mechanism at all, on the reasoning that its configuration would live in the etcd it
encrypts. That is wrong for the first two cases: the file is on disk once written, and
a replacement node is provisioned by a control plane that is already running.

The third case is the one that matters, and it is why the key is escrowed
(ADR-076 `secret-encryption-key`). On a rebuild the CLI restores it rather than
minting a new one — a new key against a restored etcd would leave every Secret in it
undecryptable, with the loss happening during the recovery.

Steps 3 to 10 apply unchanged, and the rewrite there is the more consequential of the
two clusters: the management cluster holds the secret store's own credentials, the
identity provider's, and every tenant's provisioning material.

## What this does not protect against

`secretbox` keeps the key on the control-plane host, in a file the API server reads.
It protects etcd data, etcd backups and disk snapshots. It does **not** protect
against compromise of a control-plane node — that is what KMS v2 with an external
key is for, and ADR-003 §6 records what adopting it would still have to decide.

**ONLY `core/v1` SECRETS ARE ENCRYPTED.** The provider configuration names
`resources: [secrets]`, which is that one resource and nothing else. A CUSTOM
RESOURCE holding sensitive material is stored in plaintext, and the counts in step 8
will not mention it — they are scoped to `/registry/secrets/` precisely so unrelated
resources do not read as failures.

That is a real limit and it is reached on this platform: `secrets.crossplane.io`
StoreConfigs sit beside the core Secrets in etcd and are not covered. Covering a CRD
means naming it explicitly, as `storeconfigs.secrets.crossplane.io`, which is a
decision about that resource rather than a default — so it is recorded here as known
and not done, instead of being implied by "Secrets are encrypted at rest".
