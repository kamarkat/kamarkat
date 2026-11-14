package tenant

// Which git organisation a box's repository is created in.
//
// It is the tenant's organisation and the repository is theirs (ADR-062), so it
// is never derived from anything the platform owns. What it CAN be derived from
// is the credential that will create the repository: when that credential
// reaches exactly one organisation there is only one answer, and asking for it
// is asking an operator to retype what the token already says.
//
// More than one is a REFUSAL and never a choice. The repository is created with
// the tenant's name in it, under a timestamped name that cannot be quietly
// reused -- CreateRepo treats an existing one as a collision rather than a
// resume -- so a wrong organisation is not a mistake this tool can take back.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ResolveGitOrg reports the one organisation this credential can create a
// repository in, or refuses and says what it found.
func ResolveGitOrg(ctx context.Context, cred Credential) (string, error) {
	orgs, err := credentialOrgs(ctx, cred)
	if err != nil {
		return "", fmt.Errorf(`could not read which organisations this credential reaches, so the
one to create the box in cannot be established: %w

Name it with --git-org.`, err)
	}
	switch len(orgs) {
	case 0:
		return "", fmt.Errorf(`this credential (%s) reaches no organisation, so there is nowhere to
create the box.

A box's repository belongs to the TENANT's organisation (ADR-062). Grant the
credential access to it, or name it with --git-org.`, cred.Kind())
	case 1:
		return orgs[0], nil
	}
	return "", fmt.Errorf(`this credential reaches %d organisations, so which one this box belongs to
is a decision rather than a lookup:

  %s

Name one with --git-org. A box's repository carries the tenant's name under a
timestamped name that cannot be reused, so creating it in the wrong
organisation is not something this can undo.`, len(orgs), strings.Join(orgs, "\n  "))
}

// credentialOrgs lists the organisations a credential reaches.
//
// Two shapes, because GitHub issues two kinds of token and they do not answer
// the same endpoint. An App installation token has no user, so /user/orgs is
// empty for it and its scope is read from the installation; a personal access
// token has no installation. Each is asked the question it can answer rather
// than one being made to stand for both.
func credentialOrgs(ctx context.Context, cred Credential) ([]string, error) {
	if strings.HasPrefix(cred.Token, "ghs_") {
		var payload struct {
			Repositories []struct {
				Owner struct{ Login string } `json:"owner"`
			} `json:"repositories"`
		}
		if err := githubJSON(ctx, cred, "/installation/repositories?per_page=100", &payload); err != nil {
			return nil, err
		}
		seen, orgs := map[string]bool{}, []string{}
		for _, r := range payload.Repositories {
			if login := r.Owner.Login; login != "" && !seen[login] {
				seen[login] = true
				orgs = append(orgs, login)
			}
		}
		return orgs, nil
	}

	var payload []struct {
		Login string `json:"login"`
	}
	if err := githubJSON(ctx, cred, "/user/orgs?per_page=100", &payload); err != nil {
		return nil, err
	}
	orgs := make([]string, 0, len(payload))
	for _, o := range payload {
		if o.Login != "" {
			orgs = append(orgs, o.Login)
		}
	}
	return orgs, nil
}

func githubJSON(ctx context.Context, cred Credential, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPI()+path, nil)
	if err != nil {
		return err
	}
	authorize(req, cred)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}
