### Phase 1: ADR Documentation & Design Updates

Update the recently approved **ADR-035** to reflect the separation of concerns.

**File: `docs/adr/035-enterprise-pki-and-delegated-trust-via-infisical-oss.md`**
```markdown
# Hub Operator and Cert Operator Responsibilities

To maintain modularity, PKI and identity bootstrapping are extracted from the Hub Operator into a dedicated **Cert Operator**.

The **Cert Operator** participates only in Day-0 bootstrap.
Responsibilities:
* Watch `SpokePool` provisioning.
* Create Machine Identities in Infisical.
* Generate the 72h bootstrap certificate via Infisical PKI.
* Store bootstrap artifacts as `ClusterResourceSet` payloads in the `platform-capi` namespace.

The **Cert Operator** SHALL NOT:
* Rotate, renew, or reconcile Spoke leaf certificates (Day-1+).
* Act as an active PKI proxy for workloads.

The **Hub Operator** is completely decoupled from PKI and Machine Identity management.
```

---

### Phase 2: Eradicate Crossplane Secret Traversal

Run the following commands to delete the anti-pattern:
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

**Files to DELETE (Old Kyverno Transforms):**
```bash
rm manifests/hub-core-services/security/infisical-auth-crs-transform.yaml
rm manifests/hub-core-services/security/argocd-agent-cert.yaml
rm manifests/hub-core-services/security/nats-leafnode-cert.yaml
rm manifests/hub-core-services/security/observability-cert.yaml
```

---

### Phase 3: Static Offline Root CA Distribution (Day-0)

**Important:** The Offline Root CA public cert is imported during the `hub bootstrap` CLI phase.

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
*(Your `hub bootstrap` command should `sed` or patch this file to insert the actual `ca.crt` string).*

---

### Phase 4: Scaffold and Implement `cert-operator`

Run Kubebuilder to initialize the new operator:
```bash
mkdir -p operators/cert-operator
cd operators/cert-operator
kubebuilder init --domain nutgraf.in --repo github.com/kamarkat/kamarkat/operators/cert-operator
# We don't create a new API. The operator will watch the existing Crossplane SpokePool XR.
```

