# Runbook: Bootstrap, Binaries and Teardown

Operational detail extracted from the README, which now describes what the
platform is rather than how to drive it. Commands here were written against the
tree as it stood on 2026-09-07 and are not re-verified by this move.

## Binaries

### 1. Hub (`hub`)
Bootstrap and manage Hub (Management) Clusters on Hetzner Cloud using Cluster API.

**Build:**
```bash
make build-hub
# or
go build -o bin/kakt ./cmd/hub
```

**Usage:**
```bash
# Step 1: Bootstrap Hub Cluster with Ubuntu (default, production-ready)

# Legacy way
# export HCLOUD_TOKEN=$(cat k8-secrets/hetzner/token)
# ./bin/kakt bootstrap \
#   --name=hub \
#   --region=hel1 \
#   --debug 2>&1 | tee .zero-ops/bootstrap-hub.log

# New way
./scripts/hub-bootstrap.sh

# There is no step 2. Infisical encryption-key recovery used to go through AWS
# Secrets Manager and an IAM user; it is now the Infisical-based escrow, which
# bootstrap installs itself before hub-operator starts reconciling. No AWS
# profile, no IAM user, no command to run.

# Step 3: Configure GitHub Access (Secret Zero)
# This enables ArgoCD to sync manifests from ALL repositories in the soloz-io organization
# Creates organization-wide credentials (repo-creds) for:
# - zero-ops repository (platform manifests)
# - fleet-registry repository (tenant configurations)
# - Any other repositories in the soloz-io organization
# CRITICAL: Must run BEFORE init-secrets so platform-data namespace exists
export GITHUB_TOKEN=$(cat k8-secrets/github/github-pat-token)
./bin/kakt configure-github-access \
  --ghcr-pat=$GITHUB_TOKEN \
  --kubeconfig=k8-secrets/kubeconfig/hub.kubeconfig

# Step 4: Wait for ArgoCD to sync and create namespaces
# ArgoCD will create platform-data, platform-security, and other namespaces
# Note: We must poll until the namespace exists, as kubectl wait fails if it is missing.
until kubectl get namespace platform-data --kubeconfig=k8-secrets/kubeconfig/hub.kubeconfig >/dev/null 2>&1; do
  echo "Waiting for ArgoCD to create platform-data namespace..."
  sleep 5
done

kubectl wait --for=jsonpath='{.status.phase}'=Active namespace/platform-data \
  --timeout=60s --kubeconfig=k8-secrets/kubeconfig/hub.kubeconfig

# Step 5: Initialize bootstrap secrets (Secret Zero)
# This generates CA certificate, Infisical master keys, and backs them up to AWS
# TLS is enabled from Day 0 - no upgrade step needed
# Step 3.5 automatically bootstraps Infisical (Org, Project, Machine Identity)
# No port-forward needed - the CLI uses pod exec + REST API internally
INFISICAL_API_URL=http://localhost:8080 ./bin/kakt init-secrets --kubeconfig=k8-secrets/kubeconfig/hub.kubeconfig

# Step 6: Wait for ArgoCD to sync and deploy database
kubectl wait --for=condition=ready pod -l cnpg.io/cluster=platform-db \
  -n platform-data --timeout=600s \
  --kubeconfig=k8-secrets/kubeconfig/hub.kubeconfig

# Steps 7-8 (former manual UI steps) are now automated in init-secrets Step 3.5
# The deprecated `hub configure-eso` has been replaced by automatic bootstrapping.

# Teardown Hub cluster
./bin/kakt teardown --name=hub

# Only use if local kind cluster does not exist and already pivoted
export HCLOUD_TOKEN=$(cat k8-secrets/hetzner/token) && ./bin/kakt teardown --name=hub --force --confirm 2>&1 | tee .zero-ops/teardown-hub.log

# Teardown spoke clusters (independent of hub)
export HCLOUD_TOKEN=$(cat k8-secrets/hetzner/token) && ./bin/kakt spoke teardown --force 2>&1 | tee .zero-ops/teardown-spoke.log

# Teardown specific spoke cluster
export HCLOUD_TOKEN=$(cat k8-secrets/hetzner/token) && ./bin/kakt spoke teardown --name=spoke-pool-eu-prod-01 --force 2>&1 | tee .zero-ops/teardown-spoke.log

```

**Command Execution Order (CRITICAL):**

