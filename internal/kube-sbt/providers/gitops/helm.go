package gitops

import (
	"fmt"
	"strings"
)

// tierResources defines Kubernetes resource quotas per tier (15.5, 15.7).
var tierResources = map[string]map[string]string{
	"basic": {
		"requestsCPU":    "500m",
		"requestsMemory": "1Gi",
		"limitsCPU":      "1",
		"limitsMemory":   "2Gi",
	},
	"standard": {
		"requestsCPU":    "1",
		"requestsMemory": "2Gi",
		"limitsCPU":      "2",
		"limitsMemory":   "4Gi",
	},
	"premium": {
		"requestsCPU":    "2",
		"requestsMemory": "4Gi",
		"limitsCPU":      "4",
		"limitsMemory":   "8Gi",
	},
	"enterprise": {
		"requestsCPU":    "4",
		"requestsMemory": "8Gi",
		"limitsCPU":      "8",
		"limitsMemory":   "16Gi",
	},
}

// tierDatabaseType maps tier to database isolation strategy.
var tierDatabaseType = map[string]string{
	"basic":      "shared",
	"standard":   "shared",
	"premium":    "dedicated",
	"enterprise": "dedicated",
}

// generateHelmValues produces the tenant values.yaml content for the
// Universal Tenant Helm Chart (15.4, 15.5).
// AN UNKNOWN TIER IS AN ERROR, not a basic tenant.
//
// This defaulted twice over: unknown resources became tierResources["basic"],
// and an unmapped database type became "shared". Both are written into the
// tenant's own values.yaml, so a tier this code does not recognise produced a
// committed downgrade -- basic limits and the shared database for a tenant
// entitled to dedicated -- with nothing anywhere reporting it. The two tables
// above are the authority on what a tier means; a tier absent from them is one
// this build cannot provision.
func generateHelmValues(tenantID, tier, email string, extra map[string]interface{}) ([]byte, error) {
	res := tierResources[tier]
	dbType := tierDatabaseType[tier]
	if res == nil || dbType == "" {
		return nil, fmt.Errorf("tenant %s: tier %q is not one this build knows how to "+
			"provision, and there is no safe default: writing one would commit resources "+
			"and a database topology the tenant may not be entitled to", tenantID, tier)
	}

	// Dedicated DB name for premium/enterprise
	dbName := "shared-db"
	if dbType == "dedicated" {
		dbName = fmt.Sprintf("tenant-%s-db", tenantID)
	}

	// ArgoCD agent mode: managed for basic/standard, autonomous for premium/enterprise
	agentMode := "managed"
	if tier == "premium" || tier == "enterprise" {
		agentMode = "autonomous"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("tenantId: %q\n", tenantID))
	sb.WriteString(fmt.Sprintf("tier: %q\n", tier))
	sb.WriteString(fmt.Sprintf("ownerEmail: %q\n", email))
	sb.WriteString(fmt.Sprintf("assigned: true\n"))

	// Resources (15.7)
	sb.WriteString("resources:\n")
	sb.WriteString(fmt.Sprintf("  requestsCPU: %q\n", res["requestsCPU"]))
	sb.WriteString(fmt.Sprintf("  requestsMemory: %q\n", res["requestsMemory"]))
	sb.WriteString(fmt.Sprintf("  limitsCPU: %q\n", res["limitsCPU"]))
	sb.WriteString(fmt.Sprintf("  limitsMemory: %q\n", res["limitsMemory"]))

	// Database (15.10 — Crossplane XR hint)
	sb.WriteString("database:\n")
	sb.WriteString(fmt.Sprintf("  type: %q\n", dbType))
	sb.WriteString(fmt.Sprintf("  name: %q\n", dbName))

	// ArgoCD agent (16.4, 16.5)
	sb.WriteString("argocdAgent:\n")
	sb.WriteString(fmt.Sprintf("  enabled: true\n"))
	sb.WriteString(fmt.Sprintf("  mode: %q\n", agentMode))

	// Extra values from caller
	for k, v := range extra {
		sb.WriteString(fmt.Sprintf("%s: %v\n", k, v))
	}

	return []byte(sb.String()), nil
}

// generateWarmSlotValues produces values.yaml for an unassigned warm pool slot (15.11).
func generateWarmSlotValues(slotID, tier string) []byte {
	res := tierResources[tier]
	if res == nil {
		res = tierResources["basic"]
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("tenantId: %q\n", slotID))
	sb.WriteString(fmt.Sprintf("tier: %q\n", tier))
	sb.WriteString("assigned: false\n")
	sb.WriteString("resources:\n")
	sb.WriteString(fmt.Sprintf("  requestsCPU: %q\n", res["requestsCPU"]))
	sb.WriteString(fmt.Sprintf("  requestsMemory: %q\n", res["requestsMemory"]))
	sb.WriteString(fmt.Sprintf("  limitsCPU: %q\n", res["limitsCPU"]))
	sb.WriteString(fmt.Sprintf("  limitsMemory: %q\n", res["limitsMemory"]))
	sb.WriteString("database:\n")
	sb.WriteString(fmt.Sprintf("  type: %q\n", tierDatabaseType[tier]))
	sb.WriteString("argocdAgent:\n")
	sb.WriteString("  enabled: false\n")
	return []byte(sb.String())
}
