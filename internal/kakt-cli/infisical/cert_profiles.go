package infisical

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kamarkat/kamarkat/internal/pki"
	"strings"
)

// fleetCAName is the certificate's SUBJECT, and fleetCASlug is what the API
// keys the object by.
//
// Two different strings for two different fields, and conflating them cost a
// bootstrap: creation sends {"name": fleetCASlug, "configuration": {"commonName":
// fleetCAName}}, while the lookup below matched the listing's "name" against
// fleetCAName -- the subject. So an existing CA was never recognised, every
// re-run tried to create a second one, and upstream answered the duplicate with
//
//	HTTP 500 {"message":"Something went wrong","error":"Create"}
//
// which names neither the field nor the conflict. The lookup and the create now
// read the same constant, so they cannot disagree again.
const (
	fleetCAName = "Fleet Intermediate CA"
	fleetCASlug = "fleet-intermediate-ca"
)

type caEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// caStatusPendingCertificate is the state a root CA sits in between being
// created and having its self-signed certificate generated. A CA in this state
// answers issuance requests with "CA is not active".
const caStatusPendingCertificate = "pending-certificate"

type policyEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// requiredProfiles is derived from the single authoritative definition in
// internal/pki, which the hub-operator's self-healing reconcile reads too. It was
// previously a second hardcoded copy, and the two drifted — see that package.
var requiredProfiles = pki.RequiredProfiles

// ensureCertificateProfiles creates all required certificate profiles if they do not exist.
// It looks up the Fleet Intermediate CA, finds or creates a certificate policy, then creates
// each profile. This replaces the manual Step 3.6 UI action from the old workflow and
// satisfies ADR-042 PKI_READY exit criteria.
func ensureCertificateProfiles(ctx context.Context, podName, adminJWT, projectID string) error {
	// Phase 1: Ensure Fleet Intermediate CA exists and is active (once for all profiles)
	cas, err := listCertificateAuthorities(ctx, podName, adminJWT, projectID)
	if err != nil {
		return fmt.Errorf("list certificate authorities: %w", err)
	}

	var fleetCAID, fleetCAStatus string
	for _, ca := range cas {
		if ca.Name == fleetCASlug {
			fleetCAID, fleetCAStatus = ca.ID, ca.Status
			break
		}
	}
	if fleetCAID == "" {
		fleetCAID, err = createFleetIntermediateCA(ctx, podName, adminJWT, projectID)
		if err != nil {
			return fmt.Errorf("create CA %q: %w", fleetCAName, err)
		}
		fleetCAStatus = caStatusPendingCertificate
		fmt.Printf("[infisical-bootstrap] Created CA %q: id=%s\n", fleetCAName, fleetCAID)
	} else {
		fmt.Printf("[infisical-bootstrap] Found CA %q: id=%s status=%s\n", fleetCAName, fleetCAID, fleetCAStatus)
	}

	// Activation keys off status, not off whether this run created the CA.
	//
	// Creating and activating are two API calls, so anything that interrupts a
	// bootstrap between them leaves a CA that exists, is found by the check
	// above, and can never issue. That is not a hypothetical: it is how this
	// fleet's CA ended up stuck at pending-certificate, which stayed invisible
	// until the certificates issued by its predecessor expired days later and
	// every renewal failed with "CA is not active".
	if fleetCAStatus == caStatusPendingCertificate {
		if err := activateRootCA(ctx, podName, adminJWT, fleetCAID); err != nil {
			return fmt.Errorf("activate CA %q: %w", fleetCAName, err)
		}
		fmt.Printf("[infisical-bootstrap] Activated CA %q: id=%s\n", fleetCAName, fleetCAID)
	}

	// Phase 2: Resolve certificate policy (one policy covers all profiles)
	policyID, err := resolveCertificatePolicy(ctx, podName, adminJWT, projectID)
	if err != nil {
		return fmt.Errorf("resolve certificate policy: %w", err)
	}

	// Phase 3: Create each required profile idempotently
	for _, p := range requiredProfiles {
		found, err := CheckCertificateProfile(ctx, podName, adminJWT, projectID, p.Slug)
		if err != nil {
			return fmt.Errorf("check profile %q: %w", p.Slug, err)
		}
		if found {
			fmt.Printf("[infisical-bootstrap] ✓ Certificate profile %q already exists\n", p.Slug)
			continue
		}

		if err := createCertProfile(ctx, podName, adminJWT, projectID, fleetCAID, policyID, p.Slug, p.TTLDays); err != nil {
			return fmt.Errorf("create certificate profile %q: %w", p.Slug, err)
		}
		fmt.Printf("[infisical-bootstrap] ✓ Certificate profile %q created (TTL: %d days)\n", p.Slug, p.TTLDays)
	}

	// Phase 4: the two things pki-issuer v1.x needs on top of the profiles.
	//
	// An Issuer names an application and a profile and no project, so the server
	// resolves the project from the organisation's ACTIVE Certificate Manager
	// project, and the applications routes refuse every request until one is set.
	// Both steps are here rather than in a phase of their own because this
	// function already holds the only thing they need -- the cert-manager
	// project's id -- and because a profile without an application to issue
	// through is not a finished PKI.
	if err := ensureActiveCertManagerProject(ctx, podName, adminJWT, projectID); err != nil {
		return err
	}
	profileIDs, err := resolveCertificateProfileIDs(ctx, podName, adminJWT, projectID)
	if err != nil {
		return err
	}
	return ensurePKIApplication(ctx, podName, adminJWT, profileIDs)
}

