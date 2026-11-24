package openbao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Restored-store recovery (ADR-117 §7, ADR-092).
//
// A restore recreates a CloudNativePG Cluster, so its UID changes: each restored
// database is a recovery incarnation. `kakt openbao recover` brings the box's
// OpenBao back to a trustworthy state for every incarnation in ADR-092 recovery
// mode, one verified step at a time, and records each step only after its effect
// is checked. A re-run resumes from the record; nothing runs on ordinary
// reconciliation. The record for an incarnation lives in OpenBao itself, at
// kv/platform/recovery/<cluster>/<uid>, written by the recovery identity.
//
// Steps for an openbao-db incarnation:
//
//	authorization   hub-operator re-applied all desired authorization against it
//	tokens          every token restored with the store is revoked
//	generated       hub-operator reports the platform secrets written
//	static:<role>@<application cluster uid>
//	                each application login role's password re-established by
//	                OpenBao, after that application's own database is serving
//
// An application database restored alone records only its static step.

// Recovery role and identity (manifests/hub-core-services/openbao/values.yaml).
const (
	roleRecovery = "openbao-recovery"
	saRecovery   = "openbao-recovery"
	// DatabaseOpenBao is the Cluster holding OpenBao's storage.
	DatabaseOpenBao = "openbao-db"
)

// AppRole is one hub application login role (HubEnvironment.spec.database.roles).
type AppRole struct {
	Role     string // e.g. hub_zitadel
	Database string // e.g. zitadel
	Cluster  string // e.g. zitadel-db
}

// StaticRoleName is the OpenBao static role for this login role.
func (r AppRole) StaticRoleName() string { return strings.ReplaceAll(r.Role, "_", "-") }

// DatabaseState is what the cluster says about one CloudNativePG Cluster.
type DatabaseState struct {
	Name     string
	UID      string
	Created  time.Time
	Ready    bool
	Recovery bool // spec.bootstrap.recovery: ADR-092 recovery mode
	// RecoveryPoint is the point the restore targets, when the declaration names
	// one (a recovery target time, or a named backup's end). Zero means the end of
	// the archive: the latest state.
	RecoveryPoint time.Time
}

// HubState is what hub-operator reports on the HubEnvironment.
type HubState struct {
	OpenBaoIncarnation     string
	PlatformSecretsWritten bool
	// PlatformSecretsMessage names the openbao-db incarnation a True condition
	// covers, and the missing keys a False one refused to regenerate.
	PlatformSecretsMessage string
	Roles                  []AppRole
	// Applications report TenantSecretsPresent the same way, per incarnation.
	Applications []ApplicationSecrets
}

// ApplicationSecrets is one tenant application's TenantSecretsPresent condition.
type ApplicationSecrets struct {
	Name    string
	Present bool
	Message string
}

// missingAfterRestore lists what the hub and every application have NOT
// confirmed present in incarnation uid, with hub-operator's reason.
func (h HubState) missingAfterRestore(uid string) []string {
	var out []string
	switch {
	case h.PlatformSecretsWritten && strings.Contains(h.PlatformSecretsMessage, uid):
	case !h.PlatformSecretsWritten && h.PlatformSecretsMessage != "":
		out = append(out, "hub platform secrets: "+h.PlatformSecretsMessage)
	default:
		out = append(out, "hub platform secrets: not yet checked against incarnation "+uid)
	}
	for _, a := range h.Applications {
		switch {
		case a.Present && strings.Contains(a.Message, uid):
		case !a.Present && a.Message != "":
			out = append(out, "application "+a.Name+": "+a.Message)
		default:
			out = append(out, "application "+a.Name+": not yet checked against incarnation "+uid)
		}
	}
	return out
}

