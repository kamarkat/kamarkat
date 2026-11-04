# Universal Tenant Helm Chart

Helm chart for provisioning tenant resources in Spoke Pool clusters.

## Overview

This chart generates all necessary Kubernetes resources for a tenant:
- Namespace with labels
- RBAC (ServiceAccount, Role, RoleBinding)
- ResourceQuota based on tier
- AtlasMigration CR for schema provisioning
- ConfigMap with migration files

## Usage

### Install via ArgoCD ApplicationSet

The chart is automatically deployed via ArgoCD ApplicationSet when a tenant values file is added to `fleet-registry/tenants/{tenant-id}/values.yaml`.

### Manual Installation (Testing)

```bash
helm install tenant-acme charts/universal-tenant \
  --set tenantId=acme \
  --set tier=starter \
  --set region=fsn1
```

### Values Schema

| Parameter | Description | Required | Default |
|-----------|-------------|----------|---------|
| `tenantId` | Tenant identifier | Yes | `""` |
| `tier` | Tenant tier (starter/enterprise) | Yes | `starter` |
| `region` | Region for resources | Yes | `fsn1` |
| `database.schemaName` | PostgreSQL schema name | No | `tenant_{tenantId}` |
| `database.migrations.gitRepo` | Git repo with migrations | No | `https://github.com/kamarkat/kamarkat` |
| `database.migrations.gitRevision` | Git branch/tag | No | `main` |
| `database.migrations.gitPath` | Path to migrations | No | `migrations/tenant-baseline` |
| `resourceQuota.cpu` | CPU limit | No | `1000m` |
| `resourceQuota.memory` | Memory limit | No | `2Gi` |
| `resourceQuota.storage` | Storage limit | No | `10Gi` |
| `resourceQuota.pods` | Max pods | No | `10` |

## Generated Resources

### Sync Wave 0 (Prerequisites)
- Namespace: `tenant-{tenantId}`
- ServiceAccount: `tenant-{tenantId}`
- Role: `tenant-{tenantId}-role`
- RoleBinding: `tenant-{tenantId}-binding`
- ResourceQuota: `tenant-{tenantId}-quota`

### Sync Wave 2 (Schema Provisioning)
- ConfigMap: `tenant-{tenantId}-migrations` (contains SQL files)
- AtlasMigration CR: `tenant-{tenantId}` (applies migrations)

## AtlasMigration CR

The chart generates an AtlasMigration CR that:
- Connects to shared CNPG cluster via PgBouncer
- Reads migrations from ConfigMap
- Applies migrations with template variable substitution (`{{.tenant_id}}`)
- Reports status via `status.conditions[Ready]`

## Example Values File

```yaml
tenantId: acme
tier: starter
region: fsn1
database:
  schemaName: tenant_acme
  migrations:
    gitRepo: https://github.com/kamarkat/kamarkat
    gitRevision: main
    gitPath: migrations/tenant-baseline
resourceQuota:
  cpu: "2000m"
  memory: "4Gi"
  storage: "20Gi"
  pods: "20"
```
