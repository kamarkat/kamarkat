// Package appplaneutils implements IApplicationPlaneUtils (Tasks 31.1–31.10).
package appplaneutils

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/interfaces"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)
// Utils implements interfaces.IApplicationPlaneUtils.
type Utils struct {
	provisioner interfaces.IProvisioner
	mu          sync.RWMutex
	customTypes map[string]customResourceType // 31.6
}

type customResourceType struct {
	Name         string
	RequiredKeys []string
	CostPerHour  float64
}

// New creates a Utils. provisioner is used for batch operations (31.10).
func New(provisioner interfaces.IProvisioner) *Utils {
	return &Utils{
		provisioner: provisioner,
		customTypes: make(map[string]customResourceType),
	}
}

// ─── 31.1 Resource Configuration Validation ──────────────────────────────────

func (u *Utils) ValidateResourceConfig(_ context.Context, spec models.ResourceSpec) error {
	if spec.Type == "" {
		return fmt.Errorf("appplaneutils: resource type is required")
	}
	if spec.Name == "" {
		return fmt.Errorf("appplaneutils: resource name is required")
	}
	known := map[string]bool{
		"namespace": true, "database": true, "s3bucket": true,
		"redis": true, "cluster": true,
	}
	u.mu.RLock()
	_, isCustom := u.customTypes[spec.Type]
	u.mu.RUnlock()
	if !known[spec.Type] && !isCustom {
		return fmt.Errorf("appplaneutils: unknown resource type %q", spec.Type)
	}
	if isCustom {
		u.mu.RLock()
		ct := u.customTypes[spec.Type]
		u.mu.RUnlock()
		for _, k := range ct.RequiredKeys {
			if _, ok := spec.Parameters[k]; !ok {
				return fmt.Errorf("appplaneutils: custom resource %q missing required parameter %q", spec.Type, k)
			}
		}
	}
	return nil
}

// ─── 31.5 Resource Dependency Validation ─────────────────────────────────────

func (u *Utils) ValidateResourceDependencies(_ context.Context, specs []models.ResourceSpec) error {
	types := make(map[string]bool, len(specs))
	for _, s := range specs {
		types[s.Type] = true
	}
	for _, dep := range []string{"database", "redis", "s3bucket"} {
		if types[dep] && !types["namespace"] {
			return fmt.Errorf("appplaneutils: resource type %q requires a namespace resource", dep)
		}
	}
	return nil
}

// ─── 31.2 Resource Naming ─────────────────────────────────────────────────────

func (u *Utils) GenerateResourceName(tenantID, resourceType string) string {
	tid := tenantID
	if len(tid) > 8 {
		tid = tid[:8]
	}
	return fmt.Sprintf("tenant-%s-%s", tid, resourceType)
}

// ─── 31.3 Resource Requirements ──────────────────────────────────────────────

var tierSpecs = map[string][]models.ResourceSpec{
	"basic": {
		{Type: "namespace", Name: ""},
		{Type: "database", Name: "", Parameters: map[string]interface{}{"size": "small", "shared": true}},
	},
	"standard": {
		{Type: "namespace", Name: ""},
		{Type: "database", Name: "", Parameters: map[string]interface{}{"size": "medium", "shared": true}},
	},
	"premium": {
		{Type: "namespace", Name: ""},
		{Type: "database", Name: "", Parameters: map[string]interface{}{"size": "medium", "shared": false}},
		{Type: "s3bucket", Name: ""},
	},
	"enterprise": {
		{Type: "namespace", Name: ""},
		{Type: "database", Name: "", Parameters: map[string]interface{}{"size": "large", "replicas": 3}},
		{Type: "s3bucket", Name: ""},
		{Type: "redis", Name: ""},
		{Type: "cluster", Name: ""},
	},
}

func (u *Utils) CalculateResourceRequirements(_ context.Context, tier string) ([]models.ResourceSpec, error) {
	specs, ok := tierSpecs[tier]
	if !ok {
		return nil, fmt.Errorf("appplaneutils: unknown tier %q", tier)
	}
	out := make([]models.ResourceSpec, len(specs))
	copy(out, specs)
	return out, nil
}

// ─── 31.4 Template Generation ────────────────────────────────────────────────

var resourceTemplates = map[string]string{
	"namespace": `apiVersion: v1
kind: Namespace
metadata:
  name: {{.name}}
  labels:
    tenant-id: {{.tenant_id}}
    tier: {{.tier}}`,
	"database": `apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: {{.name}}
  namespace: {{.namespace}}
spec:
  instances: {{.replicas}}
  storage:
    size: {{.storage_size}}`,
	"s3bucket": `apiVersion: s3.crossplane.io/v1alpha1
kind: Bucket
metadata:
  name: {{.name}}
spec:
  forProvider:
    region: {{.region}}`,
}

