package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kamarkat/kamarkat/internal/platform"
)

// A vendored CRD whose schema a shipped custom resource depends on.
//
// ArgoCD compares a desired resource against the live cluster by DRY-RUN
// APPLYING it, so a release that changes a CRD's schema and the resources using
// the new fields in the same Application cannot sync: the API server still
// serves the old CRD, rejects the dry-run with 422, and ArgoCD raises
// ComparisonError -- which stops it reaching the sync that would install the new
// CRD. The Application that would fix the problem is the one blocked by it.
//
// Observed on 2026-11-19, two releases after the issuer migration shipped:
//
//	serverSideDiff error: ... failed to create typed patch object:
//	ClusterIssuer/infisical-signing-issuer: .spec.application: field not
//	declared in schema
//
// Every certificate on the box then waited on an issuer that could not be
// reconfigured, and the bootstrap timed out waiting for a workload whose
// certificate renewal was stuck behind it.
//
// Day-0 breaks the circle, because Day-0 applies directly and has no comparison
// step to deadlock. Not an Application: an Application carrying these would need
// the same comparison, against the same stale schema.
type vendoredCRD struct {
	// name is the CRD as the API server knows it.
	name string

	// path is the vendored definition, in the platform tree. It must be in the
	// embed set: a released CLI has no checkout, and a CRD it cannot read is one
	// it cannot repair.
	path string

	// field is a property the SHIPPED resources use and the superseded schema
	// does not declare, as a JSON path under a version's openAPIV3Schema
	// properties. It is how this decides whether anything needs doing, and it is
	// the same question ArgoCD's dry-run asks.
	field string

	// why names what breaks while the live schema is stale, for the log line an
	// operator reads when this fires.
	why string
}

// vendoredCRDs is the set Day-0 keeps current. Deliberately short: a CRD belongs
// here when the platform ships resources that use fields a previous release's
// schema did not declare, which is a migration, not a routine.
var vendoredCRDs = []vendoredCRD{
	{
		name:  "clusterissuers.infisical-issuer.infisical.com",
		path:  "manifests/hub-core-services/security/files/infisical-issuer-crd.yaml",
		field: "application",
		why: "the box's certificate issuers cannot adopt the v1.x configuration, so every " +
			"certificate renewal fails behind them",
	},
	{
		name:  "issuers.infisical-issuer.infisical.com",
		path:  "manifests/hub-core-services/security/files/infisical-issuer-crd.yaml",
		field: "application",
		why:   "namespaced issuers cannot adopt the v1.x configuration",
	},
}

// ensureVendoredCRDSchemas brings the live CRDs up to what this release's
// resources declare, before the boundary that reconciles those resources runs.
//
// ON BOUNDARY 01 because that is where platform-security-infra lives
// (01-platform-infra-appset.yaml), and it manages both the CRDs and the issuers
// that use them -- the pairing that produces the deadlock. Repairing it after
// that boundary would be repairing it after the thing it blocks.
//
// Does nothing when the live schema already declares the field. The repair ends
// in a restart of ArgoCD's application controller, which caches the API schema
// at startup and goes on rejecting the dry-run against a CRD it has not noticed
// changed; restarting it on every bootstrap of every box, to no effect, is not a
// price worth paying for a migration that happens twice a year.
func (o *Orchestrator) ensureVendoredCRDSchemas(ctx context.Context, kubeconfig string) error {
	applied := map[string]bool{}
	refreshNeeded := false

	for _, crd := range vendoredCRDs {
		current, err := o.crdDeclaresField(ctx, kubeconfig, crd)
		if err != nil {
			return err
		}
		if current {
			continue
		}

		fmt.Printf("[boundary01] %s does not declare .spec.%s, so %s\n", crd.name, crd.field, crd.why)
		if !applied[crd.path] {
			if err := o.applyVendoredCRD(ctx, kubeconfig, crd); err != nil {
				return err
			}
			applied[crd.path] = true
		}
		refreshNeeded = true
	}

	if !refreshNeeded {
		return nil
	}
	return o.refreshArgoCDSchemaCache(ctx, kubeconfig)
}

// crdDeclaresField reports whether the live CRD already declares the field.
//
// A CRD that is absent entirely is NOT reported as stale: nothing has installed
// it yet, this boundary is about to, and applying it here would take ownership of
// an object the Application is the Day-1 reconciler for. Only a CRD that exists
// and serves a superseded schema is this function's subject.
func (o *Orchestrator) crdDeclaresField(ctx context.Context, kubeconfig string, crd vendoredCRD) (bool, error) {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "crd", crd.name,
		"-o", "jsonpath={.spec.versions[*].schema.openAPIV3Schema.properties.spec.properties."+crd.field+".type}").Output()
	if err != nil {
		// Not installed, or the cluster cannot be reached. Either way there is no
		// stale schema to replace, and guessing which it was would make a
		// transient API error look like a migration.
		return true, nil
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// applyVendoredCRD replaces the live definition with the one this release ships.
//
// --force-conflicts, which is not optional here. The field managers on
// .spec.versions are whatever last wrote them -- argocd-controller on a box
// ArgoCD has synced, kubectl-replace on one repaired by hand -- and a
// server-side apply without it fails on the conflict rather than resolving it.
// Taking ownership is the intent: this release's definition is the authority, and
// leaving the field managed by whatever set it last is how the schema drifted.
//
// Read through platform.ReadFile so a released binary uses its embedded copy and
// a checkout uses the tree, with no branch here.
func (o *Orchestrator) applyVendoredCRD(ctx context.Context, kubeconfig string, crd vendoredCRD) error {
	body, err := platform.ReadFile(crd.path)
	if err != nil {
		return fmt.Errorf("the vendored definition of %s cannot be read, so its schema "+
			"cannot be brought up to what this release's resources declare: %w", crd.name, err)
	}

	apply := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"apply", "--server-side", "--force-conflicts", "-f", "-")
	apply.Stdin = bytes.NewReader(body)
	if out, err := apply.CombinedOutput(); err != nil {
		return fmt.Errorf("applying %s: %w\n%s", crd.path, err, out)
	}
	fmt.Printf("[boundary01] ✓ %s updated to this release's schema\n", crd.name)
	return nil
}
