package teardown

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Releasing the box's DNS records is part of tearing it down, and until now
// nothing did it.
//
// external-dns runs `--policy=sync`, which deletes a record when the source that
// declared it disappears -- so a cluster coming apart LOOKS like it cleans up
// after itself. It does not. That deletion only happens if external-dns is still
// running when the HTTPRoutes go, and it reconciles once a minute, so whether a
// record survives its own cluster is a race between that loop and the machines
// being destroyed underneath it.
//
// dashboard.dev.nutgraf.in lost that race in the other direction and stayed in
// the zone pointing at 65.109.41.23, a load balancer that no longer exists, owned
// by a box called `hub-hybrid-dev` that no longer exists either. Six sibling
// records created by the same external-dns at the same moment were deleted
// normally. The difference was timing, nothing else.
//
// An orphan is unrecoverable from inside Kubernetes. external-dns will not adopt,
// update or delete a record whose ownership TXT names an owner that is not its
// own -- it ignores it, logs nothing, and reports "All records are already up to
// date" while the hostname resolves to a decommissioned address. Every later box
// on that domain inherits the problem, and the only repair is deleting the record
// from the zone by hand.
//
// So this deletes deliberately, on the way out, rather than hoping a controller
// notices in time.

const (
	hetznerAPIBase = "https://api.hetzner.cloud/v1"

	// dnsReclaimTimeout bounds the whole exchange. Teardown is a cleanup path:
	// it must not hang on a DNS API that is slow or unreachable, because the
	// resources that cost money have already been dealt with by the time this
	// runs.
	dnsReclaimTimeout = 30 * time.Second
)

// dnsOwnership is what this box needs to know to identify its own records, read
// out of the cluster while the cluster still exists.
type dnsOwnership struct {
	token     string
	ownerID   string
	zoneHost  string // the domain external-dns was scoped to, e.g. dev.nutgraf.in
	txtPrefix string
}

type hetznerZone struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type hetznerRecord struct {
	Value string `json:"value"`
}

type hetznerRRSet struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Records []hetznerRecord `json:"records"`
}

// DNSOwner, DNSZone and DNSToken let a caller state the box's DNS identity when
// the cluster can no longer be asked.
//
// readDNSOwnership reads all three from the running cluster, which is right while
// there is one and useless afterwards -- and "afterwards" is exactly when records
// are orphaned. A box torn down by a build with no reclaim, or one whose teardown
// could not reach the API server, leaves records nothing will ever delete:
// external-dns ignores a record whose owner does not match, silently, so no later
// box can correct it and the hostname resolves to a decommissioned address
// forever.
//
// Supplied explicitly, these make that recoverable.
type DNSOverride struct {
	Owner string
	Zone  string
	Token string
}

// resolveDNSOwnership prefers what the caller stated, and falls back to the
// cluster. Stated values win because the only reason to state them is that the
// cluster's answer is unavailable or wrong.
func (o *Orchestrator) resolveDNSOwnership(ctx context.Context) (dnsOwnership, bool) {
	if o.DNS.Owner != "" && o.DNS.Zone != "" {
		token := o.DNS.Token
		if token == "" {
			token = strings.TrimSpace(os.Getenv("HCLOUD_DNS_TOKEN"))
		}
		if token == "" {
			token = strings.TrimSpace(os.Getenv("HETZNER_DNS_API_TOKEN"))
		}
		if token == "" {
			fmt.Println("[teardown] ⚠️  a DNS owner and zone were given but no token; set")
			fmt.Println("[teardown]     HETZNER_DNS_API_TOKEN (or HCLOUD_DNS_TOKEN) to release them")
			return dnsOwnership{}, false
		}
		return dnsOwnership{
			token:     token,
			ownerID:   o.DNS.Owner,
			zoneHost:  o.DNS.Zone,
			txtPrefix: "extdns-",
		}, true
	}
	return o.readDNSOwnership(ctx)
}

