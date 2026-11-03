package database

import "fmt"

// Endpoint names the objects CloudNativePG derives from a cluster's name.
//
// It exists because the cluster used to be a LITERAL in three format strings -- the
// superuser secret, the CA secret and the primary service were all spelled
// `platform-db...` in Go -- while `HubEnvironment.Spec.Database.ClusterRef` already
// declared which cluster to use and was ignored. So the API took a cluster name the
// code could not honour, and anything that wanted a second cluster could not have one:
// ADR-112 gives each platform application its own, and the role manager could reach
// exactly one by construction.
//
// Deriving the three names from one value also keeps them consistent. They are
// CloudNativePG's conventions, not this platform's, and spelling them separately is how
// two of them end up pointing at different clusters.
type Endpoint struct {
	// Cluster is the CNPG Cluster's name, from Spec.Database.ClusterRef.
	Cluster string
	// Namespace is where that Cluster runs.
	Namespace string
}

// SuperuserSecret holds the credentials CNPG generates for the cluster's superuser.
func (e Endpoint) SuperuserSecret() string { return e.Cluster + "-superuser" }

// CASecret holds the cluster's certificate authority.
func (e Endpoint) CASecret() string { return e.Cluster + "-ca" }

// PrimaryHost is the read-write service: the primary, never a pooler. Role changes and
// migrations are writes and must not be routed through PgBouncer.
func (e Endpoint) PrimaryHost() string {
	return fmt.Sprintf("%s-rw.%s.svc", e.Cluster, e.Namespace)
}

// Validate refuses an incomplete endpoint.
//
// Both halves are required and neither has a safe default. Defaulting the cluster would
// reinstate exactly the defect this type removes -- a connection to `platform-db` made
// on behalf of an application whose data is somewhere else -- and it would do it
// silently, because a successful connection to the wrong cluster looks like success.
func (e Endpoint) Validate() error {
	if e.Cluster == "" {
		return fmt.Errorf("no database cluster given: Spec.Database.ClusterRef names it, " +
			"and connecting to an assumed cluster would read and write the wrong application's data")
	}
	if e.Namespace == "" {
		return fmt.Errorf("no database namespace given for cluster %q", e.Cluster)
	}
	return nil
}
