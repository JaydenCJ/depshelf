// In-process CLI tests: Main(argv, stdout, stderr) is exercised exactly as
// the binary would be, against temp-dir stores. No network, no subprocess.
package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// run invokes the CLI and captures both streams.
func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Main(args, &out, &errb)
	return code, out.String(), errb.String()
}

// makeTgz builds a minimal npm tarball in memory, with a decoy entry first
// to prove the scanner finds package.json wherever it sits.
func makeTgz(t *testing.T, pkgJSON string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range []struct{ name, body string }{
		{"package/README.md", "# demo\n"},
		{"package/deep/package.json", `{"name":"decoy"}`}, // nested: must be ignored
		{"package/package.json", pkgJSON},
	} {
		tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body))})
		tw.Write([]byte(e.body))
	}
	tw.Close()
	gz.Close()
	path := filepath.Join(t.TempDir(), "demo-pkg-1.2.3.tgz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVersionAndHelpExitZero(t *testing.T) {
	for _, arg := range []string{"version", "--version"} {
		code, out, _ := run(t, arg)
		if code != 0 || strings.TrimSpace(out) != "depshelf 0.1.0" {
			t.Errorf("%s -> code %d, out %q", arg, code, out)
		}
	}
	code, out, _ := run(t, "help")
	if code != 0 || !strings.Contains(out, "depshelf 0.1.0") {
		t.Fatalf("help -> code %d, out %q", code, out)
	}
	// Asking a subcommand for help is not a usage error.
	code, out, _ = run(t, "import", "--help")
	if code != 0 || !strings.Contains(out, "npm|pypi") {
		t.Fatalf("import --help -> code %d, out %q", code, out)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	code, _, errOut := run(t)
	if code != 2 || !strings.Contains(errOut, "Usage:") {
		t.Fatalf("no args: code %d, stderr %q", code, errOut)
	}
	code, _, errOut = run(t, "frobnicate")
	if code != 2 || !strings.Contains(errOut, "frobnicate") {
		t.Fatalf("unknown command: code %d, stderr %q", code, errOut)
	}
}

func TestImportNpmReadsPackageJSONFromTarball(t *testing.T) {
	storeDir := t.TempDir()
	tgz := makeTgz(t, `{"name":"demo-pkg","version":"1.2.3"}`)
	code, out, errOut := run(t, "import", "npm", "--store", storeDir, tgz)
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "imported npm demo-pkg@1.2.3") {
		t.Fatalf("out = %q", out)
	}
	raw, err := os.ReadFile(filepath.Join(storeDir, "npm", "demo-pkg", "packument.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	dist := doc["versions"].(map[string]any)["1.2.3"].(map[string]any)["dist"].(map[string]any)
	if !strings.HasPrefix(dist["integrity"].(string), "sha512-") {
		t.Fatal("generated packument lacks integrity")
	}
	if doc["dist-tags"].(map[string]any)["latest"] != "1.2.3" {
		t.Fatal("dist-tags.latest not set")
	}
	if _, err := os.Stat(filepath.Join(storeDir, "npm", "demo-pkg", "tarballs", "demo-pkg-1.2.3.tgz.sha256")); err != nil {
		t.Fatal("sidecar missing:", err)
	}
}

func TestImportNpmKeepsLatestTagSemverCorrect(t *testing.T) {
	// Import 2.0.0 first, then 1.9.0: latest must stay 2.0.0 even though
	// 1.9.0 arrived later.
	storeDir := t.TempDir()
	for _, v := range []string{"2.0.0", "1.9.0"} {
		tgz := makeTgz(t, `{"name":"demo-pkg","version":"`+v+`"}`)
		if code, _, errOut := run(t, "import", "npm", "--store", storeDir, tgz); code != 0 {
			t.Fatalf("import %s: %s", v, errOut)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(storeDir, "npm", "demo-pkg", "packument.json"))
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	if latest := doc["dist-tags"].(map[string]any)["latest"]; latest != "2.0.0" {
		t.Fatalf("latest = %v", latest)
	}
	if n := len(doc["versions"].(map[string]any)); n != 2 {
		t.Fatalf("versions merged badly: %d entries", n)
	}
}

func TestImportNpmRejectsGarbageTarball(t *testing.T) {
	storeDir := t.TempDir()
	bad := filepath.Join(t.TempDir(), "not-a-tarball-1.0.0.tgz")
	os.WriteFile(bad, []byte("plain text"), 0o644)
	code, _, errOut := run(t, "import", "npm", "--store", storeDir, bad)
	if code != 3 || !strings.Contains(errOut, "gzip") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestImportPypiInfersProjectFromFilename(t *testing.T) {
	storeDir := t.TempDir()
	// Wheel dist segment "Demo_Pkg" must normalize to "demo-pkg".
	whl := filepath.Join(t.TempDir(), "Demo_Pkg-1.0.0-py3-none-any.whl")
	os.WriteFile(whl, []byte("wheel-bytes"), 0o644)
	code, out, errOut := run(t, "import", "pypi", "--store", storeDir, whl)
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "imported pypi demo-pkg") {
		t.Fatalf("out = %q", out)
	}
	raw, err := os.ReadFile(filepath.Join(storeDir, "pypi", "demo-pkg", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"Demo_Pkg-1.0.0-py3-none-any.whl"`) {
		t.Fatalf("index missing file entry:\n%s", raw)
	}
	// Sdist "demo-pkg-1.0.0.tar.gz" cuts at the last dash: same project,
	// second file entry in the same index.
	sdist := filepath.Join(t.TempDir(), "demo-pkg-1.0.0.tar.gz")
	os.WriteFile(sdist, []byte("sdist-bytes"), 0o644)
	code, out, _ = run(t, "import", "pypi", "--store", storeDir, sdist)
	if code != 0 || !strings.Contains(out, "imported pypi demo-pkg") {
		t.Fatalf("code %d, out %q", code, out)
	}
	raw, _ = os.ReadFile(filepath.Join(storeDir, "pypi", "demo-pkg", "index.json"))
	if !strings.Contains(string(raw), `"demo-pkg-1.0.0.tar.gz"`) {
		t.Fatalf("sdist not merged into index:\n%s", raw)
	}
}

func TestImportPypiUnrecognizedExtensionNeedsOverride(t *testing.T) {
	storeDir := t.TempDir()
	odd := filepath.Join(t.TempDir(), "mystery.bin")
	os.WriteFile(odd, []byte("x"), 0o644)
	code, _, errOut := run(t, "import", "pypi", "--store", storeDir, odd)
	if code != 3 || !strings.Contains(errOut, "--name") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	// With the override it succeeds.
	code, _, errOut = run(t, "import", "pypi", "--store", storeDir, "--name", "mystery", odd)
	if code != 0 {
		t.Fatalf("override failed: %s", errOut)
	}
}

func TestImportRequiresKnownEcosystem(t *testing.T) {
	code, _, errOut := run(t, "import", "cargo", "x.crate")
	if code != 2 || !strings.Contains(errOut, "npm|pypi") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestListTextAndJSON(t *testing.T) {
	storeDir := t.TempDir()
	tgz := makeTgz(t, `{"name":"demo-pkg","version":"1.2.3"}`)
	run(t, "import", "npm", "--store", storeDir, tgz)
	code, out, _ := run(t, "list", "--store", storeDir)
	if code != 0 || !strings.Contains(out, "demo-pkg") || !strings.Contains(out, "1 package, 1 artifact") {
		t.Fatalf("text list:\n%s", out)
	}
	code, out, _ = run(t, "list", "--store", storeDir, "--format", "json")
	var pkgs []map[string]any
	if err := json.Unmarshal([]byte(out), &pkgs); err != nil || code != 0 {
		t.Fatalf("json list unparseable (%v):\n%s", err, out)
	}
	if len(pkgs) != 1 || pkgs[0]["name"] != "demo-pkg" || pkgs[0]["artifacts"].(float64) != 1 {
		t.Fatalf("json list content: %+v", pkgs)
	}
	// An empty store is an empty array, not JSON null.
	code, out, _ = run(t, "list", "--store", t.TempDir(), "--format", "json")
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty store: code %d, out %q", code, out)
	}
}

func TestVerifyCleanStoreExitsZero(t *testing.T) {
	storeDir := t.TempDir()
	tgz := makeTgz(t, `{"name":"demo-pkg","version":"1.2.3"}`)
	run(t, "import", "npm", "--store", storeDir, tgz)
	code, out, _ := run(t, "verify", "--store", storeDir)
	if code != 0 || !strings.Contains(out, "1 ok, 0 corrupt") {
		t.Fatalf("code %d, out %q", code, out)
	}
}

func TestVerifyCorruptStoreExitsOne(t *testing.T) {
	storeDir := t.TempDir()
	tgz := makeTgz(t, `{"name":"demo-pkg","version":"1.2.3"}`)
	run(t, "import", "npm", "--store", storeDir, tgz)
	victim := filepath.Join(storeDir, "npm", "demo-pkg", "tarballs", "demo-pkg-1.2.3.tgz")
	if err := os.WriteFile(victim, []byte("bitrot"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "verify", "--store", storeDir)
	if code != 1 || !strings.Contains(out, "corrupt") {
		t.Fatalf("code %d, out %q", code, out)
	}
}

func TestServeErrorExitCodes(t *testing.T) {
	// Unparseable flags are a usage error (2)…
	if code, _, _ := run(t, "serve", "--no-such-flag"); code != 2 {
		t.Fatalf("bad flag: code = %d", code)
	}
	// …while an unbindable listen address is a runtime error (3).
	code, _, errOut := run(t, "serve", "--store", t.TempDir(), "--listen", "not-an-address")
	if code != 3 || !strings.Contains(errOut, "depshelf:") {
		t.Fatalf("bad listen: code %d, stderr %q", code, errOut)
	}
}
