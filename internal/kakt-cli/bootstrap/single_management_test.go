package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// box writes a repository declaring one box and the clusters named.
//
// A management cluster is a directory holding root.yaml; a workload cluster is
// one holding infrastructure/spokepool.yaml. That is the same discrimination the
// platform makes everywhere else, so a test that built them differently would be
// testing something the product does not do.
func box(t *testing.T, boxID string, management []string, workload []string) string {
	t.Helper()
	dir := t.TempDir()
	// The box declares itself, as `kakt box init` writes it. Identity comes from
	// here and not from a cluster, which is what lets a box exist before its
	// first management cluster does.
	if err := os.MkdirAll(filepath.Join(dir, "registry"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "registry", "box.yaml"), "boxId: "+boxID+"\n")
	for _, m := range management {
		base := filepath.Join(dir, "registry", "clusters", m)
		if err := os.MkdirAll(base, 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(base, "root.yaml"), "kind: Application\n")
		write(t, filepath.Join(base, "values.yaml"), "boxId: "+boxID+"\nclusterName: "+m+"\n")
	}
	for _, w := range workload {
		base := filepath.Join(dir, "registry", "clusters", w, "infrastructure")
		if err := os.MkdirAll(base, 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(base, "spokepool.yaml"), "kind: SpokePool\n")
	}
	return dir
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// declaredInTree is an incarnation record that knows only what the tree shows.
//
// The default for tests about the ordinary lifecycle: it adds nothing the working
// tree does not already say, so those tests exercise the admission rule rather
// than the enumeration.
func declaredInTree(context.Context, string) ([]string, error) { return nil, nil }

// remembers is an incarnation record that names clusters the tree has forgotten.
func remembers(names ...string) func(context.Context, string) ([]string, error) {
	return func(context.Context, string) ([]string, error) { return names, nil }
}

// owns is a provider inventory that reports exactly these counts.
func owns(counts map[string]int) OwnedCounts {
	return func(_ context.Context, clusters []string) (map[string]int, error) {
		out := map[string]int{}
		for _, c := range clusters {
			if n, ok := counts[c]; ok {
				out[c] = n
			}
		}
		return out, nil
	}
}

// A box with no existing management cluster is admitted.
func TestAFirstManagementClusterIsAdmitted(t *testing.T) {
	dir := box(t, "acme-hub", []string{"acme-hub"}, nil)
	got, err := ManagementAdmission{
		GitopsDir: dir, Creating: "acme-hub", Incarnations: declaredInTree, Owned: owns(nil),
	}.Admit(context.Background())
	if err != nil {
		t.Fatalf("a box's first management cluster was refused: %v", err)
	}
	if got.BoxID != "acme-hub" {
		t.Errorf("BoxID = %q, want acme-hub", got.BoxID)
	}
	if got.Resuming != 0 {
		t.Errorf("Resuming = %d, want 0: nothing was built before this", got.Resuming)
	}
}

// AN EXISTING MANAGEMENT CLUSTER REFUSES A SECOND ONE.
//
// Running or wedged is not distinguished, and must not be: the only question is
// whether the box still owns management infrastructure. Both answers are "yes".
func TestAnExistingManagementClusterRefusesASecond(t *testing.T) {
	dir := box(t, "acme-hub", []string{"acme-hub", "acme-mgmt-02"}, nil)
	_, err := ManagementAdmission{
		GitopsDir: dir, Creating: "acme-mgmt-02",
		Incarnations: declaredInTree, Owned: owns(map[string]int{"acme-hub": 7}),
	}.Admit(context.Background())
	if err == nil {
		t.Fatal("a second management cluster was admitted while the first still owns " +
			"infrastructure; both would reconcile this repository's declarations")
	}
	for _, want := range []string{
		"Cannot create the management cluster",
		"acme-hub still owns 7 provider resource(s)",
		"kakt teardown --name acme-hub --gitops-dir . --confirm",
		"boxId stays acme-hub",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
}

// An unreachable cluster whose infrastructure is still there is NOT absent.
//
// This is the case the gate exists for. Nothing here asks Kubernetes, so a wedged
// control plane reads exactly like a healthy one: the servers are the evidence,
// and the refusal is identical.
func TestAnUnreachableClusterWithInfrastructureStillRefuses(t *testing.T) {
	dir := box(t, "acme-hub", []string{"acme-hub", "acme-mgmt-02"}, nil)
	_, err := ManagementAdmission{
		GitopsDir: dir, Creating: "acme-mgmt-02",
		// One server left: a control plane that answers nothing is still a
		// cluster that owns infrastructure.
		Incarnations: declaredInTree, Owned: owns(map[string]int{"acme-hub": 1}),
	}.Admit(context.Background())
	if err == nil {
		t.Fatal("a second management cluster was admitted because the first was unreachable; " +
			"its servers are still running and still billing")
	}
	if !strings.Contains(err.Error(), "acme-hub still owns 1 provider resource(s)") {
		t.Errorf("the refusal does not name what is still there:\n%v", err)
	}
}

// A VERIFIED DESTROYED predecessor admits a new incarnation.
//
// Its declaration is still in the repository and its escrow artefacts still
// exist -- both outlive the cluster by design -- so an implementation that gated
// on either would refuse forever. This is the case kubefirst cannot do without a
// human deleting the repository.
func TestAVerifiedDestroyedPredecessorAdmitsANewIncarnation(t *testing.T) {
	dir := box(t, "acme-hub", []string{"acme-hub", "acme-mgmt-02"}, nil)
	got, err := ManagementAdmission{
		GitopsDir: dir, Creating: "acme-mgmt-02",
		Incarnations: declaredInTree, Owned: owns(map[string]int{"acme-hub": 0}),
	}.Admit(context.Background())
	if err != nil {
		t.Fatalf("a new incarnation was refused although the predecessor owns nothing: %v", err)
	}
	if got.BoxID != "acme-hub" {
		t.Errorf("BoxID = %q, want the box to keep its identity across the replacement", got.BoxID)
	}
}

// An incomplete attempt at THIS incarnation is a retry, not a duplicate.
func TestAnIncompleteAttemptIsAResumeRatherThanARefusal(t *testing.T) {
	dir := box(t, "acme-hub", []string{"acme-hub"}, nil)
	got, err := ManagementAdmission{
		GitopsDir: dir, Creating: "acme-hub",
		Incarnations: declaredInTree, Owned: owns(map[string]int{"acme-hub": 3}),
	}.Admit(context.Background())
	if err != nil {
		t.Fatalf("retrying the same incarnation was refused: a bootstrap that fails "+
			"halfway could then never be resumed: %v", err)
	}
	if got.Resuming != 3 {
		t.Errorf("Resuming = %d, want 3 so the run says it is continuing an earlier "+
			"attempt rather than building clean", got.Resuming)
	}
}

// AN AMBIGUOUS PROVIDER ANSWER IS REFUSED, never read as empty.
func TestAnAmbiguousProviderAnswerIsRefused(t *testing.T) {
	dir := box(t, "acme-hub", []string{"acme-hub", "acme-mgmt-02"}, nil)
	_, err := ManagementAdmission{
		GitopsDir: dir, Creating: "acme-mgmt-02",
		Incarnations: declaredInTree,
		Owned: func(context.Context, []string) (map[string]int, error) {
			return nil, errors.New("hcloud: 503 service unavailable")
		},
	}.Admit(context.Background())
	if err == nil {
		t.Fatal("an unreadable inventory was treated as an empty one, which is the " +
			"same evidence as not having asked")
	}
	for _, want := range []string{
		"cannot establish what this box already owns",
		"503 service unavailable",
		"HCLOUD_TOKEN",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
}

// Workload clusters still running refuse a replacement management cluster.
//
// The management cluster goes last. It holds their Cluster API objects and is the
// only thing that can deprovision their load balancers, so a replacement built
// while they run leaves them unmanaged -- and then rebuilds them from the claims
// still in the repository, under names already taken.
func TestWorkloadClustersStillRunningRefuseAReplacement(t *testing.T) {
	dir := box(t, "acme-hub", []string{"acme-hub", "acme-mgmt-02"}, []string{"acme-stg-01"})
	_, err := ManagementAdmission{
		GitopsDir: dir, Creating: "acme-mgmt-02",
		// The predecessor is gone; what it managed is not.
		Incarnations: declaredInTree, Owned: owns(map[string]int{"acme-stg-01": 4}),
	}.Admit(context.Background())
	if err == nil {
		t.Fatal("a replacement was admitted while a workload cluster of this box is " +
			"still running, which orphans it and then duplicates it")
	}
	for _, want := range []string{
		"acme-stg-01 still owns 4 provider resource(s)",
		"The management cluster goes last",
		"kakt tenant remove-cluster",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
}

// Every blocker is reported at once.
func TestAllBlockersAreReportedTogether(t *testing.T) {
	dir := box(t, "acme-hub", []string{"acme-hub", "acme-mgmt-02"}, []string{"acme-stg-01"})
	_, err := ManagementAdmission{
		GitopsDir: dir, Creating: "acme-mgmt-02",
		Incarnations: declaredInTree, Owned: owns(map[string]int{"acme-hub": 7, "acme-stg-01": 4}),
	}.Admit(context.Background())
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "acme-hub still owns") ||
		!strings.Contains(err.Error(), "acme-stg-01 still owns") {
		t.Errorf("one blocker was reported and the other withheld, so the rule has to be "+
			"discovered by trial:\n%v", err)
	}
}

// A cluster the repository does not declare is not provisioned.
func TestAnUndeclaredClusterIsRefused(t *testing.T) {
	dir := box(t, "acme-hub", []string{"acme-hub"}, nil)
	_, err := ManagementAdmission{
		GitopsDir: dir, Creating: "acme-mgmt-99", Incarnations: declaredInTree, Owned: owns(nil),
	}.Admit(context.Background())
	if err == nil {
		t.Fatal("a cluster nothing declares was admitted: it would have no root " +
			"Application and nothing would reconcile it")
	}
	if !strings.Contains(err.Error(), "kakt tenant add-management --name acme-mgmt-99") {
		t.Errorf("the refusal does not say how to declare it:\n%v", err)
	}
}

// A directory that is not a box says so, rather than being admitted.
func TestADirectoryThatIsNotABoxIsRefused(t *testing.T) {
	_, err := ManagementAdmission{
		GitopsDir: t.TempDir(), Creating: "acme-hub", Incarnations: declaredInTree, Owned: owns(nil),
	}.Admit(context.Background())
	if err == nil {
		t.Fatal("a directory with no box was admitted")
	}
	if !strings.Contains(err.Error(), "is not a box") {
		t.Errorf("the refusal does not say what is wrong:\n%v", err)
	}
}

// A FORGOTTEN DECLARATION DOES NOT MAKE SURVIVING INFRASTRUCTURE INVISIBLE.
//
// The tree declares only the cluster being created -- someone removed the
// predecessor's directory believing the cluster was gone. Its servers are still
// there, and the box's own record of having declared it is what makes them
// visible here. CAPH's label names a cluster, not a box, so the identities have
// to come from the box's records; without this the check would ask about one name
// and admit a second management cluster.
func TestAForgottenDeclarationStillBlocksASecondCluster(t *testing.T) {
	dir := box(t, "acme-hub", []string{"acme-mgmt-02"}, nil)
	_, err := ManagementAdmission{
		GitopsDir: dir, Creating: "acme-mgmt-02",
		Incarnations: remembers("acme-hub"),
		Owned:        owns(map[string]int{"acme-hub": 5}),
	}.Admit(context.Background())
	if err == nil {
		t.Fatal("a second management cluster was admitted because the first one's " +
			"directory had been removed; its servers are still running")
	}
	if !strings.Contains(err.Error(), "acme-hub still owns 5 provider resource(s)") {
		t.Errorf("the refusal does not name the forgotten cluster:\n%v", err)
	}
}

// A forgotten declaration whose infrastructure is gone blocks nothing.
func TestAForgottenAndDestroyedClusterBlocksNothing(t *testing.T) {
	dir := box(t, "acme-hub", []string{"acme-mgmt-02"}, nil)
	_, err := ManagementAdmission{
		GitopsDir: dir, Creating: "acme-mgmt-02",
		Incarnations: remembers("acme-hub", "acme-hub-v0"),
		Owned:        owns(nil),
	}.Admit(context.Background())
	if err != nil {
		t.Fatalf("a new incarnation was refused over clusters that own nothing: %v", err)
	}
}

// An unreadable incarnation record is refused, not read as "none".
func TestAnUnreadableIncarnationRecordIsRefused(t *testing.T) {
	dir := box(t, "acme-hub", []string{"acme-mgmt-02"}, nil)
	_, err := ManagementAdmission{
		GitopsDir: dir, Creating: "acme-mgmt-02",
		Incarnations: func(context.Context, string) ([]string, error) {
			return nil, errors.New("not a git repository")
		},
		Owned: owns(nil),
	}.Admit(context.Background())
	if err == nil {
		t.Fatal("an unreadable incarnation record was treated as an empty one, which " +
			"is how a forgotten cluster becomes invisible")
	}
	for _, want := range []string{
		"cannot establish which management clusters this box has had",
		"not a git repository",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
}

// Missing the record entirely is refused rather than silently narrowing.
func TestNoIncarnationRecordIsRefused(t *testing.T) {
	dir := box(t, "acme-hub", []string{"acme-hub"}, nil)
	_, err := ManagementAdmission{
		GitopsDir: dir, Creating: "acme-hub", Owned: owns(nil),
	}.Admit(context.Background())
	if err == nil {
		t.Fatal("admission proceeded with no record of past incarnations")
	}
}

// History widens who is asked about; it does not make a removed cluster
// reconcilable.
//
// A cluster declared once and since removed has no root Application today, so
// provisioning it would build infrastructure nothing reconciles. It must be
// declared again first.
func TestAClusterOnlyInHistoryIsNotTreatedAsDeclared(t *testing.T) {
	dir := box(t, "acme-hub", []string{"acme-hub"}, nil)
	_, err := ManagementAdmission{
		GitopsDir: dir, Creating: "acme-mgmt-02",
		Incarnations: remembers("acme-mgmt-02"),
		Owned:        owns(nil),
	}.Admit(context.Background())
	if err == nil {
		t.Fatal("a cluster that exists only in history was treated as declared")
	}
	if !strings.Contains(err.Error(), "kakt tenant add-management --name acme-mgmt-02") {
		t.Errorf("the refusal does not say how to declare it again:\n%v", err)
	}
}