// pkiApplicationName is the application pki-issuer v1.x issues through.
//
// THE PLATFORM'S NAME, which is kamarkat. Not the `zero-ops` of
// app.kubernetes.io/part-of and managed-by=zero-ops-hub-cli: those are label
// values this change does not touch, several of them selectors, and none of them
// is what the platform is called. The name shows up in the Infisical UI beside
// the Fleet Intermediate CA and in every Issuer's spec.application, so it should
// read as the product rather than as a label.
//
// One per organisation, not per cluster: an application groups the profiles that
// may be issued through it, and the profiles are the platform's (internal/pki).
// A per-cluster name would put a cluster's identity back into the issuer values
// that this migration took out of them.
const pkiApplicationName = "kamarkat"

// ensureActiveCertManagerProject points the organisation's Certificate Manager at
// the project this bootstrap built.
//
// Idempotent through the server's own state: it reads the active project first and
// writes only when it is unset or different. A write on every run would be
// harmless on a box with one project and wrong on a box being migrated between
// two, where the last writer would decide which PKI the fleet issues from.
func ensureActiveCertManagerProject(ctx context.Context, podName, adminJWT, projectID string) error {
	output, err := kubectlExec(ctx, podName,
		"curl", "-s",
		"http://localhost:"+infisicalPort+PathCertManagerInstance,
		"-H", "Authorization: Bearer "+adminJWT,
	)
	if err != nil {
		return fmt.Errorf("read the Certificate Manager instance state: %w", err)
	}
	var state struct {
		ActiveProjectID string `json:"activeProjectId"`
	}
	if err := json.Unmarshal([]byte(output), &state); err != nil {
		return fmt.Errorf("parse the Certificate Manager instance state: %w\nresponse: %s", err, output)
	}
	if state.ActiveProjectID == projectID {
		fmt.Printf("[infisical-bootstrap] ✓ Certificate Manager's active project is already %s\n", projectID)
		return nil
	}
	if state.ActiveProjectID != "" {
		fmt.Printf("[infisical-bootstrap] Certificate Manager's active project is %s; repointing it at %s\n",
			state.ActiveProjectID, projectID)
	}
	output, err = kubectlExec(ctx, podName,
		"curl", "-s", "-X", "POST",
		"http://localhost:"+infisicalPort+PathCertManagerActiveProject,
		"-H", "Content-Type: application/json",
		"-H", "Authorization: Bearer "+adminJWT,
		"-d", fmt.Sprintf(`{"projectId":"%s"}`, projectID),
	)
	if err != nil {
		return fmt.Errorf("set the active Certificate Manager project: %w", err)
	}
	var set struct {
		ActiveProjectID string `json:"activeProjectId"`
	}
	if err := json.Unmarshal([]byte(output), &set); err != nil || set.ActiveProjectID != projectID {
		return fmt.Errorf("the active Certificate Manager project was not set to %s.\n\nWithout it "+
			"every pki-issuer request is refused with \"Set an organization active Certificate Manager "+
			"project before using Applications\", and no certificate on this box issues.\nresponse: %s",
			projectID, output)
	}
	fmt.Printf("[infisical-bootstrap] ✓ Certificate Manager's active project set to %s\n", projectID)
	return nil
}

