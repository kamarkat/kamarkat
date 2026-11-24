package constants

// The bootstrap database (ADR-117 §2, ADR-112 §7): the one CloudNativePG cluster
// Day-0 waits for before GitOps proceeds, because OpenBao -- from which every other
// credential is delivered -- stores its state there. It needs no credential input:
// its owner's password is CloudNativePG's own and OpenBao authenticates with a
// client certificate, so nothing about it depends on OpenBao or ESO.
const (
	// BootstrapDBCluster is the CNPG Cluster Day-0 waits for and OpenBao connects
	// to directly (no pooler: ADR-117 §2).
	BootstrapDBCluster = "openbao-db"

	// BootstrapDBOwnerApplication is the Argo CD Application that owns it
	// (argocd/components/02/platform-openbao-db.yaml).
	BootstrapDBOwnerApplication = "platform-openbao-db"
)
