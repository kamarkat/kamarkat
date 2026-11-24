package zitadel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// apiClient is a thin JSON transport for the issuer's management APIs.
//
// Deliberately not a generated SDK. This provider touches a small, stable
// surface — organisations, projects, roles, grants, users — and a generated
// client for the whole API would be a large dependency whose version has to
// track the server's. The request shapes it does use are asserted in tests
// against captured responses.
type apiClient struct {
	base  string
	token string
	http  *http.Client
}

// orgHeader scopes a request to one organisation.
//
// Almost every write needs it. Without it the issuer applies the request to the
// caller's own organisation, which for a service user is the platform's — so an
// omitted header does not fail, it silently creates the resource in the WRONG
// tenant. That is why every call site here passes it explicitly rather than
// relying on a default.
const orgHeader = "x-zitadel-orgid"

func (c *apiClient) do(ctx context.Context, method, path string, orgID string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("zitadel: encode %s %s: %w", method, path, err)
		}
		rdr = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return fmt.Errorf("zitadel: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	if orgID != "" {
		req.Header.Set(orgHeader, orgID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("zitadel: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The body carries the reason and the response code alone rarely does:
		// a 404 here is as often "the org header pointed somewhere else" as it is
		// "absent". Truncated because these are error paths, not audit records.
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return &apiError{Status: resp.StatusCode, Method: method, Path: path, Body: msg}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("zitadel: decode %s %s: %w", method, path, err)
	}
	return nil
}

type apiError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("zitadel: %s %s -> %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// isNotFound reports whether an error is the issuer's "does not exist".
//
// Used by the find-or-create paths, where absence is the normal case on first
// run and must not be confused with a failure. Matched on status rather than
// message so a wording change upstream does not turn provisioning into an error.
func isNotFound(err error) bool {
	var ae *apiError
	if !asAPIError(err, &ae) {
		return false
	}
	return ae.Status == http.StatusNotFound
}

// isUnchanged reports the issuer refusing a write that would change nothing.
//
// It answers 400 "... has not been changed" rather than accepting the no-op, so
// a reconcile that asserts already-correct state gets an ERROR. Treating that as
// failure makes every steady-state pass log a warning, and a log that cries wolf
// on every restart is one nobody reads when something is genuinely wrong.
//
// Matched on the message because the issuer uses one status code for several
// distinct 400s; a status-only test would swallow real rejections such as a
// malformed policy.
func isUnchanged(err error) bool {
	var ae *apiError
	if !asAPIError(err, &ae) || ae.Status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(ae.Body, "has not been changed") ||
		strings.Contains(ae.Body, "No changes")
}

func asAPIError(err error, target **apiError) bool {
	for err != nil {
		if ae, ok := err.(*apiError); ok {
			*target = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
