package health

import (
	"testing"
)

func TestExternalSecretsCount(t *testing.T) {
	checkers := ConvergedCheckers()
	var esChecker *jsonCountChecker
	for _, c := range checkers {
		if jc, ok := c.(*jsonCountChecker); ok && jc.name == "ExternalSecrets synced" {
			esChecker = jc
			break
		}
	}
	if esChecker == nil {
		t.Fatal("ExternalSecrets synced checker not found")
	}

	items := []map[string]any{
		{
			"metadata": map[string]any{"name": "sec1", "namespace": "default"},
			"status": map[string]any{
				"conditions": []any{
					map[string]any{"type": "Ready", "status": "True"},
				},
			},
		},
		{
			// Optional secret that is NOT ready - should be skipped from bad and total
			"metadata": map[string]any{"name": "ghcr-pull-secret-credentials", "namespace": "platform-controlplane"},
			"spec": map[string]any{
				"target": map[string]any{"name": "ghcr-pull-secret"},
			},
			"status": map[string]any{
				"conditions": []any{
					map[string]any{"type": "Ready", "status": "False"},
				},
			},
		},
		{
			// Another optional secret that IS ready - should be counted in total and not bad
			"metadata": map[string]any{"name": "ghcr-pull-secret-credentials", "namespace": "platform-edge"},
			"spec": map[string]any{
				"target": map[string]any{"name": "ghcr-pull-secret"},
			},
			"status": map[string]any{
				"conditions": []any{
					map[string]any{"type": "Ready", "status": "True"},
				},
			},
		},
	}

	bad, total, detail := esChecker.count(items)
	if bad != 0 {
		t.Errorf("expected 0 bad secrets, got %d (detail: %s)", bad, detail)
	}
	if total != 2 {
		t.Errorf("expected 2 total secrets (excluding the unready optional secret), got %d", total)
	}
}
