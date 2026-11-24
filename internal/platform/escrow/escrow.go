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
	"path"
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
	// BackupArtifact and RestoreArtifact hold anything else a box cannot be
	// rebuilt without, under a name of the caller's choosing.
	//
	// The master-key methods above are the same operation with a fixed name and a
	// JSON shape. They stay separate because their payload has a schema the restore
	// path reads field by field, where an artefact is opaque bytes.
	BackupArtifact(ctx context.Context, id Identity, name, payload string) error
	RestoreArtifact(ctx context.Context, id Identity, name string) (string, error)

	// BackupProjectArtifact and RestoreProjectArtifact hold material keyed by the
	// escrow PROJECT rather than by a box id, under ProjectArtifactFolder (ADR-117
	// §3). `kakt escrow init` writes them before `kakt box init` mints the box id;
	// each box has its own escrow project, so they are still per box.
	BackupProjectArtifact(ctx context.Context, name, payload string) error
	RestoreProjectArtifact(ctx context.Context, name string) (string, error)
}

// ProjectArtifactFolder is where project-keyed artefacts live.
const ProjectArtifactFolder = "/openbao"

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
// secret store can restore the store (ADR-117 §9) and still hold an identity
// database nothing can decrypt.
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

// preflightProbe is a name nothing writes: reading it proves the project type
// without needing write access (verifyProject).
const preflightProbe = "escrow-preflight-probe"

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
// ProviderCredentialArtifact names the escrowed copy of the cloud credential a
// box was built with.
//
// Escrowed for the same reason as the kubeconfig: the operations that need it
// most are the ones where the box cannot be asked. A teardown deletes servers
// through the provider's API, and the credential for that lived in the cluster
// (hetzner-credentials) and in the repository's CI secrets -- neither reachable
// when the cluster is gone, which left it as an environment variable the operator
// had to already have on the machine that bootstrapped.
//
// One artefact per provider, because a box may buy capacity from more than one
// and a single name would make the second overwrite the first.
// ArtifactProviderCredentialPrefix is the stem of every provider credential's
// artefact name.
//
// Declared rather than written inline, for the same reason
// ArtifactSpokePKIPrefix is: the enumeration that keeps ADR-076's list and this
// package honest reads declarations, and a name built inside a function body is
// a name that list cannot see. It reported this credential as documented and
// unimplemented while it had been escrowed for weeks.
const ArtifactProviderCredentialPrefix = "provider-credential-"

func ProviderCredentialArtifact(provider string) string {
	return ArtifactProviderCredentialPrefix + strings.TrimSpace(strings.ToLower(provider))
}

const ArtifactBackupSSECKey = "backup-ssec-key"

// ArtifactEscrowDestination records WHICH escrow this box was bound to, inside
// that escrow.
//
// The box's repository declares its escrow url and project id, and those are
// names rather than credentials -- which is why they are committed. But a
// committed field is editable, and an edited one would send the next privileged
// write to a project the editor controls: a redirect, not a leak, and invisible
// because every later read would succeed against the new destination.
//
// So the destination is also recorded in the destination, by the authorized
// enrolment that establishes it. A declaration that does not match what its own
// escrow says about itself is refused, and a redirect to another project finds no
// record there at all. Neither can be produced by editing Git alone.
//
// Box-scoped: the binding belongs to the box and outlives every cluster in it.
const ArtifactEscrowDestination = "escrow-destination"

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
	ArtifactEscrowDestination: true,
	// The credentials a box is built with (box_credentials.go). Box-scoped
	// because they outlive every management cluster: a successor pulls the same
	// images, archives to the same bucket and pages the same people.
	ArtifactRegistryCredential:    true,
	ArtifactAlertDestination:      true,
	ArtifactMailTransportURL:      true,
	ArtifactTailnetAuthkey:        true,
	ArtifactZitadelMasterkey:      true,
	ArtifactBackupSSECKey:         true,
	ArtifactBackupStoreCredential: true,
	// One Infisical per box, administered by one account. Keyed to the box
	// because a successor hub inherits that instance's database, and with it the
	// user row this credential logs in as.
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
// The root is the BOX's, and is named for it.
//
// It was "/hub-operator/", which named one of the readers. hub-operator reads some
// of these artefacts and is not installed when most of them are written: this
// package writes the cloud token, the master keys, the store credential and the
// at-rest encryption key during `box init` and Day-0, with no operator anywhere.
// A folder named after a controller claims that controller owns its contents, and
// for this material that is untrue. "hub" is also a word this platform retired
// (ADR-111 Revision 9 §4), and the escrow root is where a tenant met it first.
//
// Plural, because one escrow project serves every box in the organisation.
const escrowRoot = "/boxes/"

func escrowPath(id Identity, artifact string) string {
	if scope, _ := DeclaredScope(artifact); scope == ScopeCluster {
		return escrowRoot + id.BoxID + "/clusters/" + id.ClusterName
	}
	return escrowRoot + id.BoxID
}

