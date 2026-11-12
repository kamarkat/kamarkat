package escrow

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// ArtifactInfisicalAdmin is the administrator of THIS BOX'S OWN Infisical.
//
// Not the escrow's. Two Infisicals are involved in every box and they share no
// user table: the escrow is one you already hold an account on, reached with
// `kakt login`, while the box runs its own instance (ADR-003) which starts with
// no users and no organisation at all. `infisical bootstrap` creates the first
// administrator of that instance, and the email and password it is given are
// inputs to an account being created rather than credentials being looked up.
//
// Escrowed because the value stays load-bearing for the life of the box, in two
// places that are easy to miss:
//
//   - A re-run recovers the existing organisation and identity state by logging
//     in as this administrator (internal/kakt-cli/infisical/bootstrap.go). Without
//     the same password, Day-0 cannot re-enter an Infisical it already bootstrapped.
//   - It is the break-glass account for the tenant's secret store, and a
//     recovered box carries the same user row -- the restored database holds the
//     old password hash, so a successor needs the predecessor's password.
//
// Box-scoped for that last reason: one Infisical per box, outliving any single
// hub incarnation, exactly like the master keys that decrypt it.
const ArtifactInfisicalAdmin = "infisical-admin-credential"

// AdminCredential is that administrator, as the escrow holds it.
type AdminCredential struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// ResolveAdminCredential returns the box's Infisical administrator, generating
// one on first use. It reports whether this call created it.
//
// GENERATED, never asked for. A human is no better at inventing a password than
// crypto/rand is, remembers it worse, and has no reason to hold one for an
// account that exists to be used by Day-0 -- so the prompt that used to ask for
// it only moved the risk onto the operator's memory. ADR-065 is still satisfied,
// and by a stronger route: the value is generated on the operator's machine,
// written to the TENANT's escrow and to their repository, and no copy is kept
// anywhere the platform can read.
//
// The escrow is the idempotency anchor, because nothing else can be. A
// repository secret cannot be read back, so a second scaffold run would mint a
// second password and push it over the one the running Infisical was bootstrapped
// with -- locking Day-0 out of its own secret store. Here, a box that already has
// an escrowed administrator reuses it, and only a box with none gets a new one.
func ResolveAdminCredential(ctx context.Context, store EscrowClient, id Identity, email string) (AdminCredential, bool, error) {
	existing, err := store.RestoreArtifact(ctx, id, ArtifactInfisicalAdmin)
	if err != nil {
		// Unreadable is not absent. Generating over a credential that may exist is
		// how the one a successor needs gets lost.
		return AdminCredential{}, false, fmt.Errorf("reading the escrowed Infisical administrator: %w", err)
	}
	if strings.TrimSpace(existing) != "" {
		var cred AdminCredential
		if err := json.Unmarshal([]byte(existing), &cred); err != nil {
			return AdminCredential{}, false, fmt.Errorf(
				"the escrowed Infisical administrator for box %s is not readable as one: %w.\n\n"+
					"It is under %s. Day-0 logs in with it to re-enter an Infisical it has\n"+
					"already bootstrapped, so this is repaired rather than replaced: replacing\n"+
					"it would leave the running instance holding a password nothing knows.",
				id.BoxID, err, ArtifactInfisicalAdmin)
		}
		if strings.TrimSpace(cred.Password) == "" {
			return AdminCredential{}, false, fmt.Errorf(
				"the escrowed Infisical administrator for box %s has no password", id.BoxID)
		}
		// The escrowed address wins over whoever happens to be signed in now. The
		// account already exists in that Infisical under the address it was
		// created with, and a different one here would be a login for a user that
		// was never created.
		return cred, false, nil
	}

	email = strings.TrimSpace(email)
	if email == "" {
		return AdminCredential{}, false, fmt.Errorf(
			"no address for this box's Infisical administrator.\n\n" +
				"The password is generated and escrowed, but the account needs an address.\n" +
				"Sign in with `kakt login` and it is taken from that session, or set\n" +
				"INFISICAL_ADMIN_EMAIL for this run.")
	}
	password, err := generatePassword()
	if err != nil {
		return AdminCredential{}, false, err
	}
	cred := AdminCredential{Email: email, Password: password}
	payload, err := json.Marshal(cred)
	if err != nil {
		return AdminCredential{}, false, err
	}
	if err := store.BackupArtifact(ctx, id, ArtifactInfisicalAdmin, string(payload)); err != nil {
		// Fatal, not a warning. A password that reaches the repository without
		// reaching the escrow is one nobody can recover: the next run cannot read
		// the repository secret back, and the box it bootstraps cannot be re-entered.
		return AdminCredential{}, false, fmt.Errorf(
			"escrowing the generated Infisical administrator: %w.\n\n"+
				"Nothing was written to the repository. The password has to exist in the\n"+
				"escrow before it exists anywhere else, or a re-run cannot log in to the\n"+
				"Infisical this one bootstrapped.", err)
	}
	return cred, true, nil
}

// passwordAlphabet excludes the characters that break the paths this value
// travels, rather than trusting each of them to quote it.
//
// It is passed as `infisical bootstrap --password` and interpolated into a login
// body built by string formatting (`{"email":"%s","password":"%s"}`), so a quote
// or a backslash would produce malformed JSON and an unexplainable 400 during
// Day-0. It also becomes a GitHub Actions secret and a Kubernetes Secret value.
// 76 characters still gives far more entropy per character than the length needs.
const (
	passwordLower   = "abcdefghijkmnopqrstuvwxyz"
	passwordUpper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	passwordDigit   = "23456789"
	passwordSymbol  = "-_.!#%+=@^~"
	passwordLength  = 32
	passwordClasses = passwordLower + passwordUpper + passwordDigit + passwordSymbol
)

// generatePassword mints one that satisfies Infisical's own policy.
//
// Their default requires lower case, upper case, a digit and a symbol, so one of
// each is placed before the remainder is filled and the whole thing shuffled --
// a purely random string of this length would satisfy it with overwhelming
// probability, but "overwhelming probability" is not something to discover at
// the first failed bootstrap.
//
// Look-alike characters (l/I/1, O/0) are left out of the alphabet: this is a
// break-glass credential a human will one day read off a screen and type.
func generatePassword() (string, error) {
	out := make([]byte, 0, passwordLength)
	for _, class := range []string{passwordLower, passwordUpper, passwordDigit, passwordSymbol} {
		c, err := pick(class)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}
	for len(out) < passwordLength {
		c, err := pick(passwordClasses)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}
	// Fisher-Yates with crypto/rand, so the guaranteed classes are not always in
	// the first four positions.
	for i := len(out) - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", fmt.Errorf("generating the administrator password: %w", err)
		}
		out[i], out[j.Int64()] = out[j.Int64()], out[i]
	}
	return string(out), nil
}

func pick(alphabet string) (byte, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
	if err != nil {
		return 0, fmt.Errorf("generating the administrator password: %w", err)
	}
	return alphabet[n.Int64()], nil
}
