package secrets

import (
	"context"
	"testing"
)

// The box's registry credential reaches an application's scope only when that
// application declares private images, and only its own scope. A public
// application holds none; one that becomes public has it withdrawn; and another
// tenant's application on the same box never receives it by being a neighbour.
func TestRegistryCredentialReachesOnlyPrivateImageApplications(t *testing.T) {
	s, srv := storeWith(t, nil, nil)
	srv.kv[HubScope] = map[string]any{RegistryPullSecretKey: `{"auths":{"ghcr.io":{}}}`}
	srv.version[HubScope] = 1
	ctx := context.Background()

	// shop starts with private images and receives the credential...
	if _, err := s.EnsureTenantFolderAndCredentials(ctx, "c1", "acme", "shop", true, nil, false, true, false, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.kv[tenantScope][RegistryPullSecretKey]; !ok {
		t.Fatal("a private-image application did not receive the registry credential")
	}
	// ...then declares them public: the copy it held is withdrawn.
	if _, err := s.EnsureTenantFolderAndCredentials(ctx, "c1", "acme", "shop", false, nil, false, false, false, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.kv[tenantScope][RegistryPullSecretKey]; ok {
		t.Fatal("an application that became public kept the registry credential")
	}

	// web declares private images: it receives the hub's value, in its own scope.
	if _, err := s.EnsureTenantFolderAndCredentials(ctx, "c1", "acme", "web", true, nil, false, true, false, true); err != nil {
		t.Fatal(err)
	}
	if got := srv.kv["spokes/c1/tenants/web"][RegistryPullSecretKey]; got != `{"auths":{"ghcr.io":{}}}` {
		t.Fatalf("private-image application got %v", got)
	}
	if _, ok := srv.kv[tenantScope][RegistryPullSecretKey]; ok {
		t.Fatal("delivering to one application wrote the credential into another's scope")
	}

	// another tenant's public application on the same cluster: nothing.
	if _, err := s.EnsureTenantFolderAndCredentials(ctx, "c1", "other", "api", true, nil, false, true, false, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.kv["spokes/c1/tenants/api"][RegistryPullSecretKey]; ok {
		t.Fatal("a neighbouring tenant's public application received the registry credential")
	}
}
