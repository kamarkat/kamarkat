package tenant

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeRepo is one repository in a registry that behaves like ghcr: an anonymous
// request for a private OR a missing repository is refused the same way.
type fakeRepo struct {
	public  bool
	readers map[string]bool // users whose token may read it
	chart   []byte          // a chart archive, served as the only tag "1.0.0"
	broken  bool            // answers 503
}

func fakeRegistry(t *testing.T, repos map[string]*fakeRepo) string {
	t.Helper()
	srv := httptest.NewServer(nil)
	host := strings.TrimPrefix(srv.URL, "http://")
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			user := "anonymous"
			if u, p, ok := r.BasicAuth(); ok {
				if p != "tok-"+u {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				user = u
			}
			json.NewEncoder(w).Encode(map[string]string{"token": user})
			return
		}
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if auth == "" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="http://%s/token",service="%s"`, host, host))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/v2/")
		var name, op string
		for _, suffix := range []string{"/tags/list", "/manifests/", "/blobs/"} {
			if i := strings.Index(rest, suffix); i >= 0 {
				name, op = rest[:i], suffix
				break
			}
		}
		repo, exists := repos[name]
		switch {
		case exists && repo.broken:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		case !exists && auth == "anonymous":
			w.WriteHeader(http.StatusForbidden)
			return
		case !exists:
			w.WriteHeader(http.StatusNotFound)
			return
		case !repo.public && !repo.readers[auth]:
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch op {
		case "/tags/list":
			tags := []string{}
			if repo.chart != nil {
				tags = []string{"1.0.0"}
			}
			json.NewEncoder(w).Encode(map[string]any{"name": name, "tags": tags})
		case "/manifests/":
			json.NewEncoder(w).Encode(map[string]any{"layers": []map[string]string{{
				"mediaType": "application/vnd.cncf.helm.chart.content.v1.tar+gzip", "digest": "sha256:chart"}}})
		case "/blobs/":
			w.Write(repo.chart)
		}
	})
	t.Cleanup(srv.Close)
	old := registryScheme
	registryScheme = "http"
	t.Cleanup(func() { registryScheme = old })
	return host
}

func chartArchive(t *testing.T, name, values string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: name + "/values.yaml", Mode: 0o644, Size: int64(len(values))})
	tw.Write([]byte(values))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// fakeIO answers the questions add-app asks and records what happened.
type fakeIO struct {
	visibility map[string]Visibility
	published  bool
	stored     *RegistryCredential
	offered    []*RegistryCredential
	asked      []string
	requests   int
	saved      *RegistryCredential
	said       []string
	loads      int
}

func (f *fakeIO) AskVisibility(kind string, _ []Package) (Visibility, error) {
	f.asked = append(f.asked, kind)
	return f.visibility[kind], nil
}
func (f *fakeIO) AskPublished(p Package) (bool, error) {
	f.asked = append(f.asked, "published:"+p.Name)
	return f.published, nil
}
func (f *fakeIO) LoadCredential(context.Context) (*RegistryCredential, error) {
	f.loads++
	return f.stored, nil
}
func (f *fakeIO) RequestCredential(string) (*RegistryCredential, error) {
	f.requests++
	if len(f.offered) == 0 {
		return nil, fmt.Errorf("no more credentials")
	}
	c := f.offered[0]
	f.offered = f.offered[1:]
	return c, nil
}
func (f *fakeIO) SaveCredential(_ context.Context, c *RegistryCredential) error {
	f.saved = c
	return nil
}
func (f *fakeIO) Say(format string, args ...any) {
	f.said = append(f.said, fmt.Sprintf(format, args...))
}

func cred(user string) *RegistryCredential {
	return &RegistryCredential{Username: user, Token: "tok-" + user}
}

func pkg(t *testing.T, host, name string) Package {
	t.Helper()
	p, err := ParsePackage(host+"/"+name, "chart")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The registry fakes are not on ghcr.io, so private-package tests set the host
// they assert on through this: the platform holds credentials for ghcr only.
func asGHCR(t *testing.T, host string) {
	t.Helper()
	old := ghcrHost
	ghcrHost = host
	t.Cleanup(func() { ghcrHost = old })
}

func TestPublicApplicationAsksNothing(t *testing.T) {
	reg := map[string]*fakeRepo{
		"acme/charts/web": {public: true},
		"acme/shop-web":   {public: true},
	}
	host := fakeRegistry(t, reg)
	reg["acme/charts/web"].chart = chartArchive(t, "web", fmt.Sprintf("image:\n  repository: %s/acme/shop-web\n", host))
	io := &fakeIO{}
	d, err := ResolveRegistry(context.Background(), RegistryInput{Charts: []Package{pkg(t, host, "acme/charts/web")}}, io)
	if err != nil {
		t.Fatal(err)
	}
	if d.Charts != Public || d.Images != Public || len(io.asked) != 0 || io.loads != 0 || io.requests != 0 {
		t.Fatalf("decl=%+v asked=%v loads=%d requests=%d; a public app must ask and load nothing", d, io.asked, io.loads, io.requests)
	}
}

func TestFirstPrivateApplicationRequestsAndVerifies(t *testing.T) {
	reg := map[string]*fakeRepo{
		"acme/charts/web": {readers: map[string]bool{"alice": true}},
		"acme/shop-web":   {readers: map[string]bool{"alice": true}},
	}
	host := fakeRegistry(t, reg)
	asGHCR(t, host)
	reg["acme/charts/web"].chart = chartArchive(t, "web", fmt.Sprintf("image:\n  repository: %s/acme/shop-web\n", host))
	io := &fakeIO{visibility: map[string]Visibility{"charts": Private}, offered: []*RegistryCredential{cred("alice")}}
	d, err := ResolveRegistry(context.Background(), RegistryInput{Charts: []Package{pkg(t, host, "acme/charts/web")}}, io)
	if err != nil {
		t.Fatal(err)
	}
	if d.Charts != Private || d.Images != Private || io.requests != 1 || io.saved == nil || io.saved.Username != "alice" {
		t.Fatalf("decl=%+v requests=%d saved=%v", d, io.requests, io.saved)
	}
	if !containsSay(io.said, "verified") {
		t.Fatalf("said %v; access to a published private package must be reported verified", io.said)
	}
}

func TestAnotherPrivateApplicationReusesSilently(t *testing.T) {
	reg := map[string]*fakeRepo{"acme/charts/api": {readers: map[string]bool{"alice": true}}}
	host := fakeRegistry(t, reg)
	asGHCR(t, host)
	reg["acme/charts/api"].chart = chartArchive(t, "api", "replicas: 1\n")
	io := &fakeIO{stored: cred("alice"), visibility: map[string]Visibility{"charts": Private, "images": Public}}
	d, err := ResolveRegistry(context.Background(), RegistryInput{Charts: []Package{pkg(t, host, "acme/charts/api")}}, io)
	if err != nil {
		t.Fatal(err)
	}
	if d.Charts != Private || io.requests != 0 || io.saved != nil || len(io.said) != 0 {
		t.Fatalf("decl=%+v requests=%d saved=%v said=%v; a working credential is reused without a word", d, io.requests, io.saved, io.said)
	}
}

func TestUnpublishedPublicApplicationAsksVisibilityOnly(t *testing.T) {
	host := fakeRegistry(t, map[string]*fakeRepo{})
	io := &fakeIO{visibility: map[string]Visibility{"charts": Public, "images": Public}}
	d, err := ResolveRegistry(context.Background(), RegistryInput{Charts: []Package{pkg(t, host, "acme/charts/new")}}, io)
	if err != nil {
		t.Fatal(err)
	}
	if d.Charts != Public || d.Images != Public || io.loads != 0 || io.requests != 0 {
		t.Fatalf("decl=%+v loads=%d requests=%d", d, io.loads, io.requests)
	}
}

func TestUnpublishedPrivateApplicationIsNeverReportedVerified(t *testing.T) {
	host := fakeRegistry(t, map[string]*fakeRepo{})
	asGHCR(t, host)
	io := &fakeIO{visibility: map[string]Visibility{"charts": Private, "images": Private}, offered: []*RegistryCredential{cred("alice")}}
	if _, err := ResolveRegistry(context.Background(), RegistryInput{Charts: []Package{pkg(t, host, "acme/charts/new")}}, io); err != nil {
		t.Fatal(err)
	}
	if io.saved == nil || containsSay(io.said, "verified") {
		t.Fatalf("saved=%v said=%v; an unpublished package's access must not be reported verified", io.saved, io.said)
	}
}

func TestDeclaredVisibilityIsNotAsked(t *testing.T) {
	host := fakeRegistry(t, map[string]*fakeRepo{})
	io := &fakeIO{}
	d, err := ResolveRegistry(context.Background(), RegistryInput{
		Charts: []Package{pkg(t, host, "acme/charts/new")}, DeclaredCharts: Public, DeclaredImages: Public}, io)
	if err != nil || d.Charts != Public || len(io.asked) != 0 {
		t.Fatalf("decl=%+v asked=%v err=%v", d, io.asked, err)
	}
}

func TestMixedVisibilityAsksOnlyForImages(t *testing.T) {
	reg := map[string]*fakeRepo{
		"acme/charts/web":  {public: true},
		"acme/private-web": {readers: map[string]bool{"alice": true}},
	}
	host := fakeRegistry(t, reg)
	asGHCR(t, host)
	reg["acme/charts/web"].chart = chartArchive(t, "web", fmt.Sprintf("image:\n  repository: %s/acme/private-web\n", host))
	io := &fakeIO{offered: []*RegistryCredential{cred("alice")}}
	d, err := ResolveRegistry(context.Background(), RegistryInput{Charts: []Package{pkg(t, host, "acme/charts/web")}}, io)
	if err != nil {
		t.Fatal(err)
	}
	if d.Charts != Public || d.Images != Private || len(io.asked) != 0 || io.saved == nil {
		t.Fatalf("decl=%+v asked=%v saved=%v; a public chart with private images needs image access only", d, io.asked, io.saved)
	}
}

func TestRevokedCredentialIsReplacedWithOneThatKeepsOtherAppsWorking(t *testing.T) {
	reg := map[string]*fakeRepo{
		"acme/charts/web":   {readers: map[string]bool{"bob": true, "carol": true}},
		"acme/charts/other": {readers: map[string]bool{"carol": true}},
	}
	host := fakeRegistry(t, reg)
	asGHCR(t, host)
	reg["acme/charts/web"].chart = chartArchive(t, "web", "replicas: 1\n")
	reg["acme/charts/other"].chart = chartArchive(t, "other", "replicas: 1\n")
	io := &fakeIO{
		stored:     cred("revoked"),
		published:  true,
		visibility: map[string]Visibility{"charts": Private, "images": Public},
		// bob reads this app but not the other one; carol reads both.
		offered: []*RegistryCredential{cred("bob"), cred("carol")},
	}
	_, err := ResolveRegistry(context.Background(), RegistryInput{
		Charts: []Package{pkg(t, host, "acme/charts/web")},
		Others: []OtherPrivateApp{{Name: "stg/other", Packages: []Package{pkg(t, host, "acme/charts/other")}}},
	}, io)
	if err != nil {
		t.Fatal(err)
	}
	if io.requests != 2 || io.saved == nil || io.saved.Username != "carol" {
		t.Fatalf("requests=%d saved=%v; a replacement that breaks another app must be refused", io.requests, io.saved)
	}
}

func TestUnavailableRegistryAsksAndWritesNothing(t *testing.T) {
	host := fakeRegistry(t, map[string]*fakeRepo{"acme/charts/web": {broken: true}})
	io := &fakeIO{}
	_, err := ResolveRegistry(context.Background(), RegistryInput{Charts: []Package{pkg(t, host, "acme/charts/web")}}, io)
	if err == nil || !strings.Contains(err.Error(), "could not be reached") || len(io.asked) != 0 || io.loads != 0 {
		t.Fatalf("err=%v asked=%v loads=%d", err, io.asked, io.loads)
	}
}

func TestParsePackage(t *testing.T) {
	for ref, want := range map[string]string{
		"oci://ghcr.io/acme/charts/web":  "ghcr.io/acme/charts/web",
		"ghcr.io/acme/web:1.2@sha256:ab": "ghcr.io/acme/web",
		"nginx:1.27":                     "registry-1.docker.io/library/nginx",
		"docker.io/bitnami/redis":        "registry-1.docker.io/bitnami/redis",
	} {
		p, err := ParsePackage(ref, "image")
		if err != nil || p.String() != want {
			t.Errorf("%s -> %v, %v; want %s", ref, p, err, want)
		}
	}
	if _, err := ParsePackage("ghcr.io/Acme/Web", "image"); err == nil {
		t.Error("an invalid reference was accepted")
	}
}

func containsSay(said []string, word string) bool {
	for _, s := range said {
		if strings.Contains(s, word) {
			return true
		}
	}
	return false
}
