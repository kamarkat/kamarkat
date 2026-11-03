package support

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Record is one observation, already reduced to what may leave.
type Record struct {
	Collector string            `json:"collector"`
	Fields    map[string]string `json:"fields"`
}

// Payload is everything one collection run would send.
type Payload struct {
	Cluster   string   `json:"cluster"`
	Collected string   `json:"collected"`
	Records   []Record `json:"records"`
	// Skipped names collectors that produced nothing and why. A payload that
	// silently omitted a collector would be indistinguishable from a box where
	// the thing it watches is fine.
	Skipped map[string]string `json:"skipped,omitempty"`
}

// Reader is how a collector reaches its source. Injected so the adversarial
// test can present a fixture cluster full of identifying material without
// standing anything up.
type Reader interface {
	// Metrics returns the raw Prometheus text a component exposes.
	Metrics(ctx context.Context, namespace, service string, port int) (string, error)
	// Artefact returns a file the platform rendered, by repository-relative
	// glob.
	Artefact(ctx context.Context, path string) ([]byte, error)
}

// Collect runs every collector in the effective allowlist.
//
// A collector that fails is recorded as skipped and the run continues. The
// alternative -- failing the whole payload -- means one unreachable component
// costs the platform every other piece of evidence about a box, at exactly the
// moment something on it is wrong.
func Collect(ctx context.Context, a *Allowlist, r Reader, cluster string) Payload {
	p := Payload{
		Cluster:   cluster,
		Collected: time.Now().UTC().Format(time.RFC3339),
		Skipped:   map[string]string{},
	}

	for _, c := range a.Collectors {
		var (
			observed []map[string]string
			err      error
		)
		switch c.Source.Kind {
		case KindMetrics:
			observed, err = collectMetrics(ctx, r, c)
		case KindArtefact:
			observed, err = collectArtefact(ctx, r, c)
		default:
			// Parse refuses this, so reaching it means the allowlist was built
			// by something other than Parse.
			err = fmt.Errorf("unimplemented source kind %q", c.Source.Kind)
		}
		if err != nil {
			p.Skipped[c.Name] = err.Error()
			continue
		}
		if len(observed) == 0 {
			p.Skipped[c.Name] = "the query matched nothing"
			continue
		}
		for _, o := range observed {
			// Project is the only path to a payload. Nothing below appends an
			// observed map directly, and nothing should.
			p.Records = append(p.Records, Record{Collector: c.Name, Fields: c.Project(o)})
		}
	}
	return p
}

// sampleLine matches one Prometheus text-exposition sample: a metric name, an
// optional label set, and a value.
var sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{([^}]*)\})?\s+(\S+)`)

// labelPair matches one label inside that set, handling escaped quotes so a
// label value containing `",` does not split into two.
var labelPair = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

// collectMetrics scrapes a component's own /metrics and returns the samples
// whose metric name is the collector's declared query.
//
// Exact name match, not prefix: `kube_node_status_condition` must not also pick
// up `kube_node_status_condition_info` or anything else a component happens to
// expose under a longer name, because the schema was written for one series and
// the extra labels of another would be projected against it.
func collectMetrics(ctx context.Context, r Reader, c Collector) ([]map[string]string, error) {
	body, err := r.Metrics(ctx, c.Source.Namespace, c.Source.Service, c.Source.Port)
	if err != nil {
		return nil, err
	}

	var out []map[string]string
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := sampleLine.FindStringSubmatch(line)
		if m == nil || m[1] != c.Source.Query {
			continue
		}
		observed := map[string]string{}
		for _, l := range labelPair.FindAllStringSubmatch(m[2], -1) {
			observed[l[1]] = unescape(l[2])
		}
		// The sample's value, addressable by the schema under the query's own
		// name and under "value" -- several collectors want the number rather
		// than a label, and a timestamp series has no label carrying it.
		observed["value"] = m[3]
		observed[c.Source.Query] = m[3]
		out = append(out, observed)
	}
	return out, sc.Err()
}

func unescape(s string) string {
	return strings.NewReplacer(`\\`, `\`, `\"`, `"`, `\n`, "\n").Replace(s)
}

// artefactField matches a `key: value` at any indentation, which is the shape of
// everything an artefact collector reads today.
var artefactField = regexp.MustCompile(`(?m)^\s*-?\s*([A-Za-z][A-Za-z0-9_]*):\s*"?([^"\n#]+?)"?\s*$`)

// collectArtefact reads a platform-rendered file and picks out the schema's
// fields.
//
// Deliberately not a YAML parse. The file belongs to the tenant's repository and
// may carry anything; parsing it into a structure would make every value in it
// reachable, and the point of this collector is that only the named fields are.
func collectArtefact(ctx context.Context, r Reader, c Collector) ([]map[string]string, error) {
	b, err := r.Artefact(ctx, c.Source.Path)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, f := range c.Schema {
		want[f] = true
	}
	observed := map[string]string{}
	for _, m := range artefactField.FindAllStringSubmatch(string(b), -1) {
		if want[m[1]] {
			// First occurrence wins. A bundle.yaml carries two targetRevisions
			// -- the chart version and the branch this repository's values are
			// read from -- and the second is not what the schema means.
			if _, seen := observed[m[1]]; !seen {
				observed[m[1]] = strings.TrimSpace(m[2])
			}
		}
	}
	if len(observed) == 0 {
		return nil, nil
	}
	return []map[string]string{observed}, nil
}

// ── Readers ─────────────────────────────────────────────────────────────────

// ClusterReader reads from inside the box: component metrics over the cluster
// network, artefacts from the checked-out repository.
//
// No Kubernetes API access at all. ADR-077 gives the agent a ClusterRole with
// `create` on tokenreviews and subjectaccessreviews and read access to nothing,
// and a reader that listed Services to find an endpoint would need exactly the
// verb that ClusterRole withholds. The Service DNS name is constructed instead.
type ClusterReader struct {
	HTTP      *http.Client
	GitopsDir string
}

func (c *ClusterReader) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (c *ClusterReader) Metrics(ctx context.Context, namespace, service string, port int) (string, error) {
	url := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/metrics", service, namespace, port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return string(b), err
}

func (c *ClusterReader) Artefact(_ context.Context, pattern string) ([]byte, error) {
	if c.GitopsDir == "" {
		return nil, fmt.Errorf("no repository to read %s from", pattern)
	}
	matches, err := filepath.Glob(filepath.Join(c.GitopsDir, pattern))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("%s matched no file", pattern)
	}
	// Sorted so a box with several clusters reads the same one every run, and a
	// payload does not appear to change because a directory listing did.
	sort.Strings(matches)
	return os.ReadFile(matches[0])
}
