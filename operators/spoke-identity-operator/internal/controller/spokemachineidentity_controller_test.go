package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	identityv1alpha1 "github.com/kamarkat/kamarkat/operators/spoke-identity-operator/api/v1alpha1"
	"github.com/kamarkat/kamarkat/operators/spoke-identity-operator/internal/infisical"
)

func newSchemeForTest(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core v1 to scheme: %v", err)
	}
	if err := identityv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add identity v1alpha1 to scheme: %v", err)
	}
	return scheme
}

func newTestSMI(name string, clientID string) *identityv1alpha1.SpokeMachineIdentity {
	return &identityv1alpha1.SpokeMachineIdentity{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "platform-capi",
		},
		Spec: identityv1alpha1.SpokeMachineIdentitySpec{
			SpokeRef: identityv1alpha1.SpokeRef{Name: name},
		},
		Status: identityv1alpha1.SpokeMachineIdentityStatus{
			IdentityID: "identity-1",
			ClientID:   clientID,
		},
	}
}

func getWrapperSecret(t *testing.T, c client.Client, name string) (*corev1.Secret, string) {
	t.Helper()
	secret := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "platform-capi"}, secret); err != nil {
		t.Fatalf("get %s wrapper: %v", name, err)
	}
	if secret.Type != "addons.cluster.x-k8s.io/resource-set" {
		t.Fatalf("expected resource-set type, got %q", secret.Type)
	}
	payload := string(secret.Data["cluster-issuer.yaml"])
	if payload == "" {
		payload = secret.StringData["cluster-issuer.yaml"]
	}
	if payload == "" {
		t.Fatalf("cluster-issuer.yaml data missing")
	}
	return secret, payload
}

// The workload credential is delivered ONCE, to its one consumer.
//
// There were two copies and a duplicate key, both for the certificate issuer:
// pki-issuer v0.2.0 read secretData["clientSecret"] hardcoded, and the issuer
// authenticated as this same identity. v1.1.0 resolves credentials through the
// references its ClusterIssuer declares, and the issuer now has its own identity,
// so a second copy of a workload credential in cert-manager is a credential in a
// place nothing reads it from.
func TestEnsureCRSWrapperDeliversTheWorkloadCredentialOnce(t *testing.T) {
	scheme := newSchemeForTest(t)
	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "smi-spoke-a-auth", Namespace: "platform-capi"},
		Data: map[string][]byte{
			"clientId":     []byte("client-id-value"),
			"clientSecret": []byte("client-secret-value"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(authSecret).Build()
	r := &SpokeMachineIdentityReconciler{Client: c}

	smi := newTestSMI("spoke-a", "client-id-value")
	if err := r.ensureCRSWrapper(context.Background(), smi); err != nil {
		t.Fatalf("ensureCRSWrapper: %v", err)
	}

	secret := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "spoke-a-machine-identity", Namespace: "platform-capi"}, secret); err != nil {
		t.Fatalf("get identity wrapper: %v", err)
	}
	payload := string(secret.Data["identity.yaml"])
	if payload == "" {
		payload = secret.StringData["identity.yaml"]
	}

	if n := strings.Count(payload, "namespace: platform-ops"); n != 1 {
		t.Errorf("expected exactly one delivered copy, in platform-ops; got %d in:\n%s", n, payload)
	}
	if strings.Contains(payload, "cert-manager") {
		t.Errorf("the workload credential is still delivered to cert-manager, where the issuer "+
			"no longer reads it:\n%s", payload)
	}
	if strings.Contains(payload, "clientSecret:") {
		t.Errorf("the v0.2.0 duplicate clientSecret key survived; v1.1.0 honours the key its "+
			"ClusterIssuer names:\n%s", payload)
	}
	for _, key := range []string{"client-id:", "client-secret:"} {
		if n := strings.Count(payload, key); n != 1 {
			t.Errorf("expected %q exactly once, got %d", key, n)
		}
	}
}

