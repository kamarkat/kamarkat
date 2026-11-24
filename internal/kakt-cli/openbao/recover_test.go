package openbao

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVault is the slice of OpenBao the recovery identity uses.
type fakeVault struct {
	mu        sync.Mutex
	self      string           // this session's accessor
	tokens    map[string]int64 // accessor -> creation time
	revoked   []string
	kv        map[string]map[string]string
	rotations map[string]time.Time
	creds     map[string]string // static role -> password
	clock     time.Time
}

func newFakeVault(began time.Time) *fakeVault {
	return &fakeVault{
		self: "self",
		tokens: map[string]int64{
			"self":           began.Unix(),
			"restored-a":     began.Add(-48 * time.Hour).Unix(),
			"restored-b":     began.Add(-time.Hour).Unix(),
			"issued-since-1": began.Add(time.Minute).Unix(),
		},
		kv:        map[string]map[string]string{},
		rotations: map[string]time.Time{"hub-zitadel": began.Add(-72 * time.Hour)},
		creds:     map[string]string{"hub-zitadel": "old"},
		clock:     began,
	}
}

func (f *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v1/")
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case p == "auth/token/lookup-self":
		reply(map[string]any{"data": map[string]any{"accessor": f.self, "creation_time": f.tokens[f.self]}})
	case p == "auth/token/accessors" && r.Method == "LIST":
		var keys []string
		for a := range f.tokens {
			keys = append(keys, a)
		}
		reply(map[string]any{"data": map[string]any{"keys": keys}})
	case p == "auth/token/lookup-accessor":
		ct, ok := f.tokens[body["accessor"].(string)]
		if !ok {
			http.Error(w, `{"errors":["invalid accessor"]}`, 400)
			return
		}
		reply(map[string]any{"data": map[string]any{"creation_time": ct}})
	case p == "auth/token/revoke-accessor":
		a := body["accessor"].(string)
		delete(f.tokens, a)
		f.revoked = append(f.revoked, a)
		w.WriteHeader(204)
	case strings.HasPrefix(p, "kv/data/"):
		k := strings.TrimPrefix(p, "kv/data/")
		if r.Method == http.MethodGet {
			d, ok := f.kv[k]
			if !ok {
				http.NotFound(w, r)
				return
			}
			reply(map[string]any{"data": map[string]any{"data": d}})
			return
		}
		d := map[string]string{}
		for kk, vv := range body["data"].(map[string]any) {
			d[kk] = vv.(string)
		}
		f.kv[k] = d
		w.WriteHeader(204)
	case strings.HasPrefix(p, "database/static-roles/"):
		n := strings.TrimPrefix(p, "database/static-roles/")
		reply(map[string]any{"data": map[string]any{"last_vault_rotation": f.rotations[n].Format(time.RFC3339Nano)}})
	case strings.HasPrefix(p, "database/rotate-role/"):
		n := strings.TrimPrefix(p, "database/rotate-role/")
		f.clock = f.clock.Add(time.Second)
		f.rotations[n] = f.clock
		f.creds[n] = "rotated-" + f.clock.Format("150405")
		w.WriteHeader(204)
	case strings.HasPrefix(p, "database/static-creds/"):
		n := strings.TrimPrefix(p, "database/static-creds/")
		reply(map[string]any{"data": map[string]any{"username": strings.ReplaceAll(n, "-", "_"), "password": f.creds[n]}})
	default:
		http.NotFound(w, r)
	}
}

// fakeLive is the cluster.
type fakeLive struct {
	srv       *httptest.Server
	dbs       map[string]DatabaseState
	hub       HubState
	admission bool
	stale     bool
	restarts  int
	delivered []SealDelivery
	logins    map[string]string // role -> password that authenticated
	vault     *fakeVault
	// hubCatchesUpAfter: hub-operator reports the incarnation after this many polls.
	hubCatchesUpAfter int
	polls             int
	bao               DatabaseState
	missing           error // returned for a database not in dbs
}

