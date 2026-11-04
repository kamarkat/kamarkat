package teardown

import (
	"context"
	"testing"
	"time"
)

func TestOrchestratorInit(t *testing.T) {
	orch := &Orchestrator{
		ClusterName: "test-cluster",
		Force:       true,
		Debug:       true,
	}

	if orch.ClusterName != "test-cluster" {
		t.Errorf("expected ClusterName 'test-cluster', got %s", orch.ClusterName)
	}
	if !orch.Force {
		t.Errorf("expected Force to be true")
	}
	if !orch.Debug {
		t.Errorf("expected Debug to be true")
	}
}

func TestOrchestratorRunContextTimeout(t *testing.T) {
	orch := &Orchestrator{
		ClusterName: "nonexistent-test-cluster",
		Force:       true,
		Debug:       false,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err := orch.Run(ctx)
	if err != nil {
		t.Errorf("unexpected error on teardown of nonexistent cluster: %v", err)
	}
}
