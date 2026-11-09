package main

import (
	"context"
	"fmt"
	"os"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/bootstrap"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/teardown"
)

// providerOwnedCounts answers what each named cluster still owns at the provider.
//
// The SAME inventory the teardown deletes from, and the same ownership rule:
// exact label key per cluster with the value `owned`. Asking one question of two
// implementations is how a gate comes to allow what a teardown cannot remove, or
// refuse what is already gone.
var providerOwnedCounts bootstrap.OwnedCounts = func(ctx context.Context, clusters []string) (map[string]int, error) {
	if err := resolveHCloudToken(); err != nil {
		return nil, fmt.Errorf("no provider credential, so ownership cannot be established: %w", err)
	}
	token := os.Getenv("HCLOUD_TOKEN")
	inv, err := teardown.ProviderInventory(ctx, token, clusters)
	if err != nil {
		return nil, err
	}
	return teardown.OwnedByCluster(inv.Resources(), clusters), nil
}
