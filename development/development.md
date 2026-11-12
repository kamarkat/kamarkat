# Hub boostratp

- zero-ops/docs/onboarding-an-application.md

## Local Testing
### A rule of thumb: 
#### CLI change → 
./scripts/dev/local-e2e.sh 0.1.16-rc.60 clean cli scaffold bootstrap
./scripts/dev/local-e2e.sh 0.1.16-rc.60 cli bootstrap
#### manifest change → 
DOMAIN=nutgraf.in make e2e-fresh VERSION=0.1.16-rc.60
make e2e-fresh VERSION=0.1.16-rc.60    # clean → publish → cli → scaffold → bootstrap
make e2e-clean VERSION=0.1.16-rc.60    # tear the last run down and stop
make e2e       VERSION=0.1.16-rc.60    # as before — cannot reach clean

#### PROVIDER=hybrid

make cli-release VERSION=0.1.16-rc.53

DOMAIN=nutgraf.in PROVIDER=hybrid ./scripts/dev/local-e2e.sh 0.1.16-rc.60 publish

DOMAIN=nutgraf.in PROVIDER=hybrid REGION=fsn1 make e2e-fresh VERSION=0.1.16-rc.60

DOMAIN=nutgraf.in PROVIDER=hybrid make e2e-fresh VERSION=0.1.16-rc.60

PROVIDER=hybrid make e2e-workload VERSION=0.1.16-rc.60

PROVIDER=hybrid ./scripts/dev/local-e2e.sh 0.1.16-rc.60 cli bootstrap

./bin/kakt reseed --kubeconfig .local-e2e/acme-gitops/k8-secrets/kubeconfig/acme-hub.kubeconfig \
                   --bundle-version 0.1.16-rc.60

DOMAIN=nutgraf.in PROVIDER=hybrid ./scripts/dev/local-e2e.sh 0.1.16-rc.60 \
  cli bootstrap workload verify adr

#### Chart Change
1. Publish the fix as rc.4 — this is a chart change, so it needs a new version:
cd /Users/arun_subramanian/Projects/soloz-io/ide/zero-ops
export PATH="$HOME/.local/helm3:$PATH"
make publish-local VERSION=0.1.16-rc.60
2. Promote the running cluster — one edit, both fields, exactly as Renovate would:
cd .local-e2e/acme-gitops
sed -i '' 's/0\.1\.16-rc\.40/0.1.16-rc.60/g' clusters/acme-hub/bundle.yaml clusters/acme-hub/values.yaml 
git commit -am "promote acme-hub to 0.1.16-rc.60" && git push
cd ../..
3. Resume the bootstrap once it's Synced, to finish adr045-commit and what follows:
./scripts/dev/local-e2e.sh 0.1.16-rc.60 bootstrap
It skips provisioning (the cluster reports Provisioned) and picks up where it stopped.

#### Promote new chart at tenant box

Update targetRevision and bundleVersion value in below,
- registry/clusters/nutgraf-01/bundle.yaml 
- registry/clusters/nutgraf-hub/bundle.yaml

KUBECONFIG=.local-e2e/nutgraf-gitops/k8-secrets/kubeconfig/nutgraf-hub.kubeconfig \
  scripts/argocd-hard-sync.sh --promote 0.1.16-rc.112

#### MAC DNS cache clearance

- ! sudo networksetup -setdnsservers Wi-Fi 1.1.1.1 8.8.8.8   

## Tenant repo testing
### Once, on a branch you've pushed (ArgoCD reads git, not your working tree)
./kakt box init --tenant local --box soloz-io --domain local.nutgraf.in \
  --bundle-version development
cd local-gitops-* && ../kakt tenant add-management

https://app.infisical.com/organizations/89f6d208-ea9b-4b6d-b4bd-a249f50ead8d/identities/671e356d-76ad-4886-8357-635abf19d435

