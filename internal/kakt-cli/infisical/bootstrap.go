package infisical

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
)

var (
	// Resolved on USE, not at package init. Initialising these eagerly made every
	// kakt subcommand panic before it parsed its flags -- `kakt --help` included
	// -- because a package-level var is evaluated when the package is imported,
	// not when Infisical is reached.
	adminEmail    = func() string { return mustEnv("INFISICAL_ADMIN_EMAIL", "the administrator's email address") }
	adminPassword = func() string { return mustEnv("INFISICAL_ADMIN_PASSWORD", "the administrator's password") }
	// The organisation NAME is not a credential and not per-box: it is a label
	// inside the box's own Infisical, the same on every one by design.
	orgName = envOrDefault("INFISICAL_ORG_NAME", "Zero-Ops")
)

// Required. No defaults, and the ones removed here were a personal email address
// and the literal password "Password@123" -- on EVERY box, platform and tenant
// alike, because nothing set these variables anywhere in the repository.
//
// That is a shared, known credential on the administrator account of each
// tenant's secret store: the account that can read every secret the platform
// manages for them. ADR-003 makes Infisical the system of record for secret
// material, and ADR-065 says the platform holds no secret belonging to a tenant
// -- a default admin the platform chose, identical on every box, is both.
//
// Mandatory rather than generated here: these must be collected with the
// tenant's other credentials and stored on their repository, the way the escrow
// credentials are (ADR-076), so the platform never holds them.
func mustEnv(key, what string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		// Empty, and every caller checks. Returning "" rather than exiting keeps
		// the refusal at the point Infisical is actually reached, with the
		// surrounding operation's context, instead of killing an unrelated command.
		fmt.Fprintf(os.Stderr, "\n%s is not set. It is %s for THIS box's Infisical.\n"+
			"There is no default: the one that existed was identical on every box,\n"+
			"so every tenant's secret store shared one administrator credential.\n\n", key, what)
	}
	return v
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

const (
	identityName = "hub-platform-eso"

	infisicalNamespace = "platform-security"
	infisicalPort      = "8080"
)

// errAlreadyBootstrapped is returned by runBootstrapCLI when the Infisical
// server is already bootstrapped and the CLI banner-only output was
// produced. Callers must NOT reset the database when they receive this
// error — they should call getExistingOrgData to recover the existing
// org/identity IDs without destroying state.
var errAlreadyBootstrapped = errors.New("infisical instance already bootstrapped")

var cachedBootstrap *bootstrapOutput

type BootstrapResult struct {
	OrgID              string
	ProjectID          string
	ProjectSlug        string
	SecretsProjectID   string
	SecretsProjectSlug string
	ClientID           string
	ClientSecret       string
	IdentityID         string
}

