package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	identityv1alpha1 "github.com/kamarkat/kamarkat/operators/spoke-identity-operator/api/v1alpha1"
)

const (
	// workloadProjectRole is what ESO needs and all it needs: read the secret
	// values of its own cluster's project. `viewer` grants DescribeSecret and
	// ReadValue on Secrets, and no write of any kind.
	workloadProjectRole = "viewer"

	conditionTypeWorkloadScoped = "WorkloadIdentityScoped"
)

// ensureWorkloadIdentityScope narrows the spoke's ESO identity to its own
// cluster's secrets project.
//
// WHAT THIS REPLACES. The identity was granted `admin` on hub-platform AND
// hub-secrets, which is read and write across every environment and path of both
// -- so a spoke could read every other cluster's secrets and overwrite the box's
// own. `environmentSlug` on the store never constrained that: a built-in role
// carries no environment or path condition, and conditioning one needs
// PlanFeature.Rbac, which the default plan does not grant. The project is the
// boundary, so belonging to one project is the narrowing.
//
// IT WAITS, AND WAITING IS THE POINT. spec.infisical.secretsProjectSlug is
// published by hub-operator only once this cluster's project holds its
// credentials. While it is empty this returns without touching anything: an
// identity narrowed to a project that does not yet hold its secrets is a working
// spoke broken by a security correction, and there is no ordering between two
// operators that makes that safe other than the one that asks.
//
// It never touches the PKI identity, and the PKI identity never gains this.
func (r *SpokeMachineIdentityReconciler) ensureWorkloadIdentityScope(ctx context.Context, smi *identityv1alpha1.SpokeMachineIdentity, identityID string) error {
	logger := log.FromContext(ctx).WithValues("spoke", smi.Spec.SpokeRef.Name)

	slug := smi.Spec.Infisical.SecretsProjectSlug
	if slug == "" {
		logger.Info("Waiting for this cluster's secrets project to hold its credentials before " +
			"narrowing the workload identity")
		r.setCondition(smi, conditionTypeWorkloadScoped, metav1.ConditionFalse, "AwaitingSecretsProject",
			"hub-operator has not published a secrets project for this cluster yet; the "+
				"identity keeps its existing access until it does")
		return nil
	}
	if identityID == "" {
		return fmt.Errorf("no identity to scope for %s", smi.Spec.SpokeRef.Name)
	}

	projectID, err := r.InfisicalClient.FindProjectBySlug(ctx, slug)
	if err != nil {
		return fmt.Errorf("resolve this cluster's secrets project %q: %w", slug, err)
	}
	if projectID == "" {
		// The slug is a statement that the project exists and holds this cluster's
		// credentials. If it does not, something removed it, and narrowing to a
		// project that is not there would leave the identity able to read nothing.
		return fmt.Errorf("the secrets project %q is published for this cluster and does not "+
			"exist, so the identity is left as it is rather than scoped to nothing", slug)
	}

	// Everything else it currently holds. hub-platform is PKI, which belongs to the
	// issuer's identity alone; hub-secrets is the box's own, which no workload
	// cluster has any business reading.
	drop := []string{r.SecretsProjectID, r.ProjectID}
	if smi.Spec.Infisical.SecretsProjectID != "" {
		drop = append(drop, smi.Spec.Infisical.SecretsProjectID)
	}
	if smi.Spec.Infisical.ProjectID != "" {
		drop = append(drop, smi.Spec.Infisical.ProjectID)
	}

	changes, err := r.InfisicalClient.ReconcileSoleMembership(ctx, identityID, projectID, workloadProjectRole, drop)
	if err != nil {
		return fmt.Errorf("scope the workload identity to %q: %w", slug, err)
	}

	logger.Info("The workload identity is scoped to its own cluster's project",
		"project", slug, "role", workloadProjectRole, "changes", changeStrings(changes))
	r.setCondition(smi, conditionTypeWorkloadScoped, metav1.ConditionTrue, "Scoped",
		fmt.Sprintf("%s in project %s, and a member of nothing else", workloadProjectRole, slug))
	return nil
}
