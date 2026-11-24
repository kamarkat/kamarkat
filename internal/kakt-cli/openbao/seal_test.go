package openbao

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSealDeliveryAndRestoreSelection(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	t0 := now

	r, err := NewSealRecord(t0)
	if err != nil {
		t.Fatal(err)
	}
	r, err = Deliver(ctx, cs, r, r.InForce(), t0)
	if err != nil {
		t.Fatal(err)
	}
	g1 := r.Current().ID
	sec, err := cs.CoreV1().Secrets(Namespace).Get(ctx, SealSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sec.StringData[envCurrentKeyID] != g1 || sec.StringData[envPreviousKey] != "" {
		t.Fatalf("first delivery must carry only the current generation: %v", sec.StringData)
	}

	// A record carrying a later generation and an overlap delivery, as escrow
	// history may hold for a box restored across a key change.
	t1 := t0.Add(24 * time.Hour)
	g2, err := newGeneration(t1)
	if err != nil {
		t.Fatal(err)
	}
	r.Generations = append(r.Generations, g2)
	if r, err = Deliver(ctx, cs, r, SealDelivery{Current: g2.ID, Previous: g1}, t1); err != nil {
		t.Fatal(err)
	}
	sec, _ = cs.CoreV1().Secrets(Namespace).Get(ctx, SealSecretName, metav1.GetOptions{})
	if sec.StringData[envPreviousID] != g1 || sec.StringData[envCurrentKeyID] != g2.ID {
		t.Fatalf("overlap delivery must carry new current and old previous: %v", sec.StringData)
	}

	// Restore selection: a recovery point before the change selects generation 1
	// alone; one after it selects the overlap.
	if cfg, ok := r.ConfigAt(t0.Add(time.Hour)); !ok || cfg.Current != g1 || cfg.Previous != "" {
		t.Fatalf("restore selection before the change: %+v", cfg)
	}
	if cfg, ok := r.ConfigAt(t1.Add(time.Hour)); !ok || cfg.Current != g2.ID || cfg.Previous != g1 {
		t.Fatalf("restore selection after the change: %+v", cfg)
	}
	// Before the first delivery there is no configuration in force.
	if _, ok := r.ConfigAt(t0.Add(-time.Hour)); ok {
		t.Fatal("no configuration was in force before the first delivery")
	}

	// Delivering an OLDER configuration (a restore) records it as in force now,
	// and drops the previous key from the Secret.
	t2 := t1.Add(time.Hour)
	if r, err = Deliver(ctx, cs, r, SealDelivery{Current: g1}, t2); err != nil {
		t.Fatal(err)
	}
	sec, _ = cs.CoreV1().Secrets(Namespace).Get(ctx, SealSecretName, metav1.GetOptions{})
	if _, present := sec.StringData[envPreviousKey]; present || sec.StringData[envCurrentKeyID] != g1 {
		t.Fatalf("restored configuration not delivered exactly: %v", sec.StringData)
	}
	if len(r.History) != 3 || r.InForce().Current != g1 {
		t.Fatalf("each distinct delivery is recorded once and the last is in force: %+v", r.History)
	}
	// Re-delivering what is in force records nothing new.
	if r, err = Deliver(ctx, cs, r, r.InForce(), t2.Add(time.Minute)); err != nil || len(r.History) != 3 {
		t.Fatalf("redelivery must not add history: %d %v", len(r.History), err)
	}
}

func TestSealConfigNamingMissingGenerationIsRefused(t *testing.T) {
	r, _ := NewSealRecord(now)
	if _, err := r.SecretData(SealDelivery{Current: r.Current().ID, Previous: "gen-absent"}); err == nil {
		t.Fatal("a configuration whose previous generation the escrow lacks must be refused")
	}
}

func TestParseSealRecordRejectsMalformedKeys(t *testing.T) {
	if _, err := ParseSealRecord(`{"generations":[{"id":"g","key":"abcd"}]}`); err == nil {
		t.Fatal("a seal key that is not 32 bytes must be refused")
	}
}
