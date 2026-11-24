package openbao

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// Namespace is where OpenBao, its database and its inputs live (ADR-117 §2).
	Namespace = "platform-data"

	// SealSecretName is what the StatefulSet loads with envFrom
	// (manifests/hub-core-services/openbao/kustomization.yaml).
	SealSecretName = "openbao-seal"

	envCurrentKey   = "BAO_STATIC_SEAL_CURRENT_KEY"
	envCurrentKeyID = "BAO_STATIC_SEAL_CURRENT_KEY_ID"
	envPreviousKey  = "BAO_STATIC_SEAL_PREVIOUS_KEY"
	envPreviousID   = "BAO_STATIC_SEAL_PREVIOUS_KEY_ID"
)

// SealGeneration is one 32-byte static-seal key with its permanent identifier.
type SealGeneration struct {
	ID          string    `json:"id"`
	Key         string    `json:"key"` // hex, 32 bytes
	ActivatedAt time.Time `json:"activatedAt"`
}

// SealDelivery records one seal configuration delivered to the box: which
// generation was current and which previous. It is the record a restore reads to
// know which configuration was in force at its recovery point (ADR-117 §3, §7).
type SealDelivery struct {
	ConfigID    string    `json:"configId"`
	Current     string    `json:"current"`
	Previous    string    `json:"previous,omitempty"`
	DeliveredAt time.Time `json:"deliveredAt"`
}

// SealRecord is the escrow artefact `openbao-seal`: every retained generation and
// the full delivery history.
type SealRecord struct {
	Generations []SealGeneration `json:"generations"`
	History     []SealDelivery   `json:"history"`
}

// NewSealRecord creates generation 1 with no delivery yet.
func NewSealRecord(now time.Time) (SealRecord, error) {
	g, err := newGeneration(now)
	if err != nil {
		return SealRecord{}, err
	}
	return SealRecord{Generations: []SealGeneration{g}}, nil
}

func newGeneration(now time.Time) (SealGeneration, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return SealGeneration{}, fmt.Errorf("generate seal key: %w", err)
	}
	return SealGeneration{
		ID:          "gen-" + now.UTC().Format("20060102T150405Z"),
		Key:         hex.EncodeToString(b),
		ActivatedAt: now.UTC(),
	}, nil
}

// ParseSealRecord reads the escrow payload and validates it.
func ParseSealRecord(payload string) (SealRecord, error) {
	var r SealRecord
	if err := json.Unmarshal([]byte(payload), &r); err != nil {
		return r, fmt.Errorf("the escrowed seal record is not valid JSON: %w", err)
	}
	if len(r.Generations) == 0 {
		return r, errors.New("the escrowed seal record holds no generation")
	}
	for _, g := range r.Generations {
		if b, err := hex.DecodeString(g.Key); err != nil || len(b) != 32 || g.ID == "" {
			return r, fmt.Errorf("seal generation %q is malformed: a 32-byte hex key and an id are required", g.ID)
		}
	}
	return r, nil
}

// Marshal renders the record for the escrow.
func (r SealRecord) Marshal() (string, error) {
	b, err := json.Marshal(r)
	return string(b), err
}

func (r SealRecord) generation(id string) (SealGeneration, bool) {
	for _, g := range r.Generations {
		if g.ID == id {
			return g, true
		}
	}
	return SealGeneration{}, false
}

// Current is the newest unretired generation.
func (r SealRecord) Current() SealGeneration {
	gs := append([]SealGeneration(nil), r.Generations...)
	sort.Slice(gs, func(i, j int) bool { return gs[i].ActivatedAt.Before(gs[j].ActivatedAt) })
	return gs[len(gs)-1]
}

// InForce is the configuration delivered most recently, or the current generation
// alone when nothing has been delivered yet.
func (r SealRecord) InForce() SealDelivery {
	if n := len(r.History); n > 0 {
		return r.History[n-1]
	}
	return SealDelivery{Current: r.Current().ID}
}