// resolveCertificateProfileIDs returns the id of every profile the platform
// declares, in internal/pki's order.
//
// By slug, through the same route CheckCertificateProfile uses -- which answers
// with the profile but is read there only for its status code. An id that cannot
// be resolved is an error rather than an omission: an application built from a
// partial list issues some certificates and silently refuses the rest.
func resolveCertificateProfileIDs(ctx context.Context, podName, adminJWT, projectID string) ([]string, error) {
	ids := make([]string, 0, len(requiredProfiles))
	for _, p := range requiredProfiles {
		output, err := kubectlExec(ctx, podName,
			"curl", "-s",
			"http://localhost:"+infisicalPort+fmt.Sprintf(PathCertificateProfilesBySlug, p.Slug, projectID),
			"-H", "Authorization: Bearer "+adminJWT,
		)
		if err != nil {
			return nil, fmt.Errorf("read certificate profile %q: %w", p.Slug, err)
		}
		var resp struct {
			CertificateProfile struct {
				ID string `json:"id"`
			} `json:"certificateProfile"`
		}
		if err := json.Unmarshal([]byte(output), &resp); err != nil {
			return nil, fmt.Errorf("parse certificate profile %q: %w\nresponse: %s", p.Slug, err, output)
		}
		if resp.CertificateProfile.ID == "" {
			return nil, fmt.Errorf("certificate profile %q has no id in %s; the application it is "+
				"issued through cannot be built from a partial profile list\nresponse: %s",
				p.Slug, projectID, output)
		}
		ids = append(ids, resp.CertificateProfile.ID)
	}
	return ids, nil
}

// ensurePKIApplication creates the application every Issuer names, bound to the
// platform's profiles.
//
// Adopted when it already exists, by name, because an application is keyed by
// name within the project and a second POST answers with a duplicate-name error
// rather than the existing one. The profile binding is not re-asserted on an
// adopted application: adding a profile to the platform is a change to
// internal/pki and belongs in the same release as the application that offers it.
func ensurePKIApplication(ctx context.Context, podName, adminJWT string, profileIDs []string) error {
	output, err := kubectlExec(ctx, podName,
		"curl", "-s",
		"http://localhost:"+infisicalPort+PathCertManagerApplications,
		"-H", "Authorization: Bearer "+adminJWT,
	)
	if err != nil {
		return fmt.Errorf("list PKI applications: %w", err)
	}
	var list struct {
		Applications []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"applications"`
	}
	if err := json.Unmarshal([]byte(output), &list); err != nil {
		return fmt.Errorf("parse the PKI application list: %w\nresponse: %s", err, output)
	}
	for _, a := range list.Applications {
		if a.Name == pkiApplicationName {
			fmt.Printf("[infisical-bootstrap] ✓ PKI application %q already exists: id=%s\n",
				pkiApplicationName, a.ID)
			return nil
		}
	}

	body, err := json.Marshal(map[string]any{"name": pkiApplicationName, "profileIds": profileIDs})
	if err != nil {
		return fmt.Errorf("render the PKI application request: %w", err)
	}
	output, err = kubectlExec(ctx, podName,
		"curl", "-s", "-X", "POST",
		"http://localhost:"+infisicalPort+PathCertManagerApplications,
		"-H", "Content-Type: application/json",
		"-H", "Authorization: Bearer "+adminJWT,
		"-d", string(body),
	)
	if err != nil {
		return fmt.Errorf("create PKI application %q: %w", pkiApplicationName, err)
	}
	var created struct {
		Application struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"application"`
	}
	if err := json.Unmarshal([]byte(output), &created); err != nil || created.Application.ID == "" {
		return fmt.Errorf("PKI application %q was not created; every ClusterIssuer names it, so no "+
			"certificate issues without it\nresponse: %s", pkiApplicationName, output)
	}
	fmt.Printf("[infisical-bootstrap] ✓ PKI application %q created with %d profiles: id=%s\n",
		pkiApplicationName, len(profileIDs), created.Application.ID)
	return nil
}

