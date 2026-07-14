// Tests for the read-through policy. Upstreams are in-process
// httptest servers on 127.0.0.1 — no real network is ever touched, and
// "time passing" is simulated by rewinding file mtimes, never by sleeping.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JaydenCJ/depshelf/internal/integrity"
	"github.com/JaydenCJ/depshelf/internal/store"
)

// fakeRegistry is a minimal in-process npm registry + PyPI simple index.
type fakeRegistry struct {
	*httptest.Server
	hits    atomic.Int64
	tarball []byte
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{tarball: []byte("tarball-bytes-v1")}
	mux := http.NewServeMux()
	mux.HandleFunc("/left-pad", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		fmt.Fprintf(w, `{"name":"left-pad","dist-tags":{"latest":"1.3.0"},"versions":{"1.3.0":{
			"dist":{"tarball":"%s/left-pad/-/left-pad-1.3.0.tgz",
			"integrity":"%s"}}}}`,
			f.URL, integrity.SHA512SRI(f.tarball))
	})
	mux.HandleFunc("/left-pad/-/left-pad-1.3.0.tgz", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		w.Write(f.tarball)
	})
	mux.HandleFunc("/lying-pad", func(w http.ResponseWriter, r *http.Request) {
		// Advertises one digest, serves different bytes: a poisoned mirror.
		fmt.Fprintf(w, `{"versions":{"1.0.0":{"dist":{"tarball":"%s/lying-pad/-/lying-pad-1.0.0.tgz",
			"integrity":"%s"}}}}`, f.URL, integrity.SHA512SRI([]byte("advertised")))
	})
	mux.HandleFunc("/lying-pad/-/lying-pad-1.0.0.tgz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not what was advertised"))
	})
	mux.HandleFunc("/simple/demo/", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if strings.Contains(r.Header.Get("Accept"), "application/vnd.pypi.simple.v1+json") {
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			fmt.Fprintf(w, `{"meta":{"api-version":"1.0"},"name":"demo","files":[
				{"filename":"demo-1.0.0.tar.gz","url":"%s/packages/demo-1.0.0.tar.gz",
				 "hashes":{"sha256":"%s"}}]}`, f.URL, integrity.SHA256Hex(f.tarball))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<a href="../../packages/demo-1.0.0.tar.gz#sha256=%s">demo-1.0.0.tar.gz</a>`,
			integrity.SHA256Hex(f.tarball))
	})
	mux.HandleFunc("/packages/demo-1.0.0.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		w.Write(f.tarball)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func newMirror(t *testing.T, f *fakeRegistry, offline bool) (*Mirror, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Store: st, Offline: offline}
	if f != nil {
		o.NpmUpstream = f.URL
		o.PypiUpstream = f.URL + "/simple"
	}
	return New(o), st
}

// rewindMetadata makes cached metadata look older than the TTL without
// sleeping.
func rewindMetadata(t *testing.T, st *store.Store, eco, name string) {
	t.Helper()
	file := "packument.json"
	if eco == "pypi" {
		file = "index.json"
	}
	path := filepath.Join(st.Root, eco, name, file)
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestNpmPackumentMissFetchesAndCaches(t *testing.T) {
	f := newFakeRegistry(t)
	m, st := newMirror(t, f, false)
	data, src, err := m.NpmPackument(context.Background(), "left-pad")
	if err != nil {
		t.Fatal(err)
	}
	if src != SourceUpstream || !strings.Contains(string(data), `"left-pad"`) {
		t.Fatalf("src=%s data=%q", src, data)
	}
	if _, _, err := st.ReadMetadata("npm", "left-pad"); err != nil {
		t.Fatal("packument not persisted:", err)
	}
}

func TestMetadataTTLGovernsRevalidation(t *testing.T) {
	f := newFakeRegistry(t)
	m, st := newMirror(t, f, false)
	m.NpmPackument(context.Background(), "left-pad")
	// Fresh cache: no upstream traffic.
	before := f.hits.Load()
	_, src, err := m.NpmPackument(context.Background(), "left-pad")
	if err != nil || src != SourceCache {
		t.Fatalf("src=%s err=%v", src, err)
	}
	if f.hits.Load() != before {
		t.Fatal("fresh cache still hit upstream")
	}
	// Expired cache: exactly one revalidation fetch.
	rewindMetadata(t, st, "npm", "left-pad")
	_, src, err = m.NpmPackument(context.Background(), "left-pad")
	if err != nil || src != SourceUpstream {
		t.Fatalf("src=%s err=%v", src, err)
	}
	if f.hits.Load() != before+1 {
		t.Fatal("expired metadata not revalidated")
	}
}

func TestOfflineServesWarmStoreForever(t *testing.T) {
	f := newFakeRegistry(t)
	warm, st := newMirror(t, f, false)
	warm.NpmPackument(context.Background(), "left-pad")
	warm.NpmTarball(context.Background(), "left-pad", "left-pad-1.3.0.tgz")
	rewindMetadata(t, st, "npm", "left-pad") // stale by TTL standards
	f.Close()                                // and the network is gone

	off := New(Options{Store: st, Offline: true})
	if _, src, err := off.NpmPackument(context.Background(), "left-pad"); err != nil || src != SourceCache {
		t.Fatalf("offline should serve any cached metadata: src=%s err=%v", src, err)
	}
	if _, src, err := off.NpmTarball(context.Background(), "left-pad", "left-pad-1.3.0.tgz"); err != nil || src != SourceCache {
		t.Fatalf("offline tarball hit failed: src=%s err=%v", src, err)
	}
}

func TestOfflineMissIsNotFoundWithoutTouchingNetwork(t *testing.T) {
	m, _ := newMirror(t, nil, true) // no upstream exists at all
	if _, _, err := m.NpmPackument(context.Background(), "left-pad"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("npm metadata miss: %v", err)
	}
	if _, _, err := m.NpmTarball(context.Background(), "left-pad", "left-pad-1.0.0.tgz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("npm tarball miss: %v", err)
	}
	if _, _, err := m.PypiProject(context.Background(), "demo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pypi miss: %v", err)
	}
	// Zero-valued options got the documented defaults (which were never dialed).
	if m.npmUpstream != DefaultNpmUpstream || m.pypiUpstream != DefaultPypiUpstream {
		t.Fatalf("defaults: %s %s", m.npmUpstream, m.pypiUpstream)
	}
	if m.ttl != 15*time.Minute || m.client == nil {
		t.Fatal("ttl/client defaults not applied")
	}
}

func TestNpmPackumentUpstream404IsAuthoritative(t *testing.T) {
	f := newFakeRegistry(t)
	m, st := newMirror(t, f, false)
	// Even with a stale cached copy, a 404 means unpublished — do not
	// resurrect the package from cache.
	st.WriteMetadata("npm", "gone-pkg", []byte(`{"name":"gone-pkg"}`))
	rewindMetadata(t, st, "npm", "gone-pkg")
	_, _, err := m.NpmPackument(context.Background(), "gone-pkg")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestNpmPackumentDeadUpstreamStaleFallback(t *testing.T) {
	f := newFakeRegistry(t)
	m, st := newMirror(t, f, false)
	if _, _, err := m.NpmPackument(context.Background(), "left-pad"); err != nil {
		t.Fatal(err)
	}
	rewindMetadata(t, st, "npm", "left-pad")
	f.Close() // network goes away
	data, src, err := m.NpmPackument(context.Background(), "left-pad")
	if err != nil || src != SourceStale {
		t.Fatalf("src=%s err=%v", src, err)
	}
	if !strings.Contains(string(data), "left-pad") {
		t.Fatal("stale body wrong")
	}
	// With nothing cached, the same outage is a typed UpstreamError.
	cold := New(Options{Store: mustOpen(t), NpmUpstream: f.URL})
	_, _, err = cold.NpmPackument(context.Background(), "left-pad")
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want UpstreamError", err)
	}
}

func mustOpen(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestNpmTarballDownloadsVerifiesAndCaches(t *testing.T) {
	f := newFakeRegistry(t)
	m, _ := newMirror(t, f, false)
	path, src, err := m.NpmTarball(context.Background(), "left-pad", "left-pad-1.3.0.tgz")
	if err != nil || src != SourceUpstream {
		t.Fatalf("src=%s err=%v", src, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "tarball-bytes-v1" {
		t.Fatalf("stored %q", data)
	}
	// Second request must come from disk, not upstream.
	before := f.hits.Load()
	_, src, err = m.NpmTarball(context.Background(), "left-pad", "left-pad-1.3.0.tgz")
	if err != nil || src != SourceCache || f.hits.Load() != before {
		t.Fatalf("cached tarball re-fetched: src=%s hits=%d", src, f.hits.Load()-before)
	}
}

func TestNpmTarballRefusesPoisonAndUnpublishedFiles(t *testing.T) {
	f := newFakeRegistry(t)
	m, st := newMirror(t, f, false)
	// Bytes that do not match the advertised digest never reach the store.
	_, _, err := m.NpmTarball(context.Background(), "lying-pad", "lying-pad-1.0.0.tgz")
	if err == nil || !strings.Contains(err.Error(), "integrity mismatch") {
		t.Fatalf("poisoned tarball accepted: %v", err)
	}
	if _, ok, _ := st.ArtifactPath("npm", "lying-pad", "lying-pad-1.0.0.tgz"); ok {
		t.Fatal("poisoned tarball persisted to the store")
	}
	// Filenames no version publishes 404 — the mirror is not an open proxy.
	_, _, err = m.NpmTarball(context.Background(), "left-pad", "left-pad-9.9.9.tgz")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPypiProjectPrefersPEP691JSON(t *testing.T) {
	f := newFakeRegistry(t)
	m, st := newMirror(t, f, false)
	p, src, err := m.PypiProject(context.Background(), "demo")
	if err != nil || src != SourceUpstream {
		t.Fatalf("src=%s err=%v", src, err)
	}
	if len(p.Files) != 1 || p.Files[0].Filename != "demo-1.0.0.tar.gz" {
		t.Fatalf("files: %+v", p.Files)
	}
	// The stored form is canonical PEP 691 JSON regardless of what the
	// upstream spoke.
	raw, _, err := st.ReadMetadata("pypi", "demo")
	if err != nil || !strings.Contains(string(raw), `"api-version"`) {
		t.Fatalf("stored form wrong: %v %q", err, raw)
	}
}

func TestPypiProjectParsesLegacyHTMLUpstream(t *testing.T) {
	f := newFakeRegistry(t)
	// This upstream ignores Accept and always answers HTML with relative
	// hrefs, like many older mirrors.
	htmlOnly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/simple/demo/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<a href="../../packages/demo-1.0.0.tar.gz#sha256=%s">demo-1.0.0.tar.gz</a>`,
			integrity.SHA256Hex(f.tarball))
	}))
	defer htmlOnly.Close()
	m := New(Options{Store: mustOpen(t), PypiUpstream: htmlOnly.URL + "/simple"})
	p, _, err := m.PypiProject(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 1 {
		t.Fatalf("files: %+v", p.Files)
	}
	if p.Files[0].URL != htmlOnly.URL+"/packages/demo-1.0.0.tar.gz" {
		t.Fatalf("relative href not resolved: %q", p.Files[0].URL)
	}
}

