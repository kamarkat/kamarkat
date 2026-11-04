package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	client2 "github.com/kamarkat/kamarkat/operators/hub-operator/internal/client"
)

// ADR-095: the gateway authenticates its token exchange as a CONFIDENTIAL
// client, and that client is the platform's, not the fleet's.

func xr(clients ...interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if clients != nil {
		_ = unstructured.SetNestedSlice(u.Object, clients, "spec", "oauth", "clients")
	}
	return u
}

func TestTheExchangeClientIsAppendedWhateverTheFleetDeclared(t *testing.T) {
	// A fleet that declares nothing must still get one: the exchange is how the
	// platform delivers a credential to the backend, not a feature to opt into.
	// Requiring the declaration would be the platform depending on a tenant to
	// ask for its own plumbing (ADR-047).
	for name, obj := range map[string]*unstructured.Unstructured{
		"no oauth stanza at all": xr(nil),
		"an empty client list":   xr(),
		"one declared client": xr(map[string]interface{}{
			"name": "bff", "type": "confidential",
		}),
	} {
		t.Run(name, func(t *testing.T) {
			got := append(declaredOAuthClients(obj), gatewayExchangeClient("waypoint"))

			var found *client2.OAuthClient
			for i := range got {
				if got[i].Name == gatewayExchangeClientName("waypoint") {
					found = &got[i]
				}
			}
			if found == nil {
				t.Fatalf("no exchange client in %+v", got)
			}
			if !found.Confidential {
				// A public client would let a BROWSER perform the same exchange,
				// and then the exchange is not a boundary at all.
				t.Error("the exchange client must be confidential")
			}
			if len(found.RedirectURIs) != 0 {
				// It never runs a browser flow. A registered redirect would be a
				// login path nobody intended to expose.
				t.Errorf("expected no redirect URIs, got %v", found.RedirectURIs)
			}
			if found.ActsForUser {
				// An exchanged token takes whatever subject it was handed, so the
				// exchange client never carries a person to a target (ADR-110).
				t.Error("the exchange client must not act for users")
			}
		})
	}
}

func TestTheExchangeClientIsPerApp(t *testing.T) {
	// ADR-088: a gateway and its OAuth client belong to one product. Sharing one
	// exchange credential across a tenant's apps would let one app's gateway
	// mint tokens carrying the other's audience.
	if a, b := gatewayExchangeClientName("waypoint"), gatewayExchangeClientName("oranger"); a == b {
		t.Fatalf("two apps share one exchange client name: %q", a)
	}
}

func TestTheFleetCannotSupplyTheExchangeClient(t *testing.T) {
	// A fleet naming this client would be naming the holder of a credential that
	// mints tokens for an API. The platform's entry is appended AFTER the
	// declaration, so it is the one the gateway's credential resolves to, and a same-named fleet entry cannot stand in for it.
	obj := xr(map[string]interface{}{
		"name": "waypoint-gateway-exchange", "type": "public",
	})
	got := append(declaredOAuthClients(obj), gatewayExchangeClient("waypoint"))

	last := got[len(got)-1]
	if last.Name != gatewayExchangeClientName("waypoint") || !last.Confidential {
		t.Fatalf("the platform's entry must be last and confidential, got %+v", last)
	}
}

func TestADeclaredCallbackIsRegisteredOnEveryGatewayHost(t *testing.T) {
	// ADR-110: the code flow fails at the authorization endpoint unless the
	// callback is registered exactly, and the gateway answers on every host.
	obj := xr(map[string]interface{}{
		"name": "bff", "type": "confidential",
		"redirectPaths": []interface{}{"/api/v1/auth/callback"},
	})
	_ = unstructured.SetNestedStringSlice(obj.Object, []string{"a.example", "b.example"}, "spec", "gateway", "hostnames")

	got := declaredOAuthClients(obj)
	if len(got) != 1 {
		t.Fatalf("want one client, got %+v", got)
	}
	want := []string{"https://a.example/api/v1/auth/callback", "https://b.example/api/v1/auth/callback"}
	if len(got[0].RedirectURIs) != len(want) || got[0].RedirectURIs[0] != want[0] || got[0].RedirectURIs[1] != want[1] {
		t.Fatalf("RedirectURIs = %v; want %v", got[0].RedirectURIs, want)
	}
}

func TestOnlyOneConfidentialClientMayActForUsers(t *testing.T) {
	for name, tc := range map[string]struct {
		clients []client2.OAuthClient
		want    string
		wantErr bool
	}{
		"none declared":     {clients: []client2.OAuthClient{{Name: "bff", Confidential: true}}, want: ""},
		"one, confidential": {clients: []client2.OAuthClient{{Name: "bff", Confidential: true, ActsForUser: true}}, want: "bff"},
		"a public client":   {clients: []client2.OAuthClient{{Name: "web", ActsForUser: true}}, wantErr: true},
		"two clients": {clients: []client2.OAuthClient{
			{Name: "bff", Confidential: true, ActsForUser: true},
			{Name: "api", Confidential: true, ActsForUser: true},
		}, wantErr: true},
		"holds the exchange grant": {clients: []client2.OAuthClient{
			{Name: "x", Confidential: true, ActsForUser: true, TokenExchange: true},
		}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := actingClientOf(tc.clients)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want a refusal, got %q", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("actingClientOf = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