func pkiFixture() *pkiIdentity {
	return &pkiIdentity{ID: "pki-identity-1", ClientID: "pki-client-id", ClientSecret: "pki-client-secret"}
}

// The ClusterIssuer conforms to the schema the PINNED issuer image accepts.
//
// pki-issuer v1.1.0's IssuerSpec REQUIRES application and profile and declares
// neither certificateTemplateName nor projectId, and its authentication is
// method + universal.clientIdRef/clientSecretRef. This rendered the v0.2.0 shape
// until 2026-11-23, so the only producer of the issuer three spoke certificates
// name emitted an object the CRD could not admit.
func TestEnsureClusterIssuerCRSWrapperRendersThePinnedIssuerContract(t *testing.T) {
	t.Setenv("SECRET_STORE_URL", "https://infisical.dev.acme.example")
	scheme := newSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SpokeMachineIdentityReconciler{Client: c, ProjectID: "proj-1"}

	smi := newTestSMI("spoke-a", "client-id-value")
	if err := r.ensureClusterIssuerCRSWrapper(context.Background(), smi, pkiFixture()); err != nil {
		t.Fatalf("ensureClusterIssuerCRSWrapper: %v", err)
	}
	_, payload := getWrapperSecret(t, c, "spoke-a-cluster-issuer")

	var issuer map[string]any
	for _, doc := range strings.Split(payload, "\n---\n") {
		var parsed map[string]any
		if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
			t.Fatalf("payload document is not YAML: %v\n%s", err, doc)
		}
		if parsed["kind"] == "ClusterIssuer" {
			issuer = parsed
		}
	}
	if issuer == nil {
		t.Fatalf("no ClusterIssuer in the payload:\n%s", payload)
	}

	spec, _ := issuer["spec"].(map[string]any)
	for _, required := range []string{"url", "application", "profile", "authentication"} {
		if _, ok := spec[required]; !ok {
			t.Errorf("spec.%s is absent, and v1.1.0's IssuerSpec requires it", required)
		}
	}
	for _, gone := range []string{"certificateTemplateName", "projectId"} {
		if _, ok := spec[gone]; ok {
			t.Errorf("spec.%s is a v0.2.0 field that v1.1.0 does not declare", gone)
		}
	}
	if spec["application"] != pkiApplicationName {
		t.Errorf("spec.application is %v, want %q -- the issuer resolves it by name",
			spec["application"], pkiApplicationName)
	}
	if spec["profile"] != clusterIssuerProfile {
		t.Errorf("spec.profile is %v, want %q", spec["profile"], clusterIssuerProfile)
	}

	auth, _ := spec["authentication"].(map[string]any)
	if auth["method"] != "universal" {
		t.Errorf("spec.authentication.method is %v, want universal", auth["method"])
	}
	if _, ok := auth["universalAuth"]; ok {
		t.Error("spec.authentication.universalAuth is the v0.2.0 shape; v1.1.0 reads .universal")
	}
	universal, _ := auth["universal"].(map[string]any)
	for _, ref := range []string{"clientIdRef", "clientSecretRef"} {
		r, ok := universal[ref].(map[string]any)
		if !ok {
			t.Fatalf("spec.authentication.universal.%s is absent", ref)
		}
		if r["name"] != pkiAuthSecretName || r["namespace"] != pkiAuthNamespace {
			t.Errorf("%s points at %v/%v, want %s/%s -- the issuer's own credential, not ESO's",
				ref, r["namespace"], r["name"], pkiAuthNamespace, pkiAuthSecretName)
		}
	}
}

