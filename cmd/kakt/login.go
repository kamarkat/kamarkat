package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/browser"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/session"
)

// defaultEscrowDomain is Infisical's hosted instance, which is where a box's
// escrow is unless the operator runs their own.
const defaultEscrowDomain = "https://app.infisical.com"

// newLoginCmd authenticates the developer, as themselves, against the Infisical a
// box escrows to.
//
// WHAT THIS REPLACES. Reaching a hub used to require the escrow MACHINE identity:
// four INFISICAL_ESCROW_* values exported into the environment or left in
// k8-secrets/infisical/ by whoever ran `kakt escrow init`. A shared secret copied
// onto every workstation authenticates everyone as the same identity, cannot be
// revoked for one person, and records the box rather than the operator in the
// audit log. A login replaces the copying with an authentication: the token is
// the developer's, their permissions decide what they can read, and Infisical
// attributes the read to them.
//
// WHAT IT IS NOT. It is not Kubernetes authorization. The escrowed kubeconfig is
// cluster-admin, so reading it grants cluster-admin -- an authenticated Infisical
// read is an authorization boundary at the CREDENTIAL, not per-user RBAC inside
// the cluster. Day-to-day access remains OIDC through the box's own identity
// provider, where a person authenticates as themselves to the API server and can
// be revoked there (ADR-076).
func newLoginCmd() *cobra.Command {
	var domain, token, org string
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:          "login",
		Short:        "Authenticate to the escrow as yourself, for hub access from any workstation",
		SilenceUsage: true,
		Long: `Authenticate to the Infisical your boxes escrow to.

A browser opens, you log in as yourself, and the session is kept in this machine's
credential store -- the macOS keychain, or the login keyring on Linux. Nothing is
written to a dotfile, and no machine-identity secret is copied onto this machine.

From then on, every command that needs a hub resolves it the same way: the box's
declaration names its escrow and its box id, and the escrowed kubeconfig is read
with your identity, used, and discarded.

Automation does not log in. It sets the machine identity instead, which every
command prefers when present:

  INFISICAL_ESCROW_URL  INFISICAL_ESCROW_PROJECT_ID
  INFISICAL_ESCROW_CLIENT_ID  INFISICAL_ESCROW_CLIENT_SECRET`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			domain = strings.TrimRight(strings.TrimSpace(domain), "/")
			store, err := session.Open()
			if err != nil {
				return err
			}

			if strings.TrimSpace(token) == "" {
				token, err = browserLogin(cmd.Context(), domain, timeout)
				if err != nil {
					return err
				}
			}

			// Proved, not assumed. A token that cannot read the user's own
			// organizations is not a session, and failing here names the login
			// rather than surfacing as an authorization error against a box later.
			userID, email, orgID, err := verifySession(cmd.Context(), domain, token, org)
			if err != nil {
				return err
			}

			if err := store.Save(session.Session{
				Domain: domain, Token: token, UserID: userID, Email: email, OrgID: orgID,
			}); err != nil {
				return err
			}
			fmt.Printf("✓ logged in to %s as %s\n", domain, email)
			fmt.Printf("  session kept in %s\n", store.Name())
			fmt.Println("\nA box is reached by its own declaration, so nothing else is needed here.")
			fmt.Println("Hub access is still authorized by your permissions on that box's escrow project.")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&domain, "domain", defaultEscrowDomain, "the Infisical your boxes escrow to")
	f.StringVar(&token, "token", "",
		"the token Infisical shows when it could not push the login to this terminal; skips "+
			"the browser step (also read from INFISICAL_TOKEN)")
	f.StringVar(&org, "org", "",
		"check the session can see this organization, by id, slug or name, and record it. The "+
			"browser login is what scopes the token")
	f.DurationVar(&timeout, "timeout", 3*time.Minute, "how long to wait for the browser login")
	if env := strings.TrimSpace(os.Getenv("INFISICAL_TOKEN")); env != "" && token == "" {
		token = env
	}
	return cmd
}

