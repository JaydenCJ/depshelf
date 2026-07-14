// Tests for the minimal semver ordering behind dist-tags.latest updates.
package npmproto

import "testing"

func TestCompareVersionsCoreOrdering(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.10.0", "1.9.0", 1}, // numeric, not lexicographic
		{"2.0.0", "10.0.0", -1},
		{"1.0", "1.0.0", 0}, // missing fields count as zero
		{"v1.2.0", "1.2.0", 0},
		{"1.0.0+build.5", "1.0.0+build.9", 0}, // build metadata never affects precedence
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareVersionsPrereleasePrecedence(t *testing.T) {
	if CompareVersions("1.0.0-rc.1", "1.0.0") != -1 {
		t.Fatal("prerelease must rank below its release")
	}
	if CompareVersions("1.0.1-alpha", "1.0.0") != 1 {
		t.Fatal("higher core wins even against a prerelease")
	}
	// The SemVer 2.0.0 §11 canonical chain.
	chain := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta",
		"1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11",
		"1.0.0-rc.1", "1.0.0",
	}
	for i := 1; i < len(chain); i++ {
		if CompareVersions(chain[i-1], chain[i]) != -1 {
			t.Errorf("%s should sort before %s", chain[i-1], chain[i])
		}
	}
}

func TestCompareVersionsDegenerateInputStillTotal(t *testing.T) {
	// Garbage in, deterministic order out — never a panic.
	if got := CompareVersions("not-a-version", "not-a-version"); got != 0 {
		t.Fatalf("identical garbage should compare equal, got %d", got)
	}
	a, b := CompareVersions("abc", "abd"), CompareVersions("abd", "abc")
	if a != -b || a == 0 {
		t.Fatalf("garbage ordering not antisymmetric: %d vs %d", a, b)
	}
}
