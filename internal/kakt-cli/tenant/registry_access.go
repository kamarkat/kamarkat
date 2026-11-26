package tenant

// Package accessibility for `kakt tenant add-app`.
//
// A registry credential is requested only when an application cannot be
// deployed without one, so the first question is what a package's registry
// actually answers. An anonymous refusal is NOT evidence that a package is
// private: ghcr answers a private package and an unpublished one identically,
// and a rate limit, a 5xx or a network failure says nothing about visibility at
// all. Every result below is therefore one of a small closed set, and each maps
// to one action in add-app -- never to "assume private and ask for a token".

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Access is what a registry said about one package.
type Access int

const (
	// AccessOK: the package exists and the requester may read it.
	AccessOK Access = iota
	// AccessMissing: the registry says the package does not exist.
	AccessMissing
	// AccessDenied: the registry refused. For an anonymous request that is
	// "private, or not published"; ghcr does not say which.
	AccessDenied
	// AccessUnavailable: rate limited, a server error or no answer. Nothing
	// about visibility can be concluded.
	AccessUnavailable
)

func (a Access) String() string {
	return [...]string{"accessible", "not published", "denied", "unavailable"}[a]
}

// RegistryCredential is a registry login: a user and a token with read access.
type RegistryCredential struct {
	Username string
	Token    string
}

func (c *RegistryCredential) empty() bool { return c == nil || c.Token == "" }

// Package is one registry repository an application pulls: a chart or an image.
type Package struct {
	Host string // e.g. ghcr.io
	Name string // e.g. acme/charts/shop
	Kind string // "chart" or "image"
}

func (p Package) String() string { return p.Host + "/" + p.Name }

// ghcrHost is the one registry the platform holds credentials for. A variable
// so tests can stand a fake registry in for it.
var ghcrHost = "ghcr.io"

// registryScheme is a variable so tests can point the client at a plain-HTTP fake.
var registryScheme = "https"

var registryHTTP = &http.Client{Timeout: 20 * time.Second}

// ParsePackage turns an image or chart reference into a Package. Tags and
// digests are dropped: access is a property of the repository.
func ParsePackage(ref, kind string) (Package, error) {
	r := strings.TrimSpace(strings.TrimPrefix(ref, "oci://"))
	if r == "" {
		return Package{}, fmt.Errorf("empty %s reference", kind)
	}
	if i := strings.Index(r, "@"); i >= 0 {
		r = r[:i]
	}
	if i := strings.LastIndex(r, ":"); i > strings.LastIndex(r, "/") {
		r = r[:i]
	}
	host, name := "registry-1.docker.io", r
	if parts := strings.SplitN(r, "/", 2); len(parts) == 2 &&
		(strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		host, name = parts[0], parts[1]
	} else if !strings.Contains(r, "/") {
		name = "library/" + r
	}
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	if !regexp.MustCompile(`^[a-z0-9]+([._/-][a-z0-9]+)*$`).MatchString(name) {
		return Package{}, fmt.Errorf("%q is not a valid %s reference", ref, kind)
	}
	return Package{Host: host, Name: name, Kind: kind}, nil
}

// CheckPackage asks the registry whether the package can be read, anonymously
// when cred is empty.
func CheckPackage(ctx context.Context, p Package, cred *RegistryCredential) Access {
	status, err := registryGet(ctx, p, "/v2/"+p.Name+"/tags/list", cred, nil)
	if err != nil {
		return AccessUnavailable
	}
	return classify(status)
}

func classify(status int) Access {
	switch {
	case status == http.StatusOK:
		return AccessOK
	case status == http.StatusNotFound:
		return AccessMissing
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return AccessDenied
	default:
		return AccessUnavailable
	}
}