// bootstrapOutput maps the actual `infisical bootstrap --output json` response.
// Verified on 2026-06-06 against Infisical OSS v0.93.x:
//
//	{
//	  "message": "Successfully bootstrapped instance",
//	  "identity": {
//	    "id": "<uuid>",
//	    "name": "Instance Admin Identity",
//	    "credentials": { "token": "<jwt>" }
//	  },
//	  "organization": {
//	    "id": "<uuid>",
//	    "name": "Zero-Ops",
//	    "slug": "zero-ops",
//	    "defaultMembershipRole": "admin",
//	    "authEnforced": false,
//	    "scimEnabled": true
//	  }
//	}
type bootstrapOutput struct {
	Message  string `json:"message"`
	Identity struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Credentials struct {
			Token string `json:"token"`
		} `json:"credentials"`
	} `json:"identity"`
	Organization struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"organization"`
}

type createProjectResponse struct {
	Project struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	} `json:"project"`
}

type createIdentityResponse struct {
	Identity struct {
		ID string `json:"id"`
	} `json:"identity"`
}

type attachUniversalAuthResponse struct {
	IdentityUniversalAuth struct {
		ClientID   string `json:"clientId"`
		IdentityID string `json:"identityId"`
	} `json:"identityUniversalAuth"`
}

type createClientSecretResponse struct {
	ClientSecret string `json:"clientSecret"`
}

// getExistingOrgData recovers the org/identity state from an already-
// bootstrapped Infisical server via the API. It is used in place of the
// previous resetBootstrapState, which destructively DELETED users and
// projects from PostgreSQL on re-run and forced callers to re-bootstrap
// from scratch — that path orphaned any in-flight cert-operator
// reconcile (which had cached the old project IDs at startup).
//
// The recovery flow:
//  1. POST /api/v3/auth/login with the well-known bootstrap credentials
//     to get a user JWT (the V1 auth router has no login endpoint in
//     this Infisical version — the email/password login is at V3 only).
//     The V3 login returns {"accessToken": "<jwt>"} — no org context.
//  2. GET /api/v1/organization to get the org ID + name.
//     Returns {"organizations": [{"id": "...", "name": "...", ...}]}
//     (an array inside "organizations" key — NOT a flat object).
//  3. POST /api/v3/auth/select-organization to embed the org ID into
//     the JWT. Without this, org-scoped endpoints (identities, etc.)
//     return 401 "no organization found in request". The select-org
//     endpoint returns {"token": "<jwt>", "isMfaEnabled": false}.
//  4. GET /api/v1/identities?orgId=<org> with the org-scoped JWT to
//     find the "Instance Admin Identity" created by `infisical bootstrap`.
//
// All HTTP calls use `curl -s` (no -f) so HTTP 4xx responses with JSON
// bodies (e.g., 409 Conflict) are still parsed and handled gracefully
// by the surrounding Go code. Calls where HTTP errors are unambiguous
// hard failures (login, select-org, PATCH grant) do use `curl -f` so
// a non-2xx response surfaces immediately as a non-nil error.
func getExistingOrgData(ctx context.Context, podName string) (*bootstrapOutput, error) {
	// 1. Log in as the bootstrap admin to get a JWT.
	loginPayload := fmt.Sprintf(`{"email":"%s","password":"%s"}`, adminEmail(), adminPassword())
	loginOut, err := kubectlExec(ctx, podName,
		"curl", "-s", "-f", "-X", "POST",
		"http://localhost:"+infisicalPort+PathAuthLoginV3,
		"-H", "Content-Type: application/json",
		"-d", loginPayload,
	)
	if err != nil {
		return nil, fmt.Errorf("admin login failed: %w", err)
	}
	var loginResp struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal([]byte(loginOut), &loginResp); err != nil {
		return nil, fmt.Errorf("decode admin login: %w\nbody: %s", err, loginOut)
	}
	if loginResp.AccessToken == "" {
		return nil, fmt.Errorf("admin login returned no accessToken: %s", loginOut)
	}
	adminJWT := loginResp.AccessToken

	// 2. Get the current organization.
	orgOut, err := kubectlExec(ctx, podName,
		"curl", "-s", "-f",
		"http://localhost:"+infisicalPort+PathCurrentOrganization,
		"-H", "Authorization: Bearer "+adminJWT,
	)
	if err != nil {
		return nil, fmt.Errorf("get current organization: %w", err)
	}
	var orgResp struct {
		Organizations []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"organizations"`
	}
	if err := json.Unmarshal([]byte(orgOut), &orgResp); err != nil {
		return nil, fmt.Errorf("decode current organization: %w\nbody: %s", err, orgOut)
	}
	if len(orgResp.Organizations) == 0 || orgResp.Organizations[0].ID == "" {
		return nil, fmt.Errorf("current organization has no ID: %s", orgOut)
	}
	orgID := orgResp.Organizations[0].ID
	orgName := orgResp.Organizations[0].Name

	// 3. Select the organization to get an org-scoped JWT. The V3 login
	// returns a user JWT without an org context. Org-scoped endpoints
	// (e.g., GET /api/v1/identities) require the org ID to be embedded
	// in the token — without it they return 401/422.
	selectOut, err := kubectlExec(ctx, podName,
		"curl", "-s", "-f", "-X", "POST",
		"http://localhost:"+infisicalPort+PathAuthSelectOrgV3,
		"-H", "Content-Type: application/json",
		"-H", "Authorization: Bearer "+adminJWT,
		"-d", fmt.Sprintf(`{"organizationId":"%s"}`, orgID),
	)
	if err != nil {
		return nil, fmt.Errorf("select organization failed: %w", err)
	}
	var selectResp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(selectOut), &selectResp); err != nil {
		return nil, fmt.Errorf("decode select organization response: %w\nbody: %s", err, selectOut)
	}
	if selectResp.Token == "" {
		return nil, fmt.Errorf("select organization returned no token: %s", selectOut)
	}
	orgJWT := selectResp.Token

	// 4. Find the Instance Admin Identity using the org-scoped JWT.
	identitiesOut, err := kubectlExec(ctx, podName,
		"curl", "-s", "-f",
		"http://localhost:"+infisicalPort+fmt.Sprintf(PathIdentitiesByOrg, orgID),
		"-H", "Authorization: Bearer "+orgJWT,
	)
	if err != nil {
		return nil, fmt.Errorf("list identities: %w", err)
	}
	var identitiesResp struct {
		Identities []struct {
			Identity struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"identity"`
		} `json:"identities"`
	}
	if err := json.Unmarshal([]byte(identitiesOut), &identitiesResp); err != nil {
		return nil, fmt.Errorf("decode identities: %w\nbody: %s", err, identitiesOut)
	}
	var adminIdentityID string
	for _, item := range identitiesResp.Identities {
		if item.Identity.Name == "Instance Admin Identity" {
			adminIdentityID = item.Identity.ID
			break
		}
	}
	if adminIdentityID == "" {
		return nil, fmt.Errorf("Instance Admin Identity not found in org %s; existing bootstrap may be from a different schema", orgID)
	}

	result := &bootstrapOutput{}
	result.Identity.ID = adminIdentityID
	result.Identity.Name = "Instance Admin Identity"
	result.Identity.Credentials.Token = orgJWT
	result.Organization.ID = orgID
	result.Organization.Name = orgName
	return result, nil
}

// stripCLIBanner strips CLI update-notification banners ("A new release...") from
// the output. Returns the substring starting at the first '{', or empty string if
// no JSON body is found.
func stripCLIBanner(output string) string {
	idx := strings.Index(output, "{")
	if idx < 0 {
		return ""
	}
	return output[idx:]
}

// hostKubectl runs kubectl on the host machine (not inside a pod).
// Used for reading cluster resources like ConfigMaps that are not
// accessible from within the Infisical pod.

// kubeconfigPath is the explicit kubeconfig every kubectl invocation in this package
// uses.
//
// These call sites shell out to kubectl rather than using client-go, and they used
// to run it bare — relying on an ambient KUBECONFIG or ~/.kube/config. The Go-client
// health checks in the same phase take an explicit path, so the two disagreed
// whenever the caller had not exported KUBECONFIG: readiness passed against the hub
// while kubectl fell back to localhost:8080 and the phase died with
//
//	cannot find Infisical pod: ... dial tcp [::1]:8080: connect: connection refused
//
// which reads like Infisical is unreachable rather than like a missing flag.
var kubeconfigPath string

// SetKubeconfig makes this package's kubectl calls target the same cluster the
// caller's client-go is using. Safe to leave unset: the calls then behave as before
// and fall back to the ambient configuration.
func SetKubeconfig(path string) { kubeconfigPath = path }

// withKubeconfig prefixes --kubeconfig when one has been set.
func withKubeconfig(args ...string) []string {
	if kubeconfigPath == "" {
		return args
	}
	return append([]string{"--kubeconfig", kubeconfigPath}, args...)
}

func hostKubectl(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", withKubeconfig(args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kubectl failed: %w\noutput: %s", err, string(output))
	}
	return strings.TrimSpace(string(output)), nil
}

// getCachedProjectID reads project IDs from the hub-bootstrap-config
// ConfigMap that was populated during the first successful bootstrap.
// On re-run the ConfigMap persists, so we can skip the API project
// listing (which the user JWT cannot do — see grantOrgAdminRole docs).
// invalidateStaleBootstrapCache clears hub-bootstrap-config when it describes a
// different Infisical instance than the one now running.
//
// The cached project IDs exist so a RESUMED bootstrap does not recreate projects
// that are already there, which is correct and must be preserved — clearing them
// unconditionally would recreate the org's projects on every run. What is not safe
// is trusting them across instances.
//
// When infisical-db is rebuilt, restored, or re-bootstrapped, Infisical comes up
// with a new organization and no projects, while the ConfigMap (applied from the
// git-tracked ADR-045 artifact) still advertises the old ones. The bootstrap then
// believes hub-platform and hub-secrets exist, skips creating them, and fails
// several steps later with "create identity returned empty ID" — an error that
// names the symptom and points at the wrong component entirely.
//
// Comparing the organization ID catches that with data already in hand: no extra
// API call, and one comparison covers every ID the cache holds.
// staleCacheSuppressed is set once the cached bootstrap IDs are found to belong to a
// DIFFERENT Infisical organization than the live one. Both cache readers below honour
// it for the rest of the process.
//
// Patching the ConfigMap is NOT sufficient on its own, and relying on it was a real
// bug: hub-bootstrap-config is delivered by GitOps (03-platform-services ->
// manifests/environments/<env>/…/hub-bootstrap-config-patch.yaml), so ArgoCD restores
// the committed values within seconds of the patch. Every later read in the same run
// then gets the dead IDs back, and the bootstrap looks for projects in an
// organization that no longer exists — surfacing much later as
// "identity 'hub-platform-eso' not found in org <new-org>".
//
// Suppressing in-process is what actually holds, and it costs nothing: the correct
// IDs are written to the ConfigMap and to the generated artifact later in this same
// bootstrap, which is what heals Git.
var staleCacheSuppressed atomic.Bool

func invalidateStaleBootstrapCache(ctx context.Context, liveOrgID string) {
	cachedOrgID := getCachedConfigValue(ctx, "INFISICAL_ORGANIZATION_ID")
	cachedProject := getCachedConfigValue(ctx, "INFISICAL_PROJECT_ID")
	cachedSecrets := getCachedConfigValue(ctx, "INFISICAL_SECRETS_PROJECT_ID")
	orphanedProjects := cachedOrgID == "" && (cachedProject != "" || cachedSecrets != "")

	switch {
	case cachedOrgID == liveOrgID:
		return // cache belongs to this instance — keep it
	case cachedOrgID == "" && !orphanedProjects:
		return // nothing cached at all
	case orphanedProjects:
		// Project IDs with no organization to anchor them. Written by an older CLI,
		// or a partially-cleared ConfigMap. They cannot be validated against
		// anything, so they cannot be trusted.
		fmt.Println("[infisical-bootstrap] ⚠️  Cached project IDs have no organization ID to validate against.")
	}

	if cachedOrgID != "" {
		fmt.Printf("[infisical-bootstrap] ⚠️  Cached bootstrap state belongs to organization %s, but this instance is %s.\n",
			cachedOrgID, liveOrgID)
		fmt.Println("[infisical-bootstrap]     The database was rebuilt or restored; the cached project IDs no longer exist.")
	}
	fmt.Println("[infisical-bootstrap]     Clearing hub-bootstrap-config so projects and identities are recreated.")

	// Set BEFORE the patch: the patch can be reverted by ArgoCD, this cannot.
	staleCacheSuppressed.Store(true)

	// Blank the IDs rather than deleting the ConfigMap: other keys (CLUSTER_ID,
	// the environment slug) are unrelated to Infisical and are still valid. The
	// generated artifact on disk is rewritten wholesale later in this phase, and
	// the adr045 commit step publishes it.
	patch := `{"data":{"INFISICAL_ORGANIZATION_ID":"","INFISICAL_PROJECT_ID":"","INFISICAL_PROJECT_SLUG":"","INFISICAL_SECRETS_PROJECT_ID":"","INFISICAL_SECRETS_PROJECT_SLUG":""}}`
	if _, err := hostKubectl(ctx,
		"patch", "configmap", "-n", constants.NamespaceOps, "hub-bootstrap-config",
		"--type", "merge", "-p", patch,
	); err != nil {
		// Not fatal: a failure here means the cache stays, and the project
		// creation below will surface the real problem instead.
		fmt.Printf("[infisical-bootstrap] ⚠️  could not clear hub-bootstrap-config: %v\n", err)
		return
	}
	fmt.Println("[infisical-bootstrap] ✓ Stale bootstrap cache cleared")
}

// getCachedConfigValue reads one key from hub-bootstrap-config, returning "" when
// the ConfigMap or key is absent.
func getCachedConfigValue(ctx context.Context, key string) string {
	// Slugs are static across instances and stay readable; only the UUIDs identify
	// one particular Infisical, and those are what must not be trusted after the
	// organization fingerprint changes.
	if staleCacheSuppressed.Load() {
		switch key {
		case "INFISICAL_ORGANIZATION_ID", "INFISICAL_PROJECT_ID", "INFISICAL_SECRETS_PROJECT_ID":
			return ""
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := hostKubectl(ctx,
		"get", "configmap", "-n", constants.NamespaceOps, "hub-bootstrap-config",
		"-o", "jsonpath={.data."+key+"}",
	)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func getCachedProjectID(slug string) string {
	// This reader bypasses getCachedConfigValue and hits the ConfigMap directly, so
	// it needs the same guard — it is the one that logs "Found cached project …".
	if staleCacheSuppressed.Load() {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	key := "INFISICAL_PROJECT_ID"
	if slug == SecretsProjectSlug {
		key = "INFISICAL_SECRETS_PROJECT_ID"
	}

	out, err := hostKubectl(ctx,
		"get", "configmap", "-n", constants.NamespaceOps, "hub-bootstrap-config",
		"-o", "jsonpath={.data."+key+"}",
	)
	if err != nil {
		return ""
	}
	return out
}

// kubectlExec runs a command inside the Infisical pod via `kubectl exec`.
//
// Retry on exit code 7 (curl "connection refused"): the pod may be
// Running (phase=Running, deletionTimestamp absent) but the Node.js
// server inside has not yet bound to port 8080. This happens when a
// secret change triggers a Reloader-style rolling restart — the old pod
// is terminating and the new pod is in its startup window. We retry up
// to 30 times with a 2s back-off (60s total) before surfacing the error.

// curlAPI runs a curl inside the Infisical pod and returns the response body along
// with its HTTP status.
//
// The plain kubectlExec + "curl -s" pattern throws the status away. An API error
// then arrives as a perfectly valid JSON error object, unmarshals into the success
// struct leaving every field zero, and is reported as "returned empty ID" — an
// error that describes the parse result and hides the cause. Every diagnosis of
// such a failure has had to start by guessing whether it was 400, 401 or 500.
//
// The status is appended by curl on its own line and split off here, so callers can
// report both. Body and status are returned even on non-2xx: that is precisely when
// they are worth printing.
func curlAPI(ctx context.Context, podName string, args ...string) (body string, status int, err error) {
	full := append([]string{"curl", "-s", "-w", "\n%{http_code}"}, args...)
	out, err := kubectlExec(ctx, podName, full...)
	if err != nil {
		return "", 0, err
	}
	body, status = splitBodyStatus(out)
	return body, status, nil
}

// splitBodyStatus separates the response body from the trailing status line that
// curl -w appends. Kept separate from the exec so it can be tested directly.
func splitBodyStatus(out string) (string, int) {
	trimmed := strings.TrimRight(out, "\n")
	idx := strings.LastIndex(trimmed, "\n")
	if idx < 0 {
		if code, err := strconv.Atoi(strings.TrimSpace(trimmed)); err == nil {
			return "", code
		}
		return trimmed, 0
	}
	code, err := strconv.Atoi(strings.TrimSpace(trimmed[idx+1:]))
	if err != nil {
		return trimmed, 0
	}
	return trimmed[:idx], code
}

// apiError renders an API failure with the detail needed to act on it, truncating a
// long body so a stack of retries stays readable.
func apiError(op string, status int, body string) error {
	b := strings.TrimSpace(body)
	if len(b) > 400 {
		b = b[:400] + "…"
	}
	return fmt.Errorf("%s: HTTP %d, response: %s", op, status, b)
}

func kubectlExec(ctx context.Context, podName string, args ...string) (string, error) {
	cmdArgs := append([]string{
		"exec", "-n", infisicalNamespace, "pod/" + podName,
		"-c", "infisical-standalone-infisical",
		"--",
	}, args...)

	const (
		maxRetries = 30
		retryDelay = 2 * time.Second
		// curl exit code 7 = "Failed to connect to host or proxy"
		curlExitConnectionRefused = 7
	)

	var (
		output []byte
		err    error
	)
	for attempt := 1; attempt <= maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		cmd := exec.CommandContext(ctx, "kubectl", withKubeconfig(cmdArgs...)...)
		output, err = cmd.CombinedOutput()
		if err == nil {
			return strings.TrimSpace(string(output)), nil
		}

		// Detect curl exit-7 (connection refused) embedded in the output.
		// kubectl wraps it as "exit status 7" in the error string.
		outStr := string(output)
		if strings.Contains(outStr, "exit code 7") ||
			strings.Contains(outStr, "exit status 7") {
			fmt.Printf("[kubectlExec] Port 8080 not ready yet (curl exit 7), waiting %s (attempt %d/%d)...\n",
				retryDelay, attempt, maxRetries)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(retryDelay):
			}
			continue
		}

		// Any other error is surfaced immediately.
		return "", fmt.Errorf("kubectl exec failed: %w\noutput: %s", err, outStr)
	}
	return "", fmt.Errorf("kubectl exec failed after %d retries (port 8080 never became ready): last output: %s", maxRetries, string(output))
}

func GetInfisicalPodName(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", withKubeconfig("get", "pods", "-n", infisicalNamespace,
		"-l", "app=infisical-standalone,component=infisical",
		"--field-selector", "status.phase=Running",
		"-o", "go-template={{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{\"\\n\"}}{{end}}{{end}}")...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get infisical pod name: %w\noutput: %s", err, string(output))
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return "", fmt.Errorf("no running infisical pod found in namespace %s", infisicalNamespace)
	}
	return strings.TrimSpace(lines[0]), nil
}

// runBootstrapCLI calls infisical bootstrap and returns the JSON output.
//
// If the server is already bootstrapped (banner-only output, no JSON
// body), the function returns errAlreadyBootstrapped so the caller can
// recover the existing org/identity state via getExistingOrgData.
// This used to destructively DELETE users, projects, and Redis state
// and re-bootstrap from scratch — that path orphaned any in-flight
// cert-operator reconcile (which had cached the old project IDs at
// startup) and is no longer invoked.
func runBootstrapCLI(ctx context.Context, podName string) (string, error) {
	output, err := kubectlExec(ctx, podName,
		"infisical", "bootstrap",
		"--email", adminEmail(),
		"--password", adminPassword(),
		"--organization", orgName,
		"--domain", "http://localhost:"+infisicalPort,
		"--ignore-if-bootstrapped",
		"--output", "json",
	)
	if err != nil {
		return "", fmt.Errorf("infisical bootstrap failed: %w", err)
	}

	jsonOutput := stripCLIBanner(output)
	if jsonOutput != "" {
		return jsonOutput, nil
	}

	// Banner-only output means the server is already bootstrapped.
	// Surface this as a sentinel error — the caller (runBootstrap /
	// BootstrapInfisicalDayZero) will recover the existing org/identity
	// state via getExistingOrgData without destroying anything.
	return "", errAlreadyBootstrapped
}

func runBootstrap(ctx context.Context, podName string) (*bootstrapOutput, error) {
	if cachedBootstrap != nil {
		fmt.Println("[infisical-bootstrap] Using cached bootstrap result")
		return cachedBootstrap, nil
	}

	jsonOutput, err := runBootstrapCLI(ctx, podName)
	if errors.Is(err, errAlreadyBootstrapped) {
		fmt.Println("[infisical-bootstrap] Server already bootstrapped, recovering existing org/identity via API...")
		existing, getErr := getExistingOrgData(ctx, podName)
		if getErr != nil {
			return nil, fmt.Errorf("recover existing bootstrap: %w", getErr)
		}
		cachedBootstrap = existing
		fmt.Printf("[infisical-bootstrap] Re-using existing bootstrap: org=%s (%s), identity=%s\n",
			existing.Organization.Name, existing.Organization.ID, existing.Identity.ID)
		return cachedBootstrap, nil
	}
	if err != nil {
		return nil, err
	}

	var result bootstrapOutput
	if err := json.Unmarshal([]byte(jsonOutput), &result); err != nil {
		return nil, fmt.Errorf("failed to parse infisical bootstrap output: %w\noutput: %s", err, jsonOutput)
	}

	if result.Identity.Credentials.Token == "" {
		return nil, fmt.Errorf("infisical bootstrap returned no identity token: %s", jsonOutput)
	}
	if result.Organization.ID == "" {
		return nil, fmt.Errorf("infisical bootstrap returned no organization ID: %s", jsonOutput)
	}

	fmt.Printf("[infisical-bootstrap] Bootstrap complete: org=%s (%s)\n", result.Organization.Name, result.Organization.ID)

	// The token `infisical bootstrap` hands back is the Instance Admin Identity's
	// access token, and it is created with accessTokenTTL/accessTokenMaxTTL = 0 —
	// i.e. with NO `exp` claim. Current Infisical treats a machine-identity token
	// without `exp` as a legacy token and rejects it once the enforcement window
	// has passed:
	//
	//	HTTP 401 {"message":"Identity access token exceeded max age, please re-authenticate"}
	//
	// It is therefore unusable for the API calls that follow, and the failure is not
	// obvious: it looks like a permissions problem on a brand-new organization.
	//
	// Exchange it for the same org-scoped USER JWT the already-bootstrapped path
	// obtains via getExistingOrgData (admin login -> select organization). That path
	// has always worked, which is why a re-run against an existing instance
	// succeeded while a first run on a fresh database failed at the first
	// createProject.
	existing, err := getExistingOrgData(ctx, podName)
	if err != nil {
		return nil, fmt.Errorf("bootstrap succeeded but could not obtain a usable org-scoped token: %w", err)
	}
	if existing.Organization.ID != result.Organization.ID {
		return nil, fmt.Errorf("post-bootstrap login resolved organization %s, expected %s",
			existing.Organization.ID, result.Organization.ID)
	}

	cachedBootstrap = existing
	return cachedBootstrap, nil
}

func createProject(ctx context.Context, podName, adminJWT, projectSlug, projectType string) (string, string, error) {
	payload := map[string]any{
		"projectName": projectSlug,
		"slug":        projectSlug,
		"type":        projectType,
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal create project request: %w", err)
	}
	body := string(bodyBytes)
	output, status, err := curlAPI(ctx, podName,
		"-X", "POST",
		"http://localhost:"+infisicalPort+PathProjects,
		"-H", "Content-Type: application/json",
		"-H", "Authorization: Bearer "+adminJWT,
		"-d", body,
	)
	if err != nil {
		return "", "", fmt.Errorf("create project failed: %w", err)
	}
	if status < 200 || status >= 300 {
		// Report the real failure rather than letting the error body unmarshal into
		// an empty struct and resurface as "returned empty ID".
		return "", "", apiError(fmt.Sprintf("create project %q", projectSlug), status, output)
	}

	// 409 Conflict means project already exists — reconcile by fetching its ID
	var projectResp createProjectResponse
	if err := json.Unmarshal([]byte(output), &projectResp); err != nil {
		return "", "", fmt.Errorf("failed to parse create project response: %w\nresponse: %s", err, output)
	}
	if projectResp.Project.ID == "" {
		// Could be a 409 — try listing projects to find the slug
		listOut, listErr := kubectlExec(ctx, podName,
			"curl", "-s",
			"http://localhost:"+infisicalPort+PathProjects,
			"-H", "Authorization: Bearer "+adminJWT,
		)
		if listErr != nil {
			return "", "", fmt.Errorf("create project ambiguous (empty ID) and list failed: %w", listErr)
		}
		var listResp struct {
			Projects []struct {
				ID   string `json:"id"`
				Slug string `json:"slug"`
			} `json:"projects"`
		}
		if err := json.Unmarshal([]byte(listOut), &listResp); err != nil {
			return "", "", fmt.Errorf("create project ambiguous (empty ID) and list parse failed: %w\ndata: %s", err, listOut)
		}
		for _, p := range listResp.Projects {
			if p.Slug == projectSlug {
				fmt.Printf("[infisical-bootstrap] Project already exists: %s (id=%s)\n", p.Slug, p.ID)
				return p.ID, p.Slug, nil
			}
		}

		// Project listing returned empty (user JWT may not have project
		// membership). Check the hub-bootstrap-config ConfigMap which
		// was populated during the first successful bootstrap run.
		if cachedID := getCachedProjectID(projectSlug); cachedID != "" {
			fmt.Printf("[infisical-bootstrap] Found cached project %s (id=%s) in hub-bootstrap-config\n", projectSlug, cachedID)
			return cachedID, projectSlug, nil
		}

		return "", "", fmt.Errorf("create project returned empty ID and project '%s' not found in listing or ConfigMap", projectSlug)
	}

	fmt.Printf("[infisical-bootstrap] Project created: %s (id=%s)\n", projectResp.Project.Slug, projectResp.Project.ID)
	return projectResp.Project.ID, projectResp.Project.Slug, nil
}

func createMachineIdentity(ctx context.Context, podName, adminJWT, orgID, projectID string) (string, string, string, error) {
	// 1. Create the Machine Identity
	body := fmt.Sprintf(`{"name":"%s","organizationId":"%s"}`, identityName, orgID)
	output, status, err := curlAPI(ctx, podName,
		"-X", "POST",
		"http://localhost:"+infisicalPort+PathIdentities,
		"-H", "Content-Type: application/json",
		"-H", "Authorization: Bearer "+adminJWT,
		"-d", body,
	)
	if err != nil {
		return "", "", "", fmt.Errorf("create identity failed: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", "", "", apiError("create machine identity", status, output)
	}

	var identityResp createIdentityResponse
	if err := json.Unmarshal([]byte(output), &identityResp); err != nil {
		return "", "", "", fmt.Errorf("failed to parse create identity response: %w\nresponse: %s", err, output)
	}
	identityID := identityResp.Identity.ID
	if identityID == "" {
		// Identity may already exist — try finding it by name
		found, findErr := findIdentityByName(ctx, podName, adminJWT, identityName, orgID)
		if findErr != nil {
			return "", "", "", fmt.Errorf("create identity returned empty ID and lookup failed: %w", findErr)
		}
		identityID = found
	}
	fmt.Printf("[infisical-bootstrap] Machine Identity: id=%s\n", identityID)

	// 2. Attach Universal Auth (correct path: /api/v1/auth/universal-auth/identities/{id})
	uaBody := `{"clientSecretTrustedIps":[{"ipAddress":"0.0.0.0/0","type":"ipv4"},{"ipAddress":"::/0","type":"ipv6"}],"accessTokenTrustedIps":[{"ipAddress":"0.0.0.0/0","type":"ipv4"},{"ipAddress":"::/0","type":"ipv6"}],"accessTokenTTL":2592000,"accessTokenMaxTTL":2592000}`
	uaURL := "http://localhost:" + infisicalPort + fmt.Sprintf(PathAuthUniversalAuthIdentities, identityID)
	uaOutput, uaErr := kubectlExec(ctx, podName,
		"curl", "-s", "-X", "POST",
		uaURL,
		"-H", "Content-Type: application/json",
		"-H", "Authorization: Bearer "+adminJWT,
		"-d", uaBody,
	)
	if uaErr != nil {
		return "", "", "", fmt.Errorf("attach universal auth failed: %w", uaErr)
	}

	var authResp attachUniversalAuthResponse
	if err := json.Unmarshal([]byte(uaOutput), &authResp); err != nil {
		return "", "", "", fmt.Errorf("failed to parse universal auth response: %w\nresponse: %s", err, uaOutput)
	}
	clientID := authResp.IdentityUniversalAuth.ClientID
	if clientID == "" {
		// UA may already be configured — try fetching it
		existingUA, getErr := getUniversalAuth(ctx, podName, adminJWT, identityID)
		if getErr != nil {
			return "", "", "", fmt.Errorf("attach universal auth returned empty clientId and GET failed: %w", getErr)
		}
		clientID = existingUA
	}
	fmt.Printf("[infisical-bootstrap] Universal Auth: clientId=%s\n", clientID)

	// 3. Generate client secret (correct path: /api/v1/auth/universal-auth/identities/{id}/client-secrets)
	csURL := "http://localhost:" + infisicalPort + fmt.Sprintf(PathAuthUniversalAuthClientSecrets, identityID)
	csOutput, err := kubectlExec(ctx, podName,
		"curl", "-s", "-X", "POST",
		csURL,
		"-H", "Content-Type: application/json",
		"-H", "Authorization: Bearer "+adminJWT,
		"-d", `{"numUsesLimit":0,"ttl":0}`,
	)
	if err != nil {
		return "", "", "", fmt.Errorf("generate client secret failed: %w", err)
	}

	var secretResp createClientSecretResponse
	if err := json.Unmarshal([]byte(csOutput), &secretResp); err != nil {
		return "", "", "", fmt.Errorf("failed to parse client secret response: %w\nresponse: %s", err, csOutput)
	}
	clientSecret := secretResp.ClientSecret
	if clientSecret == "" {
		return "", "", "", fmt.Errorf("generate client secret returned empty: %s", csOutput)
	}
	fmt.Printf("[infisical-bootstrap] Client secret generated\n")

	// 4. Grant project admin role — HARD failure. Without this the
	// Machine Identity cannot read/write the cert project.
	if err := grantProjectAdminRole(ctx, podName, adminJWT, projectID, identityID); err != nil {
		return "", "", "", fmt.Errorf("grant project admin: %w", err)
	}

	// 5. Grant org-level admin role — HARD failure. The cert-operator
	// needs this to create Machine Identities for spokes on a different
	// org, which requires `identity:create` at the org scope. Previously
	// this was logged-as-success with curl -s, hiding the 403.
	//
	// NOTE: grantOrgAdminRole logs in as the admin user (email/password)
	// and uses a user-level JWT — not the adminJWT passed in here, which
	// is the Instance Admin Identity token on first run. Machine Identity
	// tokens cannot manage org-level role assignments in Infisical 0.43.x
	// (new privilege system: only user sessions can PATCH identity
	// memberships at the org scope).
	if err := grantOrgAdminRole(ctx, podName, orgID, identityID); err != nil {
		return "", "", "", fmt.Errorf("grant org admin: %w", err)
	}

	return clientID, clientSecret, identityID, nil
}

// grantProjectAdminRole adds an identity to a project with the admin role.
func grantProjectAdminRole(ctx context.Context, podName, adminJWT, projectID, identityID string) error {
	grantURL := "http://localhost:" + infisicalPort + fmt.Sprintf(PathProjectMembershipsIdentities, projectID, identityID)
	output, err := kubectlExec(ctx, podName,
		"curl", "-s", "-X", "POST",
		grantURL,
		"-H", "Content-Type: application/json",
		"-H", "Authorization: Bearer "+adminJWT,
		"-d", `{"roles":[{"role":"admin"}]}`,
	)
	if err != nil {
		return fmt.Errorf("curl failed: %w", err)
	}
	if strings.Contains(output, "Conflict") || strings.Contains(output, "already exists") {
		fmt.Printf("[infisical-bootstrap] Instance Admin Identity already has project membership\n")
		return nil
	}
	fmt.Printf("[infisical-bootstrap] Instance Admin Identity granted admin role on project\n")
	return nil
}

// grantOrgAdminRole adds an identity to the organization with the admin role.
// Required for cert-operator to create Machine Identities for spokes in any
// project within the org. Without this, `POST /api/v1/identities` returns 403
// "You are not allowed to create on identity".
//
// This function logs in as the well-known admin user (email/password) to
// obtain a user-level JWT, then selects the org to get an org-scoped JWT
// (the V3 login JWT has no org context — org-scoped endpoints return 401
// without it), then calls PATCH on the org identity-memberships endpoint.
// Three things make this necessary in Infisical 0.43.x:
//
//  1. The endpoint requires PATCH, not POST. POST returns 403
//     "Parent organization cannot do this operation" (new privilege system,
//     shouldUseNewPrivilegeSystem: true).
//  2. The caller must be a user session. Machine Identity tokens — even
//     ones with role: "admin" at the org scope — get 403 "You are not
//     allowed to access this resource". Only user JWTs (email/password
//     login) can manage org-level identity role assignments.
//  3. The user JWT must have an org context. The V3 login returns a
//     token without an org ID, so we call POST /api/v3/auth/select-organization
//     to embed the org ID before calling the PATCH endpoint.
func grantOrgAdminRole(ctx context.Context, podName, orgID, identityID string) error {
	// 1. Log in as the admin user to get a user-level JWT.
	loginPayload := fmt.Sprintf(`{"email":"%s","password":"%s"}`, adminEmail(), adminPassword())
	loginOut, err := kubectlExec(ctx, podName,
		"curl", "-s", "-f", "-X", "POST",
		"http://localhost:"+infisicalPort+PathAuthLoginV3,
		"-H", "Content-Type: application/json",
		"-d", loginPayload,
	)
	if err != nil {
		return fmt.Errorf("admin user login failed: %w", err)
	}
	var loginResp struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal([]byte(loginOut), &loginResp); err != nil {
		return fmt.Errorf("decode admin user login: %w\nbody: %s", err, loginOut)
	}
	if loginResp.AccessToken == "" {
		return fmt.Errorf("admin user login returned no accessToken: %s", loginOut)
	}
	userJWT := loginResp.AccessToken

	// 2. Select the organization to get an org-scoped JWT. Without this,
	// the PATCH below returns 401 "no organization found in request".
	selectOut, err := kubectlExec(ctx, podName,
		"curl", "-s", "-f", "-X", "POST",
		"http://localhost:"+infisicalPort+PathAuthSelectOrgV3,
		"-H", "Content-Type: application/json",
		"-H", "Authorization: Bearer "+userJWT,
		"-d", fmt.Sprintf(`{"organizationId":"%s"}`, orgID),
	)
	if err != nil {
		return fmt.Errorf("select organization failed: %w", err)
	}
	var selectResp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(selectOut), &selectResp); err != nil {
		return fmt.Errorf("decode select organization response: %w\nbody: %s", err, selectOut)
	}
	if selectResp.Token == "" {
		return fmt.Errorf("select organization returned no token: %s", selectOut)
	}
	orgJWT := selectResp.Token

	// 3. Grant org-level admin role via PATCH using the org-scoped JWT.
	grantURL := "http://localhost:" + infisicalPort + fmt.Sprintf(PathOrgIdentityMemberships, identityID)
	output, err := kubectlExec(ctx, podName,
		"curl", "-s", "-f", "-X", "PATCH",
		grantURL,
		"-H", "Content-Type: application/json",
		"-H", "Authorization: Bearer "+orgJWT,
		"-d", `{"roles":[{"role":"admin","isTemporary":false}]}`,
	)
	if err != nil {
		return fmt.Errorf("curl failed: %w", err)
	}
	if strings.Contains(output, "Conflict") || strings.Contains(output, "already exists") {
		fmt.Printf("[infisical-bootstrap] Identity already has org admin role\n")
		return nil
	}
	fmt.Printf("[infisical-bootstrap] Identity granted org admin role: %s\n", strings.TrimSpace(output))
	return nil
}

// findIdentityByName looks up a Machine Identity by name within the organization.
//
// API shape verified against live cluster (Infisical v0.160.11):
//
//	GET /api/v1/identities?orgId=<orgId>&limit=100
//	→ {
//	    "identities": [{
//	      "id":         "<membership-uuid>",  // org membership ID — NOT the identity ID
//	      "identityId": "<identity-uuid>",    // the real identity UUID
//	      "identity": {
//	        "id":   "<identity-uuid>",
//	        "name": "hub-platform-eso",
//	        ...
//	      }
//	    }],
//	    "totalCount": N
//	  }
//
// The outer `id` is the org membership UUID. Use `identity.id` (== `identityId`)
// as the identity UUID for subsequent API calls (attach-auth, grant-role, etc.).
func findIdentityByName(ctx context.Context, podName, adminJWT, name, orgID string) (string, error) {
	output, err := kubectlExec(ctx, podName,
		"curl", "-s",
		fmt.Sprintf("http://localhost:%s%s?limit=100&orgId=%s", infisicalPort, PathIdentities, orgID),
		"-H", "Authorization: Bearer "+adminJWT,
	)
	if err != nil {
		return "", fmt.Errorf("list identities failed: %w", err)
	}

	// Each element has a nested `identity` object containing the real id/name,
	// plus a top-level `identityId` which equals identity.id.
	// The outer `id` is the org membership UUID — do NOT use it.
	var listResp struct {
		Identities []struct {
			IdentityID string `json:"identityId"`
			Identity   struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"identity"`
		} `json:"identities"`
	}
	if err := json.Unmarshal([]byte(output), &listResp); err != nil {
		return "", fmt.Errorf("parse identities list: %w\nresponse: %s", err, output)
	}

	for _, item := range listResp.Identities {
		if item.Identity.Name == name {
			return item.Identity.ID, nil
		}
	}
	return "", fmt.Errorf("identity '%s' not found in org %s", name, orgID)
}