// Live is the cluster side of recovery. The production implementation reads the
// cluster with kubectl and client-go; tests substitute a fake.
type Live interface {
	Database(ctx context.Context, name string) (DatabaseState, error)
	Hub(ctx context.Context) (HubState, error)
	ConsumerAdmissionOpen(ctx context.Context) (bool, error)
	// OpenBaoStartedBefore reports whether any OpenBao server pod started before t,
	// i.e. is still holding the store it served before the restore.
	OpenBaoStartedBefore(ctx context.Context, t time.Time) (bool, error)
	RestartOpenBao(ctx context.Context) error
	// Connect opens OpenBao as the recovery identity once it is unsealed.
	Connect(ctx context.Context) (*Client, error)
	// VerifyLogin authenticates to role's database with the delivered credential.
	VerifyLogin(ctx context.Context, role AppRole, username, password string) error
	// DeliverSeal writes the seal configuration Secret; it reports whether the
	// Secret's content changed.
	DeliverSeal(ctx context.Context, r SealRecord, cfg SealDelivery) (SealRecord, bool, error)
}

// RecoverOptions drives one run.
type RecoverOptions struct {
	Live   Live
	Escrow ProjectStore
	Out    io.Writer
	// AuthorizationWait bounds the wait for hub-operator's pass over a restored store.
	AuthorizationWait time.Duration
	Poll              time.Duration
}

// RecoveryRecord is the durable record of one incarnation's recovery.
type RecoveryRecord struct {
	Cluster     string               `json:"cluster"`
	Incarnation string               `json:"incarnation"`
	Began       time.Time            `json:"began"` // OpenBao's clock: the first session's token creation
	Steps       map[string]time.Time `json:"steps"`
	// Completed is set once every step of this incarnation is verified. It is what
	// hub-operator reads to resume generating values (ADR-117 §7).
	Completed time.Time `json:"completed,omitempty"`
}

func recordPath(cluster, uid string) string {
	return "kv/data/platform/recovery/" + cluster + "/" + uid
}

func loadRecord(ctx context.Context, c *Client, cluster, uid string, began time.Time) (*RecoveryRecord, error) {
	var out struct {
		Data struct {
			Data map[string]string `json:"data"`
		} `json:"data"`
	}
	err := c.Read(ctx, recordPath(cluster, uid), &out)
	switch {
	case errors.Is(err, ErrNotFound):
		return &RecoveryRecord{Cluster: cluster, Incarnation: uid, Began: began, Steps: map[string]time.Time{}}, nil
	case err != nil:
		return nil, fmt.Errorf("read the recovery record for %s/%s: %w", cluster, uid, err)
	}
	var r RecoveryRecord
	if err := json.Unmarshal([]byte(out.Data.Data["record"]), &r); err != nil {
		return nil, fmt.Errorf("the recovery record for %s/%s is unreadable: %w", cluster, uid, err)
	}
	if r.Steps == nil {
		r.Steps = map[string]time.Time{}
	}
	return &r, nil
}

func (r *RecoveryRecord) save(ctx context.Context, c *Client) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return c.Write(ctx, recordPath(r.Cluster, r.Incarnation),
		map[string]any{"data": map[string]string{"record": string(b)}}, nil)
}

func (r *RecoveryRecord) done(step string) bool { _, ok := r.Steps[step]; return ok }

func (r *RecoveryRecord) mark(ctx context.Context, c *Client, step string, at time.Time) error {
	r.Steps[step] = at
	return r.save(ctx, c)
}

func staticStep(role AppRole, appUID string) string {
	return "static:" + role.Role + "@" + appUID
}

