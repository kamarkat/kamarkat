// Package escrow holds what a box cannot be rebuilt without, outside the box.
//
// At the repository root rather than under the operator that writes it, because
// two things need it and Go's internal rule keeps them apart otherwise: the
// operator escrows, and the CLI retrieves. An escrow only one side can reach is a
// backup with no restore.
package escrow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// EscrowClient holds what a box cannot be rebuilt without, outside the box.
//
// The Infisical master keys are the box's root secret: without them the secret
// store cannot be decrypted, and every credential the platform manages is lost
// with the cluster. They cannot be kept in the box's own Infisical -- that is the
// thing they unlock -- and they cannot be kept by the platform, because ADR-065
// says the platform holds no secret credential belonging to a tenant.
//
// So they go to an Infisical instance the TENANT controls and the platform does
// not: Infisical Cloud, or any Infisical the tenant runs elsewhere. Out-of-band by
// construction, reachable when the cluster is not.
type EscrowClient interface {
	BackupMasterKeys(ctx context.Context, id Identity, keys interface{}) error
	RestoreMasterKeys(ctx context.Context, id Identity) (interface{}, error)

	// BackupArtifact and RestoreArtifact hold anything else a box cannot be
	// rebuilt without, under a name of the caller's choosing.
	//
	// The master-key methods above are the same operation with a fixed name and a
	// JSON shape. They stay separate because their payload has a schema the restore
	// path reads field by field, where an artefact is opaque bytes.
	BackupArtifact(ctx context.Context, id Identity, name, payload string) error
	RestoreArtifact(ctx context.Context, id Identity, name string) (string, error)
}

// ArtifactKubeconfig is the cluster's admin kubeconfig.
//
// Escrowed because it is the only credential that reaches the API server when the
// identity provider cannot be used -- it is down, the cluster is broken, or nobody
// is left who can log in (ADR-076). Day-0 generates it on a runner destroyed with
// the job, so without this it exists only inside the cluster it would be used to
// repair.
const ArtifactKubeconfig = "admin-kubeconfig"

// ArtifactZitadelMasterkey is the identity provider's encryption key for data at
// rest (ADR-076, "What the escrow holds").
//
// It qualifies by the membership test rather than by being important: it is
// generated once and NEVER rotated, because it encrypts every column the identity
// provider has written and a new one orphans all of them. So a box that loses its
// secret store can restore the store from `infisical-master-keys` and still hold an
// identity database nothing can decrypt.
//
// The live source is unchanged -- the box's own secret store serves it, because a
// key read at startup must not depend on reaching another site. This is the
// recovery copy.
const ArtifactZitadelMasterkey = "zitadel-masterkey"

// ArtifactSecretEncryptionKey is the key the API server encrypts Secrets with at
// rest (ADR-003 section 6, ADR-076 "What the escrow holds").
//
// It qualifies by the same test as the others and for the starkest reason: etcd
// holds Secret data encrypted with it, and every etcd BACKUP holds the same. Lose
// the key and the cluster's Secrets are unreadable, and so is every backup taken
// while it was in force -- there is no second copy to restore from, because the
// backups are the second copy.
//
// Unlike the artefacts above it is also on the critical path: the API server reads
// it on every Secret read, so the escrow is a recovery copy of a value that must
// ALSO be present on the node. That is why it is delivered to the node from the
// management cluster and escrowed from there, rather than fetched from the escrow
// at runtime.
const ArtifactSecretEncryptionKey = "secret-encryption-key"

// ArtifactSecretEncryptionKeyPrevious is the OUTGOING key during a rotation.
//
// It exists for one window: between staging a rotation and verifying that every Secret
// has been rewritten under the new key. In that window etcd holds objects under BOTH
// generations, so a cluster that loses its control plane needs BOTH to be recovered --
// and escrowing only the new one would make the window unrecoverable, which is the
// exact failure ADR-076 exists to prevent, introduced by the procedure meant to
// improve security.
//
// Outside a rotation it is absent, and that is why the recovery contract in ADR-076
// lists it as conditional rather than required: its presence means a rotation is in
// flight, not that something is missing.
const ArtifactSecretEncryptionKeyPrevious = "secret-encryption-key-previous"

