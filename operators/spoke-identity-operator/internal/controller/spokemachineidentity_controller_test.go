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

// ensureCRSWrapper renders both infisical-auth copies. infisical-issuer v0.2.0
// authenticates via the hardcoded secretData["clientSecret"] key (secretRef.key
// is never consulted), so the cert-manager copy MUST contain clientSecret or
// the issuer logs in with an empty secret and trips identity lockout
// (401 Invalid credentials x3 -> 401 "temporarily locked" loop, CRs pending).
func TestEnsureCRSWrapperRendersClientSecretKey(t *testing.T) {
	scheme := newSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SpokeMachineIdentityReconciler{Client: c, ProjectID: "20958df6-ac81-4e11-8259-d1ddd1becb5c"}

	smi := newTestSMI("spoke-h", "b3d5ec56-623e-4a04-b82e-3960d84196c7")
	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "smi-spoke-h-auth", Namespace: "platform-capi"},
		StringData: map[string]string{
			"clientId":     "b3d5ec56-623e-4a04-b82e-3960d84196c7",
			"clientSecret": "c54dc1e82716766396779dcd75b678f1ecbc7ffcb026db8a189d6be6b0fa21c1",
		},
	}
	if err := c.Create(context.Background(), authSecret); err != nil {
		t.Fatalf("seed auth secret: %v", err)
	}

	if err := r.ensureCRSWrapper(context.Background(), smi); err != nil {
		t.Fatalf("render identity CRS wrapper: %v", err)
	}

	wrapper := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "spoke-h-machine-identity", Namespace: "platform-capi"}, wrapper); err != nil {
		t.Fatalf("get identity wrapper: %v", err)
	}
	payload := string(wrapper.Data["identity.yaml"])
	if payload == "" {
		payload = wrapper.StringData["identity.yaml"]
	}

	wantSecretBlocks := []string{
		"name: infisical-auth\n  namespace: platform-ops",
		"name: infisical-auth\n  namespace: cert-manager",
	}
	for _, want := range wantSecretBlocks {
		if !strings.Contains(payload, want) {
			t.Errorf("rendered identity.yaml missing %q in:\n%s", want, payload)
		}
	}
	// Both copies: client-id/client-secret for ESO + issuer env, clientSecret
	// for the isser's hardcoded signer lookup.
	for _, key := range []string{"client-id", "client-secret", "clientSecret"} {
		if got := strings.Count(payload, key+":"); got != 2 {
			t.Errorf("expected %q exactly twice (platform-ops + cert-manager), got %d in:\n%s", key, got, payload)
		}
	}
}

// missing machine identity material -> no wrapper is ever published (fail-closed)
func TestEnsureClusterIssuerCRSWrapperEmptyIdentityNotPublished(t *testing.T) {
	t.Setenv("INFISICAL_URL", "https://infisical.dev.acme.example")
	scheme := newSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SpokeMachineIdentityReconciler{Client: c, ProjectID: "20958df6-ac81-4e11-8259-d1ddd1becb5c"}

	smi := newTestSMI("spoke-a", "")
	if err := r.ensureClusterIssuerCRSWrapper(context.Background(), smi); err != nil {
		t.Fatalf("expected nil error for deferred (empty) identity, got %v", err)
	}

	var secret corev1.Secret
	err := c.Get(context.Background(), client.ObjectKey{Name: "spoke-a-cluster-issuer", Namespace: "platform-capi"}, &secret)
	if err == nil {
		t.Fatalf("wrapper must NOT be published when clientId is empty, got secret %q", secret.Name)
	}
}

// empty projectId -> no wrapper is published
func TestEnsureClusterIssuerCRSWrapperEmptyProjectNotPublished(t *testing.T) {
	t.Setenv("INFISICAL_URL", "https://infisical.dev.acme.example")
	scheme := newSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SpokeMachineIdentityReconciler{Client: c} // ProjectID empty

	smi := newTestSMI("spoke-b", "b3d5ec56-623e-4a04-b82e-3960d84196c7")
	if err := r.ensureClusterIssuerCRSWrapper(context.Background(), smi); err != nil {
		t.Fatalf("expected nil error for deferred (empty) project, got %v", err)
	}

	var secret corev1.Secret
	err := c.Get(context.Background(), client.ObjectKey{Name: "spoke-b-cluster-issuer", Namespace: "platform-capi"}, &secret)
	if err == nil {
		t.Fatalf("wrapper must NOT be published when projectId is empty, got secret %q", secret.Name)
	}
}

