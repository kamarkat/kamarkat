package openbao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// ClusterLive is the production Live: the box's management cluster, read with
// client-go and (for custom resources) kubectl.
type ClusterLive struct {
	CS         kubernetes.Interface
	Kubeconfig string
	// UnsealWait bounds the wait for OpenBao to come back after a restart.
	UnsealWait time.Duration
}

func (l ClusterLive) kubectlJSON(ctx context.Context, out any, args ...string) error {
	if l.Kubeconfig != "" {
		args = append([]string{"--kubeconfig", l.Kubeconfig}, args...)
	}
	b, err := exec.CommandContext(ctx, "kubectl", append(args, "-o", "json")...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("kubectl %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return err
	}
	return json.Unmarshal(b, out)
}

type cnpgCluster struct {
	Metadata struct {
		UID               string    `json:"uid"`
		CreationTimestamp time.Time `json:"creationTimestamp"`
	} `json:"metadata"`
	Spec struct {
		Bootstrap struct {
			Recovery *struct {
				Backup *struct {
					Name string `json:"name"`
				} `json:"backup"`
				RecoveryTarget *struct {
					TargetTime string `json:"targetTime"`
				} `json:"recoveryTarget"`
			} `json:"recovery"`
		} `json:"bootstrap"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

// Database reads one CloudNativePG Cluster in platform-data.
func (l ClusterLive) Database(ctx context.Context, name string) (DatabaseState, error) {
	var c cnpgCluster
	if err := l.kubectlJSON(ctx, &c, "get", "clusters.postgresql.cnpg.io", name, "-n", Namespace); err != nil {
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "not found") {
			return DatabaseState{}, fmt.Errorf("%w: %s", ErrDatabaseNotFound, name)
		}
		return DatabaseState{}, fmt.Errorf("read database %s: %w", name, err)
	}
	st := DatabaseState{
		Name:    name,
		UID:     c.Metadata.UID,
		Created: c.Metadata.CreationTimestamp,
		Ready:   c.Status.Phase == "Cluster in healthy state",
	}
	rec := c.Spec.Bootstrap.Recovery
	if rec == nil {
		return st, nil
	}
	st.Recovery = true
	switch {
	case rec.RecoveryTarget != nil && strings.TrimSpace(rec.RecoveryTarget.TargetTime) != "":
		t, err := parsePGTime(rec.RecoveryTarget.TargetTime)
		if err != nil {
			return st, fmt.Errorf("database %s: recovery target time %q: %w", name, rec.RecoveryTarget.TargetTime, err)
		}
		st.RecoveryPoint = t
	case rec.Backup != nil && rec.Backup.Name != "":
		var b struct {
			Status struct {
				StoppedAt time.Time `json:"stoppedAt"`
			} `json:"status"`
		}
		if err := l.kubectlJSON(ctx, &b, "get", "backups.postgresql.cnpg.io", rec.Backup.Name, "-n", Namespace); err != nil {
			return st, fmt.Errorf("database %s restores from backup %s: %w", name, rec.Backup.Name, err)
		}
		if b.Status.StoppedAt.IsZero() {
			return st, fmt.Errorf("database %s restores from backup %s, which reports no end time", name, rec.Backup.Name)
		}
		st.RecoveryPoint = b.Status.StoppedAt
	}
	// Otherwise the restore replays the archive to its end: the latest state.
	return st, nil
}

// parsePGTime accepts the timestamp forms CloudNativePG's targetTime takes.
func parsePGTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999-07", "2006-01-02 15:04:05.999999-07:00",
		"2006-01-02 15:04:05-07", "2006-01-02 15:04:05-07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("unrecognised timestamp")
}

// Hub reads what hub-operator reports on the HubEnvironment.
func (l ClusterLive) Hub(ctx context.Context) (HubState, error) {
	var he struct {
		Spec struct {
			Database struct {
				Roles []struct {
					Name       string `json:"name"`
					Database   string `json:"database"`
					ClusterRef string `json:"clusterRef"`
				} `json:"roles"`
			} `json:"database"`
		} `json:"spec"`
		Status struct {
			OpenBaoIncarnation string `json:"openBaoIncarnation"`
			Conditions         []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := l.kubectlJSON(ctx, &he, "get", "hubenvironment", "hub-environment"); err != nil {
		return HubState{}, fmt.Errorf("read the HubEnvironment: %w", err)
	}
	st := HubState{OpenBaoIncarnation: he.Status.OpenBaoIncarnation}
	for _, c := range he.Status.Conditions {
		if c.Type == "PlatformSecretsWritten" {
			st.PlatformSecretsWritten = c.Status == "True"
			st.PlatformSecretsMessage = c.Message
		}
	}
	for _, r := range he.Spec.Database.Roles {
		st.Roles = append(st.Roles, AppRole{Role: r.Name, Database: r.Database, Cluster: r.ClusterRef})
	}
	var apps struct {
		Items []struct {
			Metadata struct {
				Name              string     `json:"name"`
				DeletionTimestamp *time.Time `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Message string `json:"message"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := l.kubectlJSON(ctx, &apps, "get", "ainativesaases.nutgraf.in", "-A"); err != nil {
		return HubState{}, fmt.Errorf("list applications: %w", err)
	}
	for _, a := range apps.Items {
		if a.Metadata.DeletionTimestamp != nil {
			continue
		}
		app := ApplicationSecrets{Name: a.Metadata.Name}
		for _, c := range a.Status.Conditions {
			if c.Type == "TenantSecretsPresent" {
				app.Present, app.Message = c.Status == "True", c.Message
			}
		}
		st.Applications = append(st.Applications, app)
	}
	return st, nil
}

// ConsumerAdmissionOpen reports whether ESO, cert-manager and the Gateway may
// reach OpenBao (the steady-state openbao-consumers NetworkPolicy exists).
func (l ClusterLive) ConsumerAdmissionOpen(ctx context.Context) (bool, error) {
	_, err := l.CS.NetworkingV1().NetworkPolicies(Namespace).Get(ctx, "openbao-consumers", metav1.GetOptions{})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("read %s/openbao-consumers: %w", Namespace, err)
	}
}

const serverSelector = "app.kubernetes.io/name=openbao,component=server"

// OpenBaoStartedBefore reports whether any OpenBao server pod started before t.
func (l ClusterLive) OpenBaoStartedBefore(ctx context.Context, t time.Time) (bool, error) {
	pods, err := l.CS.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{LabelSelector: serverSelector})
	if err != nil {
		return false, fmt.Errorf("list OpenBao pods: %w", err)
	}
	for _, p := range pods.Items {
		if p.Status.StartTime == nil || p.Status.StartTime.Time.Before(t) {
			return true, nil
		}
	}
	return false, nil
}

// RestartOpenBao rolls the StatefulSet so every node loads the restored store.
func (l ClusterLive) RestartOpenBao(ctx context.Context) error {
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		time.Now().UTC().Format(time.RFC3339))
	if _, err := l.CS.AppsV1().StatefulSets(Namespace).Patch(ctx, "openbao", types.StrategicMergePatchType,
		[]byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("restart OpenBao: %w", err)
	}
	return nil
}

// Connect waits for OpenBao to be serving and unsealed, then logs in FRESHLY as
// the recovery identity (a restored token is never used).
func (l ClusterLive) Connect(ctx context.Context) (*Client, error) {
	wait := l.UnsealWait
	if wait == 0 {
		wait = 30 * time.Minute
	}
	deadline := time.Now().Add(wait)
	for {
		c, err := Connect(ctx, l.CS, l.Kubeconfig)
		if err == nil {
			s, serr := c.SealStatus(ctx)
			if serr == nil && s.Initialized && !s.Sealed {
				if err := c.LoginAs(ctx, l.CS, saRecovery, roleRecovery); err != nil {
					c.Close()
					return nil, err
				}
				return c, nil
			}
			c.Close()
			err = fmt.Errorf("status %+v %v", s, serr)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("OpenBao is not serving and unsealed after %s (%v). If its pods report "+
				"a seal error, the escrowed seal configuration does not match the restored store", wait, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// VerifyLogin authenticates to role's own database, as the application would.
func (l ClusterLive) VerifyLogin(ctx context.Context, role AppRole, username, password string) error {
	port, stop, err := PortForward(ctx, l.Kubeconfig, Namespace, "svc/"+role.Cluster+"-rw", 5432)
	if err != nil {
		return err
	}
	defer stop()
	dsn := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(username, password),
		Host:     "127.0.0.1:" + strconv.Itoa(port),
		Path:     "/" + role.Database,
		RawQuery: "sslmode=require&connect_timeout=10",
	}).String()
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(cctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return conn.Ping(cctx)
}

// DeliverSeal writes the configuration and reports whether the Secret changed.
func (l ClusterLive) DeliverSeal(ctx context.Context, r SealRecord, cfg SealDelivery) (SealRecord, bool, error) {
	want, err := r.SecretData(cfg)
	if err != nil {
		return r, false, err
	}
	have := map[string]string{}
	if sec, err := l.CS.CoreV1().Secrets(Namespace).Get(ctx, SealSecretName, metav1.GetOptions{}); err == nil {
		for k, v := range sec.Data {
			have[k] = string(v)
		}
	} else if !apierrors.IsNotFound(err) {
		return r, false, fmt.Errorf("read %s/%s: %w", Namespace, SealSecretName, err)
	}
	updated, err := Deliver(ctx, l.CS, r, cfg, time.Now())
	if err != nil {
		return r, false, err
	}
	return updated, !reflect.DeepEqual(have, want), nil
}