// MasterKeysBackup is what is escrowed. The field names are the ones the restore
// path reads back, so changing one is a migration rather than a rename.
type MasterKeysBackup struct {
	EncryptionKey   string    `json:"encryptionKey"`
	AuthSecret      string    `json:"authSecret"`
	CreatedAt       time.Time `json:"createdAt"`
	ClusterID       string    `json:"clusterId"`
	Version         string    `json:"version"`
	BackupTimestamp time.Time `json:"backupTimestamp"`
}

// infisicalEscrow escrows into an Infisical instance outside this box.
//
// It speaks the Infisical API directly rather than reusing either of the platform's
// existing clients. Both are large and carry concerns an escrow has none of --
// identity creation, credential rotation, folder trees, project bootstrap -- and
// both live behind Go's internal rule in packages the other side cannot import. An
// escrow is four calls: authenticate, does-it-exist, read, write.
type infisicalEscrow struct {
	baseURL      string
	clientID     string
	clientSecret string
	projectID    string

	http  *http.Client
	token string
}

// environmentSlug is the Infisical environment the escrow writes to.
//
// Fixed rather than configurable: an escrow split across environments is one a
// restore has to guess at, and the thing being escrowed has no environment of its
// own -- it belongs to a cluster, which the path already names.
const environmentSlug = "prod"

const (
	pathLogin      = "/api/v1/auth/universal-auth/login"
	pathSecretsRaw = "/api/v3/secrets/raw/%s"
	pathFolders    = "/api/v1/folders"
)

// escrowSecretName is the single secret each cluster's master-key backup occupies.
// One name, no timestamps: a restore must find exactly one answer, and a list of
// dated backups is a decision nobody is present to make when it is needed.
const escrowSecretName = "infisical-master-keys"

// Identity names what an artefact belongs to: the box, and the cluster within it.
//
// Two fields because one identifier carried two meanings and they have different
// lifetimes (ADR-111 §1). BoxID is the logical hub and is stable forever -- it keys
// the escrow and the DNS records, and a replacement hub inherits it. ClusterName is
// one Kubernetes cluster inside that box and is replaced with it.
//
// A spoke is a cluster in the box like any other, so a spoke's artefacts are keyed by
// the spoke's own name and are therefore independent of whichever hub incarnation
// manages it -- which is what makes management ownership transferable.
type Identity struct {
	BoxID       string
	ClusterName string
}

// ForCluster names one cluster within a box.
func ForCluster(boxID, clusterName string) Identity {
	return Identity{BoxID: strings.TrimSpace(boxID), ClusterName: strings.TrimSpace(clusterName)}
}

func (i Identity) validate() error {
	if i.BoxID == "" {
		return fmt.Errorf("the escrow needs the box id: artefacts are keyed by the logical hub, " +
			"which outlives any one cluster (ADR-111)")
	}
	if i.ClusterName == "" {
		return fmt.Errorf("the escrow needs the cluster name: a cluster-scoped artefact keyed by " +
			"the box alone would be overwritten by the next incarnation of it (ADR-111)")
	}
	return nil
}

// clusterScoped lists the artefacts keyed to one cluster rather than to the box.
//
// The division is by lifetime, and getting it the wrong way round fails in opposite
// directions. A box-scoped artefact placed under a cluster is LOST when that cluster
// is replaced -- the secret store's master keys would go with the hub that held them,
// which is the circularity ADR-076 exists to prevent. A cluster-scoped artefact placed
// under the box is OVERWRITTEN by the next cluster that writes it: a successor hub
// would destroy its predecessor's admin kubeconfig and at-rest key at exactly the
// moment both are needed to recover from a failed succession.
//
// Everything not listed here is box-scoped, including the per-application credentials
// the secret uploader escrows under names it composes at runtime. That is the safe
// default of the two: an application and its database outlive a hub incarnation, so
// keying their credentials to the box is correct, and the failure mode of the wrong
// choice here is loss rather than collision.
var clusterScoped = map[string]bool{
	ArtifactKubeconfig:                  true,
	ArtifactSecretEncryptionKey:         true,
	ArtifactSecretEncryptionKeyPrevious: true,
}

