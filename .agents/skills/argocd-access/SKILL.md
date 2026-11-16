# ArgoCD Access

Use when you need to inspect, sync, or debug any ArgoCD Application on the hub (including `kubectl` fallback). Verified against live hub `hub-hybrid-dev` (`65.109.41.23`).

## Endpoint (env-aware)

- **dev:** `argocd.dev.nutgraf.in` — canonical, via `hub-gateway` listener `argocd` (`manifests/hub-core-services/gateway/hub-gateway.yaml:98`) → `HTTPRoute argocd` (`httproutes.yaml:88`) → `svc/argocd-server:80` (`platform-ops`). `argocd-cm` `url: https://argocd.dev.nutgraf.in` (`manifests/argocd/environment-manager/templates/01-platform-infra-appset.yaml:53`).
- **prod:** `argocd.nutgraf.in` (apex, `manifests/environments/base/hubenvironment.yaml:13`).
- Derive: `Zone = spec.domain` from `manifests/environments/<env>/patch-hubenvironment.yaml` → `ArgoCD = "argocd."+Zone` (`internal/hub-cli/bootstrap/hubdomain.go:56`). `argocd.internal.dev.nutgraf.in` does not exist (0 hits); `argocd.nutgraf.in` for dev is legacy (`scripts/generate-local-certs.sh:38`).

## Credentials

```
Username: admin
Password: (from bootstrap log or live secret)
```

```bash
# Bootstrap log (printed once, .gitignored)
grep "ArgoCD Password" .zero-ops/bootstrap-hub.log
# e.g. mu-Ezo3o2g-UbOYQ (hub-hybrid-dev 2026-08-27)

# Live (authoritative, via hub kubeconfig)
kubectl --kubeconfig=${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/hub-hybrid-dev.kubeconfig \
  get secret argocd-initial-admin-secret -n platform-ops \
  -o jsonpath='{.data.password}' | base64 -d
# Source: internal/hub-cli/components/installer.go:96
```

Hub kubeconfig is `${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/hub-hybrid-dev.kubeconfig` (`.zero-ops/state/hub-hybrid-dev.json:35`). `${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/hub.kubeconfig` does not exist.

## Access — Port-Forward (verified, recommended)

Public `argocd.dev.nutgraf.in` currently 307-loops for the CLI (`curl -k https://argocd.dev.nutgraf.in/api/version` → `307` to same URL; `argocd login ... --grpc-web` → `stopped after 10 redirects`). Use port-forward; it bypasses the Gateway and is the only verified CLI path.

```bash
HUB_KC=${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/hub-hybrid-dev.kubeconfig

# 1. Forward (svc exposes 80/443 → pod 8080; either mapping works, 18444:443 matches runbook)
kubectl --kubeconfig=$HUB_KC port-forward -n platform-ops svc/argocd-server 18444:443 &
# Wait 3s for "Forwarding from 127.0.0.1:18444 -> 8080"

# 2. Login (creates ~/.argocd/config; thereafter omit --server)
argocd login 127.0.0.1:18444 --username admin --password <password> --grpc-web --insecure
# --grpc-web required through port-forward (HTTP/1.1 tunnel); --insecure for LE/dev cert
# Verified: 'admin:login' logged in successfully, Context '127.0.0.1:18444' updated

# 3. Verify
curl -k https://127.0.0.1:18444/api/version  # → {"Version":"v2.14.1+..."}
argocd app list --grpc-web
argocd app get <app> --grpc-web

# Cleanup
kill %1
# Or: argocd logout 127.0.0.1:18444
```

Do not pass `--server` after login — the session in `~/.argocd/config` carries it. Passing `--server` without a prior `login` hits the Gateway loop or triggers core-mode fallback (reads `argocd-cm` from current kube context like `kind-waypoint-local`).

Direct API alternative (same forward, no CLI):

```bash
curl -k -X POST https://127.0.0.1:18444/api/v1/session \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"<password>"}'  # → token
# See docs/runbooks/backup-credential-chain-recovery.md:245 for operation/refresh endpoints
```

## Access — Public Gateway (UI only, CLI currently broken)

Browser: `https://argocd.dev.nutgraf.in` (cert `argocd-server-tls` in `platform-ops`, `ingress/hub-public-certificates.yaml:68`). CLI via Gateway is not working today due to 307 loop — fix requires `argocd-cmd-params-cm: server.insecure: "true"` since Gateway terminates TLS and forwards plaintext to `argocd-server:80`. Until fixed, use port-forward above.

## Common Commands (after port-forward login, always --grpc-web)

```bash
argocd app list --grpc-web
argocd app get <app> --grpc-web
argocd app get <app> --hard-refresh --grpc-web   # invalidate stale cluster cache
argocd app diff <app> --grpc-web
argocd app sync <app> --grpc-web --force --prune
argocd app sync <app> --grpc-web --watch
argocd app wait <app> --health --grpc-web
argocd cluster list --grpc-web
```

kubectl fallback (when CLI unavailable):

```bash
HUB_KC=${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/hub-hybrid-dev.kubeconfig
kubectl --kubeconfig=$HUB_KC get applications -n platform-ops
kubectl --kubeconfig=$HUB_KC get application <app> -n platform-ops -o yaml | yq '.status | {health,sync}'
kubectl --kubeconfig=$HUB_KC get events -n platform-ops --field-selector involvedObject.name=<app>
kubectl --kubeconfig=$HUB_KC rollout status deploy/<name> -n <ns>
# Hard refresh without CLI:
kubectl --kubeconfig=$HUB_KC patch application <app> -n platform-ops --type merge \
  -p '{"metadata":{"annotations":{"argocd.argoproj.io/refresh":"hard"}}}'
```

## Troubleshooting

| Symptom | Cause / Check |
|---|---|
| `stopped after 10 redirects` on `argocd login argocd.dev.nutgraf.in` | Gateway 307 loop — use port-forward path above |
| `argocd-cm not found` / `Application CRD not found` | Passed `--server` without `argocd login` → core mode reads current kube context (`kind-waypoint-local`, `kind-hub-hybrid-dev:127.0.0.1:49934` stale). Fix: `argocd login` then omit `--server`; pin `kubectl --kubeconfig=$HUB_KC` |
| `dial tcp 127.0.0.1:18444: connection refused` | Port-forward not running or wrong port — `jobs`, `ps aux | grep port-forward` |
| `x509: certificate signed by unknown authority` | Missing `--insecure` on port-forward login |
| App `Progressing` / `Degraded` / `OutOfSync` | True app issue — `argocd app get <app> --grpc-web` conditions, then `kubectl describe pod` / `kubectl logs` |

## Anti-Patterns

- `argocd app get --server argocd.dev.nutgraf.in --grpc-web` without prior `login`
- `argocd --core` / relying on `current-context` (ambient state trap, `internal/hub-cli/pivot/orchestrator.go:31`)
- `kubectl --kubeconfig=${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/hub.kubeconfig` (phantom file)
- Omitting `--grpc-web` through port-forward

## Sync Waves

ArgoCD itself is wave 1 (`01-platform-infra-appset.yaml:42`). See `docs/adr/sync-wave-order.md`.
