package constants

// The PostgreSQL cluster Day-0 bootstraps against, and the ApplicationSet element
// that creates it.
//
// ONE DECLARATION, because two things read it and they must agree: the readiness gate
// Day-0 waits on (bootstrap.awaitBootstrapDatabaseReady) and the connection secret Day-0
// writes for Infisical. Spelled separately, they are a bootstrap that waits for one
// database and then connects to another -- and each would be editable alone, so the two
// would agree only by luck.
//
// WHY INFISICAL'S CLUSTER IS THE BOOTSTRAP CLUSTER. Day-0 needs exactly one database
// before GitOps can proceed: Infisical's. Every other credential is fetched FROM
// Infisical, so its role cannot arrive by the ordinary Infisical -> ESO path (ADR-023's
// addendum, which is why this one role is reconciled by CloudNativePG from a Secret).
// Under ADR-112 each application owns its cluster, so the cluster Day-0 waits for is the
// secret store's own and not a shared one.
//
// THERE IS NO SHARED CLUSTER. Every platform database is owned by the application that
// uses it (ADR-112), so there is no box-wide database for anything to fall back to.
const (
	// BootstrapDBCluster is the CNPG Cluster Day-0 waits for and Infisical connects to.
	BootstrapDBCluster = "infisical-db"

	// BootstrapDBPooler is the PgBouncer in front of it, which is what Infisical
	// actually dials (ADR-112 §9).
	//
	// THIS NAME IS DERIVED TWICE, IN TWO MODULES. The hub operator builds the same
	// host from database.Endpoint.PoolerHost(), and cannot import this package. Both
	// write the infisical-postgres-connection Secret, so if the two disagree the value
	// depends on which ran last -- and the symptom is not an error here but DNS
	// ENOTFOUND inside Infisical, hours later, with every ExternalSecret on the box
	// failing behind it (2026-11-05).
	//
	// It is also the name of the Pooler object in
	// manifests/hub-core-services/infisical/database/infisical-db-pooler.yaml, because
	// CloudNativePG names the pooler's Service after the object. Three places, one
	// string: change it in all of them or in none.
	BootstrapDBPooler = BootstrapDBCluster + "-pooler"

	// BootstrapDBOwnerApplication is the ApplicationSet element that creates it, in
	// NamespaceOps. Naming it lets a readiness check tell "not created yet" from
	// "nothing is going to create it": an ArgoCD Application that reports Synced and
	// Healthy while managing no resources renders nothing, and waiting out a timeout
	// ends where it began.
	BootstrapDBOwnerApplication = "platform-infisical-db"
)