#### 1. Core Infisical PKI Client
**File: `operators/cert-operator/internal/infisical/client.go`**
```go
package infisical

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: baseURL,
		Token:   token,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// IssueCertRequest matches the verified Infisical /api/v1/cert-manager/certificates endpoint
type IssueCertRequest struct {
	ProfileId  string              `json:"profileId"`
	Attributes IssueCertAttributes `json:"attributes"`
}

type IssueCertAttributes struct {
	CommonName string `json:"commonName,omitempty"`
	TTL        string `json:"ttl,omitempty"`
}

type IssueCertResponseWrapper struct {
	Certificate          *CertData `json:"certificate"`
	CertificateRequestId string    `json:"certificateRequestId"`
	Status               string    `json:"status"`
	Message              string    `json:"message,omitempty"`
}

type CertData struct {
	Certificate          string `json:"certificate"`
	IssuingCaCertificate string `json:"issuingCaCertificate"`
	CertificateChain     string `json:"certificateChain"`
	PrivateKey           string `json:"privateKey"`
	SerialNumber         string `json:"serialNumber"`
	CertificateId        string `json:"certificateId"`
}

type Identity struct {
	ID           string
	ClientID     string
	ClientSecret string
}

type Profile struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

// IssueBootstrapCertificate mints a 72-hour cert for ArgoCD Agent bootstrap.
func (c *Client) IssueBootstrapCertificate(ctx context.Context, profileId, commonName string) (*CertData, error) {
	reqBody := IssueCertRequest{
		ProfileId: profileId,
		Attributes: IssueCertAttributes{
			CommonName: commonName,
			TTL:        "72h",
		},
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal issue cert request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/api/v1/cert-manager/certificates", c.BaseURL),
		bytes.NewBuffer(payload))
	if err != nil {
		return nil, fmt.Errorf("create issue cert request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute issue cert request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("issue bootstrap cert failed: status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var result IssueCertResponseWrapper
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode issue cert response: %w", err)
	}

	if result.Certificate == nil {
		return nil, fmt.Errorf("bootstrap certificate issued nil (async order flow): %s", result.Message)
	}

	return result.Certificate, nil
}

// GetProfileIdBySlug resolves a certificate profile slug to its UUID.
// GET /api/v1/cert-manager/certificate-profiles/slug/:slug
func (c *Client) GetProfileIdBySlug(ctx context.Context, slug string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/api/v1/cert-manager/certificate-profiles/slug/%s", c.BaseURL, slug), nil)
	if err != nil {
		return "", fmt.Errorf("create get profile request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("execute get profile request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get profile by slug failed: status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode profile response: %w", err)
	}

	return result.ID, nil
}

// GetOrCreateMachineIdentity finds or creates a Machine Identity in Infisical.
func (c *Client) GetOrCreateMachineIdentity(ctx context.Context, name, orgID string) (*Identity, error) {
	existing, err := c.findIdentityByName(ctx, name, orgID)
	if err == nil && existing != "" {
		ua, err := c.getUniversalAuth(ctx, existing)
		if err != nil {
			return nil, fmt.Errorf("get universal auth for existing identity: %w", err)
		}
		return &Identity{ID: existing, ClientID: ua.IdentityUniversalAuth.ClientID, ClientSecret: ""}, nil
	}

	id, err := c.createIdentity(ctx, name, orgID)
	if err != nil {
		return nil, fmt.Errorf("create machine identity: %w", err)
	}

	if err := c.attachUniversalAuth(ctx, id); err != nil {
		return nil, fmt.Errorf("attach universal auth: %w", err)
	}

	clientID, err := c.getClientID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get client ID: %w", err)
	}

	clientSecret, err := c.generateClientSecret(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("generate client secret: %w", err)
	}

	return &Identity{ID: id, ClientID: clientID, ClientSecret: clientSecret}, nil
}

// GrantProjectAccess grants a Machine Identity access to the Infisical project.
func (c *Client) GrantProjectAccess(ctx context.Context, identityID, projectID, role string) error {
	payload := map[string]string{"role": role}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal grant access: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/api/v1/projects/%s/memberships/identities/%s", c.BaseURL, projectID, identityID),
		bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("create grant access request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("execute grant access request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusConflict {
		respBody, _ := io.ReadAll(resp.Body)
		if bytes.Contains(respBody, []byte("already")) || bytes.Contains(respBody, []byte("exists")) {
			return nil
		}
		return fmt.Errorf("grant project access failed: status %d: %s", resp.StatusCode, string(respBody))
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("grant project access failed: status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// --- private helpers ---

func (c *Client) findIdentityByName(ctx context.Context, name, orgID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/api/v1/identities?limit=100&orgId=%s", c.BaseURL, orgID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("list identities failed: status %d", resp.StatusCode)
	}

	var listResp struct {
		Identities []struct {
			Identity struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"identity"`
		} `json:"identities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return "", err
	}

	for _, item := range listResp.Identities {
		if item.Identity.Name == name {
			return item.Identity.ID, nil
		}
	}
	return "", nil
}

type universalAuthResponse struct {
	IdentityUniversalAuth struct {
		ClientID string `json:"clientId"`
	} `json:"identityUniversalAuth"`
}

func (c *Client) getUniversalAuth(ctx context.Context, identityID string) (*universalAuthResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/api/v1/auth/universal-auth/identities/%s", c.BaseURL, identityID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get universal auth failed: status %d", resp.StatusCode)
	}

	var ua universalAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&ua); err != nil {
		return nil, err
	}
	return &ua, nil
}

func (c *Client) createIdentity(ctx context.Context, name, orgID string) (string, error) {
	payload := map[string]string{
		"name":           name,
		"organizationId": orgID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/api/v1/identities", c.BaseURL), bytes.NewBuffer(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create identity failed: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Identity struct {
			ID string `json:"id"`
		} `json:"identity"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Identity.ID, nil
}