// initial identity + project -> correct clientId and projectId rendered
func TestEnsureClusterIssuerCRSWrapperInitialRendersCompletedIssuer(t *testing.T) {
	t.Setenv("INFISICAL_URL", "https://infisical.dev.acme.example")
	scheme := newSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SpokeMachineIdentityReconciler{Client: c, ProjectID: "20958df6-ac81-4e11-8259-d1ddd1becb5c"}

	smi := newTestSMI("spoke-c", "b3d5ec56-623e-4a04-b82e-3960d84196c7")
	if err := r.ensureClusterIssuerCRSWrapper(context.Background(), smi); err != nil {
		t.Fatalf("render: %v", err)
	}

	_, yamlBody := getWrapperSecret(t, c, "spoke-c-cluster-issuer")
	for _, want := range []string{
		"kind: ClusterIssuer",
		"name: infisical-fleet-issuer",
		"clientId: b3d5ec56-623e-4a04-b82e-3960d84196c7",
		"projectId: 20958df6-ac81-4e11-8259-d1ddd1becb5c",
		"name: infisical-auth",
		"key: client-secret",
		"certificateTemplateName: infrastructure-services",
		// The BOX's Infisical, from INFISICAL_URL. It was asserted as a literal
		// platform host, which is what let the hardcoded constant survive: the
		// test passed precisely because the value was wrong in the same way.
		"url: https://infisical.dev.acme.example",
	} {
		if !strings.Contains(yamlBody, want) {
			t.Errorf("rendered ClusterIssuer missing %q in:\n%s", want, yamlBody)
		}
	}
}

// spec-level projectId overrides the operator flag projectId
func TestEnsureClusterIssuerCRSWrapperSpecProjectIDOverrides(t *testing.T) {
	t.Setenv("INFISICAL_URL", "https://infisical.dev.acme.example")
	scheme := newSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SpokeMachineIdentityReconciler{Client: c, ProjectID: "flag-project-id"}

	smi := newTestSMI("spoke-d", "b3d5ec56-623e-4a04-b82e-3960d84196c7")
	smi.Spec.Infisical.ProjectID = "spec-project-id"

	if err := r.ensureClusterIssuerCRSWrapper(context.Background(), smi); err != nil {
		t.Fatalf("render: %v", err)
	}

	_, yamlBody := getWrapperSecret(t, c, "spoke-d-cluster-issuer")
	if !strings.Contains(yamlBody, "projectId: spec-project-id") {
		t.Errorf("expected spec projectId to win, got:\n%s", yamlBody)
	}
	if strings.Contains(yamlBody, "projectId: flag-project-id") {
		t.Errorf("flag projectId must not be used when spec overrides:\n%s", yamlBody)
	}
}

// reconciliation with same values -> no unnecessary update (idempotent)
func TestEnsureClusterIssuerCRSWrapperIdempotentNoUpdate(t *testing.T) {
	t.Setenv("INFISICAL_URL", "https://infisical.dev.acme.example")
	scheme := newSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&identityv1alpha1.SpokeMachineIdentity{}).Build()
	r := &SpokeMachineIdentityReconciler{Client: c, ProjectID: "20958df6-ac81-4e11-8259-d1ddd1becb5c"}

	smi := newTestSMI("spoke-e", "b3d5ec56-623e-4a04-b82e-3960d84196c7")
	if err := r.ensureClusterIssuerCRSWrapper(context.Background(), smi); err != nil {
		t.Fatalf("first render: %v", err)
	}
	before, _ := getWrapperSecret(t, c, "spoke-e-cluster-issuer")

	// Second reconcile with identical material. The wrapper content is unchanged, so
	// no update should be issued. Assert via resourceVersion staying identical.
	if err := r.ensureClusterIssuerCRSWrapper(context.Background(), smi); err != nil {
		t.Fatalf("second render: %v", err)
	}
	after, _ := getWrapperSecret(t, c, "spoke-e-cluster-issuer")
	if before.ResourceVersion != after.ResourceVersion {
		t.Errorf("wrapper was updated unnecessarily: rv %s -> %s", before.ResourceVersion, after.ResourceVersion)
	}
}

