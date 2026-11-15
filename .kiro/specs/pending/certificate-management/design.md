### 1. ADR Documentation Update

**File: `docs/adr/035-enterprise-pki-and-delegated-trust-via-infisical-oss.md`**
# Decision

## Trust Hierarchy
The platform implements a three-tier trust hierarchy:
1. Offline Root CA
2. Fleet Intermediate CA
3. Leaf Certificates

Infisical OSS cannot enforce CA-scoped authorization boundaries for Machine Identities. Therefore Intermediate CA delegation is not treated as a security boundary. The platform uses a single Fleet Intermediate CA.

### Offline Root CA
The Offline Root CA is generated outside Infisical, stored offline, never connected to production systems, and signs the Fleet Intermediate CA. The Offline Root private key is never stored in Kubernetes.

### Fleet Intermediate CA
The Fleet Intermediate CA is hosted within Infisical PKI, is the sole issuing authority for platform certificates, and signs all Spoke-issued certificates. The Fleet Intermediate private key never leaves Infisical.

### Leaf Certificates
Leaf certificates are issued through cert-manager, infisical-issuer, and Infisical PKI. All certificates chain through: Offline Root CA -> Fleet Intermediate CA -> Leaf Certificate.

# Certificate Profiles
All certificate issuance SHALL occur through centrally managed certificate profiles (not by CA ID). Profiles define the issuance boundary: Machine Identity → Project-level PKI permissions → Certificate Profile (profileId) → Fleet Intermediate CA. The Hub Operator resolves the bootstrap profile slug to UUID at runtime via GET /api/v1/cert-manager/certificate-profiles/slug/:slug.