// The issuer's credential travels WITH the issuer. Apart they are useless: a
// ClusterIssuer whose references resolve to nothing leaves every
// CertificateRequest pending, and CRS delivery is ApplyOnce.
func TestEnsureClusterIssuerCRSWrapperCarriesTheIssuersOwnCredential(t *testing.T) {
	t.Setenv("SECRET_STORE_URL", "https://infisical.dev.acme.example")
	scheme := newSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SpokeMachineIdentityReconciler{Client: c, ProjectID: "proj-1"}

	if err := r.ensureClusterIssuerCRSWrapper(context.Background(), newTestSMI("spoke-a", "x"), pkiFixture()); err != nil {
		t.Fatalf("ensureClusterIssuerCRSWrapper: %v", err)
	}
	_, payload := getWrapperSecret(t, c, "spoke-a-cluster-issuer")

	var found bool
	for _, doc := range strings.Split(payload, "\n---\n") {
		var parsed map[string]any
		_ = yaml.Unmarshal([]byte(doc), &parsed)
		meta, _ := parsed["metadata"].(map[string]any)
		if parsed["kind"] == "Secret" && meta["name"] == pkiAuthSecretName {
			found = true
			if meta["namespace"] != pkiAuthNamespace {
				t.Errorf("the issuer credential is in %v, want %s", meta["namespace"], pkiAuthNamespace)
			}
			data, _ := parsed["stringData"].(map[string]any)
			if data["client-id"] != "pki-client-id" || data["client-secret"] != "pki-client-secret" {
				t.Errorf("the issuer credential does not carry the PKI identity's material: %v", data)
			}
		}
	}
	if !found {
		t.Errorf("the payload carries no %s Secret:\n%s", pkiAuthSecretName, payload)
	}
	// ESO's credential has no business in this payload.
	if strings.Contains(payload, "name: infisical-auth") {
		t.Errorf("the issuer payload references the workload credential:\n%s", payload)
	}
}

// Fail-closed. CRS is ApplyOnce, so a payload published without a resolvable
// credential breaks that spoke's issuance permanently.
func TestEnsureClusterIssuerCRSWrapperPublishesNothingWithoutAnIssuerCredential(t *testing.T) {
	t.Setenv("SECRET_STORE_URL", "https://infisical.dev.acme.example")
	for name, pki := range map[string]*pkiIdentity{
		"no identity at all": nil,
		"no client id":       {ID: "p", ClientSecret: "s"},
		"no client secret":   {ID: "p", ClientID: "c"},
	} {
		scheme := newSchemeForTest(t)
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		r := &SpokeMachineIdentityReconciler{Client: c, ProjectID: "proj-1"}

		if err := r.ensureClusterIssuerCRSWrapper(context.Background(), newTestSMI("spoke-a", "x"), pki); err != nil {
			t.Fatalf("%s: expected a deferral rather than an error, got %v", name, err)
		}
		secret := &corev1.Secret{}
		if err := c.Get(context.Background(), client.ObjectKey{Name: "spoke-a-cluster-issuer", Namespace: "platform-capi"}, secret); err == nil {
			t.Errorf("%s: a wrapper was published", name)
		}
	}
}

func TestEnsureClusterIssuerCRSWrapperIsIdempotent(t *testing.T) {
	t.Setenv("SECRET_STORE_URL", "https://infisical.dev.acme.example")
	scheme := newSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SpokeMachineIdentityReconciler{Client: c, ProjectID: "proj-1"}
	smi := newTestSMI("spoke-a", "x")

	if err := r.ensureClusterIssuerCRSWrapper(context.Background(), smi, pkiFixture()); err != nil {
		t.Fatalf("first: %v", err)
	}
	before, _ := getWrapperSecret(t, c, "spoke-a-cluster-issuer")
	if err := r.ensureClusterIssuerCRSWrapper(context.Background(), smi, pkiFixture()); err != nil {
		t.Fatalf("second: %v", err)
	}
	after, _ := getWrapperSecret(t, c, "spoke-a-cluster-issuer")
	if before.ResourceVersion != after.ResourceVersion {
		t.Errorf("an unchanged payload was rewritten (%s -> %s)", before.ResourceVersion, after.ResourceVersion)
	}
}

