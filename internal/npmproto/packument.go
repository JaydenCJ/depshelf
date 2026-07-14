// Package npmproto understands npm registry packuments: package-name
// validation, tarball URL rewriting so clients download through the mirror,
// tarball lookup with integrity expectations, and a minimal semver ordering
// used to maintain dist-tags when importing tarballs offline.
package npmproto

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/JaydenCJ/depshelf/internal/integrity"
)

// nameRe follows the modern npm naming rules: optional lowercase scope,
// then a lowercase name; neither may start with '.' or '_'.
var nameRe = regexp.MustCompile(`^(?:@[a-z0-9][a-z0-9-_.]*/)?[a-z0-9~][a-z0-9-_.~]*$`)

// ValidateName rejects anything that is not a well-formed npm package name.
// This doubles as the path-safety gate for the store: valid names contain
// no path traversal and at most the one scope separator slash.
func ValidateName(name string) error {
	if len(name) == 0 || len(name) > 214 {
		return fmt.Errorf("invalid npm package name %q: empty or longer than 214 characters", name)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid npm package name %q", name)
	}
	return nil
}

// EscapeName encodes the scope separator for upstream registry URLs
// (registries accept "@scope%2fname" universally).
func EscapeName(name string) string {
	return strings.ReplaceAll(name, "/", "%2f")
}

// TarballBasename extracts the filename component of a dist.tarball URL.
func TarballBasename(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return path.Base(rawURL)
	}
	return path.Base(u.Path)
}

// Rewrite returns the packument with every versions.*.dist.tarball URL
// rewritten to "{base}/npm/{name}/-/{basename}" so that installs resolve
// through the mirror. All other fields pass through untouched (key order is
// not preserved; npm clients do not care).
func Rewrite(raw []byte, base, name string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("invalid packument: %v", err)
	}
	versions, _ := doc["versions"].(map[string]any)
	for _, v := range versions {
		vm, _ := v.(map[string]any)
		dist, _ := vm["dist"].(map[string]any)
		tarball, _ := dist["tarball"].(string)
		if tarball == "" {
			continue
		}
		dist["tarball"] = base + "/npm/" + name + "/-/" + TarballBasename(tarball)
	}
	return json.Marshal(doc)
}

// TarballRef locates one concrete tarball inside a packument.
type TarballRef struct {
	Version string
	URL     string // the original upstream URL
	Want    integrity.Hash
}

// FindTarball scans the packument for the version whose dist.tarball
// basename equals filename. Returns nil when no version publishes that
// file — the mirror then answers 404 instead of proxying arbitrary URLs.
func FindTarball(raw []byte, filename string) (*TarballRef, error) {
	var doc struct {
		Versions map[string]struct {
			Dist struct {
				Tarball   string `json:"tarball"`
				Integrity string `json:"integrity"`
				Shasum    string `json:"shasum"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("invalid packument: %v", err)
	}
	for ver, v := range doc.Versions {
		if v.Dist.Tarball == "" || TarballBasename(v.Dist.Tarball) != filename {
			continue
		}
		return &TarballRef{
			Version: ver,
			URL:     v.Dist.Tarball,
			Want:    integrity.FromNpmDist(v.Dist.Integrity, v.Dist.Shasum),
		}, nil
	}
	return nil, nil
}
