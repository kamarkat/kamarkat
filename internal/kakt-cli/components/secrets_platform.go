package components

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"

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
// WHERE THESE COME FROM, and it is no longer this process's environment.
//
// They are read from the box's escrow, under the box, where `kakt box init`
// enrolled them (ADR-076's amendment: an artefact belongs there when it must be
// readable while the management cluster is unavailable OR before it exists).
// Day-0 in a tenant's CI is precisely the second case -- it is creating the store
// everything else reads from.
//
// It used to read the environment and then the operator's own store, which is why
// a tenant's run could not work: the values were on the operator's workstation and
// in nine Actions secrets, the runner had neither, and a credential the bootstrap
// workflow did not forward arrived as an empty string with nothing anywhere saying
// so. The object-storage pair was asked for in that workflow's header and never
// passed.
type optionalSecret struct {
	name      string
	namespace string
	artifact  string            // the escrow artefact holding it
	keys      map[string]string // secret key -> field inside that artefact ("" = the whole payload)
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
	// declared names keys the artefact may omit because the cluster's
	// declaration states them (secret key -> declaration value), filled only
	// once the artefact supplies something: a box with no credential at all is
	// still "not configured", not "partially supplied".
	declared func(hubaccess.Coordinates) map[string]string
	// capability marks a credential that IS the capability: with it the box does
	// the thing, without it the box does not do the thing, and neither is a
	// misconfiguration.
	//
	// Distinct from destination, which is a capability that ships either way and
	// merely has nowhere to send. This one does not ship: a box that pulls only
	// public images has no use for a pull secret, and `kakt box init` does not ask
	// for one unless --private-registry says the box needs it.
	//
	// It had no category and so was refused when absent, which is how a box that
	// deliberately declined private images failed Day-0 at generate-local-secrets
	// -- init skipped the credential by design and the installer demanded it. The
	// two sides disagreed about the same choice; the ESCROW is the record of it
	// now, which is the same rule on-prem nodes follow: the declared value is the
	// switch, and nothing is defaulted either way.
	capability bool
	// compose turns the resolved inputs into the Secret's data, for credentials
	// whose stored form is not what an operator holds. A registry pull secret is
	// a docker config document; nobody has one of those lying around, they have a
	// username and a token.
	compose func(in map[string]string) map[string]string
}