// ArtifactSpokePKIPrefix names the family holding one spoke's control-plane
// certificate authorities -- `spoke-pki-ca`, `-etcd`, `-proxy`, `-sa`.
//
// A family rather than four constants because the set follows kubeadm's, and a
// name this platform did not anticipate must not fall through to the box scope:
// two spokes sharing one path would overwrite each other's trust roots, which is
// the one recovery material that cannot be regenerated or re-issued (ADR-111 ss2,
// Class E). The prefix is matched, so an unanticipated member is still scoped to
// its cluster.
const ArtifactSpokePKIPrefix = "spoke-pki-"

// ArtifactBackupSSECKey is the customer key the management-state backups are
// encrypted with (ADR-111 ss10).
//
// Box-scoped, and declared rather than left to the default below: it must be
// readable when neither the hub that wrote the backups nor the secret store
// inside it is running, which is the whole reason it is escrowed at all.
const ArtifactBackupSSECKey = "backup-ssec-key"

// SpokePKIPurposes are the trust roots Cluster API generates for a cluster, by the
// suffix it names their Secrets with: `<cluster>-ca`, `-etcd`, `-sa`, `-proxy`
// (sigs.k8s.io/cluster-api/util/secret, Purpose constants at the deployed v1.10.0).
//
// `kubeconfig` is a purpose there too and is escrowed separately, under its own
// long-standing name. `apiserver-etcd-client` is in CAPI's list but is USER-supplied
// for an external etcd, so a stacked-etcd cluster has none and its absence is not a
// gap.
//
// These four are the material a replacement hub cannot do without and cannot
// re-create: without the cluster CA it can issue no node certificate, so it can
// never roll, scale or repair that cluster's control plane again (ADR-111 ss2).
var SpokePKIPurposes = []string{"ca", "etcd", "sa", "proxy"}

// SpokePKIArtifact names the escrow artefact holding one purpose's key pair.
func SpokePKIArtifact(purpose string) string {
	return ArtifactSpokePKIPrefix + purpose
}

// ArtifactBackupStoreCredential is the object-store credential that READS the
// management-state backups (ADR-111 §10).
//
// Escrowed and box-scoped for the same reason as the key beside it, and the reason is
// worth stating because it is easy to get wrong: encryption and authentication are
// different authorities, and a successor needs both. Keeping only the encryption key
// outside the box leaves the credential that reaches the bucket inside the Infisical
// that has to be recovered from that bucket -- a circle with the same shape as the one
// ADR-076 exists to break.
//
// Separate from every CNPG archive credential, so compromise or revocation of one
// recovery authority does not yield the other.
const ArtifactBackupStoreCredential = "backup-store-credential"

// boxScoped names the box-scoped artefacts explicitly, so that every
// security-critical artefact's scope is DECLARED and none of them depends on the
// default. The default still exists, for the per-application credentials the
// secret uploader names at runtime; it must never be what places a key.
var boxScoped = map[string]bool{
	escrowSecretName:              true, // infisical-master-keys
	ArtifactZitadelMasterkey:      true,
	ArtifactBackupSSECKey:         true,
	ArtifactBackupStoreCredential: true,
}

// Scope is which identity an artefact is keyed to.
type Scope int

const (
	// ScopeBox keys an artefact to the logical hub, so it survives every cluster.
	ScopeBox Scope = iota
	// ScopeCluster keys an artefact to one cluster -- a hub incarnation or a
	// spoke -- so it is replaced with that cluster and collides with nothing.
	ScopeCluster
)

// DeclaredScope reports an artefact's scope and whether it was declared.
//
// The second return is what a verifier asserts on: an artefact that matters and is
// merely DEFAULTED is a latent misplacement, and the difference between declared
// and defaulted is invisible in the path itself.
func DeclaredScope(artifact string) (Scope, bool) {
	switch {
	case clusterScoped[artifact], strings.HasPrefix(artifact, ArtifactSpokePKIPrefix):
		return ScopeCluster, true
	case boxScoped[artifact]:
		return ScopeBox, true
	default:
		return ScopeBox, false
	}
}

