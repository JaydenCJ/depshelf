// HTTP surface tests: routing, rewriting, content negotiation, redirects,
// and the allowlist/validation gates. The upstream is an in-process
// httptest server; depshelf's handler is exercised via httptest.NewRequest.
package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JaydenCJ/depshelf/internal/allowlist"
	"github.com/JaydenCJ/depshelf/internal/integrity"
	"github.com/JaydenCJ/depshelf/internal/mirror"
	"github.com/JaydenCJ/depshelf/internal/store"
)

const tarballBody = "tarball-bytes"

func newUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var upstream *httptest.Server
	mux.HandleFunc("/left-pad", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"name":"left-pad","versions":{"1.3.0":{"dist":{
			"tarball":"%s/left-pad/-/left-pad-1.3.0.tgz","integrity":"%s"}}}}`,
			upstream.URL, integrity.SHA512SRI([]byte(tarballBody)))
	})
	mux.HandleFunc("/left-pad/-/left-pad-1.3.0.tgz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, tarballBody)
	})
	mux.HandleFunc("/simple/demo/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		fmt.Fprintf(w, `{"meta":{"api-version":"1.0"},"name":"demo","files":[
			{"filename":"demo-1.0.0.tar.gz","url":"%s/packages/demo-1.0.0.tar.gz",
			 "hashes":{"sha256":"%s"}}]}`, upstream.URL, integrity.SHA256Hex([]byte(tarballBody)))
	})
	mux.HandleFunc("/packages/demo-1.0.0.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, tarballBody)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	upstream = httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	return upstream
}

// newServer wires a full stack against the fake upstream. allow may be nil.
func newServer(t *testing.T, allow *allowlist.List, publicURL string) *Server {
	t.Helper()
	up := newUpstream(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := mirror.New(mirror.Options{
		Store:        st,
		NpmUpstream:  up.URL,
		PypiUpstream: up.URL + "/simple",
	})
	return New(Config{Mirror: m, Store: st, Allow: allow, PublicURL: publicURL})
}

func do(t *testing.T, s *Server, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = "127.0.0.1:8417"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestStatusHealthAndUnknownRoutes(t *testing.T) {
	s := newServer(t, nil, "")
	rec := do(t, s, "GET", "/", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"depshelf"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"mode":"read-through"`) {
		t.Fatal("mode missing from status")
	}
	rec = do(t, s, "GET", "/healthz", nil)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Fatalf("healthz: %d %q", rec.Code, rec.Body.String())
	}
	if rec = do(t, s, "GET", "/cargo/serde", nil); rec.Code != 404 {
		t.Fatalf("unknown route -> %d", rec.Code)
	}
}

func TestNpmPackumentRewritesTarballsToRequestHost(t *testing.T) {
	s := newServer(t, nil, "")
	rec := do(t, s, "GET", "/npm/left-pad", nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"http://127.0.0.1:8417/npm/left-pad/-/left-pad-1.3.0.tgz"`) {
		t.Fatalf("tarball not rewritten to request host:\n%s", rec.Body.String())
	}
	if rec.Header().Get("X-Depshelf-Source") != "upstream" {
		t.Fatalf("source header = %q", rec.Header().Get("X-Depshelf-Source"))
	}
	// Second request is a cache hit and the header says so.
	rec = do(t, s, "GET", "/npm/left-pad", nil)
	if rec.Header().Get("X-Depshelf-Source") != "cache" {
		t.Fatalf("second source header = %q", rec.Header().Get("X-Depshelf-Source"))
	}
	// --public-url wins over the request Host when configured.
	s2 := newServer(t, nil, "http://shelf.example.test:8417/")
	rec = do(t, s2, "GET", "/npm/left-pad", nil)
	if !strings.Contains(rec.Body.String(), `"http://shelf.example.test:8417/npm/left-pad/-/`) {
		t.Fatalf("--public-url not honored:\n%s", rec.Body.String())
	}
}

