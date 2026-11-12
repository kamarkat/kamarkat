package escrow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Initialising an escrow is one command, run by the signed-in administrator.
//
// It used to be three manual acts in Infisical's dashboard before `kakt escrow
// init` could do anything: create an account, create a machine identity, choose
// its roles. The first is irreducible -- the account is the tenant's. The other
// two were there only because the command authenticated AS the machine identity,
// which predates `kakt login`: there was no human session to act with, so the
// operator had to manufacture a credential before the CLI could speak at all.
//
// With a session, every call below is authorized by the administrator, and the
// two steps that were manual are the ones a person most easily gets wrong:
//
//   - the ORG role, where the choice is between four values with no guidance and
//     the consequence of picking Admin is an automation credential that can
//     manage the organisation it is meant to serve;
//   - the PROJECT role, which was never chosen at all -- the identity became
//     project admin as a side effect of creating the project, so least privilege
//     happened not to apply and nothing said so.
//
// WHAT IS AUTHORIZED, verified against the vendored Infisical rather than assumed
// from the route's AuthMode. Accepting a JWT says the caller may be a human; it
// does not say which humans may do this:
//
//	operation                  permission                 org member  org admin
//	create an identity         Identity.Create                 yes        yes
//	attach Universal Auth      Identity.EditAuth                NO        yes
//	issue a client secret      Identity.CreateToken             NO        yes
//
// ee/services/permission/org-permission.ts grants member Read/Create/Edit/Delete
// on Identity and withholds EditAuth, CreateToken, GetToken and RevokeAuth;
// identity-edit-auth-permission.test.ts asserts exactly that. So an org MEMBER
// can create the identity and can neither give it an auth method nor a secret --
// it would be left inert, which is worse than refusing. The refusal says so.
const (
	pathIdentities       = "/api/v1/identities"
	pathIdentitiesSearch = "/api/v1/identities/search"
	pathUniversalAuth    = "/api/v1/auth/universal-auth/identities"
)

// orgRoleNoAccess is the organisation role the automation identity gets.
//
// Infisical's own default for a new identity (identity-router.ts: `role`
// defaults to OrgMembershipRole.NoAccess), and the right one: this identity
// performs no organisation action. It reads and writes secrets in one project,
// which project membership grants and an organisation role does not.
const orgRoleNoAccess = "no-access"

// projectRoleMember is the project role it gets, and it is NOT admin.
//
// Everything the escrow does is a secret or a folder: BackupArtifact writes a
// secret, RestoreArtifact reads one, ensurePath creates the folder a cluster's
// artefacts live in. default-roles.ts buildMemberPermissionRules grants Member
// DescribeSecret, DescribeAndReadValue, ReadValue, Edit, Create and Delete on
// Secrets, and Edit, Create and Delete on SecretFolders -- all of it.
//
// Admin additionally carries membership and role management over the project,
// which an automation credential has no use for. Today the identity is a project
// admin by accident: it created the project, and Infisical adds the creating
// actor as admin. Creating the project as the HUMAN removes that accident and
// lets the identity be given what it needs instead.
const projectRoleMember = "member"

// AutomationIdentityName is the organisation's one automation identity.
//
// ONE PER ORGANISATION, not one per box. Infisical's free plan allows five
// identities, human and machine together, so an identity per box exhausts the
// plan at the fifth box -- and the escrow paths are already box-scoped
// (/hub-operator/<boxId>/...), so a second identity buys no isolation that the
// paths do not already provide. A later box reuses this one and is added to its
// own project.
//
// Fixed rather than derived, because it is how a second initialisation FINDS the
// first one's identity. A name containing the box or the project would make every
// box look like a new organisation.
const AutomationIdentityName = "kamarkat-automation"

