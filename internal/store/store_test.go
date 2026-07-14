// Tests for the plain-file store: layout, atomicity guarantees, sidecars,
// and the path-safety gates.
package store

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JaydenCJ/depshelf/internal/integrity"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOpenCreatesRootAndDocumentedLayout(t *testing.T) {
	// The layout is a documented interface (docs/store-layout.md): people
	// rsync and grep it. Moving a file is a breaking change.
	root := filepath.Join(t.TempDir(), "nested", "store")
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".tmp")); err != nil {
		t.Fatal("temp area missing:", err)
	}
	s.WriteMetadata("npm", "left-pad", []byte("{}"))
	s.WriteMetadata("pypi", "requests", []byte("{}"))
	for _, p := range []string{
		filepath.Join(root, "npm", "left-pad", "packument.json"),
		filepath.Join(root, "pypi", "requests", "index.json"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected file at %s: %v", p, err)
		}
	}
}

func TestMetadataRoundTripReplaceAndMiss(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.ReadMetadata("pypi", "requests"); err != ErrNotCached {
		t.Fatalf("miss err = %v, want ErrNotCached", err)
	}
	if err := s.WriteMetadata("npm", "left-pad", []byte(`{"name":"left-pad"}`)); err != nil {
		t.Fatal(err)
	}
	data, mtime, err := s.ReadMetadata("npm", "left-pad")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"name":"left-pad"}` || mtime.IsZero() {
		t.Fatalf("read back %q, mtime %v", data, mtime)
	}
	// Replacement is atomic and complete.
	if err := s.WriteMetadata("npm", "left-pad", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	data, _, _ = s.ReadMetadata("npm", "left-pad")
	if string(data) != "v2" {
		t.Fatalf("read %q after replace", data)
	}
}

func TestScopedNpmPackageNestsUnderScopeDirectory(t *testing.T) {
	s := newStore(t)
	s.WriteMetadata("npm", "@babel/core", []byte("{}"))
	if _, err := os.Stat(filepath.Join(s.Root, "npm", "@babel", "core", "packument.json")); err != nil {
		t.Fatal("scoped layout wrong:", err)
	}
}

func TestWriteArtifactStoresFileAndSidecar(t *testing.T) {
	s := newStore(t)
	payload := []byte("tarball bytes")
	path, sha, size, err := s.WriteArtifact("npm", "left-pad", "left-pad-1.3.0.tgz",
		bytes.NewReader(payload), integrity.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	if sha != integrity.SHA256Hex(payload) || size != int64(len(payload)) {
		t.Fatalf("sha=%s size=%d", sha, size)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("stored bytes differ: %v", err)
	}
	sidecar, err := os.ReadFile(path + ".sha256")
	if err != nil {
		t.Fatal("sidecar missing:", err)
	}
	// sha256sum -c compatible: "<hex>  <filename>\n"
	want := sha + "  left-pad-1.3.0.tgz\n"
	if string(sidecar) != want {
		t.Fatalf("sidecar = %q, want %q", sidecar, want)
	}
}

func TestWriteArtifactIntegrityGate(t *testing.T) {
	s := newStore(t)
	want := integrity.Hash{Algo: "sha256", Hex: integrity.SHA256Hex([]byte("expected"))}
	_, _, _, err := s.WriteArtifact("npm", "x", "x-1.0.0.tgz", strings.NewReader("tampered"), want)
	if err == nil {
		t.Fatal("integrity mismatch accepted")
	}
	// Nothing may be left behind: no artifact, no sidecar, no temp litter.
	if _, ok, _ := s.ArtifactPath("npm", "x", "x-1.0.0.tgz"); ok {
		t.Fatal("corrupt artifact persisted")
	}
	entries, _ := os.ReadDir(filepath.Join(s.Root, ".tmp"))
	if len(entries) != 0 {
		t.Fatalf("temp area littered: %d entries", len(entries))
	}
	// The same expectation with matching bytes goes through.
	if _, _, _, err := s.WriteArtifact("npm", "x", "x-1.0.0.tgz", strings.NewReader("expected"), want); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ArtifactPath("npm", "x", "x-1.0.0.tgz"); !ok {
		t.Fatal("artifact not stored")
	}
}

func TestPathSafetyGates(t *testing.T) {
	s := newStore(t)
	hostile := []struct{ eco, name string }{
		{"npm", "../escape"},
		{"npm", ".."},
		{"pypi", "../escape"},
		{"pypi", "UPPER"},       // non-normalized
		{"pypi", "dotted.name"}, // non-normalized
		{"cargo", "serde"},      // unknown ecosystem
	}
	for _, h := range hostile {
		if err := s.WriteMetadata(h.eco, h.name, []byte("{}")); err == nil {
			t.Errorf("WriteMetadata(%s, %q) accepted hostile input", h.eco, h.name)
		}
	}
	for _, bad := range []string{
		"", "../up.tgz", "a/b.tgz", ".hidden", "-dash-first",
		"x.tgz.sha256", // reserved: would shadow a sidecar
		strings.Repeat("a", 256),
	} {
		if err := ValidateFilename(bad); err == nil {
			t.Errorf("ValidateFilename(%q) accepted", bad)
		}
	}
	for _, good := range []string{"left-pad-1.3.0.tgz", "requests-2.32.0-py3-none-any.whl", "pkg_1.0+local.tar.gz"} {
		if err := ValidateFilename(good); err != nil {
			t.Errorf("ValidateFilename(%q) = %v", good, err)
		}
	}
}

func TestListSummarizesStoreDeterministically(t *testing.T) {
	s := newStore(t)
	s.WriteMetadata("pypi", "requests", []byte("{}"))
	s.WriteArtifact("pypi", "requests", "requests-2.32.0-py3-none-any.whl", strings.NewReader("12345"), integrity.Hash{})
	s.WriteArtifact("npm", "left-pad", "left-pad-1.3.0.tgz", strings.NewReader("123"), integrity.Hash{})
	s.WriteMetadata("npm", "@babel/core", []byte("{}"))
	pkgs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 3 {
		t.Fatalf("got %d packages: %+v", len(pkgs), pkgs)
	}
	// Sorted: npm before pypi, names ascending; scoped names included.
	if pkgs[0].Name != "@babel/core" || pkgs[1].Name != "left-pad" || pkgs[2].Name != "requests" {
		t.Fatalf("order: %+v", pkgs)
	}
	// Sidecars must not inflate artifact counts or byte totals.
	if pkgs[1].Artifacts != 1 || pkgs[1].Bytes != 3 || pkgs[1].HasMetadata {
		t.Fatalf("left-pad summary: %+v", pkgs[1])
	}
	if !pkgs[2].HasMetadata || pkgs[2].Artifacts != 1 || pkgs[2].Bytes != 5 {
		t.Fatalf("requests summary: %+v", pkgs[2])
	}
}

func TestVerifyAllPassesCleanAndDetectsBitRot(t *testing.T) {
	s := newStore(t)
	path, _, _, _ := s.WriteArtifact("npm", "x", "x-1.0.0.tgz", strings.NewReader("abc"), integrity.Hash{})
	s.WriteArtifact("pypi", "y", "y-1.0.0.tar.gz", strings.NewReader("def"), integrity.Hash{})
	results, err := s.VerifyAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results", len(results))
	}
	for _, r := range results {
		if r.Status != "ok" {
			t.Errorf("%s/%s/%s = %s (%s)", r.Ecosystem, r.Name, r.File, r.Status, r.Detail)
		}
	}
	// Flip one byte on disk: verify must name the exact victim.
	if err := os.WriteFile(path, []byte("abd"), 0o644); err != nil {
		t.Fatal(err)
	}
	results, _ = s.VerifyAll()
	var corrupt *VerifyResult
	for i := range results {
		if results[i].Status == "corrupt" {
			corrupt = &results[i]
		}
	}
	if corrupt == nil || corrupt.File != "x-1.0.0.tgz" || !strings.Contains(corrupt.Detail, "sha256 mismatch") {
		t.Fatalf("bit rot not pinpointed: %+v", results)
	}
}

func TestVerifyAllReportsHandCopiedFilesAsUnverified(t *testing.T) {
	// Files rsynced into the store without a sidecar are legitimate; verify
	// must say "unverified", not fail or guess.
	s := newStore(t)
	dir := filepath.Join(s.Root, "pypi", "manual", "files")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "manual-1.0.0.tar.gz"), []byte("x"), 0o644)
	results, _ := s.VerifyAll()
	if len(results) != 1 || results[0].Status != "unverified" {
		t.Fatalf("results: %+v", results)
	}
}