func TestNpmTarballServedWithOctetStream(t *testing.T) {
	s := newServer(t, nil, "")
	rec := do(t, s, "GET", "/npm/left-pad/-/left-pad-1.3.0.tgz", nil)
	if rec.Code != 200 || rec.Body.String() != tarballBody {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestNpmScopedNameWithEncodedSlash(t *testing.T) {
	// npm clients request scoped packages as /@scope%2fname; the decoded
	// and literal forms must route identically (both 404 at this upstream,
	// but through the same code path — not the generic route-404).
	s := newServer(t, nil, "")
	encoded := do(t, s, "GET", "/npm/@no-such%2fpkg", nil)
	literal := do(t, s, "GET", "/npm/@no-such/pkg", nil)
	if encoded.Code != literal.Code {
		t.Fatalf("encoded %d vs literal %d", encoded.Code, literal.Code)
	}
	if encoded.Code != 404 {
		t.Fatalf("code = %d, want upstream 404", encoded.Code)
	}
}

func TestNpmRequestValidation(t *testing.T) {
	s := newServer(t, nil, "")
	// Hostile package names never reach the mirror.
	for _, target := range []string{"/npm/..", "/npm/UPPER", "/npm/.hidden"} {
		if rec := do(t, s, "GET", target, nil); rec.Code != 400 {
			t.Errorf("%s -> %d, want 400", target, rec.Code)
		}
	}
	// Hostile tarball filenames are rejected before any I/O.
	if rec := do(t, s, "GET", "/npm/left-pad/-/.hidden.tgz", nil); rec.Code != 400 {
		t.Fatalf("hostile filename -> %d", rec.Code)
	}
	// Publishing is out of scope: writes are refused.
	if rec := do(t, s, "PUT", "/npm/left-pad", nil); rec.Code != 405 {
		t.Fatalf("PUT -> %d", rec.Code)
	}
}

func TestNpmAuditEndpointsAnswerEmpty(t *testing.T) {
	// npm install POSTs advisory checks; an empty document keeps installs
	// clean without inventing security data the mirror cannot know.
	s := newServer(t, nil, "")
	for _, target := range []string{
		"/npm/-/npm/v1/security/advisories/bulk",
		"/npm/-/npm/v1/security/audits/quick",
	} {
		rec := do(t, s, "POST", target, nil)
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "{}" {
			t.Errorf("%s -> %d %q", target, rec.Code, rec.Body.String())
		}
	}
}

func TestAllowlistBlocksNpmAndPypi(t *testing.T) {
	allow, err := allowlist.Parse(strings.NewReader("npm:@babel/*\npypi:requests\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(t, allow, "")
	if rec := do(t, s, "GET", "/npm/left-pad", nil); rec.Code != 403 {
		t.Fatalf("npm deny -> %d", rec.Code)
	}
	if rec := do(t, s, "GET", "/pypi/simple/demo/", nil); rec.Code != 403 {
		t.Fatalf("pypi deny -> %d", rec.Code)
	}
	if rec := do(t, s, "GET", "/pypi/files/demo/demo-1.0.0.tar.gz", nil); rec.Code != 403 {
		t.Fatalf("pypi file deny -> %d", rec.Code)
	}
}

func TestPypiProjectHTMLPage(t *testing.T) {
	s := newServer(t, nil, "")
	rec := do(t, s, "GET", "/pypi/simple/demo/", nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q", ct)
	}
	want := `href="http://127.0.0.1:8417/pypi/files/demo/demo-1.0.0.tar.gz#sha256=` +
		integrity.SHA256Hex([]byte(tarballBody)) + `"`
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("page missing rewritten href:\n%s", rec.Body.String())
	}
}

func TestPypiContentNegotiationPEP691(t *testing.T) {
	s := newServer(t, nil, "")
	rec := do(t, s, "GET", "/pypi/simple/demo/", map[string]string{
		"Accept": "application/vnd.pypi.simple.v1+json",
	})
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.pypi.simple.v1+json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `"filename": "demo-1.0.0.tar.gz"`) {
		t.Fatalf("JSON body:\n%s", rec.Body.String())
	}
}

func TestPypiRedirectsToCanonicalURL(t *testing.T) {
	s := newServer(t, nil, "")
	// PEP 503: non-normalized names redirect to the normalized page.
	rec := do(t, s, "GET", "/pypi/simple/Demo_Project/", nil)
	if rec.Code != 301 || rec.Header().Get("Location") != "/pypi/simple/demo-project/" {
		t.Fatalf("%d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	// Missing trailing slash likewise.
	rec = do(t, s, "GET", "/pypi/simple/demo", nil)
	if rec.Code != 301 || rec.Header().Get("Location") != "/pypi/simple/demo/" {
		t.Fatalf("%d -> %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestPypiFileDownload(t *testing.T) {
	s := newServer(t, nil, "")
	rec := do(t, s, "GET", "/pypi/files/demo/demo-1.0.0.tar.gz", nil)
	if rec.Code != 200 || rec.Body.String() != tarballBody {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Depshelf-Source") != "upstream" {
		t.Fatalf("source = %q", rec.Header().Get("X-Depshelf-Source"))
	}
}

func TestPypiRootIndexListsOnlyCachedProjects(t *testing.T) {
	s := newServer(t, nil, "")
	rec := do(t, s, "GET", "/pypi/simple/", nil)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "demo") {
		t.Fatalf("empty shelf should list nothing: %d %s", rec.Code, rec.Body.String())
	}
	do(t, s, "GET", "/pypi/simple/demo/", nil) // warm the cache
	rec = do(t, s, "GET", "/pypi/simple/", nil)
	if !strings.Contains(rec.Body.String(), `<a href="/pypi/simple/demo/">demo</a>`) {
		t.Fatalf("cached project missing from root:\n%s", rec.Body.String())
	}
	// And the PEP 691 flavor of the same index.
	rec = do(t, s, "GET", "/pypi/simple/", map[string]string{
		"Accept": "application/vnd.pypi.simple.v1+json",
	})
	if !strings.Contains(rec.Body.String(), `"name": "demo"`) {
		t.Fatalf("JSON root:\n%s", rec.Body.String())
	}
}

func TestPypiNotFoundAndHostilePaths(t *testing.T) {
	s := newServer(t, nil, "")
	if rec := do(t, s, "GET", "/pypi/simple/no-such-project/", nil); rec.Code != 404 {
		t.Fatalf("upstream 404 -> %d", rec.Code)
	}
	// Nested paths never route to a file; hostile filenames are rejected.
	if rec := do(t, s, "GET", "/pypi/files/demo/nested/evil", nil); rec.Code != 404 {
		t.Fatalf("nested -> %d", rec.Code)
	}
	if rec := do(t, s, "GET", "/pypi/files/demo/.hidden", nil); rec.Code != 400 {
		t.Fatalf("hostile filename -> %d", rec.Code)
	}
}