// escrowPath is where an artefact lives, which depends on what it is.
//
// ADR-111 names three scopes -- logical hub, hub incarnation, spoke identity -- and two
// path shapes implement them: a hub incarnation and a spoke are both one cluster inside
// the box, distinguished by their own name, so neither can collide with the other or
// with a later incarnation.
func escrowPath(id Identity, artifact string) string {
	if scope, _ := DeclaredScope(artifact); scope == ScopeCluster {
		return "/hub-operator/" + id.BoxID + "/clusters/" + id.ClusterName
	}
	return "/hub-operator/" + id.BoxID
}

// legacyPath is where every artefact lived before scopes existed: one flat folder
// per cluster name.
//
// It exists for MigrateArtifactPath and nothing else. No reader falls back to it:
// a read that silently tried both paths would make an unmigrated box look
// migrated, and the gap would surface on the day the escrow is used in earnest.
func legacyPath(clusterName string) string {
	return "/hub-operator/" + clusterName
}

// NewEscrowClient builds the escrow from the environment, or reports why it cannot.
//
// inClusterURL is this box's own Infisical, when the caller knows it. It is what
// makes this an escrow rather than a second copy in the same place: an escrow
// inside the thing it protects is unreachable exactly when it is needed, and this
// refuses that configuration rather than appearing to work. Callers with no cluster
// to compare against -- the CLI retrieving a copy -- pass "".
// escrowValue reads one escrow setting from the environment, or from the checkout
// that scaffolding wrote it into.
//
// # WHY A FILE AND NOT ONLY AN ENVIRONMENT VARIABLE
//
// Scaffolding writes these four to `k8-secrets/infisical/`, one file per setting,
// each named exactly after its variable. Nothing read them. So every command needing
// an escrow required four exports that the operator had already supplied once, and
// the files sat there looking like configuration while being inert -- which is how
// an operator ends up pasting a client secret into a shell on a regular basis.
//
// The environment still wins, because that is what an in-cluster caller has: the
// operator runs with these projected from a Secret and no checkout to read. The file
// is the fallback for a CLI run from the repository.
//
// Absent file, absent directory and unreadable file are all treated as "not set".
// That is NOT swallowing an error: a missing value is reported by the all-or-nothing
// check in the caller, which names every setting it could not find. Reporting a
// read failure here instead would make a box with no checkout -- the normal case in
// cluster -- fail on the absence of something it was never meant to have.
func escrowValue(name string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	dir := escrowSecretsDir()
	if dir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// escrowSecretsDir finds `k8-secrets/infisical` by walking up from the working
// directory.
//
// Walked rather than assumed, because a CLI is run from wherever the operator
// happens to be inside the checkout, and a path relative to the process would work
// from the repository root and nowhere else. Bounded, so a process started outside
// any checkout stops instead of climbing to the filesystem root.
func escrowSecretsDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, "k8-secrets", "infisical")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func NewEscrowClient(ctx context.Context, inClusterURL string) (EscrowClient, error) {
	url := escrowValue("INFISICAL_ESCROW_URL")
	clientID := escrowValue("INFISICAL_ESCROW_CLIENT_ID")
	clientSecret := escrowValue("INFISICAL_ESCROW_CLIENT_SECRET")
	projectID := escrowValue("INFISICAL_ESCROW_PROJECT_ID")

	if url == "" && clientID == "" && clientSecret == "" && projectID == "" {
		return nil, fmt.Errorf("no escrow configured")
	}
	// All four or none. Reading them from one Secret means a partial set produces a
	// caller that attempts a backup on every reconcile and fails -- an escrow that
	// appears to exist and does not work, which is worse than none.
	var missing []string
	for _, kv := range []struct{ name, value string }{
		{"INFISICAL_ESCROW_URL", url},
		{"INFISICAL_ESCROW_PROJECT_ID", projectID},
		{"INFISICAL_ESCROW_CLIENT_ID", clientID},
		{"INFISICAL_ESCROW_CLIENT_SECRET", clientSecret},
	} {
		if kv.value == "" {
			missing = append(missing, kv.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the escrow is partially configured; missing %s",
			strings.Join(missing, ", "))
	}

	if inClusterURL != "" && strings.EqualFold(
		strings.TrimSuffix(url, "/"), strings.TrimSuffix(inClusterURL, "/")) {
		return nil, fmt.Errorf("the escrow URL is this cluster's own Infisical (%s), "+
			"which is not an escrow: it holds the keys that decrypt it, and it is "+
			"unreachable exactly when they are needed", url)
	}

	e := &infisicalEscrow{
		baseURL:      strings.TrimSuffix(url, "/"),
		clientID:     clientID,
		clientSecret: clientSecret,
		projectID:    projectID,
		http:         &http.Client{Timeout: 30 * time.Second},
	}
	if err := e.authenticate(ctx); err != nil {
		return nil, err
	}
	if err := e.verifyProject(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

// verifyProject proves the project can actually hold secrets, once, at
// construction.
//
// Authenticating proves the machine identity is valid and proves nothing about
// the project. An Infisical project has a TYPE, and only a secret-manager
// project accepts the operations an escrow performs; a kms project authenticates
// perfectly and then refuses every read and write with
//
//	400 The project is of type kms. Operations of type secret-manager are not
//	    allowed.
//
// Without this the first failure came from a reconcile deep inside a bootstrap.
// The box built for forty minutes, HubEnvironment never published
// DatabaseRolesProvisioned, and boundary-04 waited on database roles that could
// not be created -- three layers away from a project created as the wrong type
// days earlier, and accepted at scaffold time on nothing more than a presence
// check.
//
// The probe is a read of a path that does not exist: one request, no write
// access needed to establish the type, and 404 is the healthy answer.
func (e *infisicalEscrow) verifyProject(ctx context.Context) error {
	_, err := e.RestoreArtifact(ctx, ForCluster("__escrow_preflight__", "__escrow_preflight__"), escrowSecretName)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "of type kms") ||
		strings.Contains(err.Error(), "secret-manager are not allowed") {
		return fmt.Errorf("escrow project %s is a KMS project, and an escrow needs a "+
			"SECRET MANAGER project.\n\n"+
			"They authenticate identically, so the credentials are not the problem: a KMS\n"+
			"project simply refuses the read and the write an escrow is made of.\n\n"+
			"Create a Secret Manager project at %s, give this machine identity write\n"+
			"access to it, and use that project's id.\n\noriginal error: %w",
			e.projectID, e.baseURL, err)
	}
	return fmt.Errorf("escrow project %s is not usable: %w", e.projectID, err)
}

// authenticate exchanges the machine identity for a token.
func (e *infisicalEscrow) authenticate(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{
		"clientId":     e.clientID,
		"clientSecret": e.clientSecret,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+pathLogin,
		bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build escrow login: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.http.Do(req)
	if err != nil {
		return fmt.Errorf("reach the escrow at %s: %w", e.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("the escrow refused these credentials (%d): %s",
			resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("read the escrow login response: %w", err)
	}
	if out.AccessToken == "" {
		return fmt.Errorf("the escrow returned no access token")
	}
	e.token = out.AccessToken
	return nil
}

func (e *infisicalEscrow) secretURL(id Identity, name string) string {
	return e.secretURLAt(escrowPath(id, name), name)
}

func (e *infisicalEscrow) secretURLAt(secretPath, name string) string {
	return fmt.Sprintf("%s%s?workspaceId=%s&environment=%s&secretPath=%s",
		e.baseURL, fmt.Sprintf(pathSecretsRaw, name), e.projectID, environmentSlug,
		secretPath)
}

// ensurePath creates this cluster's escrow folder.
//
// Infisical does not create a secret path on write: a secret written to a path that
// does not exist is a 404, not an implicit mkdir (secret-v2-bridge-service.ts
// findBySecretPath -> NotFoundError). The folder API does fill in missing parents of
// the path it is given, so one call creates both segments.
//
// Already-exists is the normal case -- every backup after the first -- and Infisical
// reports it as a 400, so it is success here.
func (e *infisicalEscrow) ensurePath(ctx context.Context, id Identity, artifact string) error {
	parent, name := path.Split(escrowPath(id, artifact))
	body, _ := json.Marshal(map[string]string{
		"workspaceId": e.projectID,
		"environment": environmentSlug,
		"path":        strings.TrimSuffix(parent, "/"),
		"name":        name,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+pathFolders,
		bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build escrow folder create: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.token)

	resp, err := e.http.Do(req)
	if err != nil {
		return fmt.Errorf("create the escrow folder: %w", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusConflict:
		return nil
	default:
		return fmt.Errorf("the escrow refused to create %s (%d): %s",
			escrowPath(id, artifact), resp.StatusCode, strings.TrimSpace(string(b)))
	}
}

// BackupArtifact writes one named artefact into this cluster's escrow.
//
// Create, then update on conflict. Infisical distinguishes the two and an escrow
// must not care: the second backup of a cluster is the normal case.
func (e *infisicalEscrow) BackupArtifact(ctx context.Context, id Identity, name, payload string) error {
	if err := id.validate(); err != nil {
		return err
	}
	if err := e.ensurePath(ctx, id, name); err != nil {
		return err
	}

	body, _ := json.Marshal(map[string]string{
		"workspaceId": e.projectID,
		"environment": environmentSlug,
		"secretPath":  escrowPath(id, name),
		"secretValue": payload,
		"type":        "shared",
	})

	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		req, err := http.NewRequestWithContext(ctx, method,
			e.baseURL+fmt.Sprintf(pathSecretsRaw, name), bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("build escrow write: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e.token)

		resp, err := e.http.Do(req)
		if err != nil {
			return fmt.Errorf("write the escrow: %w", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		// A create against an existing secret is not a failure; retry as an update.
		if method == http.MethodPost &&
			(resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusBadRequest) {
			continue
		}
		return fmt.Errorf("the escrow refused the write (%d): %s",
			resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return fmt.Errorf("the escrow accepted neither a create nor an update for %s", name)
}

// RestoreArtifact returns one named artefact, or "" when this cluster has none.
//
// Empty and an error are different answers, and callers depend on it: empty means
// nothing was ever escrowed under that name, an error means the escrow could not be
// read -- and continuing past the second would overwrite a backup that exists.
func (e *infisicalEscrow) RestoreArtifact(ctx context.Context, id Identity, name string) (string, error) {
	if err := id.validate(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		e.secretURL(id, name), nil)
	if err != nil {
		return "", fmt.Errorf("build escrow read: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)

	resp, err := e.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("read the escrow: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("the escrow refused the read (%d): %s",
			resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var out struct {
		Secret struct {
			SecretValue string `json:"secretValue"`
		} `json:"secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("read the escrow response: %w", err)
	}
	return out.Secret.SecretValue, nil
}

// BackupMasterKeys writes this cluster's Infisical master keys into the escrow.
func (e *infisicalEscrow) BackupMasterKeys(ctx context.Context, id Identity, keys interface{}) error {
	payload, err := json.Marshal(keys)
	if err != nil {
		return fmt.Errorf("marshal escrow payload: %w", err)
	}
	return e.BackupArtifact(ctx, id, escrowSecretName, string(payload))
}

// RestoreMasterKeys returns this cluster's escrowed keys, or nil when there are
// none.
func (e *infisicalEscrow) RestoreMasterKeys(ctx context.Context, id Identity) (interface{}, error) {
	raw, err := e.RestoreArtifact(ctx, id, escrowSecretName)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("the escrowed keys for %s are not readable as a backup: %w. "+
			"Generating new keys over them would make the existing store permanently "+
			"undecryptable, so this fails instead", id.ClusterName, err)
	}
	return out, nil
}

// PathMigrator copies an artefact from the flat pre-scope path to its scoped one.
//
// A capability interface, deliberately separate from EscrowClient: this is a
// one-time operation on an existing box, not part of how the escrow is used, and
// nothing that reads or writes artefacts normally should be able to reach the old
// path at all.
type PathMigrator interface {
	MigrateArtifactPath(ctx context.Context, id Identity, name string) (Migration, error)
}

// Migration is what one artefact's migration found, or did.
//
// Digest is a fingerprint, never the value: a migration is reported, and a report
// that carries key material defeats the point of having escrowed it.
type Migration struct {
	Artifact string
	From     string
	To       string
	Action   string
	Digest   string
}

// Migration actions.
const (
	// MigrationCopied means the source was written to the scoped path and the
	// destination was then read back and compared equal.
	MigrationCopied = "copied"
	// MigrationAlreadyPresent means the scoped path already held the same bytes,
	// or the two paths are the same path.
	MigrationAlreadyPresent = "already-present"
	// MigrationAbsent means the old path holds nothing under this name, so there
	// is nothing to migrate and nothing is wrong.
	MigrationAbsent = "absent"
)

// MigrateArtifactPath copies one artefact to the path its scope now gives it.
//
// Three properties, because the material is irreplaceable and the failure modes
// are silent:
//
//   - It COPIES. Nothing here generates, derives or rotates. `soloz encryption`
//     cannot serve as this migration: finding nothing at the scoped path, it mints
//     a fresh 32-byte key and applies it, and the first thing that key does is fail
//     to decrypt every Secret already written.
//   - It never overwrites. A destination holding DIFFERENT bytes is a conflict and
//     an error, because the only ways to reach that state are a successor that has
//     already written its own or a migration run against the wrong box, and both
//     are worse after a blind overwrite.
//   - It verifies. The destination is read back and compared with the source, so a
//     write the escrow accepted but did not store is a failure rather than a
//     "migrated" line in a report.
//
// The legacy copy is left in place. It costs nothing, and removing it is a separate
// decision taken after the new path has actually been used to recover something.
func (e *infisicalEscrow) MigrateArtifactPath(ctx context.Context, id Identity, name string) (Migration, error) {
	if err := id.validate(); err != nil {
		return Migration{}, err
	}
	from := legacyPath(id.ClusterName)
	to := escrowPath(id, name)
	m := Migration{Artifact: name, From: from, To: to}

	src, err := e.readAt(ctx, from, name)
	if err != nil {
		return m, fmt.Errorf("read %s from %s: %w", name, from, err)
	}
	if src == "" {
		m.Action = MigrationAbsent
		return m, nil
	}
	m.Digest = fingerprint(src)

	if from == to {
		m.Action = MigrationAlreadyPresent
		return m, nil
	}

	dst, err := e.readAt(ctx, to, name)
	if err != nil {
		return m, fmt.Errorf("read %s from %s: %w", name, to, err)
	}
	switch {
	case dst == src:
		m.Action = MigrationAlreadyPresent
		return m, nil
	case dst != "":
		return m, fmt.Errorf("%s already holds a DIFFERENT %s (source %s, destination %s): "+
			"refusing to overwrite it, because the only ways to reach this are a successor that "+
			"has written its own or a migration aimed at the wrong box, and overwriting makes "+
			"either unrecoverable", to, name, m.Digest, fingerprint(dst))
	}

	if err := e.BackupArtifact(ctx, id, name, src); err != nil {
		return m, fmt.Errorf("write %s to %s: %w", name, to, err)
	}
	back, err := e.readAt(ctx, to, name)
	if err != nil {
		return m, fmt.Errorf("read back %s from %s: %w", name, to, err)
	}
	if back != src {
		return m, fmt.Errorf("%s at %s does not match the source after writing it "+
			"(source %s, stored %s): the migration is NOT complete for this artefact",
			name, to, m.Digest, fingerprint(back))
	}
	m.Action = MigrationCopied
	return m, nil
}

// readAt reads one artefact from an explicit path. Empty and error are different
// answers, as in RestoreArtifact.
func (e *infisicalEscrow) readAt(ctx context.Context, secretPath, name string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.secretURLAt(secretPath, name), nil)
	if err != nil {
		return "", fmt.Errorf("build escrow read: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)

	resp, err := e.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("read the escrow: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("the escrow refused the read (%d): %s",
			resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Secret struct {
			SecretValue string `json:"secretValue"`
		} `json:"secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("read the escrow response: %w", err)
	}
	return out.Secret.SecretValue, nil
}

// fingerprint identifies a value without disclosing it: enough to compare two
// copies and to record in a report, not enough to be one.
func fingerprint(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])[:12]
}