// Provisioned is what an initialisation established, and what it changed.
type Provisioned struct {
	ProjectID  string
	IdentityID string
	ClientID   string
	// ClientSecret is always set: every box gets its own.
	//
	// An existing identity's secret cannot be read back -- the list endpoint
	// returns clientSecretPrefix and never the value (Infisical's
	// sanitizedClientSecretSchema) -- so a box that reuses the organisation's
	// identity is ISSUED one rather than asking an operator to find the first
	// box's copy.
	//
	// Not a rotation: Universal Auth holds many client secrets per identity, and
	// issuing one neither invalidates nor touches the others, so the box already
	// running keeps authenticating with its own. Each is separately revocable,
	// which is what makes per-box secrets better than one shared value -- losing
	// a box costs that box's credential and nothing else.
	ClientSecret string
	// IdentityCreated and SecretIssued say what this run is responsible for, so
	// a caller can report accurately and a rollback can undo only its own work.
	IdentityCreated bool
	SecretIssued    bool
	// Reused is set when the organisation already had the automation identity.
	Reused bool
}

// ProvisionEscrow creates the escrow project and ensures the organisation's
// automation identity can reach it.
//
// Idempotent, and that is the whole design of the retry path: each step asks
// before it writes. A re-run finds the identity, finds its auth method, finds its
// membership, and writes nothing. A run interrupted halfway completes the missing
// part without consuming a second identity.
func ProvisionEscrow(ctx context.Context, baseURL, sessionToken, orgID, projectName string) (Provisioned, error) {
	var p Provisioned
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	for name, v := range map[string]string{
		"escrow URL":      baseURL,
		"session":         strings.TrimSpace(sessionToken),
		"organisation id": strings.TrimSpace(orgID),
		"project name":    strings.TrimSpace(projectName),
	} {
		if v == "" {
			return p, fmt.Errorf("cannot initialise the escrow: no %s", name)
		}
	}

	c := &provisioner{
		baseURL: baseURL,
		token:   strings.TrimSpace(sessionToken),
		http:    &http.Client{Timeout: 30 * time.Second},
	}

	// 1. The project, created by the HUMAN -- which is what makes them its admin
	// and removes the invitation step that existed because a machine-created
	// project has no human members at all.
	projectID, err := c.createProject(ctx, projectName)
	if err != nil {
		return p, err
	}
	p.ProjectID = projectID

	return c.ensureAutomation(ctx, p, orgID, projectName)
}

// ensureAutomation gives the organisation's automation identity what it needs to
// reach one escrow project, and issues this box its own client secret.
//
// Shared by both entry points, because the only thing a first initialisation and
// an adoption disagree about is whether the project had to be created.
func (c *provisioner) ensureAutomation(ctx context.Context, p Provisioned, orgID, label string) (Provisioned, error) {
	projectID := p.ProjectID
	projectName := label
	// 2. The organisation's automation identity: found, or created once.
	id, clientID, found, err := c.findAutomationIdentity(ctx, orgID)
	if err != nil {
		return p, err
	}
	p.Reused = found
	if !found {
		id, err = c.createIdentity(ctx, orgID, AutomationIdentityName)
		if err != nil {
			return p, err
		}
		p.IdentityCreated = true
	}
	p.IdentityID = id

	// From here a failure leaves an identity this run created, so it is undone.
	// Only ever this run's: a pre-existing identity may be serving other boxes.
	fail := func(e error) (Provisioned, error) {
		if !p.IdentityCreated {
			return p, e
		}
		if delErr := c.deleteIdentity(ctx, id); delErr != nil {
			return p, fmt.Errorf("%w\n\nThe automation identity %s (%s) was created by this "+
				"run and could not be removed: %v\n\nDelete it in Infisical before re-running, "+
				"or it will count against the organisation's identity limit.",
				e, AutomationIdentityName, id, delErr)
		}
		return p, e
	}

	// 3. Universal Auth, and the client ID it carries. Asked for first: an
	// identity that already has it keeps the credential the running boxes use.
	ua, err := c.universalAuth(ctx, id)
	if err != nil {
		return fail(err)
	}
	if !ua {
		if err := c.attachUniversalAuth(ctx, id); err != nil {
			return fail(err)
		}
	}
	if clientID == "" {
		clientID, err = c.clientIDOf(ctx, id)
		if err != nil {
			return fail(err)
		}
	}
	p.ClientID = clientID

	// 4. This box's own client secret, whether the identity is new or reused.
	// The existing one cannot be read back, and every box needs a credential of
	// its own anyway: described by the project it was issued for, so a leak is
	// revoked for one box rather than for the organisation.
	secret, err := c.issueClientSecret(ctx, id, projectName)
	if err != nil {
		return fail(err)
	}
	p.ClientSecret = secret
	p.SecretIssued = true

	// 5. Membership of this project, as member rather than admin.
	if err := c.addToProject(ctx, projectID, id, projectRoleMember); err != nil {
		return fail(err)
	}
	return p, nil
}