func (c *Client) attachUniversalAuth(ctx context.Context, identityID string) error {
	payload := map[string]interface{}{
		"clientSecretTrustedIps": []map[string]string{
			{"ipAddress": "0.0.0.0/0"},
			{"ipAddress": "::/0"},
		},
		"accessTokenTrustedIps": []map[string]string{
			{"ipAddress": "0.0.0.0/0"},
			{"ipAddress": "::/0"},
		},
		"accessTokenTTL":      2592000,
		"accessTokenMaxTTL":   2592000,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/api/v1/auth/universal-auth/identities/%s", c.BaseURL, identityID),
		bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusBadRequest && bytes.Contains(respBody, []byte("already")) {
		return nil
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("attach universal auth failed: status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func (c *Client) getClientID(ctx context.Context, identityID string) (string, error) {
	ua, err := c.getUniversalAuth(ctx, identityID)
	if err != nil {
		return "", err
	}
	return ua.IdentityUniversalAuth.ClientID, nil
}

func (c *Client) generateClientSecret(ctx context.Context, identityID string) (string, error) {
	payload := map[string]interface{}{
		"numUsesLimit": 0,
		"ttl":          0,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/api/v1/auth/universal-auth/identities/%s/client-secrets", c.BaseURL, identityID),
		bytes.NewBuffer(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("generate client secret failed: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.ClientSecret, nil
}
```

#### 2. Spoke PKI Controller
**File: `operators/cert-operator/internal/controller/spokepool_pki_controller.go`**
```go
package controller

import (
	"context"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kamarkat/kamarkat/operators/cert-operator/internal/infisical"
)

const (
	crsNamespace       = "platform-capi"
	machineIdentityCRS = "%s-machine-identity"
	bootstrapCertCRS   = "%s-bootstrap-cert"
)

// SpokePKIReconciler watches SpokePool provisioning and generates Day-0
// bootstrap PKI artifacts (Machine Identity + bootstrap certificate).
type SpokePKIReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	InfisicalClient *infisical.Client
	ProfileSlug     string
	OrgID           string
	ProjectID       string
}

// SetupWithManager registers the SpokePool watch.
func (r *SpokePKIReconciler) SetupWithManager(mgr ctrl.Manager) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "nutgraf.in",
		Version: "v1alpha1",
		Kind:    "SpokePool",
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(u).
		Named("spokepool-pki").
		Complete(r)
}

// Reconcile handles SpokePool creation and generates Day-0 PKI payloads.
// Idempotent: if the bootstrap-cert-crs Secret already exists, this is a no-op.
func (r *SpokePKIReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("spokepool", req.Name)

	// 1. Idempotency check — if CRS secrets already exist, Day-0 is done
	crsSecretName := fmt.Sprintf(bootstrapCertCRS, req.Name)
	existingSecret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Name: crsSecretName, Namespace: crsNamespace}, existingSecret)
	if err == nil {
		logger.Info("Bootstrap CRS already exists, skipping")
		return ctrl.Result{}, nil
	} else if !errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("check existing CRS: %w", err)
	}

	// 2. Resolve environment config
	profileSlug := r.ProfileSlug
	if profileSlug == "" {
		profileSlug = os.Getenv("INFISICAL_BOOTSTRAP_PROFILE_SLUG")
	}
	if profileSlug == "" {
		return ctrl.Result{}, fmt.Errorf("INFISICAL_BOOTSTRAP_PROFILE_SLUG not configured")
	}

	orgID := r.OrgID
	if orgID == "" {
		orgID = os.Getenv("INFISICAL_ORGANIZATION_ID")
	}
	if orgID == "" {
		return ctrl.Result{}, fmt.Errorf("INFISICAL_ORGANIZATION_ID not configured")
	}

	projectID := r.ProjectID
	if projectID == "" {
		projectID = os.Getenv("INFISICAL_PROJECT_ID")
	}
	if projectID == "" {
		return ctrl.Result{}, fmt.Errorf("INFISICAL_PROJECT_ID not configured")
	}

	// 3. Resolve profile slug to UUID
	profileId, err := r.InfisicalClient.GetProfileIdBySlug(ctx, profileSlug)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve profile slug: %w", err)
	}
	logger.Info("Resolved bootstrap profile", "slug", profileSlug, "profileId", profileId)

	// 4. Create or reuse Machine Identity
	identityName := fmt.Sprintf("spoke-%s", req.Name)
	identity, err := r.InfisicalClient.GetOrCreateMachineIdentity(ctx, identityName, orgID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get or create machine identity: %w", err)
	}
	logger.Info("Machine identity ready", "identityId", identity.ID)

	// 5. Grant project-level PKI access
	if err := r.InfisicalClient.GrantProjectAccess(ctx, identity.ID, projectID, "admin"); err != nil {
		return ctrl.Result{}, fmt.Errorf("grant project access: %w", err)
	}
	logger.Info("Project access granted", "identityId", identity.ID, "projectId", projectID)

	// 6. Mint 72-hour bootstrap certificate
	commonName := fmt.Sprintf("argocd-agent:%s", req.Name)
	cert, err := r.InfisicalClient.IssueBootstrapCertificate(ctx, profileId, commonName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("issue bootstrap certificate: %w", err)
	}
	logger.Info("Bootstrap certificate issued", "serial", cert.SerialNumber)

	// 7. Build and apply CRS payloads
	if err := r.applyCRSPayloads(ctx, req.Name, identity, cert); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply CRS payloads: %w", err)
	}
	logger.Info("Bootstrap CRS payloads applied")

	return ctrl.Result{}, nil
}