// readDNSOwnership pulls the DNS credential and this box's external-dns identity
// out of the live cluster.
//
// It must run BEFORE anything is destroyed, for the same reason spokeClusterNames
// does: both facts live only in the cluster, and after teardown starts nothing can
// answer. The token is a tenant credential that the platform never stores on disk
// -- it arrives by ExternalSecret and dies with the box -- so the one chance to
// read it is now.
//
// Returns ok=false whenever anything is missing. A box with no external-dns, or
// one whose DNS is a provider this does not understand, is not an error: it has no
// records of ours to release.
func (o *Orchestrator) readDNSOwnership(ctx context.Context) (dnsOwnership, bool) {
	baseArgs, ok := o.resolveKubectlBaseArgs()
	if !ok {
		return dnsOwnership{}, false
	}

	// Namespace is discovered, not assumed. It is platform-edge today and was
	// platform-ops before; a teardown that looked in the wrong one would report
	// "no records to release" and leave the whole zone behind.
	nsArgs := append(append([]string{}, baseArgs...),
		"get", "deploy", "-A", "-l", "app.kubernetes.io/name=external-dns",
		"-o", "jsonpath={.items[0].metadata.namespace}")
	nsOut, err := exec.CommandContext(ctx, "kubectl", nsArgs...).Output()
	namespace := strings.TrimSpace(string(nsOut))
	if err != nil || namespace == "" {
		// Fall back to the name, for a Deployment that carries no such label.
		for _, candidate := range []string{"platform-edge", "platform-ops"} {
			probe := append(append([]string{}, baseArgs...),
				"get", "deploy", "external-dns", "-n", candidate, "-o", "name")
			if err := exec.CommandContext(ctx, "kubectl", probe...).Run(); err == nil {
				namespace = candidate
				break
			}
		}
	}
	if namespace == "" {
		return dnsOwnership{}, false
	}

	argsOut, err := exec.CommandContext(ctx, "kubectl", append(append([]string{}, baseArgs...),
		"get", "deploy", "external-dns", "-n", namespace,
		"-o", "jsonpath={.spec.template.spec.containers[0].args}")...).Output()
	if err != nil {
		return dnsOwnership{}, false
	}

	own := dnsOwnership{txtPrefix: "extdns-"}
	for _, raw := range strings.Split(strings.Trim(string(argsOut), "[]"), ",") {
		arg := strings.Trim(strings.TrimSpace(raw), `"`)
		switch {
		case strings.HasPrefix(arg, "--txt-owner-id="):
			own.ownerID = strings.TrimPrefix(arg, "--txt-owner-id=")
		case strings.HasPrefix(arg, "--domain-filter="):
			own.zoneHost = strings.TrimPrefix(arg, "--domain-filter=")
		case strings.HasPrefix(arg, "--txt-prefix="):
			own.txtPrefix = strings.TrimPrefix(arg, "--txt-prefix=")
		}
	}
	// Without an owner id there is no way to tell this box's records from anyone
	// else's, and deleting on any weaker evidence is how a teardown takes out a
	// zone it was never pointed at.
	if own.ownerID == "" || own.zoneHost == "" {
		return dnsOwnership{}, false
	}

	secOut, err := exec.CommandContext(ctx, "kubectl", append(append([]string{}, baseArgs...),
		"get", "secret", "hetzner-dns", "-n", namespace,
		"-o", "jsonpath={.data.api-key}")...).Output()
	if err != nil {
		return dnsOwnership{}, false
	}
	token, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(secOut)))
	if err != nil || len(token) == 0 {
		return dnsOwnership{}, false
	}
	own.token = string(token)

	return own, true
}

func (own dnsOwnership) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, hetznerAPIBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+own.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The token is in the request, never in the message.
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// zoneID finds the DNS zone that holds this box's hostnames.
//
// The zone is not the domain filter. external-dns is scoped to dev.nutgraf.in
// while the zone is nutgraf.in, so looking the filter up as a zone name finds
// nothing and the teardown reports success having deleted none of its records.
// The longest matching suffix wins, so a delegated sub-zone is preferred over its
// parent when both exist.
func (own dnsOwnership) zoneID(ctx context.Context) (int64, string, error) {
	var page struct {
		Zones []hetznerZone `json:"zones"`
	}
	if err := own.do(ctx, http.MethodGet, "/zones?per_page=100", &page); err != nil {
		return 0, "", err
	}

	best := matchZone(page.Zones, own.zoneHost)
	if best.ID == 0 {
		return 0, "", fmt.Errorf("no zone in this account holds %s", own.zoneHost)
	}
	return best.ID, best.Name, nil
}

// matchZone picks the zone that holds a domain: the longest name the domain ends
// on a label boundary of. Separate from the request so the suffix rule is
// testable without an account.
func matchZone(zones []hetznerZone, domain string) hetznerZone {
	var best hetznerZone
	for _, z := range zones {
		if domain != z.Name && !strings.HasSuffix(domain, "."+z.Name) {
			continue
		}
		if len(z.Name) > len(best.Name) {
			best = z
		}
	}
	return best
}

// ownedRecordNames returns the rrset names this box owns, read from the ownership
// TXT records external-dns maintains beside every record it manages.
//
// Ownership is asserted by the zone itself, not inferred from the hostname. A
// record whose TXT names another owner belongs to another box; a record with no
// TXT at all was created by hand and belongs to a person. Neither is ours to
// delete, and the whole value of this function is that it says no to both.
func ownedRecordNames(rrsets []hetznerRRSet, own dnsOwnership) map[string]bool {
	owned := map[string]bool{}
	// An empty owner id matches nothing. readDNSOwnership already refuses to
	// return one, and this is the second guard on the same thing: under a
	// substring test "owner=" appears in EVERY record external-dns has ever
	// written, so the empty case does not degrade to deleting nothing -- it
	// degrades to deleting the entire zone.
	if own.ownerID == "" {
		return owned
	}

	for _, rr := range rrsets {
		if rr.Type != "TXT" {
			continue
		}
		for _, rec := range rr.Records {
			if txtOwner(rec.Value) != own.ownerID {
				continue
			}
			// Both registry shapes point at the same hostname:
			//   extdns-<host>     (the record's own ownership TXT)
			//   extdns-a-<host>   (the A-record-specific one)
			host := strings.TrimPrefix(rr.Name, own.txtPrefix)
			host = strings.TrimPrefix(host, "a-")
			owned[host] = true
		}
	}
	return owned
}

