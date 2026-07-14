// Tests for allowlist parsing and glob matching — the security boundary
// that decides what the mirror will ever fetch.
package allowlist

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, src string) *List {
	t.Helper()
	l, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return l
}

func TestNilListAllowsEverything(t *testing.T) {
	var l *List
	if !l.Allowed("npm", "anything") || !l.Allowed("pypi", "anything") {
		t.Fatal("nil list must allow everything (no allowlist configured)")
	}
}

func TestEmptyFileDeniesEverything(t *testing.T) {
	// A present-but-empty allowlist is an explicit lockdown, not allow-all:
	// the operator asked for an allowlist and listed nothing.
	l := mustParse(t, "# only comments\n\n")
	if l.Allowed("npm", "left-pad") {
		t.Fatal("empty allowlist allowed a package")
	}
}

func TestExactMatchesStayInTheirEcosystem(t *testing.T) {
	l := mustParse(t, "npm:left-pad\npypi:requests\n")
	if !l.Allowed("npm", "left-pad") || !l.Allowed("pypi", "requests") {
		t.Fatal("exact entries not allowed")
	}
	if l.Allowed("npm", "left-pads") || l.Allowed("npm", "left-pa") {
		t.Fatal("near-miss names must not match")
	}
	if l.Allowed("pypi", "left-pad") || l.Allowed("npm", "requests") {
		t.Fatal("rules bled across ecosystems")
	}
}

func TestStarCrossesSlash(t *testing.T) {
	l := mustParse(t, "npm:@babel/*\n")
	if !l.Allowed("npm", "@babel/core") || !l.Allowed("npm", "@babel/preset-env") {
		t.Fatal("@babel/* must cover the whole scope")
	}
	if l.Allowed("npm", "@types/node") {
		t.Fatal("@babel/* leaked into another scope")
	}
	// A bare '*' really is full-open: it must reach scoped names too.
	open := mustParse(t, "npm:*\n")
	if !open.Allowed("npm", "@scope/pkg") || !open.Allowed("npm", "plain") {
		t.Fatal("npm:* must match every npm name")
	}
}

func TestQuestionMarkMatchesExactlyOneCharacter(t *testing.T) {
	l := mustParse(t, "npm:pkg?\n")
	if !l.Allowed("npm", "pkg1") {
		t.Fatal("pkg? should match pkg1")
	}
	if l.Allowed("npm", "pkg") || l.Allowed("npm", "pkg12") {
		t.Fatal("? must match exactly one character")
	}
}

func TestPypiPatternsNormalizeLikeNames(t *testing.T) {
	// The operator wrote the pretty name; the request path carries the
	// normalized one. Both must land on the same rule.
	l := mustParse(t, "pypi:Typing_Extensions\npypi:zope.interface*\n")
	if !l.Allowed("pypi", "typing-extensions") {
		t.Fatal("normalized name did not match denormalized pattern")
	}
	if !l.Allowed("pypi", "zope-interface") {
		t.Fatal("dotted pattern did not normalize")
	}
}

func TestCommentsAndWhitespaceTolerated(t *testing.T) {
	l := mustParse(t, "  # header\n\n  npm : left-pad \n")
	if !l.Allowed("npm", "left-pad") {
		t.Fatal("padded line not parsed")
	}
}

func TestBadInputIsAHardError(t *testing.T) {
	// A typo in an allowlist must never silently widen or narrow access.
	for _, bad := range []string{"left-pad\n", "npm:\n", "cargo:serde\n"} {
		if _, err := Parse(strings.NewReader(bad)); err == nil {
			t.Errorf("Parse(%q) accepted a malformed rule", strings.TrimSpace(bad))
		}
	}
	if _, err := Load("/nonexistent/allowlist.txt"); err == nil {
		t.Fatal("missing file must error, not silently allow-all")
	}
}
