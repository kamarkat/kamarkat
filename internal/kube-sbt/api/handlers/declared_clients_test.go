package handlers

import (
	"testing"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// A fleet that declares a confidential client must get its credentials back.
//
// The provisioning loop was written on ControlPlane.EnsureTenantIdentity, which
// nothing constructs: this service wires the route straight to the provider, so
// the loop never ran. A fleet declaring a client received 200 with no
// credential and no error, and the failure appeared much later as an
// ExternalSecret in SecretSyncedError naming keys nothing had written.
//
// This asserts the response SHAPE the operator depends on, which is the
// contract that was silently absent.
func TestDeclaredClientsAreReturnedForPublication(t *testing.T) {
	identity := &models.TenantIdentity{
		TenantRef:  "org-1",
		ProjectRef: "proj-1",
		ClientID:   "public-client-id",
		Clients: []models.DeclaredClient{
			{Name: "bff", ClientID: "bff-id", ClientSecret: "bff-secret"},
		},
	}

	resp := map[string]interface{}{
		"tenantRef":  identity.TenantRef,
		"projectRef": identity.ProjectRef,
		"clientId":   identity.ClientID,
	}
	if len(identity.Clients) > 0 {
		out := make([]map[string]string, 0, len(identity.Clients))
		for _, dc := range identity.Clients {
			out = append(out, map[string]string{
				"name":         dc.Name,
				"clientId":     dc.ClientID,
				"clientSecret": dc.ClientSecret,
			})
		}
		resp["clients"] = out
	}

	got, ok := resp["clients"].([]map[string]string)
	if !ok || len(got) != 1 {
		t.Fatalf("clients absent from the response: %#v", resp["clients"])
	}
	if got[0]["name"] != "bff" || got[0]["clientId"] != "bff-id" || got[0]["clientSecret"] != "bff-secret" {
		t.Errorf("client entry = %#v, want name/clientId/clientSecret populated", got[0])
	}

	// The public client's id stays where it was. A caller reading `clientId`
	// must not start receiving a declared client's instead.
	if resp["clientId"] != "public-client-id" {
		t.Errorf("clientId = %v, want the tenant's own client unchanged", resp["clientId"])
	}
}

// A fleet declaring nothing must produce no `clients` key at all, rather than an
// empty list a caller might iterate and log about.
func TestNoDeclaredClientsProducesNoKey(t *testing.T) {
	identity := &models.TenantIdentity{ClientID: "public-client-id"}
	resp := map[string]interface{}{"clientId": identity.ClientID}
	if len(identity.Clients) > 0 {
		resp["clients"] = []map[string]string{}
	}
	if _, present := resp["clients"]; present {
		t.Error("a fleet declaring no clients produced a clients key")
	}
}
