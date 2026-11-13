# Spoke Bootstrap (Secret Zero)

This directory contains manifests that are injected into Spoke Pool clusters at bootstrap time via ClusterResourceSet.

## Purpose

**Delivery Mechanism**: CAPI ClusterResourceSet (injected before ArgoCD Agent exists)  
**Timing**: Phase 1 - Cluster BIOS (Bootstrap)  
**Contents**: Minimal components needed to enable GitOps

## Why Separate from Spoke Catalog?

These components MUST be injected via ClusterResourceSet because:
1. **ArgoCD Agent doesn't exist yet** (chicken-and-egg problem)
2. **Without CNI, pods can't communicate** (networking prerequisite)
3. **Without CCM, LoadBalancer services don't work** (cloud integration prerequisite)

Once these bootstrap components are running, the ArgoCD Agent can pull the rest of the platform components from the Spoke Catalog.

## Components

### 1. ArgoCD Agent
**File**: `argocd-agent-templates.yaml`  
**Purpose**: Enables GitOps-driven management from Hub  
**Image**: `quay.io/argoprojlabs/argocd-agent:v0.11.0` (pinned by digest in `argocd-agent-templates.yaml`; the hub-side principal is pinned to the same build)

The ArgoCD Agent ConfigMap (`argocd-agent-config`) is dynamically generated per-cluster by the Crossplane Composition.

### 2. Cilium CNI
**File**: `cilium-addon-template.yaml`  
**Purpose**: Container networking and network policies  
**Mode**: Direct routing (no overlay)

### 3. Hetzner Cloud Controller Manager
**File**: `ccm-addon-template.yaml`  
**Purpose**: Cloud provider integration (LoadBalancer, node metadata)

## Bootstrap Flow

1. CAPI provisions Hetzner VMs
2. ClusterResourceSet injects 5 resources when cluster reaches Provisioned state:
   - ArgoCD Agent Deployment (ConfigMap)
   - ArgoCD Agent Config (ConfigMap)
   - ArgoCD Agent RBAC (ConfigMap)
   - mTLS Client Certificate (Secret)
   - CA Certificate (Secret)
3. Cilium CNI starts (enables pod networking)
4. Hetzner CCM starts (enables LoadBalancer services)
5. ArgoCD Agent starts within 2 minutes
6. Agent connects to Hub using mTLS authentication
7. Agent pulls Applications from Hub (Spoke Catalog deployment begins)

## Security

- mTLS authentication using cert-manager generated certificates
- ServiceAccount with ClusterRole limited to agent's own cluster
- No cross-cluster access (NFR-4.4)

## Requirements

- FR-1.2: Automated Cluster Bootstrap
- AC-3: ArgoCD Agent Bootstrap
- NFR-4.1: mTLS authentication
- NFR-4.4: RBAC limited to own cluster

## Related Files

- **Spoke Catalog**: `manifests/spoke-catalog/` (deployed via ArgoCD after bootstrap)
- **Certificates**: `manifests/hub-core-services/security/`
- **Composition**: `xrds/compositions/spokepool-hetzner.yaml`
- **ADR**: `docs/adr/0001-clusterresourceset-addon-template-management.md`
