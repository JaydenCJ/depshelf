// Tests for digest parsing and stream verification. Every expectation is a
// fixed vector — no randomness, no I/O.
package integrity

import (
	"crypto/sha512"
	"encoding/base64"
	"strings"
	"testing"
)

// sha256("hello\n") and sha1("hello\n"), computed once and pinned.
const (
	helloSHA256 = "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	helloSHA1   = "f572d396fae9206628714fb2ce00f72e94f2258f"
)

func TestKnownDigestVectors(t *testing.T) {
	if got := SHA256Hex([]byte("hello\n")); got != helloSHA256 {
		t.Fatalf("SHA256Hex = %s, want %s", got, helloSHA256)
	}
	if got := SHA1Hex([]byte("hello\n")); got != helloSHA1 {
		t.Fatalf("SHA1Hex = %s, want %s", got, helloSHA1)
	}
}

func TestSHA512SRIRoundTripsThroughParseSRI(t *testing.T) {
	sri := SHA512SRI([]byte("payload"))
	if !strings.HasPrefix(sri, "sha512-") {
		t.Fatalf("SRI %q lacks sha512- prefix", sri)
	}
	h, err := ParseSRI(sri)
	if err != nil {
		t.Fatalf("ParseSRI(%q): %v", sri, err)
	}
	if h.Algo != "sha512" || len(h.Hex) != sha512.Size*2 {
		t.Fatalf("round trip produced %+v", h)
	}
	// Unpadded base64 occurs in the wild and must parse identically.
	sum := sha512.Sum512([]byte("payload"))
	unpadded := "sha512-" + base64.RawStdEncoding.EncodeToString(sum[:])
	h2, err := ParseSRI(unpadded)
	if err != nil || h2 != h {
		t.Fatalf("unpadded SRI: %+v, %v", h2, err)
	}
}

func TestParseSRIRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "sha512", "sha512-", "md5-AAAA", "sha512-!!notbase64!!", "sha512-AAAA"} {
		if _, err := ParseSRI(bad); err == nil {
			t.Errorf("ParseSRI(%q) accepted garbage", bad)
		}
	}
}

func TestFromNpmDistDigestSelection(t *testing.T) {
	// dist.integrity (sha512) wins over the legacy sha1 shasum.
	if h := FromNpmDist(SHA512SRI([]byte("data")), helloSHA1); h.Algo != "sha512" {
		t.Fatalf("expected sha512 preferred, got %+v", h)
	}
	// Without SRI, fall back to the shasum (case-normalized).
	if h := FromNpmDist("", strings.ToUpper(helloSHA1)); h.Algo != "sha1" || h.Hex != helloSHA1 {
		t.Fatalf("shasum fallback = %+v", h)
	}
	// Metadata predating SRI with a truncated shasum must not fabricate an
	// expectation the mirror would then fail to satisfy.
	if h := FromNpmDist("sha512-notb64!!", "abc123"); !h.IsZero() {
		t.Fatalf("expected zero Hash, got %+v", h)
	}
}

func TestDigesterAcceptsMatchAndRejectsMismatch(t *testing.T) {
	d, err := NewDigester(Hash{Algo: "sha256", Hex: helloSHA256})
	if err != nil {
		t.Fatal(err)
	}
	d.Write([]byte("hel"))
	d.Write([]byte("lo\n")) // split writes must accumulate correctly
	sha, n, err := d.Sum()
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	if sha != helloSHA256 || n != 6 {
		t.Fatalf("Sum = (%s, %d)", sha, n)
	}

	d2, _ := NewDigester(Hash{Algo: "sha256", Hex: helloSHA256})
	d2.Write([]byte("tampered"))
	if _, _, err := d2.Sum(); err == nil {
		t.Fatal("mismatched stream accepted")
	}
}

func TestDigesterVerifiesNonCanonicalAlgo(t *testing.T) {
	d, err := NewDigester(Hash{Algo: "sha1", Hex: helloSHA1})
	if err != nil {
		t.Fatal(err)
	}
	d.Write([]byte("hello\n"))
	sha, _, err := d.Sum()
	if err != nil {
		t.Fatalf("sha1 expectation should pass: %v", err)
	}
	// The canonical sidecar digest is still sha256 regardless of what was enforced.
	if sha != helloSHA256 {
		t.Fatalf("canonical digest = %s, want sha256", sha)
	}
}

func TestNewDigesterRejectsMalformedExpectation(t *testing.T) {
	for _, bad := range []Hash{
		{Algo: "md5", Hex: "aa"},
		{Algo: "sha256", Hex: "zz"},
		{Algo: "sha256", Hex: "abcd"}, // wrong length
	} {
		if _, err := NewDigester(bad); err == nil {
			t.Errorf("NewDigester(%+v) accepted malformed expectation", bad)
		}
	}
}

func TestZeroHashSkipsEnforcement(t *testing.T) {
	d, err := NewDigester(Hash{})
	if err != nil {
		t.Fatal(err)
	}
	d.Write([]byte("anything"))
	if _, _, err := d.Sum(); err != nil {
		t.Fatalf("zero expectation must not fail: %v", err)
	}
	if s := (Hash{}).String(); s != "none" {
		t.Fatalf("zero Hash String = %q", s)
	}
	if s := (Hash{Algo: "sha256", Hex: "ab"}).String(); s != "sha256:ab" {
		t.Fatalf("String = %q", s)
	}
}
