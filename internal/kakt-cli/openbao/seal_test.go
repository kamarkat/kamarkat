package openbao

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSealDeliveryRotationAndRestoreSelection(t *testing.T) {
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

	// Rotation: overlap configuration carries both.
	t1 := t0.Add(24 * time.Hour)
	r, overlap, err := r.Rotate(t1)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = Deliver(ctx, cs, r, overlap, t1); err != nil {
		t.Fatal(err)
	}
	sec, _ = cs.CoreV1().Secrets(Namespace).Get(ctx, SealSecretName, metav1.GetOptions{})
	if sec.StringData[envPreviousID] != g1 || sec.StringData[envCurrentKeyID] == g1 {
		t.Fatalf("overlap delivery must carry new current and old previous: %v", sec.StringData)
	}

	// After re-wrap: previous dropped from the Secret, but kept in the escrow record.
	t2 := t1.Add(time.Hour)
	if r, err = Deliver(ctx, cs, r, SealDelivery{Current: overlap.Current}, t2); err != nil {
		t.Fatal(err)
	}
	sec, _ = cs.CoreV1().Secrets(Namespace).Get(ctx, SealSecretName, metav1.GetOptions{})
	if _, present := sec.StringData[envPreviousKey]; present {
		t.Fatal("a previous key no longer configured must disappear from the Secret")
	}
	if _, ok := r.generation(g1); !ok {
		t.Fatal("the retired generation must stay in the escrow record")
	}

	// A restore to a point inside the rotation selects the overlap configuration.
	cfg, ok := r.ConfigAt(t1.Add(30 * time.Minute))
	if !ok || cfg.Previous != g1 || cfg.Current != overlap.Current {
		t.Fatalf("restore selection inside the rotation: %+v", cfg)
	}
	// Before the first delivery there is no configuration in force.
	if _, ok := r.ConfigAt(t0.Add(-time.Hour)); ok {
		t.Fatal("no configuration was in force before the first delivery")
	}
	if len(r.History) != 3 {
		t.Fatalf("each distinct delivery is recorded once: %d", len(r.History))
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