1. **`kakt bootstrap-mgmt`** - Creates Kubernetes cluster and deploys ArgoCD
2. **`kakt configure-github-access`** - Injects GitHub credentials (enables ArgoCD sync and namespace creation)
3. **Wait for namespaces** - ArgoCD creates platform-data, platform-security, etc.
4. **`kakt init-secrets`** - Generates CA, Infisical keys, AND bootstraps Infisical (Org/Project/Machine Identity)
5. **Wait for Database** - ArgoCD syncs and deploys PostgreSQL cluster with TLS

**Why this order matters:**
- GitHub credentials must be injected BEFORE `init-secrets` (ArgoCD needs to create platform-data namespace)
- `init-secrets` requires platform-data namespace to exist (created by ArgoCD sync)
- `init-secrets` generates CA certificate offline and injects it before CNPG starts (Day-0 Deterministic Injection)
- Step 3.5 of `init-secrets` auto-creates the Infisical Machine Identity (replaces `configure-eso`)
- CNPG uses the CLI-generated CA (via spec.certificates.serverCASecret)
- Infisical uses the same CA for TLS verification (via DB_ROOT_CERT)
- Both CNPG and Infisical start with TLS enabled on first boot - no restart loops
- `init-secrets` also creates the `infisical-auth` Secret and patches `hub-bootstrap-config`
- Escrow for disaster recovery is installed by bootstrap itself, before hub-operator
  starts reconciling: a Secret that appears later means every backup until then was
  skipped, including the one covering the window where the box is least recoverable
- If you skip `configure-github-access`, ArgoCD cannot sync and namespaces won't be created
- GitHub credentials are organization-scoped (repo-creds type) to support multiple repositories (zero-ops, fleet-registry, etc.)

### 2. OpenSBT (`opensbt`)
SaaS Builder Toolkit control plane for multi-tenant application management.

**Build:**
```bash
make build-opensbt
```

**Usage:**
```bash
# Set environment variables
export KRATOS_PUBLIC_URL=http://kratos-public:4433
export HYDRA_PUBLIC_URL=http://hydra-public:4444
export NATS_URLS=nats://nats:4222
export DATABASE_URL=postgres://postgres:postgres@localhost:5432/opensbt

./bin/opensbt
```

### 3. Tenant API (`zero-ops-api`)
REST API for tenant lifecycle management with PostgreSQL backend.

**Build:**
```bash
make build-api
```

**Usage:**
```bash
# Start API with Docker Compose
cd cmd/zero-ops-api
docker-compose up -d

# Create a tenant
curl -X POST http://localhost:8080/api/v1/tenants \
  -H "Content-Type: application/json" \
  -d '{
    "name": "acme-corp",
    "email": "admin@acme.com",
    "plan": "professional"
  }'
```

### 4. Auth Proxy (`auth-proxy`)
OAuth2/JWT authentication proxy for Ory Hydra/Kratos integration.

**Build:**
```bash
make build-auth-proxy
```

### 5. MCP Server (`mcp-server`)
Model Context Protocol server for AI agent integration.

**Build:**
```bash
make build-mcp-server
```

## Development

**Build all binaries:**
```bash
make build-all
```

**Build specific binary:**
```bash
make build-hub
make build-opensbt
make build-auth-proxy
make build-mcp-server
```

**Run tests:**
```bash
make test
```

**Docker builds:**
```bash
docker build -f cmd/hub/Dockerfile -t zero-ops/hub:latest .
docker build -f cmd/opensbt/Dockerfile -t zero-ops/opensbt:latest .
docker build -f cmd/auth-proxy/Dockerfile -t zero-ops/auth-proxy:latest .
docker build -f cmd/mcp-server/Dockerfile -t zero-ops/mcp-server:latest .
```

## OS Support (Hub Cluster)

Hub supports Ubuntu only:

**Ubuntu (default):** Production-ready with ClusterClass support. Uses KubeadmControlPlane for scalable cluster topology management. Immutable OS with atomic updates, no Packer build required.

## State Management

Hub bootstrap state is tracked in `.zero-ops/state/<cluster-name>.json`. Bootstrap logs are saved to `.zero-ops/bootstrap-<cluster-name>.log`. To retry a failed bootstrap or start fresh:

```bash
# Clear state for specific cluster
rm -f .zero-ops/state/<cluster-name>.json

# Example: clear state for 'kakt' cluster
rm -f .zero-ops/state/hub.json

# View bootstrap logs
cat .zero-ops/bootstrap-hub.log
```

