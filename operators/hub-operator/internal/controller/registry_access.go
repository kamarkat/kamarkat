package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kamarkat/kamarkat/internal/platform/escrow"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/constant"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/openbao"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/secrets"
)

// registryHost is the one registry the box holds a credential for.
const registryHost = "ghcr.io"

// The box's registry credential (ADR-076, amended): enrolled in the escrow
// whenever an application first needs it -- by `kakt tenant add-app`, or
// `kakt escrow enroll-registry-credential` -- and converged into OpenBao here on
// every reconcile. Day-0 is no longer its only way in, so a credential enrolled
// months after bootstrap reaches the applications that need it within one pass.
//
// Absence is not an error and changes nothing: a box whose applications are all
// public has none, and one that once had a credential keeps the last value.

// syncRegistryCredential writes the escrowed registry credential into the hub
// scope as a docker config, when it differs from what is there.
func (r *HubEnvironmentReconciler) syncRegistryCredential(ctx context.Context, store escrow.EscrowClient, boxID string) error {
	payload, err := store.RestoreArtifact(ctx, escrow.ForCluster(boxID, boxID), escrow.ArtifactRegistryCredential)
	if err != nil {
		return fmt.Errorf("read the escrowed registry credential: %w", err)
	}
	f := escrow.ParseFields(payload)
	user, token := f[escrow.FieldRegistryUser], f[escrow.FieldRegistryToken]
	if token == "" {
		return nil
	}
	want := dockerConfig(registryHost, user, token)
	have, err := r.Store.GetSecret(ctx, secrets.HubScope, secrets.RegistryPullSecretKey)
	if err != nil && !errors.Is(err, openbao.ErrNotFound) {
		return err
	}
	if have == want {
		return nil
	}
	if err := r.Store.UpdateSecret(ctx, secrets.HubScope, secrets.RegistryPullSecretKey, want); err != nil {
		return fmt.Errorf("write the registry credential into the hub scope: %w", err)
	}
	log.FromContext(ctx).Info("registry credential converged from the escrow")
	return nil
}

func dockerConfig(host, user, token string) string {
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
	b, _ := json.Marshal(map[string]any{"auths": map[string]any{
		host: map[string]string{"username": user, "password": token, "auth": auth}}})
	return string(b)
}

// registryLogin reads the user and token for host back out of a docker config.
func registryLogin(config, host string) (string, string, bool) {
	var c struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if json.Unmarshal([]byte(config), &c) != nil {
		return "", "", false
	}
	a, ok := c.Auths[host]
	if !ok || a.Password == "" {
		return "", "", false
	}
	return a.Username, a.Password, true
}

// chartRegistrySecretName is one application's GitOps-engine credential.
func chartRegistrySecretName(tenantID, appID string) string {
	return "chart-registry-" + tenantID + "-" + appID
}

// reconcileChartRegistry gives the GitOps engine a credential for an
// application's chart repository when, and only when, it declares private
// charts. The repository is the application's own declaration, never assumed
// from an organisation. Native Argo CD repository credentials: a Secret labelled
// argocd.argoproj.io/secret-type=repository in the engine's namespace.
func (r *AINativeSaaSReconciler) reconcileChartRegistry(ctx context.Context, tenantID, appID string, private bool, repository string) error {
	key := types.NamespacedName{Namespace: constant.NamespaceOps, Name: chartRegistrySecretName(tenantID, appID)}
	if !private {
		existing := &corev1.Secret{}
		if err := r.Get(ctx, key, existing); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		return ignoreNotFound(r.Delete(ctx, existing))
	}
	repository = strings.TrimSuffix(strings.TrimPrefix(repository, "oci://"), "/")
	if repository == "" {
		return fmt.Errorf("registry.charts is private but no chartRepository is declared")
	}
	host := strings.SplitN(repository, "/", 2)[0]
	config, err := r.Store.GetSecret(ctx, secrets.HubScope, secrets.RegistryPullSecretKey)
	if err != nil {
		if errors.Is(err, openbao.ErrNotFound) {
			return fmt.Errorf("%s/%s declares private charts and the box holds no registry credential yet; "+
				"enrol one with `kakt tenant add-app` or `kakt escrow enroll-registry-credential`", tenantID, appID)
		}
		return err
	}
	user, token, ok := registryLogin(config, host)
	if !ok {
		return fmt.Errorf("the box's registry credential is for %s, not %s", registryHost, host)
	}
	want := map[string][]byte{
		"type": []byte("helm"), "enableOCI": []byte("true"),
		"name": []byte(tenantID + "-" + appID + "-charts"), "url": []byte(repository),
		"username": []byte(user), "password": []byte(token),
	}
	secret := &corev1.Secret{}
	err = r.Get(ctx, key, secret)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{
				"argocd.argoproj.io/secret-type": "repository",
				"app.kubernetes.io/managed-by":   "hub-operator",
				"tenant-id":                      tenantID, "app-id": appID,
			}},
			Type: corev1.SecretTypeOpaque, Data: want,
		})
	}
	if err != nil {
		return err
	}
	secret.Data = want
	return r.Update(ctx, secret)
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