// secret rotation keeps the same clientId (rotation-stable): wrapper must NOT be
// regenerated and the rendered clientId must remain stable across rotation.
func TestEnsureClusterIssuerCRSWrapperClientSecretRotationNoRegen(t *testing.T) {
	t.Setenv("INFISICAL_URL", "https://infisical.dev.acme.example")
	scheme := newSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&identityv1alpha1.SpokeMachineIdentity{}).Build()
	r := &SpokeMachineIdentityReconciler{Client: c, ProjectID: "20958df6-ac81-4e11-8259-d1ddd1becb5c"}

	stableClientID := "b3d5ec56-623e-4a04-b82e-3960d84196c7"
	smi := newTestSMI("spoke-f", stableClientID)
	if err := r.ensureClusterIssuerCRSWrapper(context.Background(), smi); err != nil {
		t.Fatalf("first render: %v", err)
	}
	before, yamlBefore := getWrapperSecret(t, c, "spoke-f-cluster-issuer")

	// Rotation rotates only the client secret; the clientId is stable. Reconcile again.
	if err := r.ensureClusterIssuerCRSWrapper(context.Background(), smi); err != nil {
		t.Fatalf("re-render after rotation: %v", err)
	}
	after, yamlAfter := getWrapperSecret(t, c, "spoke-f-cluster-issuer")

	if before.ResourceVersion != after.ResourceVersion {
		t.Errorf("rotation must not regenerate the wrapper (rv %s -> %s)", before.ResourceVersion, after.ResourceVersion)
	}
	if yamlBefore != yamlAfter {
		t.Errorf("rotation must not change the rendered ClusterIssuer payload")
	}
	if !strings.Contains(yamlAfter, "clientId: "+stableClientID) {
		t.Errorf("rotation-stable clientId missing from payload:\n%s", yamlAfter)
	}
}

// clientId actually changes (identity recreated): wrapper updates so future
// (re)provisioned spokes receive the new identifier.
func TestEnsureClusterIssuerCRSWrapperClientIDChangesUpdatesWrapper(t *testing.T) {
	t.Setenv("INFISICAL_URL", "https://infisical.dev.acme.example")
	scheme := newSchemeForTest(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&identityv1alpha1.SpokeMachineIdentity{}).Build()
	r := &SpokeMachineIdentityReconciler{Client: c, ProjectID: "20958df6-ac81-4e11-8259-d1ddd1becb5c"}

	smi := newTestSMI("spoke-g", "old-client-id-00000000-0000-0000-0000-000000000000")
	if err := r.ensureClusterIssuerCRSWrapper(context.Background(), smi); err != nil {
		t.Fatalf("first render: %v", err)
	}

	smi.Status.ClientID = "new-client-id-11111111-1111-1111-1111-111111111111"
	if err := r.ensureClusterIssuerCRSWrapper(context.Background(), smi); err != nil {
		t.Fatalf("re-render after clientId change: %v", err)
	}

	_, yamlBody := getWrapperSecret(t, c, "spoke-g-cluster-issuer")
	if !strings.Contains(yamlBody, "clientId: new-client-id-11111111-1111-1111-1111-111111111111") {
		t.Errorf("updated clientId not present in wrapper payload:\n%s", yamlBody)
	}
	if strings.Contains(yamlBody, "old-client-id") {
		t.Errorf("stale clientId still present in wrapper payload:\n%s", yamlBody)
	}
}

// --- Phase 2 lockout self-heal (P0 single authority) ---

const testSpokeClientID = "b3d5ec56-623e-4a04-b82e-3960d84196c7"

func newSelfHealReconciler(t *testing.T, handler http.HandlerFunc) (*SpokeMachineIdentityReconciler, client.Client, *httptest.Server) {
	t.Helper()
	t.Setenv("INFISICAL_CLIENT_ID", "op-client")
	t.Setenv("INFISICAL_CLIENT_SECRET", "op-secret")

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