// ConfigAt is the configuration that was in force at t: the last delivery at or
// before t. A restore delivers exactly this (ADR-117 §7); it is never inferred.
func (r SealRecord) ConfigAt(t time.Time) (SealDelivery, bool) {
	var found *SealDelivery
	for i := range r.History {
		if !r.History[i].DeliveredAt.After(t) {
			found = &r.History[i]
		}
	}
	if found == nil {
		return SealDelivery{}, false
	}
	return *found, true
}

// SecretData renders a configuration as the environment the static seal reads.
func (r SealRecord) SecretData(cfg SealDelivery) (map[string]string, error) {
	cur, ok := r.generation(cfg.Current)
	if !ok {
		return nil, fmt.Errorf("seal configuration %s names generation %q, which the escrow does not hold",
			cfg.ConfigID, cfg.Current)
	}
	data := map[string]string{envCurrentKey: cur.Key, envCurrentKeyID: cur.ID}
	if cfg.Previous != "" {
		prev, ok := r.generation(cfg.Previous)
		if !ok {
			return nil, fmt.Errorf("seal configuration %s names previous generation %q, which the escrow "+
				"does not hold -- a backup taken under it could not be unsealed", cfg.ConfigID, cfg.Previous)
		}
		data[envPreviousKey] = prev.Key
		data[envPreviousID] = prev.ID
	}
	return data, nil
}

// Deliver writes cfg as Secret openbao-seal and appends it to the history when it
// differs from what is in force. It returns the record to write back to the escrow
// BEFORE the Secret is relied on, so a delivery is never unrecorded.
func Deliver(ctx context.Context, cs kubernetes.Interface, r SealRecord, cfg SealDelivery, now time.Time) (SealRecord, error) {
	data, err := r.SecretData(cfg)
	if err != nil {
		return r, err
	}
	if inForce := r.InForce(); len(r.History) == 0 || inForce.Current != cfg.Current || inForce.Previous != cfg.Previous {
		cfg.DeliveredAt = now.UTC()
		cfg.ConfigID = fmt.Sprintf("cfg-%d", len(r.History)+1)
		r.History = append(r.History, cfg)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SealSecretName,
			Namespace: Namespace,
			Labels:    map[string]string{"app.kubernetes.io/part-of": "openbao", "app.kubernetes.io/managed-by": "kakt"},
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: data,
	}
	existing, err := cs.CoreV1().Secrets(Namespace).Get(ctx, SealSecretName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = cs.CoreV1().Secrets(Namespace).Create(ctx, sec, metav1.CreateOptions{})
	case err == nil:
		// Replace the data wholesale: a previous key that is no longer configured
		// must disappear, or envFrom would keep presenting it.
		existing.Data = nil
		existing.StringData = data
		_, err = cs.CoreV1().Secrets(Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return r, fmt.Errorf("deliver %s/%s: %w", Namespace, SealSecretName, err)
	}
	return r, nil
}

// EnsureSealRecord makes sure the box escrow holds the seal record, creating
// generation 1 the first time (ADR-117 §3). An existing record is validated and
// kept, never regenerated: a new key would leave the store sealed.
func EnsureSealRecord(ctx context.Context, box ProjectStore, now time.Time) (bool, error) {
	raw, err := box.RestoreProjectArtifact(ctx, ArtifactSeal)
	if err != nil {
		return false, fmt.Errorf("read the escrowed seal record: %w", err)
	}
	if strings.TrimSpace(raw) != "" {
		if _, err := ParseSealRecord(raw); err != nil {
			return false, err
		}
		return false, nil
	}
	r, err := NewSealRecord(now)
	if err != nil {
		return false, err
	}
	payload, err := r.Marshal()
	if err != nil {
		return false, err
	}
	if err := box.BackupProjectArtifact(ctx, ArtifactSeal, payload); err != nil {
		return false, fmt.Errorf("escrow the seal record: %w", err)
	}
	return true, nil
}
