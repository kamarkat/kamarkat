# The local Kind profile

Everything the platform provides for a **local Kind stack** is defined here, and
only here. A local stack uses it to run the operator, isolated sandboxes, and
previews on a laptop, so code builds and preview logic can be checked before
dev.

The only local code outside this folder is preview-access's local mode,
`operators/ephemeral-job-operator/internal/previewaccess/localmode/`. Go's
`internal` rule keeps it inside the operator module.

## What it provides

| Part | Where | What it is |
|---|---|---|
| Operator | `kind/operator` | `config/default` with `--network-policy=none`: Kind enforces no network policy, so the operator renders none (ADR-106 is enforced from dev onward). |
| Sandboxed runtime | `kind/runtime` | The ADR-107 RuntimeClass `sandboxed` (gVisor) and its per-node installer. Kind's kube-proxy resolves Services per packet, so it needs none of Cilium's bypass. |
| Previews | `kind/preview-access` | preview-access in **local development mode** (ADR-108, "Local development mode"). |
| Installer | `kind-profile.sh` | Applies the runtime and preview-access, waits for both, and checks each one works. It fails, naming the step, otherwise. |

## What it does not provide (verified in dev only)

- network policy: ADR-106 ingress and egress, renderer pairing;
- for previews: a separate registrable domain, login, the single-use handoff, the
  partitioned preview cookie, and admission (`409 PreviewTargetInUse`, the
  `expiresAt` rule). The reconciler still never makes a second record on one
  target Ready.

## Safety

preview-access refuses local mode unless **every node's `providerID` starts
with `kind://`**. Local mode can't run anywhere but Kind.

## Using it

1. **Install the operator** from `scripts/local/kind/operator` instead of
   `operators/ephemeral-job-operator/config/default`. It contains exactly one
   operator image reference, so a local image rewrite still applies.
2. **Run the profile** after the operator is ready. Every variable is required:

   ```sh
   KUBECONFIG=<kind kubeconfig> \
   OPERATOR_IMAGE=<locally built operator image, name:tag> \
   LOCAL_NAMESPACE=<namespace the app's SDK creates PreviewAccess in> \
   LOCAL_APP_ORIGINS=<the apps' origins, comma-separated, e.g. http://localhost:3002,http://localhost:3007> \
   LOCAL_SUBJECT=<the app's development identity subject> \
   LOCAL_PREVIEW_BASE='http://{host}:<preview port>' \
   kamarkat/scripts/local/kind-profile.sh
   ```

3. **Reach the previews:** `kubectl -n platform-ops port-forward svc/preview-access <preview port>:8081`.
   `*.preview.localhost` resolves to loopback in Chrome, Firefox and Safari, so
   `http://<status.host>:<preview port>/` reaches the preview with no hosts-file
   entry.
4. **Reach the app API:** route `/_platform/preview-access/` on the app's origin
   to `preview-access.platform-ops.svc.cluster.local:8080`. The codes and state
   endpoints accept only an origin listed in `LOCAL_APP_ORIGINS` as `Origin`
   (each exactly `scheme://host[:port]`), and act as `LOCAL_SUBJECT` in
   `LOCAL_NAMESPACE` for every app. A refusal page posts to each listed origin;
   the browser delivers it only to the actual parent. Set the PreviewAccess `ownerSubject` to that subject.

The profile is idempotent. Re-running it after changing the installer re-runs
the installer, because its ConfigMap name carries a hash of its content.