### box init mints the box's three names, creates + pushes the repo with NO
### clusters, sets the secrets and leaves a checkout; add-management declares the
### first cluster in it (no --name), which you then commit and push yourself.
### A run that stops part-way: kakt box init --resume <checkout>
cd local-gitops

export HCLOUD_TOKEN=$(cat /Users/arun_subramanian/Projects/soloz-io/ide/zero-ops/k8-secrets/hetzner/token)

/Users/arun_subramanian/Projects/soloz-io/ide/zero-ops/kakt bootstrap --name local-hub --provider hetzner --region hel1 \
  --gitops-dir .

## Cluster Teardown

./kakt teardown --name test-hub --confirm

## Fresh Bootstrap

./kakt bootstrap \
  --name devbox \
  --provider hetzner \
  --region hel1 \
  --on-prem \
  --tailnet-name taila4c44b.ts.net \
  --debug

## Release

- gh release create v0.1.14 --generate-notes
- gh run watch

- ./bin/kakt reseed --kubeconfig k8-secrets/kubeconfig/hub-hybrid-dev.kubeconfig \
  --bundle-version 0.1.14     

- KUBECONFIG=k8-secrets/kubeconfig/hub-hybrid-dev.kubeconfig \                   
  kubectl get app platform-infisical-db -n platform-ops \
  -o jsonpath='{.spec.source.helm.values}{"\n"}'

### What the run should do:

1. Package 33 components, 5 provider charts, 8 tenant + catalogue charts, and the bundle chart — 42 total
2. Refuse if any portability reference reappears (passes on main right now)
3. Assert each chart renders exactly what ArgoCD applies today — 28 verified identical locally
4. Build four CLI binaries with the version injected, and fail if the built CLI doesn't report 0.1.0
5. Push to oci://ghcr.io/kamarkat/charts and attach the binaries to the release

- Make the GHCR packages public — ADR-063 requires a tenant to mirror without asking permission, and ArgoCD has no OCI credential.

## Download the released CLI

gh release download v0.1.15 --pattern 'kakt-darwin-arm64' --output kakt --clobber
chmod +x kakt

## Scaffold a real tenant repo 

cd /Users/arun_subramanian/Projects/kamarkat/kamarkat
./kakt box init --tenant test-tenant --box soloz-io --domain test-tenant.nutgraf.in

Or from your current directory:

/Users/arun_subramanian/Projects/kamarkat/kamarkat/kakt box init --tenant test-tenant --box soloz-io --domain test-tenant.nutgraf.in

### Interactive

kakt box init --tenant test-tenant --box soloz-io --domain test-tenant.nutgraf.in

Asks for each credential it does not have (no echo, a browser on the page that
issues it), then creates the repo and sets them on it. Declaring the management
cluster and dispatching the bootstrap are your next two steps.

### Non-interactive:

kakt box init --no-prompt --tenant tenant1 --box soloz-io --domain tenant1.nutgraf.in \
  --provider-token "$HCLOUD_TOKEN" --gitops-token "$GITOPS_PAT"

No secrets → nothing is created, and the handover instructions say what is
missing. The repository is created only once the set is complete, so a re-run
after supplying the rest is the whole recovery.

## Delete

gh repo delete soloz-io/test-tenant-gitops --yes

## Boundary activation

Which boundaries are open (activation is not reported as ArgoCD drift — a closed
boundary looks idle, not failed):

kubectl get appproject -n platform-ops -o custom-columns=\
NAME:.metadata.name,INACTIVE:.spec.syncWindows | grep boundary-

Open one by hand if a run was interrupted:

kubectl patch appproject boundary-03 -n platform-ops \
  --type merge -p '{"spec":{"syncWindows":null}}'

# Provision workers

## Dell Hub VM
./scripts/hybrid/provision-flatcar-worker.sh --cluster hub
./scripts/hybrid/provision-flatcar-worker.sh --node 1

## Dell Spoke VM
./scripts/hybrid/provision-flatcar-worker.sh --cluster spoke
./scripts/hybrid/provision-flatcar-worker.sh --node 2