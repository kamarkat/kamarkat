package escrow

import (
	"fmt"
	"sort"
	"strings"
)

// The credentials a box is BUILT with, and where they live.
//
// Every one of these used to be an Actions secret on the tenant's repository,
// set by `kakt box init` and forwarded into Day-0 by the bootstrap workflow. Two
// things were wrong with that, and the second is the one that bit:
//
//   - An Actions secret cannot be read back. A value set there can never be
//     compared with the value in use, so drift between them is undetectable by
//     construction -- the argument that moved the box's store administrator out
//     of there first (ADR-076).
//   - The workflow has to forward each one explicitly, and a credential it does
//     not forward arrives as an empty string with no warning anywhere. The
//     object-storage pair was documented in that workflow's own header and never
//     passed, so a tenant who set both secrets correctly got a Day-0 that refused
//     for want of them.
//
// They are escrowed instead, under the box, for the same reason the provider
// credential already was: a first bootstrap has no secret store to read them from
// -- it is the thing being created. ADR-076's admission test carries that clause
// now, "readable when the management cluster is unavailable, or does not exist
// yet".
//
// Four repository secrets remain, the escrow's own coordinates, because CI cannot
// read an escrow it cannot find; and GITOPS_REPO_WRITE_TOKEN, because the
// tenant's application builds read it from their git organisation and a build
// cannot reach an escrow.

// ArtifactRegistryCredential is the credential that pulls the tenant's own
// private images. The platform's images are public, so a box without it builds.
const ArtifactRegistryCredential = "registry-credential"

// ArtifactAlertDestination is where this box's alerts page (ADR-078 add.5 §5).
//
// Escrowed rather than held in the box because the box cannot be the authority
// for where its own failures are reported: the moment that matters is the moment
// it is not answering.
const ArtifactAlertDestination = "alert-destination"

// ArtifactMailTransportURL is the box's outbound mail transport.
//
// The identity provider needs it while it is being created, which is before
// anything inside the box can hold it.
const ArtifactMailTransportURL = "mail-transport-url"

// ArtifactTailnetAuthkey admits nodes on the tenant's own premises.
//
// A control plane joins the tailnet from cloud-init, during its own first boot,
// with no cluster yet to read a Secret from.
const ArtifactTailnetAuthkey = "tailnet-authkey"

// FieldAccessKeyID and the names beside it are the fields a multi-value
// credential carries inside its artefact.
//
// `key=value` lines, one per field, which is the shape the object-store
// credential already arrives in -- an AWS credentials file. Reusing it means one
// parser and one thing for an operator to learn, rather than a format per
// artefact.
const (
	FieldAccessKeyID     = "aws_access_key_id"
	FieldSecretAccessKey = "aws_secret_access_key"
	FieldRegistryUser    = "username"
	FieldRegistryToken   = "token"
	FieldAlertmanagerURL = "alertmanager_url"
	FieldHeartbeatURL    = "heartbeat_url"
)

// ParseFields reads a credential's `key=value` lines.
//
// Blank lines and `#` comments are skipped, and an INI section header is
// tolerated: an AWS credentials file opens with `[default]`, and refusing the
// file the provider hands you is refusing it for being correct.
//
// Values are NOT trimmed of quotes. A credential that contains a quote contains
// it, and stripping one would corrupt a secret silently -- which is worse than
// refusing it.
func ParseFields(payload string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(payload, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return fields
}

// RenderFields is ParseFields' inverse, for an enroller holding separate values.
//
// Sorted, so the same inputs produce the same bytes: an artefact whose
// serialisation varied would read as rotated every time it was written, and
// enrolment refuses to overwrite a credential that differs.
func RenderFields(fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		if strings.TrimSpace(fields[k]) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, fields[k])
	}
	return b.String()
}

// RequireFields reports which of the named fields an artefact does not carry.
//
// All of them or none, which is the rule the credentials themselves impose: a
// registry credential with a username and no token authenticates as nobody, and
// fails at use rather than at configuration.
func RequireFields(payload string, names ...string) []string {
	fields := ParseFields(payload)
	var missing []string
	for _, n := range names {
		if strings.TrimSpace(fields[n]) == "" {
			missing = append(missing, n)
		}
	}
	return missing
}
