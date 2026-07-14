// Tests for npm name validation, packument rewriting and tarball lookup.
package npmproto

import (
	"encoding/json"
	"strings"
	"testing"
)

const samplePackument = `{
  "name": "left-pad",
  "dist-tags": {"latest": "1.3.0"},
  "readme": "pads left",
  "versions": {
    "1.2.0": {
      "name": "left-pad", "version": "1.2.0",
      "dist": {
        "tarball": "https://registry.example.test/left-pad/-/left-pad-1.2.0.tgz",
        "shasum": "f572d396fae9206628714fb2ce00f72e94f2258f"
      }
    },
    "1.3.0": {
      "name": "left-pad", "version": "1.3.0",
      "dist": {
        "tarball": "https://registry.example.test/left-pad/-/left-pad-1.3.0.tgz",
        "integrity": "sha512-Wz4tXpKgLwzC7om8dGl1w0G8HbPKBOZBnRhpUdM/eg7dwOSg2N9G5cikPAEKOHibJM+A2sIY8kkbBpO2C0d0/w==",
        "shasum": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
      }
    }
  }
}`

func TestValidateNameSeparatesRealFromHostile(t *testing.T) {
	for _, name := range []string{"left-pad", "lodash.merge", "@babel/core", "@types/node", "a", "under_score"} {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v", name, err)
		}
	}
	// These are the names a traversal or cache-poisoning attempt would use.
	for _, name := range []string{
		"", "..", "../etc", "a/b", "@scope/../up", ".hidden", "_private",
		"UPPER", "@scope/", "@/name", "name/", strings.Repeat("a", 215),
	} {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) accepted a hostile name", name)
		}
	}
}

func TestEscapeNameAndTarballBasename(t *testing.T) {
	if got := EscapeName("@babel/core"); got != "@babel%2fcore" {
		t.Fatalf("EscapeName = %q", got)
	}
	if got := EscapeName("left-pad"); got != "left-pad" {
		t.Fatalf("EscapeName = %q", got)
	}
	cases := map[string]string{
		"https://registry.example.test/a/-/a-1.0.0.tgz":    "a-1.0.0.tgz",
		"https://registry.example.test/@s/b/-/b-2.0.0.tgz": "b-2.0.0.tgz",
		"https://registry.example.test/x/-/x%2B1.tgz":      "x+1.tgz", // percent-decoding applies
		"imported/local-1.0.0.tgz":                         "local-1.0.0.tgz",
	}
	for in, want := range cases {
		if got := TarballBasename(in); got != want {
			t.Errorf("TarballBasename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRewritePointsEveryTarballAtTheMirror(t *testing.T) {
	out, err := Rewrite([]byte(samplePackument), "http://127.0.0.1:8417", "left-pad")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	versions := doc["versions"].(map[string]any)
	for ver, v := range versions {
		tarball := v.(map[string]any)["dist"].(map[string]any)["tarball"].(string)
		want := "http://127.0.0.1:8417/npm/left-pad/-/left-pad-" + ver + ".tgz"
		if tarball != want {
			t.Errorf("version %s tarball = %q, want %q", ver, tarball, want)
		}
	}
}

func TestRewritePreservesUnrelatedFields(t *testing.T) {
	out, err := Rewrite([]byte(samplePackument), "http://127.0.0.1:8417", "left-pad")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(out, &doc)
	if doc["readme"] != "pads left" {
		t.Fatal("rewrite dropped a passthrough field")
	}
	if doc["dist-tags"].(map[string]any)["latest"] != "1.3.0" {
		t.Fatal("rewrite mangled dist-tags")
	}
	// integrity must survive so clients can still verify downloads
	dist := doc["versions"].(map[string]any)["1.3.0"].(map[string]any)["dist"].(map[string]any)
	if !strings.HasPrefix(dist["integrity"].(string), "sha512-") {
		t.Fatal("rewrite dropped dist.integrity")
	}
}

func TestRewriteScopedNameKeepsScopeInPath(t *testing.T) {
	raw := `{"versions":{"1.0.0":{"dist":{"tarball":"https://registry.example.test/@babel/core/-/core-1.0.0.tgz"}}}}`
	out, err := Rewrite([]byte(raw), "http://127.0.0.1:8417", "@babel/core")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"http://127.0.0.1:8417/npm/@babel/core/-/core-1.0.0.tgz"`) {
		t.Fatalf("scoped rewrite wrong: %s", out)
	}
}

func TestRewriteEdgeCases(t *testing.T) {
	// Versions without dist objects pass through; invalid JSON is fatal.
	raw := `{"versions":{"0.0.1":{"deprecated":"broken"}},"time":{}}`
	if _, err := Rewrite([]byte(raw), "http://127.0.0.1:8417", "x"); err != nil {
		t.Fatalf("packument without dist objects must pass through: %v", err)
	}
	if _, err := Rewrite([]byte("{nope"), "http://127.0.0.1:8417", "x"); err == nil {
		t.Fatal("invalid packument accepted")
	}
}

func TestFindTarballResolvesFilenameToIntegrity(t *testing.T) {
	// The strongest advertised digest wins: sha512 when present…
	ref, err := FindTarball([]byte(samplePackument), "left-pad-1.3.0.tgz")
	if err != nil || ref == nil {
		t.Fatalf("FindTarball: ref=%v err=%v", ref, err)
	}
	if ref.Version != "1.3.0" || ref.Want.Algo != "sha512" {
		t.Fatalf("ref = %+v", ref)
	}
	if ref.URL != "https://registry.example.test/left-pad/-/left-pad-1.3.0.tgz" {
		t.Fatalf("URL = %s", ref.URL)
	}
	// …the legacy sha1 shasum otherwise.
	ref, err = FindTarball([]byte(samplePackument), "left-pad-1.2.0.tgz")
	if err != nil || ref == nil || ref.Want.Algo != "sha1" {
		t.Fatalf("shasum fallback ref = %+v, err=%v", ref, err)
	}
}

func TestFindTarballUnknownFilenameIsNil(t *testing.T) {
	// A nil ref makes the mirror 404 instead of proxying arbitrary paths.
	ref, err := FindTarball([]byte(samplePackument), "evil-9.9.9.tgz")
	if err != nil {
		t.Fatal(err)
	}
	if ref != nil {
		t.Fatalf("unpublished filename resolved: %+v", ref)
	}
}