// browserLogin runs the loopback flow Infisical's web app implements.
//
// THE PROTOCOL, read from their source rather than assumed
// (frontend/src/pages/auth/SelectOrgPage/SelectOrgPage.tsx:300 and
// frontend/src/pages/auth/CliRedirectPage/route.tsx):
//
//  1. We listen on 127.0.0.1 and open <domain>/login?callback_port=<port>.
//  2. On successful login their SelectOrgPage stores, in sessionStorage,
//     {expiry: now+30s, data: base64({JTWToken, email, privateKey}), callbackPort}
//     and navigates to /cli-redirect.
//  3. That route's beforeLoad decodes it and does
//     axios.post("http://127.0.0.1:<callbackPort>/", payload). On success it
//     clears sessionStorage, so the page renders "You've successfully logged in
//     to the Infisical CLI". On any failure the entry stays and the page renders
//     the token with a Copy button.
//
// THE THIRTY SECONDS MATTER. If that window lapses before beforeLoad runs, the
// entry is removed and NO POST IS ATTEMPTED -- and because the entry is gone the
// page shows the same cheerful success screen with no token on it. So "the
// browser said success and the CLI got nothing" is not necessarily a transport
// failure, and there is nothing to paste in that case: the remedy is to run this
// again and complete the browser steps promptly.
//
// It is not a registered OAuth client -- no client id, no state parameter, no
// PKCE -- so there is nothing in the payload to bind it to this attempt. What
// binds it is the port, which this process opened and nothing else is listening
// on, and the Origin of the request, which must be the login domain. The listener
// closes on the first credential it accepts.
func browserLogin(ctx context.Context, domain string, timeout time.Duration) (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("could not open a local callback port: %w", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	loginURL := fmt.Sprintf("%s/login?callback_port=%d", domain, port)

	received := make(chan string, 1)
	rejected := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Their post is axios with a JSON body, which the browser preflights, so
		// the OPTIONS has to be answered or the POST never leaves the page.
		w.Header().Set("Access-Control-Allow-Origin", domain)
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// The only binding available: this credential must come from the page we
		// sent the operator to. A POST from anywhere else is answered and dropped,
		// so a stray local process cannot plant a session in this store.
		if origin := r.Header.Get("Origin"); origin != "" && origin != domain {
			w.WriteHeader(http.StatusForbidden)
			rejected <- fmt.Errorf("a login arrived from %q rather than %s, and was refused",
				origin, domain)
			return
		}
		// `JTWToken` is spelled as they spell it, typo included: it is their field
		// name, and a corrected spelling decodes to nothing.
		var body struct {
			Email    string `json:"email"`
			JTWToken string `json:"JTWToken"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			rejected <- fmt.Errorf("the browser posted something unreadable: %w", err)
			return
		}
		if strings.TrimSpace(body.JTWToken) == "" {
			w.WriteHeader(http.StatusBadRequest)
			rejected <- fmt.Errorf("the browser completed the login but posted no token")
			return
		}
		w.WriteHeader(http.StatusOK)
		received <- body.JTWToken
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(listener) }()
	defer srv.Close()

	fmt.Printf("Opening %s\n", loginURL)
	if err := browser.Open(loginURL); err != nil {
		fmt.Printf("Could not open a browser (%v). Open that address yourself.\n", err)
	}
	fmt.Printf("Waiting for the browser to finish, up to %s. Ctrl-C cancels.\n", timeout)

	select {
	case token := <-received:
		fmt.Println("Login received.")
		return token, nil
	case err := <-rejected:
		return "", err
	case <-time.After(timeout):
		// Worded from their code, because the usual cause is not a transport
		// failure and telling someone to paste a token they were never shown is
		// worse than saying nothing.
		return "", fmt.Errorf("no login arrived within %s.\n\n"+
			"Infisical pushes the session to this terminal only within 30 seconds of the "+
			"login completing. If that window lapsed it drops the token and still shows "+
			"\"You've successfully logged in to the Infisical CLI\" -- with nothing to copy.\n\n"+
			"Run this again and complete the browser steps without pausing. If the page DID "+
			"show a token with a Copy button, pass it directly:\n\n"+
			"  kakt login --domain %s --token <token>", timeout, domain)
	case <-ctx.Done():
		return "", fmt.Errorf("login cancelled")
	}
}

// openBrowser asks the desktop to open a URL, with no dependency to do it.
// verifySession proves the token is a usable session and returns who it belongs
// to.
//
// Two reads, both on routes whose verifyAuth lists AuthMode.JWT -- a user session
// -- so neither is an assumption about the API:
//
//	GET /api/v2/users/me      who this is (user-router.ts:496)
//	GET /api/v1/organization  which organizations it can see, and the id to record
//
// The first exists because the second does not return an address, and "logged in
// as session on <org slug>" is not an answer to "as whom".
func verifySession(ctx context.Context, domain, token, wantOrg string) (userID, email, orgID string, err error) {
	userID, email, err = currentUserIdentity(ctx, domain, token)
	if err != nil {
		return "", "", "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, domain+"/api/v1/organization", nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("could not reach %s: %w", domain, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", "", "", fmt.Errorf("%s refused the token: it is not a valid session.\n\n"+
			"If you pasted it, check it is the whole token; if the browser posted it, log in again", domain)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", "", fmt.Errorf("%s answered %d when asked which organizations this session can see",
			domain, resp.StatusCode)
	}
	var body struct {
		Organizations []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Slug string `json:"slug"`
		} `json:"organizations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", "", fmt.Errorf("could not read the organizations response: %w", err)
	}
	if len(body.Organizations) == 0 {
		return "", "", "", fmt.Errorf("this session belongs to no organization, so it can read no escrow")
	}
	chosen := body.Organizations[0]
	if wantOrg != "" {
		var found bool
		for _, o := range body.Organizations {
			if o.ID == wantOrg || o.Slug == wantOrg || o.Name == wantOrg {
				chosen, found = o, true
				break
			}
		}
		if !found {
			names := make([]string, 0, len(body.Organizations))
			for _, o := range body.Organizations {
				names = append(names, o.Slug)
			}
			return "", "", "", fmt.Errorf("this session sees no organization %q; it sees: %s",
				wantOrg, strings.Join(names, ", "))
		}
	}
	return userID, email, chosen.ID, nil
}

// currentUserIdentity asks who the session belongs to, and is also the proof that
// the token is a session at all.
//
// A 401 here is the one failure worth distinguishing: everything else this
// command does afterwards would report it as an authorization problem against a
// box, which sends the operator to ask for permissions they already have.
func currentUserIdentity(ctx context.Context, domain, token string) (userID, email string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, domain+"/api/v2/users/me", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("could not reach %s: %w", domain, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", "", fmt.Errorf("%s refused the token: it is not a valid session.\n\n"+
			"If you pasted it, check it is the whole token; if the browser posted it, log in again", domain)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("%s answered %d when asked whose session this is", domain, resp.StatusCode)
	}
	var body struct {
		User struct {
			ID        string `json:"id"`
			Email     string `json:"email"`
			FirstName string `json:"firstName"`
			LastName  string `json:"lastName"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", fmt.Errorf("could not read the identity response: %w", err)
	}

	// The id is required, because it is what authorization is decided on. A
	// session without one cannot be checked against the box's declared
	// administrator, and accepting it would mean storing a session that every
	// later command has to treat as unidentified.
	userID = strings.TrimSpace(body.User.ID)
	if userID == "" {
		return "", "", fmt.Errorf("%s returned a session with no user id. Authorization is "+
			"decided on that id rather than on an address, so there is nothing to check this "+
			"session against", domain)
	}

	email = strings.TrimSpace(body.User.Email)
	if email == "" {
		// An account with no address on it is unusual rather than broken -- an SSO
		// identity can carry a name and no mail -- and refusing the login for it
		// would be refusing a session the server accepts. Display only.
		if name := strings.TrimSpace(body.User.FirstName + " " + body.User.LastName); name != "" {
			email = name
		} else {
			email = "this session"
		}
	}
	return userID, email, nil
}

// newWhoamiCmd says which identity this machine would use, and where it is kept.
func newWhoamiCmd() *cobra.Command {
	var domain string
	cmd := &cobra.Command{
		Use:          "whoami",
		Short:        "Show the identity kakt would use to reach a hub",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Reported in the order Open() tries them, so what is printed is what
			// would be used rather than what exists.
			if id := strings.TrimSpace(os.Getenv("INFISICAL_ESCROW_CLIENT_ID")); id != "" {
				fmt.Println("machine identity (from the environment)")
				fmt.Printf("  client id  %s\n", id)
				fmt.Printf("  escrow     %s\n", os.Getenv("INFISICAL_ESCROW_URL"))
				fmt.Println("\nThis takes precedence over any stored session, so automation and this")
				fmt.Println("workstation behave the same way.")
				return nil
			}
			domain = strings.TrimRight(strings.TrimSpace(domain), "/")
			s, err := session.Current(cmd.Context(), domain)
			if err != nil {
				return err
			}
			store, err := session.Open()
			if err != nil {
				return err
			}
			// Asked of the server, not of the store. "What did I save" is not the
			// question anyone has; "can I still use this, and as whom" is -- and the
			// brief's four outcomes (logged in, authorized, credential retrieved,
			// cluster reachable) start with this one. It also means a session whose
			// stored identity predates a change still reports the truth.
			liveID, live, liveErr := currentUserIdentity(cmd.Context(), s.Domain, s.Token)
			switch {
			case liveErr == nil:
				fmt.Printf("%s at %s\n", live, s.Domain)
				// The id, because it is what a box declares as its platform
				// administrator and what authorization is decided on. An address
				// can be changed; this cannot.
				fmt.Printf("  identity      %s\n", liveID)
			case strings.Contains(liveErr.Error(), "not a valid session"):
				return fmt.Errorf("%s no longer accepts this session.\n\nRun: kakt login --domain %s",
					s.Domain, s.Domain)
			default:
				// Unreachable is not invalid, and saying so is the difference
				// between "log in again" and "check your network".
				fmt.Printf("%s at %s (stored; could not reach %s to confirm)\n",
					s.Email, s.Domain, s.Domain)
			}
			if s.OrgID != "" {
				fmt.Printf("  organization  %s\n", s.OrgID)
			}
			fmt.Println("\nTo authorize this identity for a box, declare it in that box's")
			fmt.Println("registry/clusters/<cluster>/values.yaml as platformAdminIdentity.")
			fmt.Printf("  session kept in %s\n", store.Name())
			fmt.Println("\nWhether this identity can reach a given box is decided by its permissions")
			fmt.Println("on that box's escrow project, not by being logged in.")
			return nil
		},
	}
	cmd.Flags().StringVar(&domain, "domain", defaultEscrowDomain, "the Infisical to report the session for")
	return cmd
}

// newLogoutCmd removes the stored session.
func newLogoutCmd() *cobra.Command {
	var domain string
	cmd := &cobra.Command{
		Use:          "logout",
		Short:        "Remove this machine's stored session",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			domain = strings.TrimRight(strings.TrimSpace(domain), "/")
			store, err := session.Open()
			if err != nil {
				return err
			}
			if err := store.Delete(domain); err != nil {
				var none *session.ErrNoSession
				if errors.As(err, &none) {
					fmt.Printf("no session for %s\n", domain)
					return nil
				}
				return err
			}
			fmt.Printf("✓ session for %s removed from %s\n", domain, store.Name())
			// Said because it is the part people get wrong: the token is gone from
			// this machine, and anything already written out of the escrow is not.
			fmt.Println("  Any kubeconfig a command wrote is temporary and already removed;")
			fmt.Println("  credentials you exported into a shell are not.")
			return nil
		},
	}
	cmd.Flags().StringVar(&domain, "domain", defaultEscrowDomain, "the Infisical to log out of")
	return cmd
}
