package bootstrap

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// fakeEscrow is an escrow that records what it was asked to hold, keyed the way
// the real one keys it: box id, cluster, artefact name. That key is the subject
// of several of these tests -- a box-scoped artefact written under a cluster's
// identity is unreachable by the command that looks for it.
type fakeEscrow struct {
	artifacts map[string]string
	readErr   error
	writeErr  error
	writes    int
}

func newFakeEscrow() *fakeEscrow { return &fakeEscrow{artifacts: map[string]string{}} }

func key(id escrow.Identity, name string) string {
	return fmt.Sprintf("%s/%s/%s", id.BoxID, id.ClusterName, name)
}

func (f *fakeEscrow) RestoreArtifact(_ context.Context, id escrow.Identity, name string) (string, error) {
	if f.readErr != nil {
		return "", f.readErr
	}
	return f.artifacts[key(id, name)], nil
}

func (f *fakeEscrow) BackupArtifact(_ context.Context, id escrow.Identity, name, payload string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes++
	f.artifacts[key(id, name)] = payload
	return nil
}

func (f *fakeEscrow) BackupMasterKeys(context.Context, escrow.Identity, interface{}) error {
	return nil
}
func (f *fakeEscrow) RestoreMasterKeys(context.Context, escrow.Identity) (interface{}, error) {
	return nil, nil
}

// A fresh box escrows its provider credential, and it is readable afterwards
// through the identity a later command resolves.
func TestBootstrapEnrollsItsProviderCredential(t *testing.T) {
	store := newFakeEscrow()
	if err := enrollProviderCredential(context.Background(), store, "acme-box", "acme-hub", "hetzner", "tok-1"); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	// Box-scoped: the box on both halves, which is where every reader looks.
	got, err := store.RestoreArtifact(context.Background(),
		escrow.ForCluster("acme-box", "acme-box"), escrow.ProviderCredentialArtifact("hetzner"))
	if err != nil || got != "tok-1" {
		t.Fatalf("the credential is not readable through the box's identity: %q, %v", got, err)
	}
}

// Re-running a bootstrap against the same box writes nothing.
func TestEnrollmentIsIdempotent(t *testing.T) {
	store := newFakeEscrow()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := enrollProviderCredential(ctx, store, "acme-box", "acme-hub", "hetzner", "tok-1"); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if store.writes != 1 {
		t.Errorf("three identical enrolments performed %d writes, want 1", store.writes)
	}
}

// A different credential is never replaced implicitly: the escrowed copy may be
// the only one that can reach infrastructure the box already owns.
func TestConflictingCredentialRequiresExplicitRotation(t *testing.T) {
	store := newFakeEscrow()
	ctx := context.Background()
	if err := enrollProviderCredential(ctx, store, "acme-box", "acme-hub", "hetzner", "tok-1"); err != nil {
		t.Fatal(err)
	}
	err := enrollProviderCredential(ctx, store, "acme-box", "acme-hub", "hetzner", "tok-2")
	if err == nil {
		t.Fatal("a different credential silently replaced the escrowed one")
	}
	for _, want := range []string{"Refusing to replace", "--rotate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	if got := store.artifacts[key(escrow.ForCluster("acme-box", "acme-box"),
		escrow.ProviderCredentialArtifact("hetzner"))]; got != "tok-1" {
		t.Errorf("the escrowed credential is now %q; the original was overwritten", got)
	}
}

// Enrolment failures fail the bootstrap. Each of these used to be a printed
// warning stepped over, which left a box whose infrastructure only the
// bootstrapping machine could destroy.
func TestEnrollmentFailuresAreFatal(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		why    string
		store  escrow.EscrowClient
		box    string
		token  string
		expect string
	}{
		{"no escrow at all", nil, "acme-box", "tok", "not built without one"},
		{"no box id", newFakeEscrow(), "", "tok", "box id is required"},
		{"no credential", newFakeEscrow(), "acme-box", "", "no hetzner credential to enrol"},
		{"escrow unreadable", &fakeEscrow{artifacts: map[string]string{}, readErr: fmt.Errorf("401")},
			"acme-box", "tok", "could not read"},
		{"escrow unwritable", &fakeEscrow{artifacts: map[string]string{}, writeErr: fmt.Errorf("403")},
			"acme-box", "tok", "Nothing has been provisioned"},
	} {
		err := enrollProviderCredential(ctx, c.store, c.box, "acme-hub", "hetzner", c.token)
		if err == nil {
			t.Errorf("%s: enrolment succeeded", c.why)
			continue
		}
		if !strings.Contains(err.Error(), c.expect) {
			t.Errorf("%s: error does not mention %q: %v", c.why, c.expect, err)
		}
	}
}

// Box-scoped and cluster-scoped artefacts are different keys, and hub succession
// is what makes the difference load-bearing: a successor carries a new
// clusterName under the SAME boxId, so the box's credential is still found while
// the cluster's own kubeconfig is its own (ADR-111 §1).
func TestSuccessionKeepsTheBoxAndReplacesTheCluster(t *testing.T) {
	store := newFakeEscrow()
	ctx := context.Background()

	if err := enrollProviderCredential(ctx, store, "acme-box", "acme-hub", "hetzner", "tok-1"); err != nil {
		t.Fatal(err)
	}
	// The successor: same box, new cluster. Its bootstrap finds the box's
	// credential already escrowed and writes nothing.
	if err := enrollProviderCredential(ctx, store, "acme-box", "acme-hub-2", "hetzner", "tok-1"); err != nil {
		t.Fatalf("the successor could not reuse the box's credential: %v", err)
	}
	if store.writes != 1 {
		t.Errorf("succession wrote the credential again (%d writes)", store.writes)
	}

	// Cluster-scoped artefacts stay separate: each cluster's kubeconfig is its
	// own, under the box they share.
	for cluster, payload := range map[string]string{"acme-hub": "kubeconfig-1", "acme-hub-2": "kubeconfig-2"} {
		if err := store.BackupArtifact(ctx, escrow.ForCluster("acme-box", cluster),
			escrow.ArtifactKubeconfig, payload); err != nil {
			t.Fatal(err)
		}
	}
	for cluster, want := range map[string]string{"acme-hub": "kubeconfig-1", "acme-hub-2": "kubeconfig-2"} {
		got, err := store.RestoreArtifact(ctx, escrow.ForCluster("acme-box", cluster), escrow.ArtifactKubeconfig)
		if err != nil || got != want {
			t.Errorf("%s: kubeconfig is %q, want %q (%v)", cluster, got, want, err)
		}
	}
}