// A ROTATED ISSUER SECRET MUST REACH THE WRAPPER. This inverts what the payload
// used to assert: it carried a rotation-stable clientId and read the secret from
// elsewhere, so a rotation deliberately did not regenerate it. The payload now
// CONTAINS the credential, so a wrapper left unchanged after a rotation would
// hand the next provisioned spoke a secret that no longer authenticates.
func TestEnsureClusterIssuerCRSWrapperFollowsARotatedIssuerSecret(t *testing.T) {
	t.Setenv("SECRET_STORE_URL", "https://infisical.dev.acme.example")
	scheme := newSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SpokeMachineIdentityReconciler{Client: c, ProjectID: "proj-1"}
	smi := newTestSMI("spoke-a", "x")

	if err := r.ensureClusterIssuerCRSWrapper(context.Background(), smi, pkiFixture()); err != nil {
		t.Fatalf("first: %v", err)
	}
	rotated := pkiFixture()
	rotated.ClientSecret = "pki-client-secret-rotated"
	if err := r.ensureClusterIssuerCRSWrapper(context.Background(), smi, rotated); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	_, payload := getWrapperSecret(t, c, "spoke-a-cluster-issuer")
	if !strings.Contains(payload, "pki-client-secret-rotated") {
		t.Errorf("the rotated issuer secret did not reach the wrapper:\n%s", payload)
	}
}

// --- Phase 2 lockout self-heal (P0 single authority) ---

const testSpokeClientID = "b3d5ec56-623e-4a04-b82e-3960d84196c7"

func newSelfHealReconciler(t *testing.T, handler http.HandlerFunc) (*SpokeMachineIdentityReconciler, client.Client, *httptest.Server) {
	t.Helper()
	t.Setenv("SECRET_STORE_CLIENT_ID", "op-client")
	t.Setenv("SECRET_STORE_CLIENT_SECRET", "op-secret")

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	scheme := newSchemeForTest(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&identityv1alpha1.SpokeMachineIdentity{}).Build()
	r := &SpokeMachineIdentityReconciler{
		Client:          k8s,
		InfisicalClient: infisical.NewClient(srv.URL),
	}
	return r, k8s, srv
}