# Certificate Issuance Model
Each Spoke cluster deploys cert-manager and infisical-issuer. Leaf certificates are requested locally through the Infisical /api/v1/cert-manager/* API family (not deprecated /api/v1/pki/* endpoints). The Fleet Intermediate CA signs the certificate. The private key is generated and stored locally in the Spoke cluster. Crossplane is never involved.

# Bootstrap Exception
ADR-025 requires deterministic cluster bootstrap. The ArgoCD Agent must establish an mTLS connection before GitOps becomes available.

## Bootstrap Certificate
The Hub Operator SHALL issue a temporary bootstrap certificate.
Characteristics:
* TTL: 72 hours
* Single purpose
* ArgoCD Agent only
* Delivered through ClusterResourceSet
* Replaced after GitOps initialization

## Bootstrap Private Key Handling
The Hub Operator temporarily receives bootstrap certificate private key material during Day-0 provisioning. This is a controlled exception — the private key is received in-memory from the POST /api/v1/cert-manager/certificates/ API response, embedded directly into a ClusterResourceSet payload, and never stored in Hub-side persistent storage. After the Spoke receives the payload, the private key exists only on the Spoke cluster and cert-manager takes ownership. This exception is limited exclusively to the 72-hour ArgoCD bootstrap certificate and does not extend to Day-1+ lifecycle operations.

# Trust Distribution
The Offline Root CA public certificate is injected during Hub bootstrap. No per-Spoke trust bundles exist. No Intermediate CA bundles are distributed.

# Hub Operator Responsibilities
The Hub Operator participates only in Day-0 bootstrap to create the Machine Identity, generate the bootstrap certificate, and store bootstrap artifacts in ClusterResourceSet payloads. The Hub Operator SHALL NOT rotate, renew, reconcile, or distribute leaf certificates.

# Crossplane Responsibilities
Crossplane SHALL NOT generate, copy, read, or traverse certificate or private key material.

# Identity Separation
Infrastructure identity (cert-manager, infisical-issuer) and workload identity (SPIRE) remain separated. SPIRE SHALL NOT be used for infrastructure certificates.

# PKI Profiles
Certificate validity is governed centrally.
| Profile | TTL |
|---|---|
| Infrastructure Services | 24h |
| Database Clients | 4h |
| Service Mesh Components | 1h |
| Human Access | 15m |
| Bootstrap Certificate | 72h |

# Revocation Strategy
The platform does not implement CRLs or OCSP. Compromise mitigation relies on short certificate TTLs, Machine Identity rotation, Fleet Intermediate replacement, and Trust anchor replacement when required.

# Infisical OSS Authorization Limitation
Infisical OSS does not provide Certificate Authority–scoped authorization controls for Machine Identities. PKI permissions are effectively project-scoped.
Accordingly, the platform adopts a single Fleet Intermediate CA model and does not rely on Intermediate CA separation as a security boundary.
Security isolation is enforced through:
- Machine Identity protection
- Short-lived certificates
- Audit logging
- SPIRE workload identities
- Kubernetes namespace and cluster boundaries
rather than delegated Intermediate CA ownership.
```

### 2. Delete Crossplane Secret Traversal Anti-Pattern

Execute the following commands to eradicate the Crossplane function:
```bash
rm -rf operators/function-cert-distribution/
rm manifests/hub-core-services/crossplane/providers/function/function-cert-distribution.yaml
```

**File: `manifests/hub-core-services/crossplane/providers/kustomization.yaml`**
```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - provider-kubernetes/provider.yaml
  - function/function.yaml
```

**File: `manifests/hub-core-services/crossplane/tenant-platform/compositions/spokepool-hetzner.yaml`**
```yaml
# DELETE the entire `- step: distribute-certificates` pipeline block.
# DELETE the following base objects from the `- step: patch-and-transform` block:
# - name: argocd-agent-client-cert
# - name: alloy-client-cert
# - name: nats-leafnode-client-cert
# - name: alloy-client-cert-distribution
# - name: nats-leafnode-cert-distribution
# - name: argocd-agent-cert-distribution
# - name: infisical-auth-distribution

# UPDATE the `cluster-resource-set` resource to dynamically map the separate CRS components:
          - name: cluster-resource-set
            base:
              apiVersion: kubernetes.crossplane.io/v1alpha2
              kind: Object
              spec:
                managementPolicies: ["*"]
                forProvider:
                  manifest:
                    apiVersion: addons.cluster.x-k8s.io/v1beta1
                    kind: ClusterResourceSet
                    metadata:
                      namespace: platform-capi
                    spec:
                      clusterSelector:
                        matchLabels:
                          spoke-type: pool
                      resources:
                        - name: offline-root-ca-crs
                          kind: ConfigMap
                        - name: "" # Patched to <spokeName>-machine-identity-crs
                          kind: Secret
                        - name: "" # Patched to <spokeName>-bootstrap-cert-crs
                          kind: Secret
                        - name: cilium-addon-template
                          kind: Secret
                        - name: ccm-addon-template
                          kind: Secret
                        - name: csi-addon-template
                          kind: Secret
                        - name: argocd-crds-template
                          kind: Secret
                        - name: argocd-redis
                          kind: ConfigMap
                        - name: argocd-core-components
                          kind: ConfigMap
                        - name: argocd-agent-deployment
                          kind: ConfigMap
                        - name: "" # Patched to <spokeName>-argocd-agent-params
                          kind: ConfigMap
                        - name: argocd-agent-rbac
                          kind: ConfigMap
                      # NOTE: Must use "Once" not "Reconcile". "Reconcile" causes CAPI to fight with
                      # cert-manager — CRS restores the original bootstrap payload, cert-manager
                      # overwrites it with the renewed cert, creating an infinite loop.
                      strategy: Once
                providerConfigRef:
                  name: kubernetes-provider
            patches:
              - type: FromCompositeFieldPath
                fromFieldPath: metadata.name
                toFieldPath: metadata.name
                transforms:
                  - type: string
                    string:
                      type: Format
                      fmt: "%s-bootstrap"
              - type: FromCompositeFieldPath
                fromFieldPath: metadata.name
                toFieldPath: spec.forProvider.manifest.metadata.name
                transforms:
                  - type: string
                    string:
                      type: Format
                      fmt: "%s-bootstrap"
              - type: FromCompositeFieldPath
                fromFieldPath: metadata.name
                toFieldPath: spec.forProvider.manifest.spec.clusterSelector.matchLabels.cell-id
              - type: FromCompositeFieldPath
                fromFieldPath: metadata.name
                toFieldPath: spec.forProvider.manifest.spec.resources[1].name
                transforms:
                  - type: string
                    string:
                      type: Format
                      fmt: "%s-machine-identity-crs"
              - type: FromCompositeFieldPath
                fromFieldPath: metadata.name
                toFieldPath: spec.forProvider.manifest.spec.resources[2].name
                transforms:
                  - type: string
                    string:
                      type: Format
                      fmt: "%s-bootstrap-cert-crs"
              - type: FromCompositeFieldPath
                fromFieldPath: metadata.name
                toFieldPath: spec.forProvider.manifest.spec.resources[10].name
                transforms:
                  - type: string
                    string:
                      type: Format
                      fmt: "%s-argocd-agent-params"
```

**Files to DELETE:**
*   `manifests/hub-core-services/security/infisical-auth-crs-transform.yaml`
*   `manifests/hub-core-services/security/argocd-agent-cert.yaml`
*   `manifests/hub-core-services/security/nats-leafnode-cert.yaml`
*   `manifests/hub-core-services/security/observability-cert.yaml`

### 3. Static Root CA Distribution (Day-0)

**Important — Import existing Offline Root public certificate.**
Never generate Root CA during hub bootstrap. Never store Root private key in the platform.

The Offline Root CA must already exist (generated and stored offline by the PKI operator). Hub bootstrap imports only the public certificate for trust distribution.

**Note:** The Offline Root CA public certificate is public information. A ConfigMap communicates intent more clearly than a Secret and avoids unnecessary RBAC restrictions. Use ConfigMap, not Secret.

**File: `manifests/platform-capi/offline-root-ca-crs.yaml`**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: offline-root-ca-crs
  namespace: platform-capi
  labels:
    addons.cluster.x-k8s.io/resource-set: "true"
binaryData:
  root-ca.yaml: |
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: offline-root-ca
      namespace: argocd
      labels:
        platform.nutgraf.in/trust-anchor: "true"
    data:
      ca.crt: "PLACEHOLDER_IMPORTED_DURING_HUB_BOOTSTRAP"
```

### 4. Configuration Updates

**File: `manifests/hub-core-services/hub-environment/hub-bootstrap-config.yaml`**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: hub-bootstrap-config
  namespace: platform-ops
  annotations:
    argocd.argoproj.io/sync-wave: "0"
data:
  CLUSTER_ID: "hub-production"
  CLUSTER_REGION: "ap-south-1"
  SECRET_STORE_PROJECT_SLUG: "hub-platform"
  SECRET_STORE_SECRETS_PROJECT_SLUG: "hub-secrets"
  SECRET_STORE_ENVIRONMENT_SLUG: "dev"
  DOMAIN: "nutgraf.in"
  SECRET_STORE_BOOTSTRAP_PROFILE_SLUG: "argocd-bootstrap"
```

**File: `operators/hub-operator/config/manager/manager.yaml`**
```yaml
# Under the container env section:
        - name: SECRET_STORE_BOOTSTRAP_PROFILE_SLUG
          valueFrom:
            configMapKeyRef:
              name: hub-bootstrap-config
              key: SECRET_STORE_BOOTSTRAP_PROFILE_SLUG
```

### 5. Hub Operator Implementation

**File: `operators/hub-operator/internal/secrets/infisical.go`**
```go
package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// NOTE: The Infisical API expects the request body at:
// POST /api/v1/cert-manager/certificates/
// With profileId at the top level and certificate attributes nested under "attributes".
// Verified against backend/src/server/routes/v1/certificate-router.ts:127-173

type IssueCertRequestAttributes struct {
	CommonName string `json:"commonName,omitempty"`
	TTL        string `json:"ttl,omitempty"`
}

type IssueCertRequest struct {
	ProfileId    string                      `json:"profileId"`
	Attributes   *IssueCertRequestAttributes `json:"attributes,omitempty"`
}

type IssueCertResponseWrapper struct {
	Certificate          *IssueCertData `json:"certificate"`
	CertificateRequestId string         `json:"certificateRequestId"`
	Status               string         `json:"status"`
	Message              string         `json:"message,omitempty"`
}

type IssueCertData struct {
	Certificate          string `json:"certificate"`
	IssuingCaCertificate string `json:"issuingCaCertificate"`
	CertificateChain     string `json:"certificateChain"`
	PrivateKey           string `json:"privateKey"`
	SerialNumber         string `json:"serialNumber"`
	CertificateId        string `json:"certificateId"`
}

// IssueBootstrapCertificate mints a 72-hour cert for ArgoCD Agent bootstrap.
// The Machine Identity must already have project-level PKI permissions (via grantProjectAccess).
func (c *InfisicalClient) IssueBootstrapCertificate(ctx context.Context, profileId, commonName string) (*IssueCertData, error) {
	reqBody := IssueCertRequest{
		ProfileId: profileId,
		Attributes: &IssueCertRequestAttributes{
			CommonName: commonName,
			TTL:        "72h",
		},
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/api/v1/cert-manager/certificates", c.BaseURL),
		bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to issue bootstrap cert: status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var result IssueCertResponseWrapper
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if result.Certificate == nil {
		return nil, fmt.Errorf("bootstrap certificate issuance returned nil (likely async/order flow for external CA): %s", result.Message)
	}

	return result.Certificate, nil
}
```

**File: `operators/hub-operator/internal/controller/spokepool_controller.go`**
```go
package controller

import (
	// ... standard imports ...
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Reconcile handles the creation of the Spoke's bootstrap payloads
func (r *SpokePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// ... fetch SpokePool CR ...

	bootstrapProfileSlug := os.Getenv("SECRET_STORE_BOOTSTRAP_PROFILE_SLUG")
	if bootstrapProfileSlug == "" {
		return ctrl.Result{}, fmt.Errorf("SECRET_STORE_BOOTSTRAP_PROFILE_SLUG is not set")
	}

	// Resolve profile slug to ID (slugs survive backup/restore, UUIDs do not)
	// IMPLEMENT: ProfileSlugToID must call GET /api/v1/cert-manager/certificate-profiles/slug/:slug
	// and return the profile.id (UUID). This endpoint is confirmed in the Infisical OSS codebase
	// at backend/src/server/routes/v1/certificate-profiles-router.ts:496.
	pkiProfileId, err := r.Infisical.ProfileSlugToID(ctx, bootstrapProfileSlug)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve bootstrap profile: %w", err)
	}

	// 1. Generate Machine Identity (leveraging existing /api/v1/identities + UA auth flow)
	machineIdentity, err := r.Infisical.GetOrCreateSpokeMachineIdentity(ctx, spoke.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 1a. Grant project-level PKI access to the Machine Identity
	// Without this, the identity's API calls will get 403 Forbidden.
	// Use the highest PKI role ("admin" or custom PKI role) since the identity
	// needs to issue certificates under the project's PKI profiles.
	if err := r.Infisical.grantProjectAccess(ctx, machineIdentity.ID, "admin"); err != nil {
		return ctrl.Result{}, fmt.Errorf("grant project access to machine identity: %w", err)
	}

	// 2. Mint 72h Bootstrap Certificate (ADR-035 Exception)
	commonName := fmt.Sprintf("argocd-agent:%s", spoke.Name)
	cert, err := r.Infisical.IssueBootstrapCertificate(ctx, pkiProfileId, commonName)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 3. Construct Separate CRS Secrets
	identityYAML := fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: infisical-auth
  namespace: platform-ops
type: Opaque
stringData:
  client-id: %s
  client-secret: %s
`, machineIdentity.ClientID, machineIdentity.ClientSecret)

	certYAML := fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: argocd-agent-client-cert
  namespace: argocd
  labels:
    platform.nutgraf.in/bootstrap: "true"
type: kubernetes.io/tls
stringData:
  tls.crt: |
%s
  tls.key: |
%s
  ca.crt: |
%s
`, indent(cert.Certificate), indent(cert.PrivateKey), indent(cert.IssuingCaCertificate))

	identityCRS := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-machine-identity-crs", spoke.Name),
			Namespace: "platform-capi",
			Labels: map[string]string{
				"addons.cluster.x-k8s.io/resource-set": "true",
			},
		},
		Type: "addons.cluster.x-k8s.io/resource-set",
		StringData: map[string]string{
			"identity.yaml": identityYAML,
		},
	}

	certCRS := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-bootstrap-cert-crs", spoke.Name),
			Namespace: "platform-capi",
			Labels: map[string]string{
				"addons.cluster.x-k8s.io/resource-set": "true",
			},
		},
		Type: "addons.cluster.x-k8s.io/resource-set",
		StringData: map[string]string{
			"bootstrap-cert.yaml": certYAML,
		},
	}

	// Apply CRS Secrets to API Server (must be fully idempotent)
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, identityCRS, func() error {
		identityCRS.Labels = map[string]string{
			"addons.cluster.x-k8s.io/resource-set": "true",
		}
		identityCRS.Type = "addons.cluster.x-k8s.io/resource-set"
		identityCRS.StringData = map[string]string{
			"identity.yaml": identityYAML,
		}
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, certCRS, func() error {
		certCRS.Labels = map[string]string{
			"addons.cluster.x-k8s.io/resource-set": "true",
		}
		certCRS.Type = "addons.cluster.x-k8s.io/resource-set"
		certCRS.StringData = map[string]string{
			"bootstrap-cert.yaml": certYAML,
		}
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// BootstrapAdoptionCheck verifies cert-manager has adopted the bootstrap secret.
// Once adopted, the bootstrap label is removed and status is recorded.
func (r *SpokePoolReconciler) BootstrapAdoptionCheck(ctx context.Context, spoke *SpokePool) error {
	secret := &corev1.Secret{}
	if err := r.Client.Get(ctx, client.ObjectKey{
		Name:      "argocd-agent-client-cert",
		Namespace: "argocd",
	}, secret); err != nil {
		return client.IgnoreNotFound(err)
	}

	// cert-manager sets its own labels when it adopts a secret.
	// Detect adoption by checking for cert-manager controller labels.
	_, adopted := secret.Labels["cert-manager.io/certificate-name"]
	if adopted {
		spoke.Status.BootstrapCertificateAdopted = true
		spoke.Status.BootstrapCertificateExpires = nil

		// Remove bootstrap label to signal cleanup to operators
		delete(secret.Labels, "platform.nutgraf.in/bootstrap")
		if err := r.Client.Update(ctx, secret); err != nil {
			return err
		}
		return r.Client.Status().Update(ctx, spoke)
	}
	return nil
}

func indent(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = "  " + line // 2-space YAML indent
		}
	}
	return strings.Join(lines, "\n")
}
```

### 5.5. Hub Operator Consolidation Notice

**Critical: There are now two `InfisicalClient` implementations in the hub-operator:**
- `operators/hub-operator/internal/client/infisical.go` — older client (authenticates via k8s secret)
- `operators/hub-operator/internal/secrets/infisical.go` — newer client (env-based credentials, already has Machine Identity CRUD)

Before implementing the changes in section 5, consolidate onto one client. The `secrets.InfisicalClient` is more complete (it already has `createMachineIdentityInInfisical`, `attachUniversalAuth`, `generateClientSecret`, `grantProjectAccess`, `findIdentityByName`). Add `IssueBootstrapCertificate` and `ProfileSlugToID` to that client rather than creating a third variant.

### 6. ArgoCD Agent Day-0 Configuration

**File: `manifests/spoke/spoke-bootstrap/argocd-agent-templates.yaml`**
```yaml
# Update the deployment arguments to map the root CA correctly
# ... inside deployment.yaml container spec ...
                - name: ARGOCD_AGENT_TLS_SECRET_NAME
                  value: "argocd-agent-client-cert"
                - name: ARGOCD_AGENT_TLS_ROOT_CA_PATH
                  value: "/etc/argocd/ca/ca.crt"
              volumeMounts:
                - name: tls-certs
                  mountPath: /etc/argocd/tls
                  readOnly: true
                - name: root-ca
                  mountPath: /etc/argocd/ca
                  readOnly: true
          volumes:
            - name: tls-certs
              secret:
                secretName: argocd-agent-client-cert
            - name: root-ca
              configMap:
                name: offline-root-ca
```

### 6.5. infisical-issuer CRD Verification

The `ClusterIssuer` spec shown below assumes a `spec.infisical` field. The actual [infisical-issuer](https://github.com/Infisical/infisical-issuer) project introduces its own issuer CRD (likely `InfisicalIssuer` or similar) rather than extending cert-manager's native issuer schema.

**Before implementation:**
- Verify the actual CRD shape from the infisical-issuer project
- Confirm whether a `ClusterIssuer` extension or a separate `InfisicalIssuer` CRD is used
- Validate the authentication fields match the released API

The following manifests use the assumed schema. Adjust to match the real CRD before merging.

### 7. Spoke-Local PKI Issuance (Day-1+)

Sync waves (ADR-025 deterministic bootstrap):

| Wave | Resource |
|------|----------|
| 0    | cert-manager CRDs |
| 1    | cert-manager controller |
| 2    | infisical-issuer |
| 3    | InfisicalIssuer / ClusterIssuer |
| 4    | Certificate objects |

**File: `manifests/spoke/spoke-catalog/infra/cert-manager-crds.yaml`**
```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: cert-manager-crds
  namespace: platform-ops
  annotations:
    argocd.argoproj.io/sync-wave: "0"
spec:
  project: platform-infrastructure
  source:
    repoURL: https://charts.jetstack.io
    chart: cert-manager
    targetRevision: v1.16.0
  destination:
    server: https://kubernetes.default.svc
    namespace: cert-manager
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: cert-manager
  namespace: platform-ops
  annotations:
    argocd.argoproj.io/sync-wave: "1"
spec:
  project: platform-infrastructure
  source:
    repoURL: https://charts.jetstack.io
    chart: cert-manager
    targetRevision: v1.16.0
    helm:
      values: |
        installCRDs: false
        crds:
          enabled: false
  destination:
    server: https://kubernetes.default.svc
    namespace: cert-manager
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

**File: `manifests/spoke/spoke-catalog/infra/infisical-issuer.yaml`**
```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: infisical-issuer
  namespace: platform-ops
  annotations:
    argocd.argoproj.io/sync-wave: "2"
spec:
  project: platform-infrastructure
  source:
    repoURL: 'https://dl.cloudsmith.io/public/infisical/helm-charts/helm/charts/'
    chart: infisical-issuer
    targetRevision: 0.1.0
  destination:
    server: https://kubernetes.default.svc
    namespace: platform-ops
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

**File: `manifests/spoke/spoke-catalog/infra/cluster-issuer.yaml`**
```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: infisical-fleet-issuer
  annotations:
    argocd.argoproj.io/sync-wave: "3"
spec:
  infisical:
    url: https://infisical.nutgraf.in
    auth:
      universalAuth:
        credentialsRef:
          clientId:
            name: infisical-auth
            key: client-id
            namespace: platform-ops
          clientSecret:
            name: infisical-auth
            key: client-secret
            namespace: platform-ops
```

**File: `manifests/spoke/spoke-catalog/infra/certificates.yaml`**
```yaml
# Adopts the Day-0 bootstrap secret and handles continuous rotation
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: argocd-agent-client-cert
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "4"
spec:
  secretName: argocd-agent-client-cert
  duration: 24h
  renewBefore: 8h
  commonName: argocd-agent:{{ .Values.spokeName }}
  issuerRef:
    name: infisical-fleet-issuer
    kind: ClusterIssuer
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: alloy-client-cert
  namespace: platform-observability
  annotations:
    argocd.argoproj.io/sync-wave: "4"
spec:
  secretName: alloy-client-cert
  duration: 24h
  renewBefore: 8h
  commonName: alloy:{{ .Values.spokeName }}
  issuerRef:
    name: infisical-fleet-issuer
    kind: ClusterIssuer
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: nats-leafnode-client-cert
  namespace: platform-messaging
  annotations:
    argocd.argoproj.io/sync-wave: "4"
spec:
  secretName: nats-leafnode-client-cert
  duration: 24h
  renewBefore: 8h
  commonName: nats:{{ .Values.spokeName }}
  issuerRef:
    name: infisical-fleet-issuer
    kind: ClusterIssuer
```

**File: `manifests/spoke/spoke-catalog/infra/kustomization.yaml`**
```yaml
resources:
  # ... existing ...
  - cert-manager-crds.yaml
  - cert-manager.yaml
  - infisical-issuer.yaml
  - cluster-issuer.yaml
  - certificates.yaml
```