// registryGet performs the OCI token dance: the registry's 401 names the token
// realm, a token is requested (with basic auth when a credential is given), and
// the request is repeated with it. body, when non-nil, receives the response.
func registryGet(ctx context.Context, p Package, pathAndQuery string, cred *RegistryCredential, body io.Writer, accept ...string) (int, error) {
	do := func(token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			registryScheme+"://"+p.Host+pathAndQuery, nil)
		if err != nil {
			return nil, err
		}
		for _, a := range accept {
			req.Header.Add("Accept", a)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return registryHTTP.Do(req)
	}
	resp, err := do("")
	if err != nil {
		return 0, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		token, status, err := fetchToken(ctx, challenge, p, cred)
		if err != nil {
			return 0, err
		}
		if token == "" {
			return status, nil
		}
		if resp, err = do(token); err != nil {
			return 0, err
		}
	}
	defer resp.Body.Close()
	if body != nil && resp.StatusCode == http.StatusOK {
		if _, err := io.Copy(body, io.LimitReader(resp.Body, 64<<20)); err != nil {
			return 0, err
		}
	}
	return resp.StatusCode, nil
}

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// fetchToken returns a bearer token, or the status the token endpoint refused
// with. A refused token request is a refusal of the package, not an outage.
func fetchToken(ctx context.Context, challenge string, p Package, cred *RegistryCredential) (string, int, error) {
	params := map[string]string{}
	for _, m := range challengeParam.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}
	realm := params["realm"]
	if realm == "" {
		return "", http.StatusUnauthorized, nil
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", 0, err
	}
	q := u.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", "repository:"+p.Name+":pull")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", 0, err
	}
	if !cred.empty() {
		req.SetBasicAuth(cred.Username, cred.Token)
	}
	resp, err := registryHTTP.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, nil
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", 0, err
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	return t.Token, http.StatusOK, nil
}

// ChartImages reads the image repositories a published chart deploys, from the
// values of its newest tag. ok is false when no published chart can be read --
// the caller then relies on declared visibility rather than guessing.
func ChartImages(ctx context.Context, chart Package, cred *RegistryCredential) (images []Package, ok bool, err error) {
	var tags struct {
		Tags []string `json:"tags"`
	}
	var buf strings.Builder
	status, err := registryGet(ctx, chart, "/v2/"+chart.Name+"/tags/list", cred, &buf)
	if err != nil || status != http.StatusOK {
		return nil, false, err
	}
	if json.Unmarshal([]byte(buf.String()), &tags) != nil || len(tags.Tags) == 0 {
		return nil, false, nil
	}
	sort.Strings(tags.Tags)
	tag := tags.Tags[len(tags.Tags)-1]

	var manifest struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"layers"`
	}
	buf.Reset()
	status, err = registryGet(ctx, chart, "/v2/"+chart.Name+"/manifests/"+tag, cred, &buf,
		"application/vnd.oci.image.manifest.v1+json")
	if err != nil || status != http.StatusOK || json.Unmarshal([]byte(buf.String()), &manifest) != nil {
		return nil, false, err
	}
	for _, l := range manifest.Layers {
		if l.MediaType != "application/vnd.cncf.helm.chart.content.v1.tar+gzip" {
			continue
		}
		var blob strings.Builder
		if status, err := registryGet(ctx, chart, "/v2/"+chart.Name+"/blobs/"+l.Digest, cred, &blob); err != nil || status != http.StatusOK {
			return nil, false, err
		}
		refs, err := valuesImageRefs(blob.String())
		if err != nil {
			return nil, false, nil
		}
		seen := map[string]bool{}
		for _, r := range refs {
			p, err := ParsePackage(r, "image")
			if err != nil || seen[p.String()] {
				continue
			}
			seen[p.String()] = true
			images = append(images, p)
		}
		return images, true, nil
	}
	return nil, false, nil
}

// valuesImageRefs reads <chart>/values.yaml from a chart archive and returns
// every image reference in it: a map carrying `repository` (with an optional
// `registry`) under an image-like key, or a string that names a registry host.
func valuesImageRefs(archive string) ([]string, error) {
	gz, err := gzip.NewReader(strings.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("chart has no values.yaml")
		}
		if err != nil {
			return nil, err
		}
		if path.Base(h.Name) != "values.yaml" || strings.Count(strings.Trim(h.Name, "/"), "/") != 1 {
			continue
		}
		var v any
		if err := yaml.NewDecoder(tr).Decode(&v); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		var refs []string
		collectImages(v, "", &refs)
		return refs, nil
	}
}

var hostRef = regexp.MustCompile(`^[a-z0-9.-]+\.[a-z]{2,}(:[0-9]+)?/[a-z0-9._/-]+([:@][\w.:-]+)?$`)

func collectImages(v any, key string, refs *[]string) {
	switch t := v.(type) {
	case map[string]any:
		if repo, ok := t["repository"].(string); ok && strings.Contains(strings.ToLower(key), "image") && repo != "" {
			if reg, ok := t["registry"].(string); ok && reg != "" {
				repo = strings.TrimSuffix(reg, "/") + "/" + repo
			}
			*refs = append(*refs, repo)
		}
		for k, c := range t {
			collectImages(c, k, refs)
		}
	case []any:
		for _, c := range t {
			collectImages(c, key, refs)
		}
	case string:
		if strings.Contains(strings.ToLower(key), "image") && hostRef.MatchString(t) {
			*refs = append(*refs, t)
		}
	}
}
