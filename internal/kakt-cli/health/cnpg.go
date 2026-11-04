package health

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// CNPGClusterHealth is a HealthChecker that waits for a CloudNativePG
// Cluster CR to reach the "Cluster in healthy state" phase.
//
// The CNPG cluster goes through these phases (from CloudNativePG docs):
//   - Setting up primary: the initdb Job is running
//   - Waiting for the instances to become active: post-initdb
//   - Cluster in healthy state: all instances are ready and replicating
//
// For external services that connect to the database (e.g., Infisical),
// only the "Cluster in healthy state" phase guarantees that the primary
// instance is accepting connections.
type CNPGClusterHealth struct {
	// OwnerApplication is the ArgoCD Application that creates this cluster, if
	// one does. Named so the check can tell "not created yet" from "nothing is
	// going to create it" -- the second is not worth waiting thirty minutes for.
	OwnerApplication string
	OwnerNamespace   string

	ClusterName string
	Namespace   string
}

// NewCNPGClusterHealth returns a checker for the named CNPG cluster.
func NewCNPGClusterHealth(name, namespace string) *CNPGClusterHealth {
	return &CNPGClusterHealth{ClusterName: name, Namespace: namespace}
}

// Name returns the checker identifier.
func (c *CNPGClusterHealth) Name() string {
	return fmt.Sprintf("CNPG cluster %s/%s", c.Namespace, c.ClusterName)
}

// Check probes the CNPG cluster phase. We use kubectl JSONPath because
// the typed clientset would require importing the postgresql.cnpg.io/v1
// package, which is a first-party dependency we want to keep out of the
// health package. JSONPath keeps this layer decoupled.
func (c *CNPGClusterHealth) Check(ctx context.Context, kubeconfig string) error {
	args := []string{
		"--kubeconfig", kubeconfig,
		"get", "cluster.postgresql.cnpg.io", c.ClusterName,
		"-n", c.Namespace,
		"-o", "jsonpath={.status.phase}",
	}
	out, err := runKubectl(ctx, args)
	if err != nil {
		if c.OwnerApplication != "" {
			if fatal := ApplicationOwnsNothing(ctx, kubeconfig,
				c.OwnerNamespace, c.OwnerApplication); fatal != nil {
				return fatal
			}
		}
		return fmt.Errorf("%s: %w", c.Name(), err)
	}
	phase := strings.TrimSpace(string(out))
	if phase != "Cluster in healthy state" {
		return fmt.Errorf("phase=%q (need \"Cluster in healthy state\")", phase)
	}
	return nil
}

// PgBouncerPoolerHealth is a HealthChecker that waits for a CNPG Pooler
// CR's Deployment to be available. The Pooler CR creates a PgBouncer
// deployment that fronts the cluster's primary instance.
//
// External services connect to the pooler's service (e.g.,
// platform-db-pooler.<namespace>.svc) to talk to PostgreSQL. If the
// pooler deployment isn't available, those connections fail with
// DNS resolution errors.
type PgBouncerPoolerHealth struct {
	PoolerName string
	Namespace  string
}

// NewPgBouncerPoolerHealth returns a checker for the named pooler.
func NewPgBouncerPoolerHealth(poolerName, namespace string) *PgBouncerPoolerHealth {
	return &PgBouncerPoolerHealth{PoolerName: poolerName, Namespace: namespace}
}

// Name returns the checker identifier.
func (p *PgBouncerPoolerHealth) Name() string {
	return fmt.Sprintf("PgBouncer pooler %s/%s", p.Namespace, p.PoolerName)
}

// Check probes the pooler's deployment.
//
// The pooler CR creates a Deployment stamped with the
// cnpg.io/poolerName=<poolerName> label. We list by label so we don't
// need to know the deployment's generated name.
func (p *PgBouncerPoolerHealth) Check(ctx context.Context, kubeconfig string) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("%s: failed to load kubeconfig: %w", p.Name(), err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("%s: failed to create kubernetes client: %w", p.Name(), err)
	}

	deployments, err := clientset.AppsV1().Deployments(p.Namespace).
		List(ctx, metav1.ListOptions{LabelSelector: fmt.Sprintf("cnpg.io/poolerName=%s", p.PoolerName)})
	if err != nil {
		return fmt.Errorf("%s: list deployments: %w", p.Name(), err)
	}
	if len(deployments.Items) == 0 {
		return fmt.Errorf("no Deployment found with label cnpg.io/poolerName=%s", p.PoolerName)
	}
	// The pooler may have multiple deployments across restarts; check the newest.
	dep := deployments.Items[0]
	if dep.Status.AvailableReplicas > 0 {
		return nil
	}
	return fmt.Errorf("Deployment %s: %d/%d replicas available",
		dep.Name, dep.Status.AvailableReplicas, dep.Status.Replicas)
}