func optionalSecrets() []optionalSecret {
	return []optionalSecret{
		{
			name: "s3-object-storage", namespace: constants.NamespaceOps,
			artifact: escrow.ArtifactBackupStoreCredential,
			keys: map[string]string{
				"access-key-id":     escrow.FieldAccessKeyID,
				"secret-access-key": escrow.FieldSecretAccessKey,
			},
			secType: corev1.SecretTypeOpaque,
			what:    "CNPG Barman backups of the platform database",
		},
		// THE SAME CREDENTIAL, in the shape infisical-db reads it.
		//
		// All hub CNPG clusters read platform-data/s3-credentials-escrowed directly
		// (ADR-112 amendment). CNPG instances start at boundary 02 before Infisical
		// (boundary 03), so retrieving archive credentials via Infisical/ESO created
		// an unsolvable dependency race at instance startup where CNPG cached credential
		// misses and disabled WAL archiving indefinitely. Sourcing from escrowed backup
		// credentials at Day-0 guarantees credentials exist before boundary 02 starts.
		//
		// Three keys, region included, because CNPG takes it as a discrete key.
		// `kakt box init` enrols the key pair alone; the region is the cluster's
		// declared one, where box init created the bucket (objectstore.Endpoint).
		// A region in the artefact wins. Neither is refused by name rather than
		// delivered incomplete.
		{
			name: "s3-credentials-escrowed", namespace: constants.NamespaceData,
			artifact: escrow.ArtifactBackupStoreCredential,
			keys: map[string]string{
				"access-key-id":     escrow.FieldAccessKeyID,
				"secret-access-key": escrow.FieldSecretAccessKey,
				"region":            "region",
			},
			declared: func(c hubaccess.Coordinates) map[string]string {
				return map[string]string{"region": c.Region}
			},
			secType: corev1.SecretTypeOpaque,
			what:    "CNPG Barman backups of all hub PostgreSQL clusters",
		},
		{
			name: "grafana-cloud", namespace: constants.NamespaceOps,
			artifact: escrow.ArtifactAlertDestination,
			keys: map[string]string{
				// ADR-078 add.5 §5: where this box's Alertmanager pages, and its
				// heartbeat. Metrics, logs and dashboards run in the box.
				"irm-alertmanager-url": escrow.FieldAlertmanagerURL,
				"irm-heartbeat-url":    escrow.FieldHeartbeatURL,
			},
			secType:     corev1.SecretTypeOpaque,
			what:        "where the box's alerts page (ADR-078 add.5 §5)",
			destination: true,
		},
		{
			// The issuer's outbound mail (ADR-060 amendment): the owner's Resend
			// SMTP URL, smtps://resend:<api-key>@smtp.resend.com:465. hub-operator
			// uploads only its password, to /<tenant>-mgmt.
			name: "zitadel-smtp", namespace: constants.NamespaceOps,
			artifact: escrow.ArtifactMailTransportURL,
			keys: map[string]string{
				// One value: the transport URL is the whole artefact, which an
				// empty field name marks.
				"resend": "",
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
			name: "ghcr-pull-secret", namespace: constants.NamespaceOps,
			artifact:   escrow.ArtifactRegistryCredential,
			capability: true,
			keys: map[string]string{
				"username": escrow.FieldRegistryUser,
				"token":    escrow.FieldRegistryToken,
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
// ONE SOURCE: the box's escrow, under the box.
//
// It was the environment and then the operator's own store, and a tenant's CI has
// neither -- the values sat on the workstation that ran `box init` and in nine
// Actions secrets the bootstrap workflow had to forward one by one. `box init`
// enrols them now and this reads them back, which is the same path the provider
// credential has taken since ADR-076.
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

	// The box's own escrow, read once for all of them. The declaration names it:
	// a cluster is always built from the repository that declares it (ADR-072),
	// so nothing here needs a flag or an environment variable.
	coords, err := hubaccess.Declaration(i.GitopsDir, i.ClusterName)
	if err != nil {
		return fmt.Errorf("the credentials this box was built with are in its escrow, and "+
			"this cluster's declaration is what names it: %w", err)
	}
	access, err := hubaccess.Open(ctx, coords)
	if err != nil {
		return fmt.Errorf("opening this box's escrow to read the credentials it was built "+
			"with: %w", err)
	}

	for _, s := range optionalSecrets() {
		payload, err := hubaccess.BoxCredential(ctx, access, coords.BoxID, s.artifact)
		if err != nil {
			return err
		}
		data, found, missing := resolveOptional(s, payload)
		if s.declared != nil && len(found) > 0 {
			missing = fillDeclared(data, missing, s.declared(coords))
		}
		switch {
		case len(found) == 0 && s.destination:
			// Not an error. The capability ships and runs; it simply has nowhere
			// remote to send what it collects, which a tenant may well intend.
			fmt.Printf("[secrets] %s not supplied: metrics and logs are collected "+
				"but not shipped anywhere remote.\n", s.name)
			continue
		case len(found) == 0 && s.capability:
			// Not an error either, and for a different reason: the box declined
			// this capability, and the escrow holding nothing is how it said so.
			fmt.Printf("[secrets] %s not escrowed: this box does not use %s.\n",
				s.name, s.what)
			continue
		case len(found) == 0:
			// A capability this box is configured for cannot work without it.
			// Rendering refuses to dispatch without these, so reaching here
			// means Day-0 was run by hand and should say so rather than build a
			// box that looks finished.
			return fmt.Errorf("this box's escrow holds no %s, and %s needs it.\n\n"+
				"`kakt box init` enrols it from the value you supply; a box created before "+
				"it did, or one that declined this capability, has none. Enrol it and "+
				"re-run:\n\n  kakt escrow enroll-%s --box %s\n\n"+
				"It is read from the escrow and from nowhere else: a copy in this process's "+
				"environment is a copy nothing can compare (ADR-076).",
				s.artifact, s.what, s.artifact, coords.BoxID)
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
func fieldNames(s optionalSecret) []string {
	var out []string
	for _, field := range s.keys {
		if field == "" {
			continue
		}
		out = append(out, field)
	}
	sort.Strings(out)
	return out
}

// resolveOptional maps one escrowed artefact onto the Secret's keys.
//
// payload is what the escrow holds under that artefact, or "" when it holds
// nothing. A single-value credential -- a URL, an auth key -- is the whole
// payload, which its key marks with an empty field name; everything else is
// `key=value` lines, the shape an AWS credentials file already uses.
func resolveOptional(s optionalSecret, payload string) (data map[string]string, found, missing []string) {
	data = map[string]string{}
	fields := escrow.ParseFields(payload)
	for key, field := range s.keys {
		var v string
		if field == "" {
			v = strings.TrimSpace(payload)
		} else {
			v = strings.TrimSpace(fields[field])
		}
		if v != "" {
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

// fillDeclared supplies missing keys from the cluster's declaration, returning
// the keys still missing.
func fillDeclared(data map[string]string, missing []string, declared map[string]string) []string {
	var still []string
	for _, key := range missing {
		if v := strings.TrimSpace(declared[key]); v != "" {
			data[key] = v
			continue
		}
		still = append(still, key)
	}
	return still
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