// identity.yaml-style payload matching what ensureCRSWrapper renders: two
// infisical-auth copies (platform-ops + cert-manager) with identical creds.
func seedIdentityWrapper(t *testing.T, c client.Client, smi *identityv1alpha1.SpokeMachineIdentity) {
	t.Helper()
	payload := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: infisical-auth
  namespace: platform-ops
type: Opaque
stringData:
  client-id: %s
  client-secret: secret-123
---
apiVersion: v1
kind: Secret
metadata:
  name: infisical-auth
  namespace: cert-manager
type: Opaque
stringData:
  client-id: %s
  client-secret: secret-123
`, testSpokeClientID, testSpokeClientID)
	wrapper := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-machine-identity", smi.Spec.SpokeRef.Name),
			Namespace: smi.Namespace,
		},
		Type:       "addons.cluster.x-k8s.io/resource-set",
		StringData: map[string]string{"identity.yaml": payload},
	}
	if err := c.Create(context.Background(), wrapper); err != nil {
		t.Fatalf("seed wrapper: %v", err)
	}
}

func readyCondition(smi *identityv1alpha1.SpokeMachineIdentity) *metav1.Condition {
	for i := range smi.Status.Conditions {
		if smi.Status.Conditions[i].Type == conditionTypeReady {
			return &smi.Status.Conditions[i]
		}
	}
	return nil
}

// identity locked -> clear-lockout called with admin token, re-probe healthy.

func TestEnsureIdentityUnlockClearsLockout(t *testing.T) {
	var sawClear bool
	handler := func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		clientID := body["clientId"]
		switch r.URL.Path {
		case infisical.PathAuthUniversalAuthLogin:
			if clientID == "op-client" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"accessToken":"op-token","expiresIn":60}`))
				return
			}
			if sawClear {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"accessToken":"t","expiresIn":60}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"This identity auth method is temporarily locked out"}`))
		case infisical.PathAuthUniversalAuthClearLockout:
			sawClear = true
			if got := r.Header.Get("Authorization"); got != "Bearer op-token" {
				t.Errorf("expected admin bearer token, got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"deleted":1}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}

	r, c, _ := newSelfHealReconciler(t, handler)
	smi := newTestSMI("spoke-s1", testSpokeClientID)
	seedIdentityWrapper(t, c, smi)

	if err := r.ensureIdentityUnlock(context.Background(), smi, log.FromContext(context.Background())); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !sawClear {
		t.Fatal("expected clear-lockout to be called for a locked identity")
	}
	if cond := readyCondition(smi); cond != nil && cond.Status == metav1.ConditionFalse {
		t.Fatalf("unexpected failure condition: %+v", cond)
	}
}

// invalid credentials -> NEVER clear; surface Ready=False AuthenticationDefect.
func TestEnsureIdentityUnlockDefectDoesNotClear(t *testing.T) {
	var sawClear bool
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case infisical.PathAuthUniversalAuthLogin:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Invalid credentials"}`))
		case infisical.PathAuthUniversalAuthClearLockout:
			sawClear = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"deleted":1}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}

	r, c, _ := newSelfHealReconciler(t, handler)
	smi := newTestSMI("spoke-s2", testSpokeClientID)
	seedIdentityWrapper(t, c, smi)

	// Simulate the reconcile order: Ready=True is set before the self-heal runs
	// so the defect condition must override it.
	smi.Status.Conditions = append(smi.Status.Conditions, metav1.Condition{
		Type: conditionTypeReady, Status: metav1.ConditionTrue, Reason: "Reconciled", LastTransitionTime: metav1.Now(),
	})

	if err := r.ensureIdentityUnlock(context.Background(), smi, log.FromContext(context.Background())); err != nil {
		t.Fatalf("expected nil error (defect is surfaced, not fatal), got %v", err)
	}
	if sawClear {
		t.Fatal("must NEVER clear lockout on Invalid credentials")
	}
	cond := readyCondition(smi)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "AuthenticationDefect" {
		t.Fatalf("expected Ready=False AuthenticationDefect, got %+v", cond)
	}
}

// no credentials material yet -> skip, no API traffic.
func TestEnsureIdentityUnlockSkipsWithoutMaterial(t *testing.T) {
	var calls int
	handler := func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}
	r, _, _ := newSelfHealReconciler(t, handler)
	smi := newTestSMI("spoke-s3", testSpokeClientID)

	if err := r.ensureIdentityUnlock(context.Background(), smi, log.FromContext(context.Background())); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no API calls without material, got %d", calls)
	}
}

// empty status clientId -> fail-closed skip.
func TestEnsureIdentityUnlockSkipsWithoutClientID(t *testing.T) {
	var calls int
	handler := func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}
	r, _, _ := newSelfHealReconciler(t, handler)
	smi := newTestSMI("spoke-s4", "")
	if err := r.ensureIdentityUnlock(context.Background(), smi, log.FromContext(context.Background())); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no API calls without clientId, got %d", calls)
	}
}

// parser contract: picks client-id/client-secret from the platform-ops copy.
func TestParseIdentityAuthDoc(t *testing.T) {
	payload := `apiVersion: v1
kind: Secret
metadata:
  name: infisical-auth
  namespace: platform-ops
type: Opaque
stringData:
  client-id: cid-1
  client-secret: csec-1
---
apiVersion: v1
kind: Secret
metadata:
  name: infisical-auth
  namespace: cert-manager
type: Opaque
stringData:
  client-id: cid-1
  client-secret: csec-1
`
	id, secret, ok := parseIdentityAuthDoc(payload)
	if !ok || id != "cid-1" || secret != "csec-1" {
		t.Fatalf("unexpected parse result: %q %q %v", id, secret, ok)
	}
	if _, _, ok := parseIdentityAuthDoc(""); ok {
		t.Fatal("empty payload must not parse")
	}
}
