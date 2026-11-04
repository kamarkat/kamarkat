package secrets

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Credential self-healing metrics. These expose the outcome of validating and
// rotating the shared Machine Identity credentials stored in Infisical so that
// operators can observe staleness/rotation at a glance without digging into logs.
const (
	metricCredentialStatusValid   = "valid"
	metricCredentialStatusRotated = "rotated"
	metricCredentialStatusFailed  = "rotation_failed"
	metricCredentialStatusMissing = "missing"
	credentialMetricsNamespace    = "zeroops"
	credentialMetricsSubsystem    = "infisical_credentials"
)

var (
	// credentialStatus reflects the last observed outcome of credential
	// validation/rotation for a cluster (1.0 for the current state).
	//
	// The LABEL is still named "cell" while the value in it is a cluster name.
	// A metric label is queried by dashboards and alert rules, so renaming it is
	// a change to observability surface rather than to internals, and belongs
	// with whatever updates those queries -- not with a source rename.
	credentialStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: credentialMetricsNamespace,
			Subsystem: credentialMetricsSubsystem,
			Name:      "status",
			Help:      "Outcome of shared Infisical credential validation/rotation per cell (valid|rotated|rotation_failed|missing).",
		},
		[]string{"cell", "status"},
	)

	// credentialRotationTotal counts self-healing rotations performed.
	credentialRotationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: credentialMetricsNamespace,
			Subsystem: credentialMetricsSubsystem,
			Name:      "rotations_total",
			Help:      "Total number of self-healing rotations of shared Infisical credentials.",
		},
		[]string{"cell"},
	)
)

func init() {
	metrics.Registry.MustRegister(credentialStatus, credentialRotationTotal)
}

func setCredentialStatus(cell, status string) {
	for _, s := range []string{
		metricCredentialStatusValid,
		metricCredentialStatusRotated,
		metricCredentialStatusFailed,
		metricCredentialStatusMissing,
	} {
		credentialStatus.WithLabelValues(cell, s).Set(0)
	}
	credentialStatus.WithLabelValues(cell, status).Set(1)
}

func incCredentialRotation(cell string) {
	credentialRotationTotal.WithLabelValues(cell).Inc()
}
