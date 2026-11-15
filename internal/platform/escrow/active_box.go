package escrow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ONE CONFIGURED BOX PER WORKSTATION CONTEXT.
//
// Not a limit on how many boxes a tenant owns or runs -- a tenant may hold
// several on purpose, and ADR-111's replacement journey depends on it. The limit
// is on how many this operator's store is configured for at once, because the
// store holds exactly one set of escrow coordinates and nothing in it says which
// box they belong to.
//
// Without that association the store silently misroutes. `kakt escrow init
// --name <new>` found a project id on disk and prepared THAT project, ignoring
// the name it was given, so a second box joined the first box's escrow: its
// master keys and its administrator credential landed in a project belonging to
// another box, where deleting that project would destroy this box's only route
// back. The artefacts stayed separate, because they are keyed by box
// (escrowPath), so nothing was corrupted and nothing said anything either.
//
// So the store records which box it is configured for, and the commands that
// would configure a second one refuse and name the first. `kakt escrow clean`
// is how an operator moves this workstation to another box.
const ActiveBoxFile = "active-box"

// EscrowSettingNames are the values a configured workstation holds. Listed once
// here so `escrow clean` removes exactly what `escrow init` wrote.
var EscrowSettingNames = []string{
	"ESCROW_PROVIDER_URL",
	"ESCROW_PROVIDER_PROJECT_ID",
	"ESCROW_PROVIDER_ORG_ID",
	"ESCROW_PROVIDER_CLIENT_ID",
	"ESCROW_PROVIDER_CLIENT_SECRET",
}

// storedFileValue reads one setting FROM THE STORE ONLY, ignoring the environment.
//
// Deliberately not StoredValue. The environment is how automation supplies these
// (a CI job exports all four), and a workstation guard that read it would refuse
// every run inside CI -- where there is no operator, no interactive
// initialisation and nothing to protect against. The guards here are about what
// this operator's disk is configured for.
func storedFileValue(name string) string {
	dir, err := StorePath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ConfiguredProject returns the escrow project this workstation is configured
// for, or "" when it is not configured.
func ConfiguredProject() string { return storedFileValue("ESCROW_PROVIDER_PROJECT_ID") }

// ConfiguredURL returns the escrow this workstation is configured against.
func ConfiguredURL() string { return storedFileValue("ESCROW_PROVIDER_URL") }

// ActiveBox returns the box this workstation's escrow is bound to, or "".
//
// Empty is not an error: an escrow may be configured before any box exists,
// which is the ordinary order -- `escrow init` then `box init`.
func ActiveBox() string { return storedFileValue(ActiveBoxFile) }

// BindActiveBox records that this workstation's escrow belongs to boxID.
//
// Called when a box is actually created, never when one is merely rendered: a
// dry run that claimed the workstation would leave an operator unable to create
// the box they were previewing.
func BindActiveBox(boxID string) error {
	boxID = strings.TrimSpace(boxID)
	if boxID == "" {
		return fmt.Errorf("no box id to bind this workstation's escrow to")
	}
	if _, err := WriteStoredValues(map[string]string{ActiveBoxFile: boxID}); err != nil {
		return fmt.Errorf("recording the active box: %w", err)
	}
	return nil
}

// ClearLocal removes this workstation's escrow configuration and its box
// binding, and reports what it removed.
//
// LOCAL ONLY. The escrow project, every artefact in it, the machine identity and
// its client secrets, the box's repository secrets and its clusters are all
// untouched -- a cleaned workstation is a workstation that has forgotten a box,
// not a box that has been destroyed. The box keeps running: its CI reads its own
// repository secrets, and its escrow remains the authority for its recovery.
func ClearLocal() ([]string, error) {
	dir, err := StorePath()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, name := range append(append([]string{}, EscrowSettingNames...), ActiveBoxFile) {
		p := filepath.Join(dir, name)
		switch err := os.Remove(p); {
		case err == nil:
			removed = append(removed, name)
		case os.IsNotExist(err):
			// Absent is the desired state, so this is not a failure.
		default:
			return removed, fmt.Errorf("remove %s: %w", p, err)
		}
	}
	return removed, nil
}
