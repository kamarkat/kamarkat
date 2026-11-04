package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
	"github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(computev1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr string
	var enableLeaderElection bool
	var provisioningBudget time.Duration

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")

	// ADR-052 §11: the p95 cold-start budget for the placement class this
	// operator serves. It is a measured input — the ADR deliberately declines to
	// name a number, because the correct value depends on machine type, image
	// size and registry locality, and an invented one would be adopted as though
	// it had been measured. This value only bounds when the operator starts
	// *reporting* a wait as over budget; it never triggers deletion (§11 rule 3).
	flag.DurationVar(&provisioningBudget, "provisioning-budget", 6*time.Minute,
		"p95 cold-start budget for the burst placement class; over-budget waits are reported, never deleted.")

	// ADR-106: whether this cluster enforces network policy through Cilium.
	// REQUIRED, with no default, because both wrong answers are silent: a
	// default of "cilium" makes a cluster without Cilium fail every storage-
	// using job on a missing CRD, and a default of "none" leaves a production
	// cluster's sandboxes without their storage and peer policies. Each install
	// states it: config/default says cilium; the local profile says none,
	// because ADR-106 is enforced from dev onward, by agreement with product.
	var networkPolicy string
	flag.StringVar(&networkPolicy, "network-policy", "",
		"How sandbox network policy is enforced: cilium (render CiliumNetworkPolicies) or none (a cluster with no network policy enforcement). Required.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	mode, err := controller.ParseNetworkPolicyMode(networkPolicy)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                server.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "ephemeral-job-operator.compute.nutgraf.in",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	// Index Events by involved object name so the capacity assessment can find
	// an unschedulable Pod's autoscaler events without listing every Event in
	// the namespace. This is the read that makes ADR-052 §12 satisfiable from
	// namespaced objects alone.
	if err := mgr.GetFieldIndexer().IndexField(ctx, &corev1.Event{}, "involvedObject.name",
		func(o client.Object) []string {
			e, ok := o.(*corev1.Event)
			if !ok {
				return nil
			}
			return []string{e.InvolvedObject.Name}
		},
	); err != nil {
		setupLog.Error(err, "unable to index events by involved object")
		os.Exit(1)
	}

	// A typed clientset, for one thing only: reading a failed workload's log tail so
	// the terminal callback can say WHY it failed. A log is a subresource request, so
	// the manager's client cannot serve it.
	//
	// Not fatal if it cannot be built. The operator's whole job is unaffected; what is
	// lost is the most specific line of a failure message, and refusing to start over
	// that would trade every workload for one diagnostic.
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "no clientset: failure messages will omit the workload's log tail")
		clientset = nil
	}

	if err := (&controller.EphemeralJobReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		ProvisioningBudget: provisioningBudget,
		Clientset:          clientset,
		NetworkPolicy:      mode,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "EphemeralJob")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting ephemeral-job-operator", "provisioningBudget", provisioningBudget)
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