func (r *SpokePKIReconciler) applyCRSPayloads(ctx context.Context, spokeName string, id *infisical.Identity, cert *infisical.CertData) error {
	identityYAML := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: infisical-auth
  namespace: platform-ops
type: Opaque
stringData:
  client-id: %s
  client-secret: %s
`, id.ClientID, id.ClientSecret)

	certYAML := fmt.Sprintf(`apiVersion: v1
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

	// Create Machine Identity CRS
	identityCRS := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf(machineIdentityCRS, spokeName),
			Namespace: crsNamespace,
			Labels:    map[string]string{"addons.cluster.x-k8s.io/resource-set": "true"},
		},
		Type: "addons.cluster.x-k8s.io/resource-set",
		StringData: map[string]string{"identity.yaml": identityYAML},
	}
	if err := r.Create(ctx, identityCRS); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create identity CRS: %w", err)
	}

	// Create Bootstrap Certificate CRS
	certCRS := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf(bootstrapCertCRS, spokeName),
			Namespace: crsNamespace,
			Labels:    map[string]string{"addons.cluster.x-k8s.io/resource-set": "true"},
		},
		Type: "addons.cluster.x-k8s.io/resource-set",
		StringData: map[string]string{"bootstrap-cert.yaml": certYAML},
	}
	if err := r.Create(ctx, certCRS); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create cert CRS: %w", err)
	}

	return nil
}

func indent(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = "    " + line
		}
	}
	return strings.Join(lines, "\n")
}
```

#### 3. Setup Deployment for `cert-operator`
Deploy `cert-operator` identically to `hub-operator` via ArgoCD, scheduled at `sync-wave: 1`.

**File: `manifests/hub-core-services/cert-operator/deployment.yaml`** (Standard Kubebuilder deployment, omitted for brevity but ensure ENV vars `INFISICAL_BOOTSTRAP_PROFILE_SLUG` and Auth Tokens are mounted).

---

### Phase 5: Wire Crossplane Composition to the New Secrets

**File: `manifests/hub-core-services/crossplane/tenant-platform/compositions/spokepool-hetzner.yaml`**

Update the `cluster-resource-set` resource to inject the secrets built by `cert-operator`:

```yaml
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
                      # ADR-035: Must be "Once". If "Reconcile", CAPI will endlessly overwrite
                      # cert-manager's renewed certificates with the expired 72h bootstrap cert.
                      strategy: Once
                      resources:
                        - name: offline-root-ca-crs
                          kind: ConfigMap
                        - name: "" # Patched to <spokeName>-machine-identity
                          kind: Secret
                        - name: "" # Patched to <spokeName>-bootstrap-cert
                          kind: Secret
                        # ... other static CNI/CCM resources ...
            patches:
              - type: FromCompositeFieldPath
                fromFieldPath: metadata.name
                toFieldPath: spec.forProvider.manifest.spec.resources[1].name
                transforms:
                  - type: string
                    string: { type: Format,                     fmt: "%s-machine-identity" }
              - type: FromCompositeFieldPath
                fromFieldPath: metadata.name
                toFieldPath: spec.forProvider.manifest.spec.resources[2].name
                transforms:
                  - type: string
                    string: { type: Format, fmt: "%s-bootstrap-cert" }
```

---

### Phase 6: Update ArgoCD Agent Configuration

**File: `manifests/spoke/spoke-bootstrap/argocd-agent-templates.yaml`**

```yaml
# Update the deployment arguments inside argocd-agent-deployment ConfigMap
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

---

### Phase 7: Spoke-Local Day-1 PKI Delegation (GitOps)

**File: `manifests/spoke/spoke-catalog/infra/infisical-issuer.yaml`**
```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: infisical-issuer
  namespace: platform-ops
  annotations:
    argocd.argoproj.io/sync-wave: "2" # Runs after cert-manager operator
spec:
  project: platform-infrastructure
  source:
    repoURL: 'https://dl.cloudsmith.io/public/infisical/helm-charts/helm/charts/'
    chart: infisical-issuer
    targetRevision: 0.1.0 # Ensure this matches upstream published chart version
  destination:
    server: https://kubernetes.default.svc
    namespace: platform-ops
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
```

**File: `manifests/spoke/spoke-catalog/infra/cluster-issuer.yaml`**
```yaml
# Deploys the Custom Resource provided by the infisical-issuer chart
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: infisical-fleet-issuer
  annotations:
    argocd.argoproj.io/sync-wave: "3" # After infisical-issuer is ready
spec:
  # NOTE: Exact structure subject to Infisical Issuer CRD spec.
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
# 1. Adopts the Day-0 bootstrap secret and handles continuous rotation
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

# 2. Alloy Client
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

# 3. NATS Leaf Node
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
  # ... existing CAPI, CNPG, Crossplane ...
  - cert-manager-crds.yaml
  - cert-manager.yaml
  - infisical-issuer.yaml
  - cluster-issuer.yaml
  - certificates.yaml
```

### Review Completion
This plan fulfills the ADR-035 directive, completely decouples the `hub-operator` by introducing `cert-operator`, relies on `strategy: Once` for the CRS to prevent infinite CAPI loops, safely injects the Root CA as a ConfigMap, and centralizes standard TTLs to 24h across Spoke certificates.
