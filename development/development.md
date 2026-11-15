# Hub boostratp

- docs/onboarding-an-application.md

## Local Testing

rm -f ~/go/bin/kakt /Users/arun_subramanian/Projects/soloz-io/ide/kamarkat/kakt
rm -rf /Users/arun_subramanian/Projects/soloz-io/ide/kamarkat/bin

### Where the names come from
1. The escrow (creates a real project)

go run ./cmd/kakt escrow init --name nutgraf-escrow-3
2. The box

go run ./cmd/kakt box init \
  --tenant nutgraf --git-org nutgraf-inc --domain nutgraf.in --region hel1 \
  --bundle-version development \
  --out /Users/arun_subramanian/Projects/soloz-io/ide/boxes/box2

3. The management cluster

go run ./cmd/kakt tenant add-management \
  --gitops-dir /Users/arun_subramanian/Projects/soloz-io/ide/boxes/box2

4. Day-0, locally

go run ./cmd/kakt bootstrap-mgmt \
  --name <the cluster name step 3 printed> \
  --gitops-dir /Users/arun_subramanian/Projects/soloz-io/ide/boxes/box2 \
  --provider hetzner --region hel1
  
# Provision workers

## Dell Hub VM
./scripts/hybrid/provision-flatcar-worker.sh --cluster hub
./scripts/hybrid/provision-flatcar-worker.sh --node 1

## Dell Spoke VM
./scripts/hybrid/provision-flatcar-worker.sh --cluster spoke
./scripts/hybrid/provision-flatcar-worker.sh --node 2