// Recover runs every step that is due, in order, and reports what remains.
// It returns nil when every incarnation in recovery mode is completely recovered.
func Recover(ctx context.Context, opt RecoverOptions) error {
	if opt.Out == nil {
		opt.Out = io.Discard
	}
	out := opt.Out
	if opt.Poll == 0 {
		opt.Poll = 10 * time.Second
	}
	if opt.AuthorizationWait == 0 {
		opt.AuthorizationWait = 15 * time.Minute
	}
	live := opt.Live

	hub, err := live.Hub(ctx)
	if err != nil {
		return err
	}
	bao, err := live.Database(ctx, DatabaseOpenBao)
	if err != nil {
		return err
	}
	apps := map[string]DatabaseState{}
	for _, r := range hub.Roles {
		if _, seen := apps[r.Cluster]; seen {
			continue
		}
		st, err := live.Database(ctx, r.Cluster)
		if err != nil {
			return err
		}
		apps[r.Cluster] = st
	}
	var inRecovery []string
	if bao.Recovery {
		inRecovery = append(inRecovery, DatabaseOpenBao)
	}
	for name, st := range apps {
		if st.Recovery {
			inRecovery = append(inRecovery, name)
		}
	}
	if len(inRecovery) == 0 {
		return errors.New("no database is in ADR-092 recovery mode, so there is nothing to recover. " +
			"Enter recovery by pointing the database's descriptor at its database-recovery overlay " +
			"(and, for openbao-db, the OpenBao component at openbao-recovery)")
	}
	sort.Strings(inRecovery)
	fmt.Fprintf(out, "Databases in recovery mode: %s\n", strings.Join(inRecovery, ", "))

	if bao.Recovery {
		open, err := live.ConsumerAdmissionOpen(ctx)
		if err != nil {
			return err
		}
		if open {
			// After settling, the live Cluster may keep its restore stanza: a
			// completed record is a finished recovery, not a refusal.
			if done, err := recordCompleted(ctx, live, DatabaseOpenBao, bao.UID); err == nil && done {
				fmt.Fprintf(out, "openbao-db incarnation %s is already recovered.\n", bao.UID)
				return nil
			}
			return errors.New("openbao-db is in recovery mode but OpenBao's consumer admission is open " +
				"(NetworkPolicy openbao-consumers exists). Point the OpenBao component at its " +
				"openbao-recovery overlay first: consumers must not reach a store that still holds " +
				"restored tokens")
		}
		if !bao.Ready {
			return fmt.Errorf("openbao-db (incarnation %s) is still restoring. Re-run when the "+
				"Cluster reports a healthy state", bao.UID)
		}
		if err := restoreSeal(ctx, opt, bao); err != nil {
			return err
		}
	}

	c, err := live.Connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	self, err := lookupSelf(ctx, c)
	if err != nil {
		return err
	}

	pending := []string{}
	var baoRec *RecoveryRecord
	if bao.Recovery {
		if baoRec, err = loadRecord(ctx, c, DatabaseOpenBao, bao.UID, self.created); err != nil {
			return err
		}
		if err := recoverStore(ctx, opt, c, self, bao, baoRec); err != nil {
			return err
		}
		if hub, err = live.Hub(ctx); err != nil {
			return err
		}
		if !baoRec.done("generated") {
			// Only a pass over THIS incarnation counts, by hub-operator and by every
			// application; anything missing from the restored store is reported, not
			// replaced, and keeps recovery open until it is restored.
			if missing := hub.missingAfterRestore(bao.UID); len(missing) > 0 {
				for _, m := range missing {
					pending = append(pending, "openbao-db: "+m)
				}
			} else {
				if err := baoRec.mark(ctx, c, "generated", self.created); err != nil {
					return err
				}
				fmt.Fprintf(out, "  ✓ every hub and application secret is present in the restored store; none was regenerated\n")
			}
		}
	}

	// Static passwords: for every role when the store was restored, and for the
	// role of every application database restored on its own.
	appRecs := map[string]*RecoveryRecord{}
	for _, role := range hub.Roles {
		app := apps[role.Cluster]
		if !bao.Recovery && !app.Recovery {
			continue
		}
		var appRec *RecoveryRecord
		if app.Recovery {
			if appRec = appRecs[role.Cluster]; appRec == nil {
				if appRec, err = loadRecord(ctx, c, role.Cluster, app.UID, self.created); err != nil {
					return err
				}
				appRecs[role.Cluster] = appRec
			}
		}
		step := staticStep(role, app.UID)
		if (baoRec == nil || baoRec.done(step)) && (appRec == nil || appRec.done(step)) {
			continue
		}
		if !app.Ready {
			// The restore must complete first: rotating before it would let the
			// restore bring back the old password hash.
			pending = append(pending, fmt.Sprintf("%s: %s is still restoring; its password is "+
				"re-established once it is serving", role.Role, role.Cluster))
			continue
		}
		if err := reestablishStatic(ctx, live, c, role); err != nil {
			return err
		}
		for _, rec := range []*RecoveryRecord{baoRec, appRec} {
			if rec != nil && !rec.done(step) {
				if err := rec.mark(ctx, c, step, self.created); err != nil {
					return err
				}
			}
		}
		fmt.Fprintf(out, "  ✓ %s: password re-established by OpenBao and a login verified\n", role.Role)
	}

	if len(pending) > 0 {
		fmt.Fprintf(out, "\nRecovery is not complete:\n")
		for _, p := range pending {
			fmt.Fprintf(out, "  - %s\n", p)
		}
		fmt.Fprintf(out, "\nRe-run `kakt openbao recover` once they are resolved; completed steps are kept.\n")
		return ErrRecoveryIncomplete
	}
	// Every step of every incarnation is done: mark the records complete, which
	// lets hub-operator generate values for genuinely new resources again.
	completed := []*RecoveryRecord{baoRec}
	for _, r := range appRecs {
		completed = append(completed, r)
	}
	for _, r := range completed {
		if r == nil || !r.Completed.IsZero() {
			continue
		}
		r.Completed = self.created
		if err := r.save(ctx, c); err != nil {
			return err
		}
	}
	for _, name := range inRecovery {
		fmt.Fprintf(out, "  ✓ %s: recovery record complete\n", name)
	}
	fmt.Fprintf(out, "\nEvery incarnation in recovery mode is recovered. Settle each database's "+
		"descriptor (ADR-092)%s; the preflight then proves no recovery mode remains.\n",
		map[bool]string{true: ", and the OpenBao component's, which restores consumer admission", false: ""}[bao.Recovery])
	return nil
}