// NewEscrowClient builds the escrow from the environment, or reports why it cannot.
//
// inClusterURL is this box's own Infisical, when the caller knows it. It is what
// makes this an escrow rather than a second copy in the same place: an escrow
// inside the thing it protects is unreachable exactly when it is needed, and this
// refuses that configuration rather than appearing to work. Callers with no cluster
// to compare against -- the CLI retrieving a copy -- pass "".
// escrowValue reads one escrow setting: the environment, then this operator's
// store under the home directory.
//
// The walk up the working directory for `k8-secrets/infisical` is gone, and with
// it the assumption that a command runs inside a checkout. See store.go.
func escrowValue(name string) string { return StoredValue(name) }

func NewEscrowClient(ctx context.Context, inClusterURL string) (EscrowClient, error) {
	url := escrowValue("ESCROW_PROVIDER_URL")
	clientID := escrowValue("ESCROW_PROVIDER_CLIENT_ID")
	clientSecret := escrowValue("ESCROW_PROVIDER_CLIENT_SECRET")
	projectID := escrowValue("ESCROW_PROVIDER_PROJECT_ID")

	if url == "" && clientID == "" && clientSecret == "" && projectID == "" {
		return nil, fmt.Errorf("no escrow configured")
	}
	// All four or none. Reading them from one Secret means a partial set produces a
	// caller that attempts a backup on every reconcile and fails -- an escrow that
	// appears to exist and does not work, which is worse than none.
	var missing []string
	for _, kv := range []struct{ name, value string }{
		{"ESCROW_PROVIDER_URL", url},
		{"ESCROW_PROVIDER_PROJECT_ID", projectID},
		{"ESCROW_PROVIDER_CLIENT_ID", clientID},
		{"ESCROW_PROVIDER_CLIENT_SECRET", clientSecret},
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

// NewEscrowClientForToken builds the same client against an ALREADY AUTHENTICATED
// identity, for a caller holding a token rather than a machine credential.
//
// This is what makes a developer's own login reach the escrow. `kakt login`
// obtains a user session from the Infisical the box escrows to, and the API
// accepts a user session on the secret routes the escrow uses -- their
// verifyAuth lists AuthMode.JWT beside AuthMode.IDENTITY_ACCESS_TOKEN. So the
// read is performed AS THE DEVELOPER, authorized by that person's permissions on
// the escrow project and attributed to them in Infisical's audit log, rather than
// as a shared machine identity whose client secret had to be copied onto their
// workstation first.
//
// The coordinates are passed rather than read from the environment, because a
// workstation that has never bootstrapped a box has no k8-secrets/infisical
// directory to walk up to and no reason to acquire one: the box declares its own
// escrow url and project id, both of which are names rather than credentials.
//
// Same four calls, same project verification. The only difference is where the
// bearer token came from, which is why this is a constructor and not a second
// client.
func NewEscrowClientForToken(ctx context.Context, url, projectID, token string) (EscrowClient, error) {
	url = strings.TrimSpace(url)
	projectID = strings.TrimSpace(projectID)
	token = strings.TrimSpace(token)

	var missing []string
	for _, kv := range []struct{ name, value string }{
		{"escrow url", url},
		{"escrow project id", projectID},
		{"session token", token},
	} {
		if kv.value == "" {
			missing = append(missing, kv.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("cannot reach the escrow: no %s", strings.Join(missing, ", "))
	}

	e := &infisicalEscrow{
		baseURL:   strings.TrimSuffix(url, "/"),
		projectID: projectID,
		http:      &http.Client{Timeout: 30 * time.Second},
		token:     token,
	}
	// No authenticate(): the token IS the authentication. verifyProject still
	// runs, because a session that is valid says nothing about whether the
	// project it is pointed at can hold an escrow -- the failure a KMS project
	// produces is identical for a user and for a machine identity.
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
// days earlier, and accepted when the box is rendered on nothing more than a presence
// check.
//
// The probe is a read of a path that does not exist: one request, no write
// access needed to establish the type, and 404 is the healthy answer.
func (e *infisicalEscrow) verifyProject(ctx context.Context) error {
	_, err := e.RestoreArtifact(ctx, ForCluster("__escrow_preflight__", "__escrow_preflight__"), preflightProbe)
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
	return e.ensureFolder(ctx, escrowPath(id, artifact))
}

// ensureFolder creates the folder an artefact path names, tolerating "exists".
func (e *infisicalEscrow) ensureFolder(ctx context.Context, folder string) error {
	parent, name := path.Split(folder)
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
			folder, resp.StatusCode, strings.TrimSpace(string(b)))
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
	return e.writeAt(ctx, escrowPath(id, name), name, payload)
}

// BackupProjectArtifact writes a project-keyed artefact (ADR-117 §3).
func (e *infisicalEscrow) BackupProjectArtifact(ctx context.Context, name, payload string) error {
	if err := e.ensureFolder(ctx, ProjectArtifactFolder); err != nil {
		return err
	}
	return e.writeAt(ctx, ProjectArtifactFolder, name, payload)
}

// RestoreProjectArtifact reads a project-keyed artefact; empty means absent.
func (e *infisicalEscrow) RestoreProjectArtifact(ctx context.Context, name string) (string, error) {
	return e.readAt(ctx, ProjectArtifactFolder, name)
}

// writeAt creates or updates one artefact at an explicit path.
func (e *infisicalEscrow) writeAt(ctx context.Context, secretPath, name, payload string) error {
	body, _ := json.Marshal(map[string]string{
		"workspaceId": e.projectID,
		"environment": environmentSlug,
		"secretPath":  secretPath,
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
