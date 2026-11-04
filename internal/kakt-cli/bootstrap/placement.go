package bootstrap

// Where the platform's own control components run.
//
// THIS MIRRORS environment-manager.cloudControlPlacement in
// manifests/argocd/environment-manager/templates/_placement.tpl. That template is
// the canonical statement of the rule; this is the same rule expressed for the
// components installed before it exists.
//
// Two expressions rather than one because of when each runs. CAPI and its
// providers are installed by the CLI during capi-init, long before ArgoCD or the
// environment-manager chart exist, so nothing Helm-rendered can place them. A
// test asserts the two agree (placement_test.go); if it fails, the two halves of
// the platform have started disagreeing about where the platform runs.
//
// The rule: keep the platform's controllers off the tenant's premises.
//
// They reconcile the box itself -- leases renewed against the API server every
// few seconds, watches on every Cluster and Machine. On a hybrid box the on-prem
// worker reaches the API server over VXLAN-over-Tailscale, and that path drops
// packets. Measured on acme-hub, 2026-09-16: every CAPI and CAPH controller was
// on the on-prem node, CAPH had restarted 9 times on "failed to renew lease
// platform-capi/hetzner.cluster.x-k8s.io: timed out waiting for the condition;
// leader election lost", and a workload cluster whose server had been deleted
// went unnoticed because each restart is a window in which nothing reconciles.
//
// This REVERSES the earlier decision recorded on HybridDriver.CAPIPlacement, and
// the reversal is deliberate rather than a rediscovery. That decision moved these
// controllers ONTO the on-prem node because the cpx22 control plane was at 89%
// memory and they were losing leader election there. Both observations are real;
// what changed is that the control plane's own leader election was fixed by
// raising its lease timeouts (hetzner-mgmt-ubuntu-v1.yaml: lease-duration 60s,
// renew-deadline 40s, retry-period 10s -- verified zero restarts over 79
// minutes), so the saturation argument no longer decides it, and the network
// argument does.
//
// NotIn rather than a control-plane nodeSelector: a node lacking the label
// satisfies NotIn, so this is a no-op where every node is cloud, and a hybrid box
// that later gains Hetzner workers gets them as a valid home without this
// changing. The toleration is what makes the control-plane node schedulable at
// all -- without it the affinity selects a node nothing may be placed on, and the
// pods stay Pending, which is worse than the fault being fixed.
const cloudControlPlacementPatch = `{"spec":{"deployment":{` +
	`"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":` +
	`{"nodeSelectorTerms":[{"matchExpressions":[` +
	`{"key":"workload-location","operator":"NotIn","values":["on-prem"]}]}]}}},` +
	`"tolerations":[{"key":"node-role.kubernetes.io/control-plane",` +
	`"operator":"Exists","effect":"NoSchedule"}],` +
	// Explicitly null, not omitted. A merge patch leaves untouched keys in
	// place, and every box bootstrapped before this carries
	// nodeSelector: {workload-location: on-prem} from the decision above. Left
	// there it contradicts the affinity -- must be on-prem, must not be on-prem
	// -- and the controllers stay Pending for ever. Null is what removes it.
	`"nodeSelector":null` +
	`}}}`