// provisioner performs the calls as the signed-in administrator.
type provisioner struct {
	baseURL string
	token   string
	http    *http.Client
}

func (c *provisioner) do(ctx context.Context, method, path string, body interface{}) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("build the request to %s: %w", path, err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return 0, nil, fmt.Errorf("build the request to %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("reach %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

func ok(status int) bool { return status >= 200 && status < 300 }

// refusedForPermission turns 401 and 403 into the one error that is actionable.
//
// Named per operation, because the permission a person is missing is not the
// same for each and "403" sends them to the wrong setting. The organisation role
// is what grants these, and only Admin carries the two auth permissions.
func (c *provisioner) refusedForPermission(status int, raw []byte, what string) error {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return nil
	}
	return fmt.Errorf(`your Infisical account is not allowed to %s (%d).

Initialising an escrow needs an ORGANISATION ADMIN. An organisation member can
create an identity but cannot give it an authentication method or a client
secret, so the identity would exist and be unusable -- which is why this stops
here rather than leaving one behind.

Either have an organisation admin run this, or ask one to raise your role, then
run it again. Nothing was left half-created.

Infisical said: %s`, what, status, strings.TrimSpace(string(raw)))
}

func (c *provisioner) createProject(ctx context.Context, name string) (string, error) {
	status, raw, err := c.do(ctx, http.MethodPost, pathProjects, map[string]interface{}{
		"projectName": name,
		"type":        projectTypeSecretManager,
	})
	if err != nil {
		return "", err
	}
	if permErr := c.refusedForPermission(status, raw, "create a project in this organisation"); permErr != nil {
		return "", permErr
	}
	if !ok(status) {
		return "", fmt.Errorf("the escrow refused to create project %q (%d): %s",
			name, status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Project struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"project"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("the reply to creating %q is not the expected shape: %w\n\nreply: %s",
			name, err, strings.TrimSpace(string(raw)))
	}
	if out.Project.ID == "" {
		return "", fmt.Errorf("the escrow created %q and returned no project id\n\nreply: %s",
			name, strings.TrimSpace(string(raw)))
	}
	// The type is asserted for the same reason it is sent: a project of another
	// type authenticates identically and refuses every read and write.
	if t := strings.TrimSpace(out.Project.Type); t != "" && t != projectTypeSecretManager {
		return "", fmt.Errorf("the escrow created %q as a %q project, and an escrow must be %q. "+
			"Delete it and re-run", name, t, projectTypeSecretManager)
	}
	return out.Project.ID, nil
}

// findAutomationIdentity looks for the organisation's designated identity.
//
// Searched by the fixed name, which is what makes a second box reuse the first
// box's identity instead of adding another to a five-identity budget.
func (c *provisioner) findAutomationIdentity(ctx context.Context, orgID string) (id, clientID string, found bool, err error) {
	status, raw, err := c.do(ctx, http.MethodGet,
		// orgId on the LIST query; organizationId in the CREATE body. The two
		// are spelled differently, and sending the body's name here is a 422
		// naming `orgId` as missing.
		//
		// Shape taken from operators/spoke-identity-operator/internal/infisical
		// (client.go:398, paths.go), which has been calling these endpoints
		// against the live service for longer than this has existed.
		pathIdentities+"?limit=100&orgId="+orgID, nil)
	if err != nil {
		return "", "", false, err
	}
	if permErr := c.refusedForPermission(status, raw, "list this organisation's identities"); permErr != nil {
		return "", "", false, permErr
	}
	if !ok(status) {
		// Unreadable is not absent. Creating one here would be how an
		// organisation acquires a second automation identity every time the
		// listing endpoint has a bad minute.
		return "", "", false, fmt.Errorf("could not read this organisation's identities (%d): %s",
			status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Identities []struct {
			Identity struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"identity"`
		} `json:"identities"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", false, fmt.Errorf("the identity listing is not the expected shape: %w", err)
	}
	for _, i := range out.Identities {
		if strings.EqualFold(strings.TrimSpace(i.Identity.Name), AutomationIdentityName) {
			return i.Identity.ID, "", true, nil
		}
	}
	return "", "", false, nil
}

func (c *provisioner) createIdentity(ctx context.Context, orgID, name string) (string, error) {
	status, raw, err := c.do(ctx, http.MethodPost, pathIdentities, map[string]interface{}{
		"name":           name,
		"organizationId": orgID,
		"role":           orgRoleNoAccess,
	})
	if err != nil {
		return "", err
	}
	if permErr := c.refusedForPermission(status, raw, "create an identity in this organisation"); permErr != nil {
		return "", permErr
	}
	if !ok(status) {
		// The identity limit is the expected refusal on a full organisation, and
		// it is the one a person can act on without reading a status code.
		if strings.Contains(strings.ToLower(string(raw)), "limit") {
			return "", fmt.Errorf(`this organisation has no room for another identity (%d).

Infisical's free plan allows five identities, humans and machines together. The
platform needs one automation identity for the whole organisation and creates no
others, so if this is full, something else is using the slots.

Remove an identity you no longer need, or reuse the existing automation identity
by passing its credentials to this command.

Infisical said: %s`, status, strings.TrimSpace(string(raw)))
		}
		return "", fmt.Errorf("the escrow refused to create the automation identity (%d): %s",
			status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Identity struct {
			ID string `json:"id"`
		} `json:"identity"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Identity.ID == "" {
		return "", fmt.Errorf("the escrow created the automation identity and returned no id\n\nreply: %s",
			strings.TrimSpace(string(raw)))
	}
	return out.Identity.ID, nil
}

func (c *provisioner) deleteIdentity(ctx context.Context, id string) error {
	status, raw, err := c.do(ctx, http.MethodDelete, pathIdentities+"/"+id, nil)
	if err != nil {
		return err
	}
	if !ok(status) && status != http.StatusNotFound {
		return fmt.Errorf("%d: %s", status, strings.TrimSpace(string(raw)))
	}
	return nil
}

// universalAuth reports whether the identity already has the method attached.
func (c *provisioner) universalAuth(ctx context.Context, id string) (bool, error) {
	status, raw, err := c.do(ctx, http.MethodGet, pathUniversalAuth+"/"+id, nil)
	if err != nil {
		return false, err
	}
	switch {
	case ok(status):
		return true, nil
	case status == http.StatusNotFound:
		return false, nil
	}
	if permErr := c.refusedForPermission(status, raw, "read this identity's authentication method"); permErr != nil {
		return false, permErr
	}
	return false, fmt.Errorf("could not read the identity's authentication method (%d): %s",
		status, strings.TrimSpace(string(raw)))
}

func (c *provisioner) attachUniversalAuth(ctx context.Context, id string) error {
	// The same body spoke-identity-operator sends (client.go:515-525): the
	// access-token TTLs stated rather than defaulted. They match the server's
	// current defaults, and stating them is what that client does against the
	// live service.
	status, raw, err := c.do(ctx, http.MethodPost, pathUniversalAuth+"/"+id, map[string]interface{}{
		"accessTokenTTL":    2592000,
		"accessTokenMaxTTL": 2592000,
	})
	if err != nil {
		return err
	}
	if permErr := c.refusedForPermission(status, raw,
		"configure an authentication method on this identity"); permErr != nil {
		return permErr
	}
	if !ok(status) {
		return fmt.Errorf("the escrow refused to configure Universal Auth on the automation "+
			"identity (%d): %s", status, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (c *provisioner) clientIDOf(ctx context.Context, id string) (string, error) {
	status, raw, err := c.do(ctx, http.MethodGet, pathUniversalAuth+"/"+id, nil)
	if err != nil {
		return "", err
	}
	if !ok(status) {
		return "", fmt.Errorf("could not read the automation identity's client id (%d): %s",
			status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		IdentityUniversalAuth struct {
			ClientID string `json:"clientId"`
		} `json:"identityUniversalAuth"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.IdentityUniversalAuth.ClientID == "" {
		return "", fmt.Errorf("the reply carries no client id\n\nreply: %s", strings.TrimSpace(string(raw)))
	}
	return out.IdentityUniversalAuth.ClientID, nil
}

// issueClientSecret creates this box's credential on the automation identity.
//
// Returned once by Infisical and never readable again: the listing carries
// clientSecretPrefix and no value. So a box gets its own rather than borrowing a
// copy an operator has to go and find, and the description names the project it
// belongs to -- which is how one box's credential is revoked without taking the
// organisation's automation down with it.
func (c *provisioner) issueClientSecret(ctx context.Context, id, project string) (string, error) {
	description := "kamarkat platform automation"
	if p := strings.TrimSpace(project); p != "" {
		description += " (" + p + ")"
	}
	status, raw, err := c.do(ctx, http.MethodPost, pathUniversalAuth+"/"+id+"/client-secrets",
		map[string]interface{}{
			"description": description,
			// 0 is never expires, stated rather than defaulted
			// (spoke-identity-operator client.go:555-567). An expiring secret
			// takes the box's escrow access with it, silently.
			"ttl": 0,
		})
	if err != nil {
		return "", err
	}
	if permErr := c.refusedForPermission(status, raw,
		"issue a client secret for this identity"); permErr != nil {
		return "", permErr
	}
	if !ok(status) {
		return "", fmt.Errorf("the escrow refused to issue a client secret (%d): %s",
			status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || strings.TrimSpace(out.ClientSecret) == "" {
		// Not logged, and not echoed: the body holds the secret.
		return "", fmt.Errorf("the escrow issued a client secret and the reply does not carry it")
	}
	return out.ClientSecret, nil
}

// addToProject grants an EXISTING identity membership of this box's escrow project.
//
// The route is the one under the `/projects/:projectId/memberships` prefix, with
// the identity in the PATH:
//
//	POST /api/v1/projects/<project>/memberships/identities/<identity>
//
// Not `POST /api/v1/projects/<project>/identities`, which is registered under
// the same `/projects` prefix, reads almost identically, and does something
// else entirely: its operationId is `createProjectMachineIdentity` and its body
// takes a `name`, because it CREATES a new project-scoped identity rather than
// admitting one that already exists. Sending a membership body to it is a 422
// asking for `name` -- a validation error that names a field the caller never
// meant to send, which is how the mistake hides.
func (c *provisioner) addToProject(ctx context.Context, projectID, identityID, role string) error {
	status, raw, err := c.do(ctx, http.MethodPost,
		pathProjects+"/"+projectID+"/memberships/identities/"+identityID,
		// `role`, singular -- the field spoke-identity-operator sends
		// (client.go:268-279). The route also accepts a `roles` array; this is
		// the form known to work.
		map[string]string{"role": role})
	if err != nil {
		return err
	}
	// Already a member is success. A re-run of a partially completed
	// initialisation has to be able to finish, and Infisical answers a duplicate
	// membership with a conflict rather than a no-op.
	if status == http.StatusConflict || strings.Contains(strings.ToLower(string(raw)), "already") {
		return nil
	}
	if permErr := c.refusedForPermission(status, raw,
		"add an identity to this escrow project"); permErr != nil {
		return permErr
	}
	if !ok(status) {
		return fmt.Errorf("the escrow refused to give the automation identity access to the "+
			"project (%d): %s", status, strings.TrimSpace(string(raw)))
	}
	return nil
}

// ProvisionEscrowWithIdentity creates the project and admits an identity the
// operator manages themselves.
//
// For an organisation whose policy is that the CLI creates no identities. The
// project is still created by the administrator -- that part is not an escape
// hatch, it is what makes them its admin -- and the supplied identity is given
// the same project role the managed one gets.
//
// Nothing is created, nothing is rotated and nothing is rolled back: this run
// owns no identity.
func ProvisionEscrowWithIdentity(ctx context.Context, baseURL, sessionToken, orgID, projectName, clientID string) (Provisioned, error) {
	var p Provisioned
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return p, fmt.Errorf("no client id: there is no identity to admit")
	}
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	c := &provisioner{baseURL: baseURL, token: strings.TrimSpace(sessionToken),
		http: &http.Client{Timeout: 30 * time.Second}}

	projectID, err := c.createProject(ctx, projectName)
	if err != nil {
		return p, err
	}
	p.ProjectID = projectID
	p.ClientID = clientID

	id, err := c.identityByClientID(ctx, orgID, clientID)
	if err != nil {
		return p, err
	}
	if err := c.addToProject(ctx, projectID, id, projectRoleMember); err != nil {
		return p, err
	}
	p.IdentityID = id
	return p, nil
}

// identityByClientID resolves a supplied client id to the identity that carries
// it, so the membership call names an identity rather than a credential.
func (c *provisioner) identityByClientID(ctx context.Context, orgID, clientID string) (string, error) {
	status, raw, err := c.do(ctx, http.MethodGet,
		// orgId on the LIST query; organizationId in the CREATE body. The two
		// are spelled differently, and sending the body's name here is a 422
		// naming `orgId` as missing.
		//
		// Shape taken from operators/spoke-identity-operator/internal/infisical
		// (client.go:398, paths.go), which has been calling these endpoints
		// against the live service for longer than this has existed.
		pathIdentities+"?limit=100&orgId="+orgID, nil)
	if err != nil {
		return "", err
	}
	if permErr := c.refusedForPermission(status, raw, "list this organisation's identities"); permErr != nil {
		return "", permErr
	}
	if !ok(status) {
		return "", fmt.Errorf("could not read this organisation's identities (%d): %s",
			status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Identities []struct {
			Identity struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"identity"`
		} `json:"identities"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("the identity listing is not the expected shape: %w", err)
	}
	for _, i := range out.Identities {
		got, err := c.clientIDOf(ctx, i.Identity.ID)
		if err != nil {
			// An identity with no Universal Auth has no client id, which is not
			// an error here -- it is simply not the one being looked for.
			continue
		}
		if got == clientID {
			return i.Identity.ID, nil
		}
	}
	return "", fmt.Errorf("no identity in this organisation carries client id %s.\n\n"+
		"Check the value, or omit --client-id and let this manage the organisation's "+
		"automation identity", clientID)
}

// ProvisionEscrowInProject prepares an escrow project that ALREADY EXISTS.
//
// The case a box that is already running presents: its repository declares
// `escrow.projectId`, that project holds its master keys, and what is missing is
// only an automation identity with access to it and a client secret this machine
// can use. Creating a project here would make a second, empty escrow and leave
// the box's own untouched and unmentioned -- which is exactly what happened
// before this existed.
//
// The declared project is adopted rather than created. Everything else is the
// same as a first initialisation: the organisation's one automation identity,
// found or created, given membership of this project, and issued this box's own
// client secret.
func ProvisionEscrowInProject(ctx context.Context, baseURL, sessionToken, orgID, projectID, label string) (Provisioned, error) {
	var p Provisioned
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return p, fmt.Errorf("no project id: there is no escrow to adopt")
	}
	c, err := newProvisioner(baseURL, sessionToken, orgID)
	if err != nil {
		return p, err
	}
	p.ProjectID = projectID
	return c.ensureAutomation(ctx, p, orgID, label)
}

// newProvisioner is the shared client construction, so the two entry points
// cannot disagree about timeouts or trimming.
func newProvisioner(baseURL, sessionToken, orgID string) (*provisioner, error) {
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	for name, v := range map[string]string{
		"escrow URL":      baseURL,
		"session":         strings.TrimSpace(sessionToken),
		"organisation id": strings.TrimSpace(orgID),
	} {
		if v == "" {
			return nil, fmt.Errorf("cannot reach the escrow: no %s", name)
		}
	}
	return &provisioner{
		baseURL: baseURL,
		token:   strings.TrimSpace(sessionToken),
		http:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}
