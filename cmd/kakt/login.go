package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

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
			email, orgID, err := verifySession(cmd.Context(), domain, token, org)
			if err != nil {
				return err
			}

			if err := store.Save(session.Session{
				Domain: domain, Token: token, Email: email, OrgID: orgID,
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
	f.StringVar(&token, "token", "", "a session token, instead of opening a browser (also honoured from INFISICAL_TOKEN)")
	f.StringVar(&org, "org", "", "the organization to scope the session to, by id, slug or name")
	f.DurationVar(&timeout, "timeout", 3*time.Minute, "how long to wait for the browser login")
	if env := strings.TrimSpace(os.Getenv("INFISICAL_TOKEN")); env != "" && token == "" {
		token = env
	}
	return cmd
}

// browserLogin runs the loopback flow: a local listener, a browser at the
// provider's login page, and the session posted back to us.
//
// THIS IS THE VENDOR'S OWN MECHANISM, and it is worth saying that it is not a
// registered OAuth client. Infisical's web app posts the session to
// http://127.0.0.1:<port> when its login URL carries ?callback_port=<port>, which
// is how their CLI authenticates; there is no client registration and no PKCE, so
// the contract is theirs to change. That is why a pasted token is accepted
// alongside it -- their own CLI offers the same escape -- and why automation uses
// a machine identity rather than this.
func browserLogin(ctx context.Context, domain string, timeout time.Duration) (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("could not open a local callback port: %w", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("%s/login?callback_port=%d", domain, port)

	received := make(chan string, 1)
	failed := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// The browser's origin is the login page, so the preflight has to be
		// answered or the POST never arrives.
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
		// `JTWToken` is spelled exactly as the provider spells it, typo included:
		// it is their field name, and a corrected spelling decodes to nothing.
		var body struct {
			Email    string `json:"email"`
			JTWToken string `json:"JTWToken"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			failed <- fmt.Errorf("the browser sent something unreadable: %w", err)
			return
		}
		w.WriteHeader(http.StatusOK)
		if strings.TrimSpace(body.JTWToken) == "" {
			failed <- fmt.Errorf("the browser completed the login but sent no token")
			return
		}
		received <- body.JTWToken
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(listener) }()
	defer srv.Close()

	fmt.Printf("Opening %s\n", url)
	if err := openBrowser(url); err != nil {
		fmt.Printf("Could not open a browser (%v).\n", err)
	}
	fmt.Println("\nIf the browser cannot reach this terminal, copy the token it shows and paste it here.")
	go func() {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return
		}
		if t := strings.TrimSpace(line); t != "" {
			received <- t
		}
	}()

	select {
	case t := <-received:
		return t, nil
	case err := <-failed:
		return "", err
	case <-time.After(timeout):
		return "", fmt.Errorf("no login arrived within %s.\n\n"+
			"Re-run, or pass the token directly: kakt login --domain %s --token <token>",
			timeout, domain)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// openBrowser asks the desktop to open a URL, with no dependency to do it.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "linux":
		return exec.Command("xdg-open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return fmt.Errorf("no browser opener for %s", runtime.GOOS)
	}
}

// verifySession proves the token is a usable session and returns who it belongs
// to.
//
// /api/v1/organization is the endpoint the platform's own Infisical bootstrap
// already calls with a user session (getExistingOrgData), so this is the same
// authentication mode on the same route rather than a new assumption about the
// API.
func verifySession(ctx context.Context, domain, token, wantOrg string) (email, orgID string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, domain+"/api/v1/organization", nil)
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
		return "", "", fmt.Errorf("%s answered %d when asked which organizations this session can see",
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
		return "", "", fmt.Errorf("could not read the organizations response: %w", err)
	}
	if len(body.Organizations) == 0 {
		return "", "", fmt.Errorf("this session belongs to no organization, so it can read no escrow")
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
			return "", "", fmt.Errorf("this session sees no organization %q; it sees: %s",
				wantOrg, strings.Join(names, ", "))
		}
	}
	// The identity is the session's, and the API does not return the address on
	// this route, so the organization stands in for "as whom" until a command
	// performs a read and Infisical attributes it.
	return "session on " + chosen.Slug, chosen.ID, nil
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
			fmt.Printf("%s at %s\n", s.Email, s.Domain)
			if s.OrgID != "" {
				fmt.Printf("  organization  %s\n", s.OrgID)
			}
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