// getUniversalAuth retrieves the clientId from the Universal Auth configuration.
func getUniversalAuth(ctx context.Context, podName, adminJWT, identityID string) (string, error) {
	uaURL := "http://localhost:" + infisicalPort + fmt.Sprintf(PathAuthUniversalAuthIdentities, identityID)
	output, err := kubectlExec(ctx, podName,
		"curl", "-s",
		uaURL,
		"-H", "Authorization: Bearer "+adminJWT,
	)
	if err != nil {
		return "", fmt.Errorf("get universal auth failed: %w", err)
	}

	var uaResp struct {
		IdentityUniversalAuth struct {
			ClientID string `json:"clientId"`
		} `json:"identityUniversalAuth"`
	}
	if err := json.Unmarshal([]byte(output), &uaResp); err != nil {
		return "", fmt.Errorf("parse universal auth: %w\nresponse: %s", err, output)
	}
	if uaResp.IdentityUniversalAuth.ClientID == "" {
		return "", fmt.Errorf("universal auth has no clientId: %s", output)
	}
	return uaResp.IdentityUniversalAuth.ClientID, nil
}

// GetBootstrapToken re-runs the idempotent infisical bootstrap CLI and returns
// the admin identity token + organization ID. Call this when you need a fresh
// admin-scoped token for post-bootstrap health gates (e.g., cert-manager profile check).
// The bootstrap is a no-op if already bootstrapped (--ignore-if-bootstrapped).
func GetBootstrapToken(ctx context.Context, podName string) (adminJWT, orgID string, err error) {
	boot, err := runBootstrap(ctx, podName)
	if err != nil {
		return "", "", err
	}
	return boot.Identity.Credentials.Token, boot.Organization.ID, nil
}