func TestPypiFileDownloadsWithSha256Verification(t *testing.T) {
	f := newFakeRegistry(t)
	m, _ := newMirror(t, f, false)
	path, src, err := m.PypiFile(context.Background(), "demo", "demo-1.0.0.tar.gz")
	if err != nil || src != SourceUpstream {
		t.Fatalf("src=%s err=%v", src, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "tarball-bytes-v1" {
		t.Fatalf("stored %q", data)
	}
	sidecar, err := os.ReadFile(path + ".sha256")
	if err != nil || !strings.HasPrefix(string(sidecar), integrity.SHA256Hex(f.tarball)) {
		t.Fatalf("sidecar wrong: %v %q", err, sidecar)
	}
}

func TestPypiNotFoundPaths(t *testing.T) {
	f := newFakeRegistry(t)
	m, _ := newMirror(t, f, false)
	// A file no index entry mentions must not be proxied.
	if _, _, err := m.PypiFile(context.Background(), "demo", "demo-9.9.9.tar.gz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unlisted file: %v", err)
	}
	// An upstream 404 for the whole project is authoritative.
	if _, _, err := m.PypiProject(context.Background(), "no-such-project"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing project: %v", err)
	}
}

func TestPypiProjectDeadUpstreamFallsBackToStale(t *testing.T) {
	f := newFakeRegistry(t)
	m, st := newMirror(t, f, false)
	if _, _, err := m.PypiProject(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	rewindMetadata(t, st, "pypi", "demo")
	f.Close()
	p, src, err := m.PypiProject(context.Background(), "demo")
	if err != nil || src != SourceStale {
		t.Fatalf("src=%s err=%v", src, err)
	}
	if len(p.Files) != 1 {
		t.Fatal("stale project empty")
	}
}
