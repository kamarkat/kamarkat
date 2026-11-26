package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kamarkat/kamarkat/internal/platform/escrow"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/constant"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/openbao"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/secrets"
)

// A credential enrolled in the escrow after Day-0 reaches the hub scope on the
// next reconcile, and an unchanged one is not rewritten.
func TestRegistryCredentialConvergesFromTheEscrow(t *testing.T) {
	kv := &miniKV{data: map[string]map[string]any{}}
	ts := httptest.NewServer(kv)
	t.Cleanup(ts.Close)
	r := &HubEnvironmentReconciler{Store: secrets.NewStore(openbao.NewWithClient(ts.URL, ts.Client(), "t"))}
	store := newMockEscrowStore()
	ctx := context.Background()

	if err := r.syncRegistryCredential(ctx, store, "box1"); err != nil {
		t.Fatal(err)
	}
	if kv.writes != 0 {
		t.Fatal("a box with no escrowed registry credential had one written")
	}

	store.artifacts["box1/box1/"+escrow.ArtifactRegistryCredential] = "username=acme-bot\ntoken=ghp_x"
	if err := r.syncRegistryCredential(ctx, store, "box1"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := kv.data[secrets.HubScope][secrets.RegistryPullSecretKey].(string)
	if user, token, ok := registryLogin(cfg, registryHost); !ok || user != "acme-bot" || token != "ghp_x" {
		t.Fatalf("hub scope holds %q", cfg)
	}
	if err := r.syncRegistryCredential(ctx, store, "box1"); err != nil || kv.writes != 1 {
		t.Fatalf("an unchanged credential was rewritten (writes=%d, err=%v)", kv.writes, err)
	}
}

// The GitOps engine holds a credential for an application's chart repository
// only while that application declares private charts, for exactly the
// repository it declares.
func TestChartRegistryCredentialFollowsTheDeclaration(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := newFakeSecretStore()
	store.data[k(secrets.HubScope, secrets.RegistryPullSecretKey)] = dockerConfig("ghcr.io", "acme-bot", "ghp_x")
	r := &AINativeSaaSReconciler{Client: c, Store: store}
	ctx := context.Background()
	key := types.NamespacedName{Namespace: constant.NamespaceOps, Name: chartRegistrySecretName("acme", "shop")}

	if err := r.reconcileChartRegistry(ctx, "acme", "shop", true, "oci://ghcr.io/acme/charts"); err != nil {
		t.Fatal(err)
	}
	var s corev1.Secret
	if err := c.Get(ctx, key, &s); err != nil {
		t.Fatal(err)
	}
	if s.Labels["argocd.argoproj.io/secret-type"] != "repository" || string(s.Data["url"]) != "ghcr.io/acme/charts" ||
		string(s.Data["enableOCI"]) != "true" || string(s.Data["password"]) != "ghp_x" {
		t.Fatalf("repository credential is %v %v", s.Labels, s.Data)
	}

	if err := r.reconcileChartRegistry(ctx, "acme", "shop", false, ""); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, &s); !apierrors.IsNotFound(err) {
		t.Fatalf("a public-chart application kept its repository credential: %v", err)
	}
	if err := r.reconcileChartRegistry(ctx, "acme", "web", false, ""); err != nil {
		t.Fatal(err)
	}
}

// miniKV is just enough of OpenBao's KV v2 for the hub scope.
type miniKV struct {
	mu     sync.Mutex
	data   map[string]map[string]any
	writes int
}

func (m *miniKV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v1/kv/data/")
	if r.Method == http.MethodGet {
		d, ok := m.data[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": d, "metadata": map[string]any{"version": 1}}})
		return
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	m.data[p] = body.Data
	m.writes++
	w.WriteHeader(http.StatusNoContent)
}