// FindProjectBySlug lists all Infisical projects and returns the UUID of the
// one matching the given slug. Uses admin bootstrap token for auth.
func FindProjectBySlug(ctx context.Context, podName, adminJWT, slug string) (string, error) {
	output, err := kubectlExec(ctx, podName,
		"curl", "-s",
		"http://localhost:"+infisicalPort+PathProjects,
		"-H", "Authorization: Bearer "+adminJWT,
	)
	if err != nil {
		return "", fmt.Errorf("list projects failed: %w", err)
	}
	var listResp struct {
		Projects []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"projects"`
	}
	if err := json.Unmarshal([]byte(output), &listResp); err != nil {
		return "", fmt.Errorf("parse projects list: %w\nresponse: %s", err, output)
	}
	for _, p := range listResp.Projects {
		if p.Slug == slug {
			fmt.Printf("[infisical-bootstrap] Found project '%s' (id=%s)\n", p.Slug, p.ID)
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("project '%s' not found", slug)
}

// CheckCertificateProfile verifies that a cert-manager profile exists in Infisical.
// The projectId query parameter is required by Infisical OSS to scope the lookup
// (source: certificate-profiles-router.ts:461).
func CheckCertificateProfile(ctx context.Context, podName, adminJWT, projectID, slug string) (bool, error) {
	profilePath := fmt.Sprintf(PathCertificateProfilesBySlug, slug, projectID)
	profileURL := "http://localhost:" + infisicalPort + profilePath
	output, err := kubectlExec(ctx, podName,
		"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
		profileURL,
		"-H", "Authorization: Bearer "+adminJWT,
	)
	if err != nil {
		return false, fmt.Errorf("check profile %s: %w", slug, err)
	}
	code := strings.TrimSpace(output)
	if code == "200" {
		return true, nil
	}
	// 403 means the caller lacks project membership but does not
	// guarantee the profile is absent. On re-run the user JWT
	// (org-scoped from getExistingOrgData) does not have project
	// membership — only the Instance Admin Identity token does.
	// The profile was created during the first bootstrap, so it
	// exists. Treat 403 as "found" to skip recreate attempts.
	if code == "403" {
		return true, nil
	}
	if code == "404" {
		return false, nil
	}
	return false, fmt.Errorf("unexpected status code %s checking profile %s", code, slug)
}

// WaitForCertificateProfile polls for the existence of a cert-manager profile.
// If the profile is not found, it prints clear manual instructions for the UI step
// and retries until the timeout expires.
func WaitForCertificateProfile(ctx context.Context, podName, adminJWT, projectID, slug string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	printed := false

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		found, err := CheckCertificateProfile(ctx, podName, adminJWT, projectID, slug)
		if err != nil {
			fmt.Printf("[infisical-bootstrap] ⚠️  Profile check error (retrying): %v\n", err)
			time.Sleep(15 * time.Second)
			continue
		}
		if found {
			fmt.Printf("[infisical-bootstrap] ✓ Certificate profile '%s' found\n", slug)
			return nil
		}

		if !printed {
			fmt.Println()
			fmt.Println("╔══════════════════════════════════════════════════════════════════════════╗")
			fmt.Println("║  MANUAL ACTION REQUIRED:  Create the 'argocd-bootstrap' profile         ║")
			fmt.Println("╚══════════════════════════════════════════════════════════════════════════╝")
			fmt.Println()
			fmt.Println("  1. Port-forward Infisical:")
			fmt.Println("     kubectl port-forward -n platform-security \\")
			fmt.Println("       svc/infisical-standalone-infisical 8080:8080")
			fmt.Println()
			fmt.Println("  2. Open http://localhost:8080 in your browser")
			fmt.Printf("  3. Log in as %s with the password this box was bootstrapped with\n", adminEmail())
			fmt.Println("  4. Navigate to: Certificates → Profiles → Create Profile")
			fmt.Println("  5. Slug: argocd-bootstrap")
			fmt.Println("  6. CA: Fleet Intermediate CA (NOT infisical-db-ca — per ADR-035)")
			fmt.Println("  7. Validity: 10 years")
			fmt.Println("  8. Click 'Create'")
			fmt.Println()
			fmt.Println("  Waiting for profile to appear... (timeout: " + timeout.Round(time.Second).String() + ")")
			printed = true
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}

	return fmt.Errorf("timeout after %v waiting for certificate profile '%s' — create it in the Infisical UI, then re-run 'hub init-secrets'", timeout, slug)
}

// declaredManagementScope returns the Infisical environment this box declares for
// its OWN credentials, read from hub-bootstrap-config (ADR-111 Revision 7 §2).
//
// The same key GetInfisicalConfig reads, deliberately: the writer that creates the
// environment and the writers that put secrets in it must agree, and they agree by
// reading one declaration rather than by being passed the same argument twice. The
// ConfigMap is rendered by hub-environment from `management.infisicalEnvironmentSlug`,
// so this is the box's declaration as it reached the cluster -- which is also why
// this is read here rather than threaded down from the bootstrap flags: there are
// no flags for it, and `kakt init-secrets` has no gitops checkout to read.
//
// No default. The only possible default is a guess at which environment someone
// else's secrets are in, and guessing wrong is silent: the secrets resolve, from
// the wrong scope.
func declaredManagementScope(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	out, err := hostKubectl(ctx,
		"get", "configmap", "-n", constants.NamespaceOps, "hub-bootstrap-config",
		"-o", "jsonpath={.data.INFISICAL_ENVIRONMENT_SLUG}",
	)
	if err != nil {
		return "", fmt.Errorf("read the declared management scope from hub-bootstrap-config "+
			"in %s: %w\n\nIt is rendered by the hub-environment Application from the box's "+
			"management.infisicalEnvironmentSlug, so an absent ConfigMap means boundary 01 has "+
			"not synced yet -- the hub's secrets have nowhere to go until it has",
			constants.NamespaceOps, err)
	}
	scope := strings.TrimSpace(out)
	if scope == "" {
		return "", fmt.Errorf("hub-bootstrap-config carries no INFISICAL_ENVIRONMENT_SLUG: it is " +
			"the Infisical environment holding THIS HUB's own credentials (ADR-111 Revision 7 §2), " +
			"rendered from the box's declared management.infisicalEnvironmentSlug. Day-0 creates " +
			"that environment and writes the hub's secrets into it, so there is no value to assume")
	}
	return scope, nil
}

// ensureEnvironment makes one Infisical environment exist in one project.
//
// Infisical supplies a new project with its own default environments and nothing
// here asks for others, so before this existed a box could declare a scope that
// was never created -- and the failure is silent in the direction that matters:
// the write succeeds against nothing an ExternalSecret can read, every
// ClusterSecretStore resolves empty, and the hub reports Healthy while holding no
// credentials.
//
// Asks before writing, the way ensureCertificateProfiles does, so a re-run on a
// box that already has the environment performs no write at all.
func ensureEnvironment(ctx context.Context, podName, adminJWT, projectID, slug string) error {
	op := fmt.Sprintf("environment %q in project %s", slug, projectID)

	checkURL := "http://localhost:" + infisicalPort + fmt.Sprintf(PathProjectEnvironmentBySlug, projectID, slug)
	output, status, err := curlAPI(ctx, podName,
		checkURL,
		"-H", "Authorization: Bearer "+adminJWT,
	)
	if err != nil {
		return fmt.Errorf("check %s: %w", op, err)
	}
	switch {
	case status >= 200 && status < 300:
		fmt.Printf("[infisical-bootstrap] Environment present: %s (project %s)\n", slug, projectID)
		return nil
	case status == 404:
		// Absent: created below.
	default:
		// Deliberately NOT the 403-means-present reading CheckCertificateProfile
		// takes. A certificate profile is a manual UI step with a wait loop behind
		// it, so assuming presence there costs a retry; assuming it here would skip
		// creating the one object that makes every secret on the box resolvable,
		// and nothing downstream reports that.
		return apiError("check "+op, status, output)
	}

	payload, err := json.Marshal(map[string]any{
		// Infisical names an environment and slugs it separately. The scope is
		// declared as a slug, and a name that is not the slug would leave the UI
		// describing the hub's scope as something the declaration never said.
		"name": slug,
		"slug": slug,
	})
	if err != nil {
		return fmt.Errorf("marshal create %s: %w", op, err)
	}
	createURL := "http://localhost:" + infisicalPort + fmt.Sprintf(PathProjectEnvironments, projectID)
	output, status, err = curlAPI(ctx, podName,
		"-X", "POST",
		createURL,
		"-H", "Content-Type: application/json",
		"-H", "Authorization: Bearer "+adminJWT,
		"-d", string(payload),
	)
	if err != nil {
		return fmt.Errorf("create %s: %w", op, err)
	}
	if status >= 200 && status < 300 {
		fmt.Printf("[infisical-bootstrap] Environment created: %s (project %s)\n", slug, projectID)
		return nil
	}
	// Reconciled rather than reported, for the reason createProject reconciles a
	// 409: the check above and this call are two round trips, and a concurrent
	// bootstrap of the same box can win the race between them. Upstream answers a
	// duplicate slug with 400 "Environment with slug already exists" -- matched on
	// the message because 400 also carries the pending-deletion case, which is a
	// real refusal: a slug held by an environment awaiting deletion has to be
	// resolved in Infisical, not worked around here.
	if strings.Contains(output, "already exists") {
		fmt.Printf("[infisical-bootstrap] Environment present: %s (project %s)\n", slug, projectID)
		return nil
	}
	return apiError("create "+op, status, output)
}

// ensureManagementScope creates the box's declared management scope in both
// projects a hub addresses as a secret scope.
//
// Both, because both are read as one: the hub's secrets live in hub-secrets, and
// GetInfisicalConfig falls back to INFISICAL_PROJECT_SLUG for a box that carries
// only the cert project's slug. An environment missing from either project is a
// reader resolving nothing, so neither is left to chance.
func ensureManagementScope(ctx context.Context, podName, adminJWT, certProjectID, secretsProjectID string) error {
	scope, err := declaredManagementScope(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("[infisical-bootstrap] Declared management scope: %s\n", scope)
	if err := ensureEnvironment(ctx, podName, adminJWT, secretsProjectID, scope); err != nil {
		return err
	}
	return ensureEnvironment(ctx, podName, adminJWT, certProjectID, scope)
}

func BootstrapInfisicalDayZero(ctx context.Context) (*BootstrapResult, error) {
	fmt.Println("[infisical-bootstrap] Starting Infisical Day-0 bootstrap...")

	podName, err := GetInfisicalPodName(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot find Infisical pod: %w", err)
	}
	fmt.Printf("[infisical-bootstrap] Using pod: %s\n", podName)

	boot, err := runBootstrap(ctx, podName)
	if err != nil {
		return nil, err
	}

	orgID := boot.Organization.ID
	fmt.Printf("[infisical-bootstrap] Organization ID: %s\n", orgID)

	// Every cached ID below (projects, identities) names a row inside THIS
	// organization's database. The organization ID is therefore the instance
	// fingerprint: if it has changed, the whole cache describes an Infisical that
	// no longer exists and must not be trusted.
	invalidateStaleBootstrapCache(ctx, orgID)

	// The local state file is the THIRD cache (alongside hub-bootstrap-config and
	// the in-process one) and it is gitignored, so it survives a teardown on the
	// operator's machine. It used to short-circuit this whole function before the
	// live organization was known, which meant the fingerprint check above could
	// not run at all and every stale ID was returned wholesale.
	//
	// It is therefore consulted HERE, after runBootstrap has told us which
	// Infisical we are actually talking to, and only when it describes that same
	// instance. runBootstrap is safe to reach: on an already-bootstrapped server it
	// recovers the existing org and identity rather than creating anything.
	if state, err := loadBootstrapState(); err == nil {
		if state.OrgID == orgID {
			fmt.Printf("[infisical-bootstrap] Using cached state from %s (re-run)\n", stateFilePath())
			// The management scope is ensured on this path too. The cache short-circuits
			// the API calls that CREATE things, and the scope is one of them: a box
			// bootstrapped before the scope was declared, or whose declaration changed,
			// reaches this return with the environment still absent. Asking is one GET per
			// project when it is already there.
			if err := ensureManagementScope(ctx, podName, boot.Identity.Credentials.Token,
				state.ProjectID, state.SecretsProjectID); err != nil {
				return nil, err
			}
			return &BootstrapResult{
				OrgID:              state.OrgID,
				ProjectID:          state.ProjectID,
				ProjectSlug:        state.ProjectSlug,
				SecretsProjectID:   state.SecretsProjectID,
				SecretsProjectSlug: state.SecretsProjectSlug,
				ClientID:           state.ClientID,
				ClientSecret:       state.ClientSecret,
				IdentityID:         state.IdentityID,
			}, nil
		}
		fmt.Printf("[infisical-bootstrap] ⚠️  Local state at %s belongs to organization %s, not %s — discarding.\n",
			stateFilePath(), state.OrgID, orgID)
		if rmErr := os.Remove(stateFilePath()); rmErr != nil && !os.IsNotExist(rmErr) {
			fmt.Printf("[infisical-bootstrap] ⚠️  Could not remove stale state file: %v\n", rmErr)
		}
	}

	certProjectID, certProjectSlug, err := createProject(ctx, podName, boot.Identity.Credentials.Token, ProjectSlug, "cert-manager")
	if err != nil {
		return nil, err
	}

	secretsProjectID, secretsProjectSlug, err := createProject(ctx, podName, boot.Identity.Credentials.Token, SecretsProjectSlug, "secret-manager")
	if err != nil {
		return nil, err
	}

	// Grant project admin role to the Instance Admin Identity on both projects
	if err := grantProjectAdminRole(ctx, podName, boot.Identity.Credentials.Token, certProjectID, boot.Identity.ID); err != nil {
		return nil, fmt.Errorf("grant project admin to instance admin identity (cert): %w", err)
	}
	if err := grantProjectAdminRole(ctx, podName, boot.Identity.Credentials.Token, secretsProjectID, boot.Identity.ID); err != nil {
		return nil, fmt.Errorf("grant project admin to instance admin identity (secrets): %w", err)
	}

	// The declared management scope, BEFORE anything writes a secret. Infisical
	// created both projects above with its own default environments, and the hub's
	// scope is not one of them (ADR-111 Revision 7 §2).
	//
	// Placed after the grants rather than beside createProject because creating an
	// environment is a project-scoped permission, and the grants are what the token
	// used here holds it by.
	if err := ensureManagementScope(ctx, podName, boot.Identity.Credentials.Token,
		certProjectID, secretsProjectID); err != nil {
		return nil, err
	}

	clientID, clientSecret, identityID, err := createMachineIdentity(ctx, podName, boot.Identity.Credentials.Token, orgID, certProjectID)
	if err != nil {
		return nil, err
	}

	// Also grant Machine Identity access to the secrets project
	grantURL := "http://localhost:" + infisicalPort + fmt.Sprintf(PathProjectMembershipsIdentities, secretsProjectID, identityID)
	if grantOut, grantErr := kubectlExec(ctx, podName,
		"curl", "-s", "-X", "POST",
		grantURL,
		"-H", "Content-Type: application/json",
		"-H", "Authorization: Bearer "+boot.Identity.Credentials.Token,
		"-d", `{"roles":[{"role":"admin"}]}`,
	); grantErr != nil {
		fmt.Printf("[infisical-bootstrap] ⚠️  Grant secrets project failed: %s\n", grantErr)
	} else {
		fmt.Printf("[infisical-bootstrap] Machine Identity granted admin on secrets project: %s\n", strings.TrimSpace(grantOut))
	}

	// Automate ADR-042 PKI_READY: create all required certificate profiles
	if err := ensureCertificateProfiles(ctx, podName, boot.Identity.Credentials.Token, certProjectID); err != nil {
		return nil, fmt.Errorf("ensure certificate profiles: %w", err)
	}

	fmt.Println("[infisical-bootstrap] ✅ Infisical Day-0 bootstrap complete")
	result := &BootstrapResult{
		OrgID:              orgID,
		ProjectID:          certProjectID,
		ProjectSlug:        certProjectSlug,
		SecretsProjectID:   secretsProjectID,
		SecretsProjectSlug: secretsProjectSlug,
		ClientID:           clientID,
		ClientSecret:       clientSecret,
		IdentityID:         identityID,
	}

	// Cache the result locally so re-runs (local development) skip all API calls.
	if err := saveBootstrapState(result); err != nil {
		fmt.Printf("[infisical-bootstrap] ⚠️  Could not save bootstrap state: %v\n", err)
	}

	return result, nil
}

func BootstrapInfisicalDayZeroWithRetry(ctx context.Context, timeout time.Duration) (*BootstrapResult, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		result, err := BootstrapInfisicalDayZero(ctx)
		if err == nil {
			return result, nil
		}
		lastErr = err
		fmt.Printf("[infisical-bootstrap] ⚠️  Bootstrap attempt failed: %v\n", err)
		fmt.Println("[infisical-bootstrap] Retrying in 10 seconds...")

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}

	return nil, fmt.Errorf("infisical bootstrap failed after %v: %w", timeout, lastErr)
}