// listCertificateAuthorities returns all CAs in the given project.
func listCertificateAuthorities(ctx context.Context, podName, adminJWT, projectID string) ([]caEntry, error) {
	url := fmt.Sprintf("http://localhost:%s%s?projectId=%s", infisicalPort, PathCertificateAuthorities, projectID)
	output, err := kubectlExec(ctx, podName,
		"curl", "-s",
		url,
		"-H", "Authorization: Bearer "+adminJWT,
	)
	if err != nil {
		return nil, fmt.Errorf("curl failed: %w", err)
	}

	var resp struct {
		CertificateAuthorities []caEntry `json:"certificateAuthorities"`
	}
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return nil, fmt.Errorf("parse CA list: %w\nresponse: %s", err, output)
	}
	return resp.CertificateAuthorities, nil
}

// listCertificatePolicies returns all certificate policies in the given project.
func listCertificatePolicies(ctx context.Context, podName, adminJWT, projectID string) ([]policyEntry, error) {
	url := fmt.Sprintf("http://localhost:%s%s?projectId=%s", infisicalPort, PathCertificatePolicies, projectID)
	output, err := kubectlExec(ctx, podName,
		"curl", "-s",
		url,
		"-H", "Authorization: Bearer "+adminJWT,
	)
	if err != nil {
		return nil, fmt.Errorf("curl failed: %w", err)
	}

	var resp struct {
		CertificatePolicies []policyEntry `json:"certificatePolicies"`
	}
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return nil, fmt.Errorf("parse policy list: %w\nresponse: %s", err, output)
	}
	return resp.CertificatePolicies, nil
}

// createCertificatePolicy creates a new certificate policy with the given name and returns its ID.
// The subject policy allows argocd-agent.* common names — the cert-operator prefixes
// spoke names with "argocd-agent." when issuing bootstrap certificates.
func createCertificatePolicy(ctx context.Context, podName, adminJWT, projectID, name string) (string, error) {
	body := fmt.Sprintf(`{"projectId":"%s","name":"%s","subject":[{"type":"common_name","allowed":["argocd-agent.*"]}]}`, projectID, name)
	output, err := kubectlExec(ctx, podName,
		"curl", "-s", "-X", "POST",
		"http://localhost:"+infisicalPort+PathCertificatePolicies,
		"-H", "Content-Type: application/json",
		"-H", "Authorization: Bearer "+adminJWT,
		"-d", body,
	)
	if err != nil {
		return "", fmt.Errorf("curl failed: %w", err)
	}

	var resp struct {
		CertificatePolicy policyEntry `json:"certificatePolicy"`
	}
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return "", fmt.Errorf("parse create policy response: %w\nresponse: %s", err, output)
	}
	if resp.CertificatePolicy.ID == "" {
		return "", fmt.Errorf("create policy returned empty ID: %s", output)
	}
	return resp.CertificatePolicy.ID, nil
}