// txtOwner extracts the owner id from an external-dns registry TXT value, which
// is a comma-separated list of key=value pairs:
//
//	heritage=external-dns,external-dns/owner=dev-hybrid,external-dns/resource=...
//
// The field is parsed and compared whole rather than searched for as a substring,
// because owner ids nest: "dev" is a substring of "dev-hybrid", so a box named
// dev would claim -- and delete -- every record belonging to dev-hybrid. The ids
// are derived from environment and provider names, which makes that collision
// likely rather than exotic.
func txtOwner(value string) string {
	for _, field := range strings.Split(strings.Trim(value, `"`), ",") {
		if id, ok := strings.CutPrefix(strings.TrimSpace(field), "external-dns/owner="); ok {
			return id
		}
	}
	return ""
}

// releaseDNSRecords deletes every record in the zone that this box owns.
//
// Runs AFTER the cloud resources are gone, deliberately. external-dns recreates
// anything it is still watching, so deleting while the cluster is alive is a
// no-op that looks like it worked -- the records come back within the minute.
// Returns how many records it actually released, so a caller reports what
// happened rather than that it ran.
func (o *Orchestrator) releaseDNSRecords(ctx context.Context, own dnsOwnership) int {
	ctx, cancel := context.WithTimeout(ctx, dnsReclaimTimeout)
	defer cancel()

	zoneID, zoneName, err := own.zoneID(ctx)
	if err != nil {
		o.reportDNSResidue(own, fmt.Sprintf("the zone could not be resolved: %v", err))
		return 0
	}

	var page struct {
		RRSets []hetznerRRSet `json:"rrsets"`
	}
	if err := own.do(ctx, http.MethodGet, fmt.Sprintf("/zones/%d/rrsets?per_page=500", zoneID), &page); err != nil {
		o.reportDNSResidue(own, fmt.Sprintf("the zone's records could not be listed: %v", err))
		return 0
	}

	owned := ownedRecordNames(page.RRSets, own)
	if len(owned) == 0 {
		return 0
	}

	fmt.Printf("[teardown] Releasing DNS records owned by '%s' in %s...\n", own.ownerID, zoneName)

	var failed []string
	var released int
	for _, rr := range page.RRSets {
		host := strings.TrimPrefix(rr.Name, own.txtPrefix)
		host = strings.TrimPrefix(host, "a-")
		if !owned[host] {
			continue
		}
		path := fmt.Sprintf("/zones/%d/rrsets/%s/%s", zoneID, rr.Name, rr.Type)
		if err := own.do(ctx, http.MethodDelete, path, nil); err != nil {
			failed = append(failed, fmt.Sprintf("%s %s (%v)", rr.Name, rr.Type, err))
			continue
		}
		fmt.Printf("[teardown]       released %s %s\n", rr.Name, rr.Type)
		released++
	}

	fmt.Printf("[teardown] Released %d DNS record(s)\n", released)
	if len(failed) > 0 {
		fmt.Println("[teardown] ⚠️  These records could not be deleted and are now ORPHANED —")
		fmt.Println("[teardown]     no future box can claim them, because external-dns ignores")
		fmt.Println("[teardown]     records owned by an id that is not its own:")
		for _, f := range failed {
			fmt.Printf("[teardown]       %s\n", f)
		}
		fmt.Printf("[teardown]     Delete them from the %s zone by hand.\n", zoneName)
	}
	return released
}

// reportDNSResidue says plainly that records were left behind, and why it
// matters.
//
// Silence here is what produced the orphan this function exists to prevent: the
// records simply stayed, nothing mentioned them, and the next box on the domain
// inherited a hostname it could not correct.
func (o *Orchestrator) reportDNSResidue(own dnsOwnership, reason string) {
	fmt.Println("[teardown] ⚠️  DNS records were NOT released:", reason)
	fmt.Printf("[teardown]     Records under %s owned by external-dns id '%s' remain in the\n", own.zoneHost, own.ownerID)
	fmt.Println("[teardown]     zone. They will not be adopted, updated or deleted by any")
	fmt.Println("[teardown]     future box — external-dns ignores records it does not own, and")
	fmt.Println("[teardown]     logs nothing when it does. Delete them by hand.")
}
