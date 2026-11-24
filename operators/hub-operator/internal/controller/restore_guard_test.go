package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/openbao"
)

// The guard decides from ADR-092's own state: is openbao-db a restore, and is the
// recovery record of THIS incarnation complete.
func TestRestoreGuardFollowsTheIncarnationsRecoveryRecord(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := func(restored bool) *cnpgv1.Cluster {
		c := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{
			Name: openBaoDatabase, Namespace: openBaoNamespace, UID: types.UID("u2")}}
		c.Spec.Bootstrap = &cnpgv1.BootstrapConfiguration{}
		if restored {
			c.Spec.Bootstrap.Recovery = &cnpgv1.BootstrapRecovery{Source: "openbao-db-archive"}
		} else {
			c.Spec.Bootstrap.InitDB = &cnpgv1.BootstrapInitDB{}
		}
		return c
	}
	record := map[string]string{} // incarnation -> record JSON
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := strings.TrimPrefix(r.URL.Path, "/v1/kv/data/platform/recovery/openbao-db/")
		rec, ok := record[uid]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]string{"record": rec}}})
	}))
	defer srv.Close()
	bao := openbao.NewWithClient(srv.URL, srv.Client(), "t")
	ctx := context.Background()

	cases := []struct {
		name    string
		objs    []*cnpgv1.Cluster
		rec     string
		refused bool
	}{
		{"a box being built (no store yet)", nil, "", false},
		{"a store never restored", []*cnpgv1.Cluster{cluster(false)}, "", false},
		{"a restore with no record yet", []*cnpgv1.Cluster{cluster(true)}, "", true},
		{"a restore part-way through", []*cnpgv1.Cluster{cluster(true)},
			`{"steps":{"authorization":"2026-11-24T10:00:00Z"}}`, true},
		{"a restore whose recovery is complete", []*cnpgv1.Cluster{cluster(true)},
			`{"steps":{},"completed":"2026-11-24T11:00:00Z"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(scheme)
			for _, o := range tc.objs {
				b = b.WithObjects(o)
			}
			delete(record, "u2")
			if tc.rec != "" {
				record["u2"] = tc.rec
			}
			err := NewRestoreGuard(b.Build(), bao)(ctx)
			if tc.refused != errors.Is(err, ErrRestoreUnfinished) {
				t.Fatalf("refused=%v, want %v (err %v)", err != nil, tc.refused, err)
			}
		})
	}
}
