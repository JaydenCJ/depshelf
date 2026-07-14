// Minimal semver ordering — just enough to keep dist-tags.latest honest
// when `depshelf import npm` adds versions to an offline store. Build
// metadata is ignored; prerelease precedence follows SemVer 2.0.0 §11.
package npmproto

import (
	"strconv"
	"strings"
)

// CompareVersions returns -1, 0 or 1 for a < b, a == b, a > b under semver
// ordering. Non-numeric core fields compare as strings, so degenerate
// inputs still order deterministically instead of panicking.
func CompareVersions(a, b string) int {
	aCore, aPre := splitPrerelease(a)
	bCore, bPre := splitPrerelease(b)
	if c := compareCore(aCore, bCore); c != 0 {
		return c
	}
	return comparePrerelease(aPre, bPre)
}

// splitPrerelease drops "+build" metadata and cuts at the first '-'.
func splitPrerelease(v string) (core, pre string) {
	v, _, _ = strings.Cut(strings.TrimPrefix(v, "v"), "+")
	core, pre, _ = strings.Cut(v, "-")
	return core, pre
}

func compareCore(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		av, bv := "0", "0"
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		if c := compareIdent(av, bv, true); c != 0 {
			return c
		}
	}
	return 0
}

// comparePrerelease: a release (empty pre) outranks any prerelease; two
// prereleases compare identifier by identifier, shorter-is-lower on ties.
func comparePrerelease(a, b string) int {
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := compareIdent(as[i], bs[i], false); c != 0 {
			return c
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

// compareIdent compares one dotted identifier. Numeric identifiers compare
// numerically and rank below alphanumeric ones (SemVer §11.4); in the core
// version (coreField) both sides are normally numeric anyway.
func compareIdent(a, b string, coreField bool) int {
	an, aerr := strconv.ParseUint(a, 10, 64)
	bn, berr := strconv.ParseUint(b, 10, 64)
	switch {
	case aerr == nil && berr == nil:
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		}
		return 0
	case aerr == nil && !coreField:
		return -1 // numeric < alphanumeric in prereleases
	case berr == nil && !coreField:
		return 1
	}
	return strings.Compare(a, b)
}