func (l *fakeLive) Database(_ context.Context, name string) (DatabaseState, error) {
	st, ok := l.dbs[name]
	if !ok {
		if l.missing != nil {
			return DatabaseState{}, l.missing
		}
		return DatabaseState{}, errors.New("no such cluster " + name)
	}
	return st, nil
}
func (l *fakeLive) Hub(context.Context) (HubState, error) {
	l.polls++
	h := l.hub
	if l.polls > l.hubCatchesUpAfter {
		h.OpenBaoIncarnation = l.dbs[DatabaseOpenBao].UID
		if h.PlatformSecretsWritten && h.PlatformSecretsMessage == "" {
			h.PlatformSecretsMessage = "Day-0 and application secrets are in OpenBao (openbao-db incarnation " +
				l.dbs[DatabaseOpenBao].UID + ")"
		}
	}
	return h, nil
}
func (l *fakeLive) ConsumerAdmissionOpen(context.Context) (bool, error) { return l.admission, nil }
func (l *fakeLive) OpenBaoStartedBefore(context.Context, time.Time) (bool, error) {
	return l.stale, nil
}
func (l *fakeLive) RestartOpenBao(context.Context) error { l.restarts++; l.stale = false; return nil }
func (l *fakeLive) Connect(context.Context) (*Client, error) {
	return &Client{addr: l.srv.URL, http: l.srv.Client(), token: "t"}, nil
}
func (l *fakeLive) VerifyLogin(_ context.Context, role AppRole, _, password string) error {
	l.vault.mu.Lock()
	defer l.vault.mu.Unlock()
	if password != l.vault.creds[role.StaticRoleName()] {
		return errors.New("password authentication failed")
	}
	l.logins[role.Role] = password
	return nil
}
func (l *fakeLive) DeliverSeal(_ context.Context, r SealRecord, cfg SealDelivery) (SealRecord, bool, error) {
	l.delivered = append(l.delivered, cfg)
	changed := r.InForce().Current != cfg.Current || r.InForce().Previous != cfg.Previous
	if changed {
		cfg.DeliveredAt = now.Add(1000 * time.Hour)
		r.History = append(r.History, cfg)
	}
	return r, changed, nil
}

type memEscrow map[string]string

func (m memEscrow) BackupProjectArtifact(_ context.Context, n, p string) error { m[n] = p; return nil }
func (m memEscrow) RestoreProjectArtifact(_ context.Context, n string) (string, error) {
	return m[n], nil
}

var zitadel = AppRole{Role: "hub_zitadel", Database: "zitadel", Cluster: "zitadel-db"}

func recoverySetup(t *testing.T, baoRecovery, appRecovery, appReady bool) (*fakeLive, memEscrow) {
	t.Helper()
	began := now
	v := newFakeVault(began)
	srv := httptest.NewServer(v)
	t.Cleanup(srv.Close)
	rec, _ := NewSealRecord(now.Add(-100 * time.Hour))
	rec.History = []SealDelivery{{ConfigID: "cfg-1", Current: rec.Current().ID, DeliveredAt: now.Add(-100 * time.Hour)}}
	payload, _ := rec.Marshal()
	esc := memEscrow{ArtifactSeal: payload}
	return &fakeLive{
		srv: srv, vault: v, logins: map[string]string{},
		dbs: map[string]DatabaseState{
			DatabaseOpenBao: {Name: DatabaseOpenBao, UID: "bao-2", Ready: true, Recovery: baoRecovery, Created: now},
			"zitadel-db":    {Name: "zitadel-db", UID: "zit-2", Ready: appReady, Recovery: appRecovery},
		},
		hub: HubState{OpenBaoIncarnation: "bao-1", PlatformSecretsWritten: true, Roles: []AppRole{zitadel}},
	}, esc
}