// ErrDatabaseNotFound is a Cluster that does not exist (yet).
var ErrDatabaseNotFound = errors.New("database not found")

// ErrRecoveryIncomplete is a run that made progress but is waiting on something.
var ErrRecoveryIncomplete = errors.New("recovery incomplete")

// restoreSeal delivers the seal configuration that was in force at the restore's
// recovery point (ADR-117 §7) and restarts OpenBao onto the restored store.
func restoreSeal(ctx context.Context, opt RecoverOptions, bao DatabaseState) error {
	raw, err := opt.Escrow.RestoreProjectArtifact(ctx, ArtifactSeal)
	if err != nil {
		return fmt.Errorf("read the escrowed seal record: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return errors.New("the escrow holds no OpenBao seal record; the store cannot be unsealed")
	}
	rec, err := ParseSealRecord(raw)
	if err != nil {
		return err
	}
	cfg, _, err := SealConfigForStore(ctx, opt.Live, rec)
	if err != nil {
		return err
	}
	updated, changed, err := opt.Live.DeliverSeal(ctx, rec, cfg)
	if err != nil {
		return err
	}
	if len(updated.History) != len(rec.History) {
		payload, err := updated.Marshal()
		if err != nil {
			return err
		}
		if err := opt.Escrow.BackupProjectArtifact(ctx, ArtifactSeal, payload); err != nil {
			return fmt.Errorf("record the restored seal configuration in the escrow: %w", err)
		}
	}
	stale, err := opt.Live.OpenBaoStartedBefore(ctx, bao.Created)
	if err != nil {
		return err
	}
	if changed || stale {
		fmt.Fprintf(opt.Out, "Restarting OpenBao onto the restored store (seal configuration for %s)\n",
			pointLabel(bao.RecoveryPoint))
		return opt.Live.RestartOpenBao(ctx)
	}
	return nil
}

// SealConfigForStore is the seal configuration openbao-db needs now: the one in
// force at its recovery point while it is a restore in recovery mode, the latest
// otherwise. Day-0 and `kakt openbao recover` both deliver through this, so a
// reconstructed hub is unsealed with the key its restored data was written under.
func SealConfigForStore(ctx context.Context, live Live, rec SealRecord) (SealDelivery, DatabaseState, error) {
	st, err := live.Database(ctx, DatabaseOpenBao)
	if err != nil {
		// No store yet, on a first build: the latest. Anything else is unknown, and
		// guessing could deliver a key the restored data was not written under.
		if errors.Is(err, ErrDatabaseNotFound) {
			return rec.InForce(), st, nil
		}
		return SealDelivery{}, st, err
	}
	if !st.Recovery {
		return rec.InForce(), st, nil
	}
	cfg, err := SealConfigForRecoveryPoint(rec, st.RecoveryPoint)
	return cfg, st, err
}

// SealConfigForRecoveryPoint is the configuration in force at point; the zero
// point is the end of the archive, i.e. the latest configuration.
func SealConfigForRecoveryPoint(rec SealRecord, point time.Time) (SealDelivery, error) {
	if point.IsZero() {
		return rec.InForce(), nil
	}
	cfg, ok := rec.ConfigAt(point)
	if !ok {
		return SealDelivery{}, fmt.Errorf("the escrow records no seal configuration in force at %s, "+
			"so the store restored to that point cannot be unsealed; it is never inferred",
			point.UTC().Format(time.RFC3339))
	}
	return cfg, nil
}

func pointLabel(t time.Time) string {
	if t.IsZero() {
		return "the end of the archive"
	}
	return t.UTC().Format(time.RFC3339)
}

type session struct {
	accessor string
	created  time.Time
}

func lookupSelf(ctx context.Context, c *Client) (session, error) {
	var out struct {
		Data struct {
			Accessor     string `json:"accessor"`
			CreationTime int64  `json:"creation_time"`
		} `json:"data"`
	}
	if err := c.Read(ctx, "auth/token/lookup-self", &out); err != nil {
		return session{}, fmt.Errorf("look up the recovery session: %w", err)
	}
	return session{accessor: out.Data.Accessor, created: time.Unix(out.Data.CreationTime, 0).UTC()}, nil
}

// recoverStore runs the openbao-db steps: authorization, then token revocation.
func recoverStore(ctx context.Context, opt RecoverOptions, c *Client, self session, bao DatabaseState, rec *RecoveryRecord) error {
	if !rec.done("authorization") {
		deadline := time.Now().Add(opt.AuthorizationWait)
		for {
			hub, err := opt.Live.Hub(ctx)
			if err != nil {
				return err
			}
			if hub.OpenBaoIncarnation == bao.UID {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("hub-operator has not re-applied authorization to openbao-db "+
					"incarnation %s within %s (it reports %q). Check its logs and "+
					"HubEnvironment condition OpenBaoAuthorizationReconciled, then re-run",
					bao.UID, opt.AuthorizationWait, hub.OpenBaoIncarnation)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(opt.Poll):
			}
		}
		if err := rec.mark(ctx, c, "authorization", self.created); err != nil {
			return err
		}
		fmt.Fprintf(opt.Out, "  ✓ authorization re-applied to incarnation %s\n", bao.UID)
	}
	if !rec.done("tokens") {
		n, err := revokeRestoredTokens(ctx, c, self.accessor, rec.Began)
		if err != nil {
			return err
		}
		if err := rec.mark(ctx, c, "tokens", self.created); err != nil {
			return err
		}
		fmt.Fprintf(opt.Out, "  ✓ %d restored token(s) revoked; only tokens issued since recovery began remain\n", n)
	}
	return nil
}

// revokeRestoredTokens revokes every token created before began except the
// recovery session's own, then verifies none remains.
func revokeRestoredTokens(ctx context.Context, c *Client, own string, began time.Time) (int, error) {
	revoked := 0
	for pass := 0; pass < 2; pass++ {
		stale, err := staleAccessors(ctx, c, own, began)
		if err != nil {
			return revoked, err
		}
		if len(stale) == 0 {
			return revoked, nil
		}
		if pass == 1 {
			return revoked, fmt.Errorf("%d restored token(s) survived revocation: %v", len(stale), stale)
		}
		for _, a := range stale {
			err := c.Write(ctx, "auth/token/revoke-accessor", map[string]any{"accessor": a}, nil)
			var ae *APIError
			if err != nil && !(errors.As(err, &ae) && ae.Status == 400) {
				return revoked, fmt.Errorf("revoke accessor %s: %w", a, err)
			}
			revoked++
		}
	}
	return revoked, nil
}

func staleAccessors(ctx context.Context, c *Client, own string, began time.Time) ([]string, error) {
	var list struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := c.List(ctx, "auth/token/accessors", &list); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("list token accessors: %w", err)
	}
	var stale []string
	for _, a := range list.Data.Keys {
		if a == own {
			continue
		}
		var info struct {
			Data struct {
				CreationTime int64 `json:"creation_time"`
			} `json:"data"`
		}
		err := c.Write(ctx, "auth/token/lookup-accessor", map[string]any{"accessor": a}, &info)
		var ae *APIError
		if err != nil && errors.As(err, &ae) && ae.Status == 400 {
			continue // expired or revoked between list and lookup
		}
		if err != nil {
			return nil, fmt.Errorf("look up accessor %s: %w", a, err)
		}
		if time.Unix(info.Data.CreationTime, 0).Before(began) {
			stale = append(stale, a)
		}
	}
	sort.Strings(stale)
	return stale, nil
}

