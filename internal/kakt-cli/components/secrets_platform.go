package components

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/opstore"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// optionalSecret is a credential the platform can use and does not require.
//
// Each names a Kubernetes Secret the hub-operator uploads to Infisical
// (internal/infisical/secret_mappings.go), from which ESO delivers it to the
// workloads that need it. Nothing created them, so the mappings had producers on
// paper and none in fact: five ExternalSecrets on every box retried forever, and
// grafana-alloy never started.
//
// The key names are the Secret's, and they are deliberately the same as the file
// names under k8-secrets/ -- that layout was already written to match, which is
// how it was clear these were meant to be read from there.
type optionalSecret struct {
	name      string
	namespace string
	dir       string            // under the operator's store
	keys      map[string]string // secret key -> environment variable
	secType   corev1.SecretType
	what      string // what it enables
	// destination marks a credential that says where a capability SENDS, rather
	// than one the capability needs to exist. The stack ships and runs either
	// way; absent a destination it has nowhere remote to put what it collects.
	//
	// Never a refusal, and never a subscription boundary: every capability is
	// available to every tenant, and a tenant's own monitoring account is not a
	// licence check.
	destination bool
	// compose turns the resolved inputs into the Secret's data, for credentials
	// whose stored form is not what an operator holds. A registry pull secret is
	// a docker config document; nobody has one of those lying around, they have a
	// username and a token.
	compose func(in map[string]string) map[string]string
}

func optionalSecrets() []optionalSecret {
	return []optionalSecret{
		{
			name: "s3-object-storage", namespace: constants.NamespaceOps, dir: "s3",
			keys: map[string]string{
				"access-key-id":     "S3_ACCESS_KEY_ID",
				"secret-access-key": "S3_SECRET_ACCESS_KEY",
			},
			secType: corev1.SecretTypeOpaque,
			what:    "CNPG Barman backups of the platform database",
		},
		{
			name: "grafana-cloud", namespace: constants.NamespaceOps, dir: "grafana-cloud",
			keys: map[string]string{
				// ADR-078 add.5 §5: where the hub's Alertmanager pages, and its
				// heartbeat. Metrics, logs and dashboards run in the box.
				"irm-alertmanager-url": "GRAFANA_IRM_ALERTMANAGER_URL",
				"irm-heartbeat-url":    "GRAFANA_IRM_HEARTBEAT_URL",
			},
			secType:     corev1.SecretTypeOpaque,
			what:        "where the box's alerts page (ADR-078 add.5 §5)",
			destination: true,
		},
		{
			// The issuer's outbound mail (ADR-060 amendment): the owner's Resend
			// SMTP URL, smtps://resend:<api-key>@smtp.resend.com:465. hub-operator
			// uploads only its password, to /<tenant>-mgmt.
			name: "zitadel-smtp", namespace: constants.NamespaceOps, dir: "smtp",
			keys: map[string]string{
				"resend": "ZITADEL_SMTP_URL",
			},
			secType:     corev1.SecretTypeOpaque,
			what:        "the identity provider sending verification and password-reset mail",
			destination: true,
		},
		{
			// A tenant's own registry credential, not the platform's.
			//
			// The platform's images are public, so this looked cosmetic -- the
			// only symptom on a platform-only box is a FailedToRetrieveImagePullSecret
			// warning next to images that pull anyway. It is not cosmetic for a
			// tenant: ADR-066 makes workloads theirs, and universal-tenant's
			// pull-secret-es.yaml and the stateless-web rollout base both mount
			// this. A tenant keeping its packages private has no way to run them
			// without it.
			name: "ghcr-pull-secret", namespace: constants.NamespaceOps, dir: "ghcr",
			keys: map[string]string{
				"username": "GHCR_USERNAME",
				"token":    "GHCR_TOKEN",
			},
			secType: corev1.SecretTypeDockerConfigJson,
			what:    "pulling private images for this tenant's workloads",
			compose: func(in map[string]string) map[string]string {
				// The registry is not one of the required keys: it has a correct
				// default, and making it one would mean a tenant using ghcr had to
				// supply it or be told their credential was half-configured.
				registry := strings.TrimSpace(os.Getenv("GHCR_REGISTRY"))
				if registry == "" {
					registry = "ghcr.io"
				}
				auth := base64.StdEncoding.EncodeToString(
					[]byte(in["username"] + ":" + in["token"]))
				return map[string]string{
					corev1.DockerConfigJsonKey: fmt.Sprintf(
						`{"auths":{%q:{"username":%q,"password":%q,"auth":%q}}}`,
						registry, in["username"], in["token"], auth),
				}
			},
		},
	}
}

