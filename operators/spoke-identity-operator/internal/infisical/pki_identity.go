package infisical

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The certificate issuer's authorization, which is not the same surface as a
// workload's secret access.
//
// WHAT THE ISSUER ACTUALLY NEEDS, read from the endpoints it calls (pki-issuer
// v1.1.0) rather than from a role's name:
//
//	GET  /cert-manager/applications/by-name/{name}        ApplicationActions.Read
//	GET  /cert-manager/certificate-profiles/slug/{slug}   CertificateProfileActions.Read
//	POST /cert-manager/certificates                       CertificateActions.Create
//	GET  /cert-manager/certificates/{id}                  CertificateActions.Read
//
// The application role `operator` covers the first, third and fourth
// (buildApplicationOperatorPermissionRules). The profile read is the odd one: its
// service resolves permission through getProjectPermission and requires
// CertificateProfileActions.Read on the PROJECT -- and of the built-in roles,
// `viewer` does not grant it and `member` does.
//
// So the minimum this identity can hold on an unlicensed Infisical is `member` on
// the Certificate Manager project plus `operator` on the application. That is
// broader than "read one profile" -- `member` also reads secret values in that
// project -- and OSS cannot narrow it, because conditioning a role needs
// PlanFeature.Rbac. What bounds it is the project's CONTENT: the Certificate
// Manager project holds PKI objects, and this platform's secrets live in
// hub-secrets and in each cluster's own project, never there.
//
// NOT THE WORKLOAD IDENTITY. ESO and the issuer have different consequences if
// compromised -- one reads a cluster's secrets, the other mints certificates the
// fleet trusts -- so they are separate identities with separate credentials, and
// neither can exercise the other's authority.

// FindApplicationByName returns the id of a PKI application, or "" when the
// organization's Certificate Manager project has no application by that name.
//
// An unreadable answer is an error and never "". Creating or scoping anything on
// the strength of a failed lookup would attach an identity to the wrong
// application, or to none.
func (c *Client) FindApplicationByName(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("no PKI application named: the issuer signs through one by name, " +
			"and defaulting it would point a cluster's certificates at another application")
	}

	token, err := c.getToken(ctx)
	if err != nil {
		return "", fmt.Errorf("authenticate: %w", err)
	}
	url := c.BaseURL + fmt.Sprintf(PathCertManagerApplicationByName, name)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("look up application %s: %w", name, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("look up application %s: %w", name, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("look up application %s: Infisical answered %d: %s",
			name, resp.StatusCode, firstLine(raw))
	}

	var parsed struct {
		Application struct {
			ID string `json:"id"`
		} `json:"application"`
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("look up application %s: a 200 this operator could not read: %w", name, err)
	}
	if parsed.Application.ID != "" {
		return parsed.Application.ID, nil
	}
	return parsed.ID, nil
}

// EnsureApplicationRole gives an identity `role` on a PKI application, creating
// the membership or correcting its role.
//
// POST creates and PATCH corrects; both are on the application's own membership
// route, which is a different surface from a project membership and is not
// interchangeable with one. An identity with the project role and not this one
// cannot issue; with this one and not the project role it cannot resolve the
// profile.
func (c *Client) EnsureApplicationRole(ctx context.Context, applicationID, identityID, role string) (bool, error) {
	if strings.TrimSpace(applicationID) == "" || strings.TrimSpace(identityID) == "" {
		return false, fmt.Errorf("an application and an identity are both required to scope " +
			"certificate issuance")
	}
	if strings.TrimSpace(role) == "" {
		return false, fmt.Errorf("no application role named for identity %s: issuance is "+
			"permitted by the role, and defaulting it would decide that here", identityID)
	}

	body, err := json.Marshal(map[string]string{"role": role})
	if err != nil {
		return false, fmt.Errorf("marshal application membership request: %w", err)
	}

	// POST first. An existing membership answers 400 or 409 saying so, and that is
	// the signal to correct it rather than a failure -- but the correction is a
	// PATCH, because a response code says a membership exists and nothing about
	// what it grants.
	status, raw, err := c.applicationMembership(ctx, "POST", applicationID, identityID, body)
	if err != nil {
		return false, err
	}
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
		return true, nil
	case status != http.StatusBadRequest && status != http.StatusConflict:
		return false, fmt.Errorf("grant %s on application %s to %s: Infisical answered %d: %s",
			role, applicationID, identityID, status, firstLine(raw))
	}

	status, raw, err = c.applicationMembership(ctx, "PATCH", applicationID, identityID, body)
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("correct %s on application %s for %s: Infisical answered %d: %s",
			role, applicationID, identityID, status, firstLine(raw))
	}
	return false, nil
}

func (c *Client) applicationMembership(ctx context.Context, method, applicationID, identityID string, body []byte) (int, []byte, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("authenticate: %w", err)
	}
	url := c.BaseURL + fmt.Sprintf(PathCertManagerAppIdentity, applicationID, identityID)
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("%s application membership request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s application membership: %w", method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}