func createCertProfile(ctx context.Context, podName, adminJWT, projectID, caID, policyID, slug string, ttlDays int) error {
	body := fmt.Sprintf(
		`{"projectId":"%s","caId":"%s","certificatePolicyId":"%s","slug":"%s","enrollmentType":"api","issuerType":"ca","apiConfig":{"autoRenew":false},"externalConfigs":null,"defaults":{"ttlDays":%d}}`,
		projectID, caID, policyID, slug, ttlDays,
	)
	output, err := kubectlExec(ctx, podName,
		"curl", "-s", "-X", "POST",
		"http://localhost:"+infisicalPort+PathCertificateProfiles,
		"-H", "Content-Type: application/json",
		"-H", "Authorization: Bearer "+adminJWT,
		"-d", body,
	)
	if err != nil {
		return fmt.Errorf("curl failed: %w", err)
	}

	if strings.Contains(output, "already exists") || strings.Contains(output, "Conflict") {
		fmt.Printf("[infisical-bootstrap] Certificate profile %q already exists (race)\n", slug)
		return nil
	}

	var resp struct {
		CertificateProfile struct {
			ID string `json:"id"`
		} `json:"certificateProfile"`
	}
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return fmt.Errorf("parse create profile response: %w\nresponse: %s", err, output)
	}
	if resp.CertificateProfile.ID == "" {
		return fmt.Errorf("create profile returned empty ID: %s", output)
	}
	return nil
}

// resolveCertificatePolicy finds an existing policy or creates a new one.
func resolveCertificatePolicy(ctx context.Context, podName, adminJWT, projectID string) (string, error) {
	policies, err := listCertificatePolicies(ctx, podName, adminJWT, projectID)
	if err != nil {
		return "", err
	}
	if len(policies) > 0 {
		fmt.Printf("[infisical-bootstrap] Using existing policy: id=%s name=%s\n", policies[0].ID, policies[0].Name)
		return policies[0].ID, nil
	}

	policyName := "zero-ops-platform-policy"
	policyID, err := createCertificatePolicy(ctx, podName, adminJWT, projectID, policyName)
	if err != nil {
		return "", fmt.Errorf("create policy %q: %w", policyName, err)
	}
	fmt.Printf("[infisical-bootstrap] Created policy: id=%s name=%s\n", policyID, policyName)
	return policyID, nil
}

// createFleetIntermediateCA creates a root CA named "Fleet Intermediate CA"
// in the given project via POST /api/v1/cert-manager/ca/internal.
// A root CA becomes active immediately when notAfter is provided — without
// it the CA stays at "pending-certificate" and cannot issue certificates.
func createFleetIntermediateCA(ctx context.Context, podName, adminJWT, projectID string) (string, error) {
	body := fmt.Sprintf(
		`{"name":"`+fleetCASlug+`","projectId":"%s","status":"active","configuration":{"type":"root","commonName":"%s","organization":"Zero-Ops","ou":"","country":"","province":"","locality":"","maxPathLength":1,"keyAlgorithm":"RSA_2048"},"notAfter":"2036-06-07T00:00:00Z"}`,
		projectID, fleetCAName,
	)
	output, err := kubectlExec(ctx, podName,
		"curl", "-s", "-X", "POST",
		"http://localhost:"+infisicalPort+PathCertificateAuthoritiesCreate,
		"-H", "Content-Type: application/json",
		"-H", "Authorization: Bearer "+adminJWT,
		"-d", body,
	)
	if err != nil {
		return "", fmt.Errorf("curl failed: %w", err)
	}

	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return "", fmt.Errorf("parse create CA response: %w\nresponse: %s", err, output)
	}
	if resp.ID == "" {
		return "", fmt.Errorf("create CA returned empty ID: %s", output)
	}
	return resp.ID, nil
}

// activateRootCA generates a self-signed certificate for the root CA,
// transitioning it from "pending-certificate" to "active" status. Root
// CAs are stuck at "pending-certificate" until this step — without it
// the cert-operator gets "CA is not active" on bootstrap cert issuance.
func activateRootCA(ctx context.Context, podName, adminJWT, caID string) error {
	body := `{"notBefore":"2026-06-07T00:00:00Z","notAfter":"2036-06-07T00:00:00Z","maxPathLength":1}`
	output, err := kubectlExec(ctx, podName,
		"curl", "-s", "-f", "-X", "POST",
		"http://localhost:"+infisicalPort+fmt.Sprintf(PathCACertificate, caID),
		"-H", "Content-Type: application/json",
		"-H", "Authorization: Bearer "+adminJWT,
		"-d", body,
	)
	if err != nil {
		return fmt.Errorf("generate CA certificate failed: %w\noutput: %s", err, output)
	}
	return nil
}