// InstallPlatformCredentials creates the credentials a hub needs to be healthy.
//
// Two sources, in order: the environment, which is how a workflow supplies a
// value it holds as a repository secret; then k8-secrets/, which is how an
// operator working in a checkout supplies one. Resolved against the checkout
// rather than the working directory, because Day-0 runs from the tenant's
// repository and k8-secrets/ is not there.
//
// All-or-nothing per secret, for the reason the escrow gives: a partial set is a
// configuration that cannot work, and a Secret built from one produces a
// workload that authenticates as nobody rather than one that is plainly absent.
func (i *Installer) InstallPlatformCredentials(ctx context.Context) error {
	config, err := clientcmd.BuildConfigFromFlags("", i.Kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	for _, s := range optionalSecrets() {
		data, found, missing := resolveOptional(s)
		switch {
		case len(found) == 0 && s.destination:
			// Not an error. The capability ships and runs; it simply has nowhere
			// remote to send what it collects, which a tenant may well intend.
			fmt.Printf("[secrets] %s not supplied: metrics and logs are collected "+
				"but not shipped anywhere remote.\n", s.name)
			continue
		case len(found) == 0:
			// A capability this box is configured for cannot work without it.
			// Scaffolding refuses to dispatch without these, so reaching here
			// means Day-0 was run by hand and should say so rather than build a
			// box that looks finished.
			return fmt.Errorf("%s was not supplied, and %s needs it.\n\n"+
				"Set %s in the environment, or put the values under ~/.kamarkat/%s/.\n"+
				"Scaffolding collects these; a hand-run Day-0 has to pass them.",
				s.name, s.what, strings.Join(envNames(s), ", "), s.dir)
		case len(missing) > 0:
			return fmt.Errorf("%s is partially supplied: missing %s.\n"+
				"All of its keys or none: a credential built from some of them "+
				"fails at use rather than at configuration", s.name, strings.Join(missing, ", "))
		}

		if s.compose != nil {
			data = s.compose(data)
		}

		if err := applySecret(ctx, clientset, s, data); err != nil {
			return err
		}
		fmt.Printf("[secrets] ✓ %s (%d keys) — %s enabled\n", s.name, len(data), s.what)
	}
	return nil
}

// resolveOptional reads every key of one secret, reporting which were found and
// which were not, so a partial set can be refused by name.
func envNames(s optionalSecret) []string {
	var out []string
	for _, env := range s.keys {
		out = append(out, env)
	}
	sort.Strings(out)
	return out
}

func resolveOptional(s optionalSecret) (data map[string]string, found, missing []string) {
	data = map[string]string{}
	for key, env := range s.keys {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			data[key] = v
			found = append(found, key)
			continue
		}
		// The operator's store, not a checkout (internal/kakt-cli/opstore).
		if v := opstore.Value(s.dir + "/" + key); v != "" {
			data[key] = v
			found = append(found, key)
			continue
		}
		missing = append(missing, key)
	}
	// Nothing found is "not configured"; some found is a mistake worth naming.
	if len(found) == 0 {
		return nil, nil, nil
	}
	return data, found, missing
}

// readCheckoutFile reads a path relative to the platform checkout.
//
// Not the working directory: Day-0 runs from the tenant's repository (ADR-072),
// and k8-secrets/ belongs to the operator's own checkout. The same reasoning and
// the same order as the Hetzner token fallback in cmd/kakt/bootstrap.go.
func readCheckoutFile(rel string) string {
	candidates := []string{rel}
	if root := strings.TrimSpace(os.Getenv("ZERO_OPS_DIR")); root != "" {
		candidates = append(candidates, filepath.Join(root, rel))
	}
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			dir := filepath.Dir(exe)
			candidates = append(candidates, filepath.Join(dir, rel), filepath.Join(filepath.Dir(dir), rel))
		}
	}
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v
			}
		}
	}
	return ""
}

func applySecret(ctx context.Context, cs kubernetes.Interface, s optionalSecret, data map[string]string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.name,
			Namespace: s.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "zero-ops-hub-cli",
				"app.kubernetes.io/component":  "secret-zero",
			},
		},
		Type:       s.secType,
		StringData: data,
	}

	_, err := cs.CoreV1().Secrets(s.namespace).Create(ctx, secret, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		// Updated rather than left alone: a re-run supplying a rotated credential
		// must replace the old one, and the operator uploads whatever is here.
		_, err = cs.CoreV1().Secrets(s.namespace).Update(ctx, secret, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("failed to write secret %s/%s: %w", s.namespace, s.name, err)
	}
	return nil
}