// reestablishStatic rotates role's password inside OpenBao, proves the rotation
// happened, and proves the delivered credential authenticates.
func reestablishStatic(ctx context.Context, live Live, c *Client, role AppRole) error {
	name := role.StaticRoleName()
	before, err := staticRotation(ctx, c, name)
	if err != nil {
		return err
	}
	if err := c.Write(ctx, "database/rotate-role/"+name, map[string]any{}, nil); err != nil {
		return fmt.Errorf("rotate %s: %w", name, err)
	}
	after, err := staticRotation(ctx, c, name)
	if err != nil {
		return err
	}
	if !after.After(before) {
		return fmt.Errorf("%s reports no rotation after rotate-role (last %s)", name, after.Format(time.RFC3339))
	}
	var creds struct {
		Data struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"data"`
	}
	if err := c.Read(ctx, "database/static-creds/"+name, &creds); err != nil {
		return fmt.Errorf("read %s's credential: %w", name, err)
	}
	if err := live.VerifyLogin(ctx, role, creds.Data.Username, creds.Data.Password); err != nil {
		return fmt.Errorf("%s's re-established password does not authenticate to %s: %w", role.Role, role.Cluster, err)
	}
	return nil
}

func staticRotation(ctx context.Context, c *Client, name string) (time.Time, error) {
	var out struct {
		Data struct {
			LastVaultRotation string `json:"last_vault_rotation"`
		} `json:"data"`
	}
	if err := c.Read(ctx, "database/static-roles/"+name, &out); err != nil {
		return time.Time{}, fmt.Errorf("read static role %s: %w", name, err)
	}
	if out.Data.LastVaultRotation == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, out.Data.LastVaultRotation)
	if err != nil {
		return time.Time{}, fmt.Errorf("static role %s reports an unreadable rotation time %q", name, out.Data.LastVaultRotation)
	}
	return t, nil
}

// recordCompleted reports whether an incarnation's recovery record is complete.
func recordCompleted(ctx context.Context, live Live, cluster, uid string) (bool, error) {
	c, err := live.Connect(ctx)
	if err != nil {
		return false, err
	}
	defer c.Close()
	r, err := loadRecord(ctx, c, cluster, uid, time.Time{})
	if err != nil {
		return false, err
	}
	return !r.Completed.IsZero(), nil
}