func record(t *testing.T, l *fakeLive, cluster, uid string) RecoveryRecord {
	t.Helper()
	d, ok := l.vault.kv["platform/recovery/"+cluster+"/"+uid]
	if !ok {
		t.Fatalf("no recovery record for %s/%s", cluster, uid)
	}
	var r RecoveryRecord
	if err := json.Unmarshal([]byte(d["record"]), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRestoredStoreRecoveryRevokesWaitsReestablishesAndRecords(t *testing.T) {
	l, esc := recoverySetup(t, true, false, true)
	l.hubCatchesUpAfter = 3 // hub-operator needs a few polls to re-apply authorization
	if err := Recover(context.Background(), RecoverOptions{Live: l, Escrow: esc, Poll: time.Millisecond}); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	// Restored tokens revoked; the session's own and one issued since are kept.
	if strings.Join(sortStrings(l.vault.revoked), ",") != "restored-a,restored-b" {
		t.Fatalf("revoked %v, want exactly the two restored tokens", l.vault.revoked)
	}
	if _, kept := l.vault.tokens["issued-since-1"]; !kept {
		t.Fatal("a token issued after recovery began was revoked")
	}
	if l.logins["hub_zitadel"] == "" || l.logins["hub_zitadel"] == "old" {
		t.Fatalf("the re-established password was not proved by a login: %v", l.logins)
	}
	r := record(t, l, DatabaseOpenBao, "bao-2")
	for _, step := range []string{"authorization", "tokens", "generated", staticStep(zitadel, "zit-2")} {
		if _, ok := r.Steps[step]; !ok {
			t.Errorf("record lacks step %s: %v", step, r.Steps)
		}
	}
}

func TestRecoveryResumesAfterInterruptionWithoutRepeatingSteps(t *testing.T) {
	// The store and zitadel-db are both restored; zitadel-db is still restoring.
	l, esc := recoverySetup(t, true, true, false)
	err := Recover(context.Background(), RecoverOptions{Live: l, Escrow: esc, Poll: time.Millisecond})
	if !errors.Is(err, ErrRecoveryIncomplete) {
		t.Fatalf("first run: %v, want incomplete", err)
	}
	if l.vault.rotations["hub-zitadel"].After(now) {
		t.Fatal("a password was rotated before its database's restore completed")
	}
	first := record(t, l, DatabaseOpenBao, "bao-2")
	if _, ok := first.Steps["tokens"]; !ok {
		t.Fatal("the token step was not recorded on the first run")
	}

	// Interrupted; a new restored token cannot appear, but a re-run must not
	// sweep again (the session is new and newer than Began, so it is safe).
	revokedBefore := len(l.vault.revoked)
	zit := l.dbs["zitadel-db"]
	zit.Ready = true
	l.dbs["zitadel-db"] = zit
	if err := Recover(context.Background(), RecoverOptions{Live: l, Escrow: esc, Poll: time.Millisecond}); err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if len(l.vault.revoked) != revokedBefore {
		t.Fatal("the resumed run repeated the token step")
	}
	if !record(t, l, DatabaseOpenBao, "bao-2").Steps[staticStep(zitadel, "zit-2")].Equal(first.Began) {
		t.Fatal("the static step was not recorded in the store's record")
	}
	if _, ok := record(t, l, "zitadel-db", "zit-2").Steps[staticStep(zitadel, "zit-2")]; !ok {
		t.Fatal("the static step was not recorded in the application's own record")
	}
}

func TestApplicationDatabaseRestoredAloneReestablishesOnlyItsRole(t *testing.T) {
	l, esc := recoverySetup(t, false, true, true)
	if err := Recover(context.Background(), RecoverOptions{Live: l, Escrow: esc, Poll: time.Millisecond}); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if len(l.vault.revoked) != 0 {
		t.Fatal("tokens were revoked although the store itself was not restored")
	}
	if l.restarts != 0 || len(l.delivered) != 0 {
		t.Fatal("OpenBao was re-sealed or restarted for an application-only restore")
	}
	if _, ok := l.vault.kv["platform/recovery/openbao-db/bao-2"]; ok {
		t.Fatal("an application-only restore wrote a store recovery record")
	}
	if _, ok := record(t, l, "zitadel-db", "zit-2").Steps[staticStep(zitadel, "zit-2")]; !ok {
		t.Fatal("the application's record lacks its static step")
	}
}

func TestRecoveryRefusesWhileConsumersCanReachTheRestoredStore(t *testing.T) {
	l, esc := recoverySetup(t, true, false, true)
	l.admission = true
	err := Recover(context.Background(), RecoverOptions{Live: l, Escrow: esc, Poll: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "consumer admission is open") {
		t.Fatalf("got %v, want a refusal while admission is open", err)
	}
	if len(l.vault.revoked) != 0 || l.restarts != 0 {
		t.Fatal("recovery acted before refusing")
	}
}

func TestRecoveryRefusesOutsideRecoveryMode(t *testing.T) {
	l, esc := recoverySetup(t, false, false, true)
	if err := Recover(context.Background(), RecoverOptions{Live: l, Escrow: esc}); err == nil ||
		!strings.Contains(err.Error(), "no database is in ADR-092 recovery mode") {
		t.Fatalf("got %v", err)
	}
}

func TestRestoreDeliversTheSealConfigurationOfItsRecoveryPoint(t *testing.T) {
	l, esc := recoverySetup(t, true, false, true)
	// The escrow history: generation 1 until t1, then the overlap configuration.
	rec, _ := ParseSealRecord(esc[ArtifactSeal])
	g1 := rec.Current().ID
	g2, _ := newGeneration(now.Add(-50 * time.Hour))
	rec.Generations = append(rec.Generations, g2)
	rec.History = append(rec.History, SealDelivery{ConfigID: "cfg-2", Current: g2.ID, Previous: g1, DeliveredAt: now.Add(-50 * time.Hour)})
	payload, _ := rec.Marshal()
	esc[ArtifactSeal] = payload

	// A restore to a point BEFORE the change must be unsealed with generation 1.
	bao := l.dbs[DatabaseOpenBao]
	bao.RecoveryPoint = now.Add(-70 * time.Hour)
	l.dbs[DatabaseOpenBao] = bao
	if err := Recover(context.Background(), RecoverOptions{Live: l, Escrow: esc, Poll: time.Millisecond}); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if len(l.delivered) != 1 || l.delivered[0].Current != g1 || l.delivered[0].Previous != "" {
		t.Fatalf("delivered %+v, want generation %s alone", l.delivered, g1)
	}
	if l.restarts != 1 {
		t.Fatalf("OpenBao restarts = %d, want 1 onto the restored store", l.restarts)
	}
	got, _ := ParseSealRecord(esc[ArtifactSeal])
	if got.InForce().Current != g1 {
		t.Fatal("the escrow does not record the restored configuration as in force")
	}

	// No configuration recorded at the point: refused, never inferred.
	if _, err := SealConfigForRecoveryPoint(rec, now.Add(-500*time.Hour)); err == nil {
		t.Fatal("a recovery point before any recorded configuration was not refused")
	}
}

func sortStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// Day-0's delivery on a reconstructed hub: historical while openbao-db is a
// restore, latest on a first build, and never a guess when the store is unreadable.
func TestSealConfigForStoreSelectsByTheStoresState(t *testing.T) {
	l, esc := recoverySetup(t, true, false, true)
	rec, _ := ParseSealRecord(esc[ArtifactSeal])
	g1 := rec.Current().ID
	g2, _ := newGeneration(now.Add(-50 * time.Hour))
	rec.Generations = append(rec.Generations, g2)
	rec.History = append(rec.History, SealDelivery{ConfigID: "cfg-2", Current: g2.ID, DeliveredAt: now.Add(-50 * time.Hour)})
	bao := l.dbs[DatabaseOpenBao]
	bao.RecoveryPoint = now.Add(-70 * time.Hour)
	l.dbs[DatabaseOpenBao] = bao

	if cfg, _, err := SealConfigForStore(context.Background(), l, rec); err != nil || cfg.Current != g1 {
		t.Fatalf("restore in recovery mode: %+v %v, want %s", cfg, err, g1)
	}
	delete(l.dbs, DatabaseOpenBao)
	l.missing = ErrDatabaseNotFound
	if cfg, _, err := SealConfigForStore(context.Background(), l, rec); err != nil || cfg.Current != g2.ID {
		t.Fatalf("first build: %+v %v, want the latest %s", cfg, err, g2.ID)
	}
	l.missing = errors.New("connection refused")
	if _, _, err := SealConfigForStore(context.Background(), l, rec); err == nil {
		t.Fatal("an unreadable store was treated as no store")
	}
}

// A restore missing an authoritative value: hub-operator refuses to regenerate
// it, so recovery stays open, names the value, and nothing is marked complete.
func TestRecoveryStaysOpenWhileARestoredValueIsMissing(t *testing.T) {
	l, esc := recoverySetup(t, true, false, true)
	l.hub.PlatformSecretsWritten = false
	l.hub.PlatformSecretsMessage = "1 of 9 writes failed: hub-zitadel-masterkey: refusing to generate a secret value during an unfinished restore"
	var out strings.Builder
	err := Recover(context.Background(), RecoverOptions{Live: l, Escrow: esc, Out: &out, Poll: time.Millisecond})
	if !errors.Is(err, ErrRecoveryIncomplete) {
		t.Fatalf("got %v, want incomplete", err)
	}
	if !strings.Contains(out.String(), "hub-zitadel-masterkey") {
		t.Fatalf("the missing value is not reported:\n%s", out.String())
	}
	r := record(t, l, DatabaseOpenBao, "bao-2")
	if _, ok := r.Steps["generated"]; ok || !r.Completed.IsZero() {
		t.Fatalf("recovery recorded as complete with a value missing: %+v", r)
	}

	// The value is restored; hub-operator's next pass reports this incarnation.
	l.hub.PlatformSecretsWritten = true
	l.hub.PlatformSecretsMessage = ""
	if err := Recover(context.Background(), RecoverOptions{Live: l, Escrow: esc, Poll: time.Millisecond}); err != nil {
		t.Fatalf("after the value is restored: %v", err)
	}
	if record(t, l, DatabaseOpenBao, "bao-2").Completed.IsZero() {
		t.Fatal("a fully recovered incarnation is not marked complete")
	}
}

// A "written" condition from before the restore says nothing about the restored store.
func TestAPreRestoreSecretsConditionDoesNotCompleteRecovery(t *testing.T) {
	l, esc := recoverySetup(t, true, false, true)
	l.hub.PlatformSecretsMessage = "Day-0 and application secrets are in OpenBao (openbao-db incarnation bao-1)"
	err := Recover(context.Background(), RecoverOptions{Live: l, Escrow: esc, Poll: time.Millisecond})
	if !errors.Is(err, ErrRecoveryIncomplete) {
		t.Fatalf("got %v; a condition for the previous incarnation completed recovery", err)
	}
	if _, ok := record(t, l, DatabaseOpenBao, "bao-2").Steps["generated"]; ok {
		t.Fatal("the generated step was recorded from a stale condition")
	}
}

// An application whose credential the restore lost keeps recovery open, even
// though the hub's own secrets are all present.
func TestRecoveryStaysOpenWhileAnApplicationsCredentialIsMissing(t *testing.T) {
	l, esc := recoverySetup(t, true, false, true)
	l.hub.Applications = []ApplicationSecrets{
		{Name: "shop", Present: true, Message: "every credential is present (openbao-db incarnation bao-2)"},
		{Name: "blog", Present: false, Message: "failed to generate spokes/c1/tenants/blog/BLOG_INTERNAL_TOKEN for tenant acme: " +
			"refusing to generate a secret value during an unfinished restore"},
	}
	var out strings.Builder
	err := Recover(context.Background(), RecoverOptions{Live: l, Escrow: esc, Out: &out, Poll: time.Millisecond})
	if !errors.Is(err, ErrRecoveryIncomplete) || !strings.Contains(out.String(), "BLOG_INTERNAL_TOKEN") {
		t.Fatalf("got %v; output:\n%s", err, out.String())
	}
	if !record(t, l, DatabaseOpenBao, "bao-2").Completed.IsZero() {
		t.Fatal("recovery marked complete with an application credential missing")
	}
}