func (u *Utils) GenerateResourceTemplate(_ context.Context, resourceType string, params map[string]interface{}) (string, error) {
	tmplStr, ok := resourceTemplates[resourceType]
	if !ok {
		u.mu.RLock()
		_, isCustom := u.customTypes[resourceType]
		u.mu.RUnlock()
		if !isCustom {
			return "", fmt.Errorf("appplaneutils: no template for resource type %q", resourceType)
		}
		tmplStr = `# custom resource: {{.name}}`
	}
	tmpl, err := template.New(resourceType).Option("missingkey=zero").Parse(tmplStr)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, params); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ─── 31.6 Custom Resource Type Registration ───────────────────────────────────

func (u *Utils) RegisterCustomResourceType(name string, requiredKeys []string, costPerHour float64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.customTypes[name] = customResourceType{Name: name, RequiredKeys: requiredKeys, CostPerHour: costPerHour}
}

// ─── 31.7 Cost Estimation ─────────────────────────────────────────────────────

var baseCostPerHour = map[string]float64{
	"namespace": 0.0,
	"database":  0.05,
	"s3bucket":  0.01,
	"redis":     0.03,
	"cluster":   0.50,
}

func (u *Utils) EstimateResourceCost(_ context.Context, specs []models.ResourceSpec) (float64, error) {
	var total float64
	for _, s := range specs {
		cost := baseCostPerHour[s.Type]
		if cost == 0 {
			u.mu.RLock()
			ct, isCustom := u.customTypes[s.Type]
			u.mu.RUnlock()
			if isCustom {
				cost = ct.CostPerHour
			}
		}
		total += cost
	}
	return total, nil
}

// ─── 31.8 Resource Migration Utilities ───────────────────────────────────────

// PlanTierMigration returns ordered steps to migrate between tiers (31.8).
func (u *Utils) PlanTierMigration(fromTier, toTier string) ([]interfaces.MigrationStep, error) {
	from, ok := tierSpecs[fromTier]
	if !ok {
		return nil, fmt.Errorf("appplaneutils: unknown from-tier %q", fromTier)
	}
	to, ok := tierSpecs[toTier]
	if !ok {
		return nil, fmt.Errorf("appplaneutils: unknown to-tier %q", toTier)
	}
	fromTypes, toTypes := resourceTypeSet(from), resourceTypeSet(to)
	var steps []interfaces.MigrationStep
	order := 1
	for t := range toTypes {
		if !fromTypes[t] {
			steps = append(steps, interfaces.MigrationStep{Order: order, Action: "add", ResourceType: t,
				Description: fmt.Sprintf("provision %s for %s tier", t, toTier)})
			order++
		}
	}
	for t := range fromTypes {
		if !toTypes[t] {
			steps = append(steps, interfaces.MigrationStep{Order: order, Action: "remove", ResourceType: t,
				Description: fmt.Sprintf("deprovision %s (not in %s tier)", t, toTier)})
			order++
		}
	}
	return steps, nil
}

func resourceTypeSet(specs []models.ResourceSpec) map[string]bool {
	m := make(map[string]bool, len(specs))
	for _, s := range specs {
		m[s.Type] = true
	}
	return m
}

// ─── 31.9 Health Check Utilities ─────────────────────────────────────────────

func (u *Utils) CheckResourceHealth(ctx context.Context, tenantID, _ string) (string, error) {
	status, err := u.provisioner.GetProvisioningStatus(ctx, tenantID)
	if err != nil {
		return "unknown", err
	}
	switch strings.ToLower(status.Status) {
	case "active", "ready", "synced":
		return "healthy", nil
	case "provisioning", "pending":
		return "provisioning", nil
	case "failed", "error":
		return "failed", nil
	default:
		return "unknown", nil
	}
}

// ─── 31.10 Batch Operations ───────────────────────────────────────────────────

func (u *Utils) BatchProvisionResources(ctx context.Context, requests []models.ProvisionRequest) ([]models.ProvisionResult, error) {
	type item struct {
		idx int
		res *models.ProvisionResult
		err error
	}
	ch := make(chan item, len(requests))
	for i, req := range requests {
		go func(idx int, r models.ProvisionRequest) {
			res, err := u.provisioner.ProvisionTenant(ctx, r)
			ch <- item{idx: idx, res: res, err: err}
		}(i, req)
	}
	results := make([]models.ProvisionResult, len(requests))
	var firstErr error
	for range requests {
		it := <-ch
		if it.err != nil && firstErr == nil {
			firstErr = it.err
		}
		if it.res != nil {
			results[it.idx] = *it.res
		}
	}
	return results, firstErr
}

// compile-time interface satisfaction check
var _ interfaces.IApplicationPlaneUtils = (*Utils)(nil)

// suppress unused import
var _ = time.Now
