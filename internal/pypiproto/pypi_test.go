// Tests for PEP 503 normalization and the PEP 691 JSON side.
package pypiproto

import (
	"bytes"
	"strings"
	"testing"
)

func TestNormalizePEP503Examples(t *testing.T) {
	cases := map[string]string{
		"Django":            "django",
		"Typing_Extensions": "typing-extensions",
		"zope.interface":    "zope-interface",
		"foo--bar__baz":     "foo-bar-baz", // runs collapse to a single dash
		"ruff":              "ruff",
		"A.-_B":             "a-b",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidNormalizedSeparatesCanonicalFromHostile(t *testing.T) {
	for _, name := range []string{"requests", "typing-extensions", "a", "b2", "zope-interface"} {
		if !ValidNormalized(name) {
			t.Errorf("ValidNormalized(%q) = false", name)
		}
	}
	for _, name := range []string{"", "Django", "zope.interface", "-lead", "trail-", "a/b", "..", strings.Repeat("a", 215)} {
		if ValidNormalized(name) {
			t.Errorf("ValidNormalized(%q) = true", name)
		}
	}
}

func TestParseJSONReadsPEP691AndRejectsGarbage(t *testing.T) {
	data := []byte(`{
	  "meta": {"api-version": "1.1"},
	  "name": "requests",
	  "files": [
	    {"filename": "requests-2.32.0-py3-none-any.whl",
	     "url": "https://files.example.test/requests-2.32.0-py3-none-any.whl",
	     "hashes": {"sha256": "deadbeef"},
	     "requires-python": ">=3.8"}
	  ]
	}`)
	p, err := ParseJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "requests" || len(p.Files) != 1 {
		t.Fatalf("parsed %+v", p)
	}
	f := p.Files[0]
	if f.Hashes["sha256"] != "deadbeef" || f.RequiresPython != ">=3.8" {
		t.Fatalf("file fields lost: %+v", f)
	}
	if _, err := ParseJSON([]byte("<html>not json</html>")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestMarshalStoredIsDeterministicAndRoundTrips(t *testing.T) {
	p := NewProject("demo")
	p.Files = []File{
		{Filename: "demo-2.0.0.tar.gz", URL: "u2", Hashes: map[string]string{"sha256": "bb"}},
		{Filename: "demo-1.0.0.tar.gz", URL: "u1", Hashes: map[string]string{"sha256": "aa"}},
	}
	first, err := p.MarshalStored()
	if err != nil {
		t.Fatal(err)
	}
	// Re-marshal with files pre-shuffled the other way: bytes must match.
	p.Files[0], p.Files[1] = p.Files[1], p.Files[0]
	second, _ := p.MarshalStored()
	if !bytes.Equal(first, second) {
		t.Fatal("stored form depends on input order")
	}
	if !bytes.HasSuffix(first, []byte("\n")) {
		t.Fatal("stored form should end with a newline (plain-file ethos)")
	}
	back, err := ParseJSON(first)
	if err != nil {
		t.Fatal(err)
	}
	if back.Name != "demo" || len(back.Files) != 2 || back.Files[0].Filename != "demo-1.0.0.tar.gz" {
		t.Fatalf("round trip lost data: %+v", back)
	}
}

func TestFindFileAndUpsert(t *testing.T) {
	p := NewProject("demo")
	p.Upsert(File{Filename: "demo-1.0.0.tar.gz", URL: "old"})
	p.Upsert(File{Filename: "demo-1.0.0.tar.gz", URL: "new"}) // replace, not duplicate
	p.Upsert(File{Filename: "demo-2.0.0.tar.gz", URL: "other"})
	if len(p.Files) != 2 {
		t.Fatalf("Upsert duplicated: %d files", len(p.Files))
	}
	if f := p.FindFile("demo-1.0.0.tar.gz"); f == nil || f.URL != "new" {
		t.Fatalf("FindFile after Upsert = %+v", f)
	}
	if p.FindFile("absent.tar.gz") != nil {
		t.Fatal("FindFile invented a file")
	}
}

func TestRenderJSONRewritesURLsToMirror(t *testing.T) {
	p := NewProject("requests")
	p.Files = []File{{
		Filename: "requests-2.32.0-py3-none-any.whl",
		URL:      "https://files.example.test/original.whl",
		Hashes:   map[string]string{"sha256": "deadbeef"},
	}}
	out, err := p.RenderJSON("http://127.0.0.1:8417")
	if err != nil {
		t.Fatal(err)
	}
	want := `"http://127.0.0.1:8417/pypi/files/requests/requests-2.32.0-py3-none-any.whl"`
	if !strings.Contains(string(out), want) {
		t.Fatalf("rewritten URL missing:\n%s", out)
	}
	if strings.Contains(string(out), "files.example.test") {
		t.Fatal("upstream URL leaked into the served page")
	}
	if !strings.Contains(string(out), `"sha256": "deadbeef"`) {
		t.Fatal("hashes must survive the rewrite")
	}
}
