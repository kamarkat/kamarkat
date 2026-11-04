One important note: the infisical-backend ClusterSecretStore is hardcoded to environmentSlug: "dev" (line 15). Similarly the whole platform is currently only designed to be deployed for dev environment only.
how do we enable stg and prod for the platform? We need the same codebase to support multiple environments like preview (ephimeral), dev, stg, prod.
Can analyze the codebase and do a web research to find how we can enable a widely adopted enterprise standard multi environment hub spoke design.


----------------

To elevate the Zero-Ops platform from a single hardcoded `dev` environment to a widely adopted, enterprise-grade multi-environment architecture (`preview`, `dev`, `stg`, `prod`), we must apply the **"Base + Overlay" GitOps Pattern** combined with **Dynamic AppSet Injection**.

This approach ensures you maintain a **single codebase (DRY)** while explicitly configuring environment-specific variables like the Infisical `environmentSlug`, domains, and resource sizes.

Here is the production-ready architecture and the exact codebase changes required, aligned strictly with **ADR-015 (GitOps Declarative State)** and **ADR-003 (Infisical Secret Management)**.

---

### Phase 1: Hub Plane – Kustomize Overlays
Currently, `manifests/hub-core-services/` contains flat manifests. We need to restructure this into a `base/` and `overlays/` pattern. This allows the Hub to deploy itself as `dev`, `stg`, or `prod`.

**1. Restructure the directories:**
Move your existing manifests into a `base` folder and create `overlays`:
```text
manifests/hub-core-services/
├── base/
│   ├── cluster-secret-store/
│   ├── hub-environment/
│   └── ... (all existing hub-core-services)
└── overlays/
    ├── dev/
    ├── stg/
    └── prod/
```

**2. Create the Production Overlay (`overlays/prod/kustomization.yaml`):**
This overlay targets the `base` and patches the hardcoded `dev` values to `prod`.

```yaml
# manifests/hub-core-services/overlays/prod/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
  - ../../base/cluster-secret-store
  - ../../base/hub-environment

patches:
  # 1. Patch the ClusterSecretStore environmentSlug to "prod"
  - target:
      kind: ClusterSecretStore
      name: infisical-backend
    patch: |-
      - op: replace
        path: /spec/provider/infisical/secretsScope/environmentSlug
        value: "prod"

  # 2. Patch the HubEnvironment CR with Prod values
  - target:
      kind: HubEnvironment
      name: hub-production
    patch: |-
      - op: replace
        path: /spec/domain
        value: "nutgraf.in"
      - op: replace
        path: /spec/secrets/infisical/environmentSlug
        value: "prod"

  # 3. Patch the ConfigMap containing bootstrap config
  - target:
      kind: ConfigMap
      name: hub-bootstrap-config
    patch: |-
      - op: replace
        path: /data/INFISICAL_ENVIRONMENT_SLUG
        value: "prod"
      - op: replace
        path: /data/DOMAIN
        value: "nutgraf.in"
```
*Note: Your ArgoCD Hub Bootstrap Apps (`manifests/argocd/bootstrap/*.yaml`) will simply point to `path: manifests/hub-core-services/overlays/prod` instead of the root directory.*

---

### Phase 2: Spoke Plane – Dynamic AppSet Injection
Spokes are dynamically provisioned by Crossplane on the Hub. You don't want to duplicate your Spoke infrastructure manifests for every environment. Instead, we use **ArgoCD ApplicationSet Template Injection** to dynamically inject the `environmentSlug` into the Spoke's `ClusterSecretStore` at runtime.

**1. Update the Spoke Infrastructure AppSet:**
Modify the Kustomize block in your `spoke-infrastructure` AppSet to dynamically patch the `environmentSlug` using the cluster's labels.

**`manifests/argocd/bootstrap/03-platform-services/spoke-catalog-appsets.yaml`**
```yaml
  template:
    spec:
      source:
        repoURL: https://github.com/kamarkat/kamarkat
        targetRevision: HEAD
        path: manifests/spoke/spoke-catalog/infra
        kustomize:
          patches:
            # (Existing crossplane-admin-credentials patch...)
            
            # ADD THIS NEW PATCH:
            # Dynamically inject the Infisical environmentSlug based on the Spoke cluster's label
            - target:
                kind: ClusterSecretStore
                name: infisical-backend
              patch: |
                - op: replace
                  path: /spec/provider/infisical/secretsScope/environmentSlug
                  # Reads the 'environment' label from the ArgoCD Cluster secret (e.g., "prod", "stg")
                  value: '{{index .metadata.labels "environment"}}'
```

---

### Phase 3: Crossplane Label Propagation (CAPI to ArgoCD)
For the AppSet template `{{index .metadata.labels "environment"}}` to work, ArgoCD must know which environment the Spoke belongs to. We rely on the `capi2argo` controller to copy the `environment` label from the CAPI cluster to the ArgoCD Cluster Secret.

**1. Update the Crossplane Composition to pass the label:**
When Crossplane creates the CAPI cluster, it must attach the `environment` label and the `take-along-label` instruction so `capi2argo` knows to sync it.

**`manifests/hub-core-services/crossplane/tenant-platform/compositions/spokepool-hetzner.yaml`**
```yaml
                    metadata:
                      namespace: platform-capi
                      labels:
                        spoke-type: pool
                        # ADD THESE LABELS:
                        environment: "" # Will be patched from SpokePool XR
                        take-along-label.capi-to-argocd.environment: ""
            patches:
              # Add this patch to pull the environment label from the XR down to CAPI
              - type: FromCompositeFieldPath
                fromFieldPath: metadata.labels['environment']
                toFieldPath: spec.forProvider.manifest.metadata.labels['environment']
```

**2. Label your SpokePool XRs appropriately:**
When you provision a new Spoke, define its environment in the labels.
**`manifests/spoke/spoke-pools/spoke-pool-eu-prod-01/spokepool.yaml`**
```yaml
apiVersion: nutgraf.in/v1alpha1
kind: SpokePool
metadata:
  name: spoke-pool-eu-prod-01
  labels:
    environment: prod  # Must exactly match the Infisical environmentSlug ('prod', 'stg', 'dev')
```

---

### Phase 4: Ephemeral Previews (PR Environments)
To support ephemeral `preview` environments (e.g., spinning up a PaaS instance for a pull request), the enterprise standard avoids full cluster provisioning due to time/cost. Instead, use **vCluster** (Virtual Clusters) deployed onto the `dev` Hub.

1.  **Infisical Setup:** Create a specific `preview` environment in Infisical.
2.  **GitOps CI Flow:** When a PR is opened, a GitHub Action commits a `HubEnvironment` CR and a vCluster definition to a temporary directory in ArgoCD.
3.  **AppSet:** An ArgoCD AppSet targets `overlays/preview` and deploys the control plane into the isolated vCluster, isolating it completely from the rest of the `dev` environment.

### Summary of Architectural Alignment
*   **100% DRY Codebase:** You did not have to duplicate the Spoke catalog or the `ClusterSecretStore` manifests. Kustomize and ArgoCD templating handle the mutation at deployment time.
*   **Security (ADR-003):** Environments are strictly isolated. A `stg` Spoke cluster will inherently only be able to pull `stg` secrets from Infisical because the `ClusterSecretStore` is locked to that slug by the Platform team's AppSet definition, preventing tenant escape.
*   **GitOps Native (ADR-015):** The source of truth for a cluster's environment lives entirely in the `SpokePool` label, propagating down the entire stack automatically.