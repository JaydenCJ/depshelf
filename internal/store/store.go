// Package store manages the on-disk cache: plain files in a transparent
// layout, atomic writes, sha256 sidecars (sha256sum-compatible), and strict
// path validation for package names and artifact filenames.
//
// Layout:
//
//	<root>/npm/<name>/packument.json
//	<root>/npm/<name>/tarballs/<file>.tgz          (+ <file>.tgz.sha256)
//	<root>/pypi/<project>/index.json
//	<root>/pypi/<project>/files/<file>             (+ <file>.sha256)
//
// Scoped npm packages nest one level deeper: npm/@scope/name/…. Everything
// is rsync-able, greppable, and restorable with plain cp.
package store

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/JaydenCJ/depshelf/internal/integrity"
	"github.com/JaydenCJ/depshelf/internal/npmproto"
	"github.com/JaydenCJ/depshelf/internal/pypiproto"
)

const (
	sidecarExt = ".sha256"
	tmpDir     = ".tmp"
)

// filenameRe accepts the characters real tarballs, wheels and sdists use.
// No slashes, no leading dot — this is the path-safety gate for artifacts.
var filenameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// ErrNotCached is returned when the requested item is not in the store.
var ErrNotCached = errors.New("not in store")

// Store is a plain-file package cache rooted at Root.
type Store struct {
	Root string
}

// Open ensures the root and its temp area exist and returns the store.
func Open(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, tmpDir), 0o755); err != nil {
		return nil, err
	}
	return &Store{Root: root}, nil
}

// ValidateEco accepts the two supported ecosystems.
func ValidateEco(eco string) error {
	if eco != "npm" && eco != "pypi" {
		return fmt.Errorf("unknown ecosystem %q (want npm or pypi)", eco)
	}
	return nil
}

// ValidateName enforces the per-ecosystem package name rules. PyPI names
// must already be in PEP 503 normalized form.
func ValidateName(eco, name string) error {
	if err := ValidateEco(eco); err != nil {
		return err
	}
	if eco == "npm" {
		return npmproto.ValidateName(name)
	}
	if !pypiproto.ValidNormalized(name) {
		return fmt.Errorf("invalid normalized PyPI project name %q", name)
	}
	return nil
}

// ValidateFilename is the artifact path-safety gate. Sidecar names are
// reserved so an artifact can never shadow its own checksum.
func ValidateFilename(file string) error {
	if len(file) > 255 || !filenameRe.MatchString(file) {
		return fmt.Errorf("invalid artifact filename %q", file)
	}
	if strings.HasSuffix(file, sidecarExt) {
		return fmt.Errorf("invalid artifact filename %q: %s is reserved for checksum sidecars", file, sidecarExt)
	}
	return nil
}

func metadataFile(eco string) string {
	if eco == "npm" {
		return "packument.json"
	}
	return "index.json"
}

func artifactsDir(eco string) string {
	if eco == "npm" {
		return "tarballs"
	}
	return "files"
}

// pkgDir validates eco+name and returns the package directory.
func (s *Store) pkgDir(eco, name string) (string, error) {
	if err := ValidateName(eco, name); err != nil {
		return "", err
	}
	// Safe: validated names contain at most the npm scope separator.
	return filepath.Join(s.Root, eco, filepath.FromSlash(name)), nil
}

// ReadMetadata returns the stored metadata document and its write time.
func (s *Store) ReadMetadata(eco, name string) ([]byte, time.Time, error) {
	dir, err := s.pkgDir(eco, name)
	if err != nil {
		return nil, time.Time{}, err
	}
	path := filepath.Join(dir, metadataFile(eco))
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, time.Time{}, ErrNotCached
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	return data, info.ModTime(), nil
}

// WriteMetadata atomically replaces the metadata document.
func (s *Store) WriteMetadata(eco, name string, data []byte) error {
	dir, err := s.pkgDir(eco, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return s.writeAtomic(filepath.Join(dir, metadataFile(eco)), func(w io.Writer) error {
		_, werr := w.Write(data)
		return werr
	})
}

// ArtifactPath returns where the artifact lives and whether it exists.
func (s *Store) ArtifactPath(eco, name, file string) (string, bool, error) {
	dir, err := s.pkgDir(eco, name)
	if err != nil {
		return "", false, err
	}
	if err := ValidateFilename(file); err != nil {
		return "", false, err
	}
	path := filepath.Join(dir, artifactsDir(eco), file)
	_, err = os.Stat(path)
	switch {
	case err == nil:
		return path, true, nil
	case errors.Is(err, fs.ErrNotExist):
		return path, false, nil
	default:
		return "", false, err
	}
}

// WriteArtifact streams r into the store, enforcing the expected digest
// (pass the zero Hash to skip enforcement). On success the artifact and a
// sha256sum-compatible sidecar are in place; on any failure nothing is.
func (s *Store) WriteArtifact(eco, name, file string, r io.Reader, want integrity.Hash) (path, sha256hex string, size int64, err error) {
	path, _, err = s.ArtifactPath(eco, name, file)
	if err != nil {
		return "", "", 0, err
	}
	dg, err := integrity.NewDigester(want)
	if err != nil {
		return "", "", 0, err
	}
	err = s.writeAtomic(path, func(w io.Writer) error {
		_, cerr := io.Copy(io.MultiWriter(w, dg), r)
		if cerr != nil {
			return cerr
		}
		sha256hex, size, cerr = dg.Sum()
		return cerr
	})
	if err != nil {
		return "", "", 0, err
	}
	sidecar := fmt.Sprintf("%s  %s\n", sha256hex, file)
	err = s.writeAtomic(path+sidecarExt, func(w io.Writer) error {
		_, werr := io.WriteString(w, sidecar)
		return werr
	})
	if err != nil {
		return "", "", 0, err
	}
	return path, sha256hex, size, nil
}

// writeAtomic writes via a temp file in <root>/.tmp and renames into place,
// so readers never observe partial content and failed downloads leave no
// trace outside the temp area.
func (s *Store) writeAtomic(final string, write func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(s.Root, tmpDir), "write-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), final)
}

// Package summarizes one cached package for `depshelf list`.
type Package struct {
	Ecosystem   string `json:"ecosystem"`
	Name        string `json:"name"`
	HasMetadata bool   `json:"has_metadata"`
	Artifacts   int    `json:"artifacts"`
	Bytes       int64  `json:"bytes"`
}

// List walks the store and returns every cached package, sorted by
// ecosystem then name for deterministic output.
func (s *Store) List() ([]Package, error) {
	var out []Package
	for _, eco := range []string{"npm", "pypi"} {
		base := filepath.Join(s.Root, eco)
		entries, err := os.ReadDir(base)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if eco == "npm" && strings.HasPrefix(e.Name(), "@") {
				subs, err := os.ReadDir(filepath.Join(base, e.Name()))
				if err != nil {
					return nil, err
				}
				for _, sub := range subs {
					if sub.IsDir() {
						pkg, err := s.summarize(eco, e.Name()+"/"+sub.Name())
						if err != nil {
							return nil, err
						}
						out = append(out, pkg)
					}
				}
				continue
			}
			pkg, err := s.summarize(eco, e.Name())
			if err != nil {
				return nil, err
			}
			out = append(out, pkg)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ecosystem != out[j].Ecosystem {
			return out[i].Ecosystem < out[j].Ecosystem
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (s *Store) summarize(eco, name string) (Package, error) {
	dir := filepath.Join(s.Root, eco, filepath.FromSlash(name))
	pkg := Package{Ecosystem: eco, Name: name}
	if _, err := os.Stat(filepath.Join(dir, metadataFile(eco))); err == nil {
		pkg.HasMetadata = true
	}
	entries, err := os.ReadDir(filepath.Join(dir, artifactsDir(eco)))
	if errors.Is(err, fs.ErrNotExist) {
		return pkg, nil
	}
	if err != nil {
		return pkg, err
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), sidecarExt) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return pkg, err
		}
		pkg.Artifacts++
		pkg.Bytes += info.Size()
	}
	return pkg, nil
}

// VerifyResult is the outcome for one artifact during `depshelf verify`.
type VerifyResult struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	File      string `json:"file"`
	Status    string `json:"status"` // "ok", "corrupt" or "unverified"
	Detail    string `json:"detail,omitempty"`
}

// VerifyAll re-hashes every stored artifact against its sidecar. Artifacts
// without a sidecar (hand-copied into the store) report "unverified" rather
// than failing — verify tells the truth, it does not guess.
func (s *Store) VerifyAll() ([]VerifyResult, error) {
	pkgs, err := s.List()
	if err != nil {
		return nil, err
	}
	var out []VerifyResult
	for _, pkg := range pkgs {
		dir := filepath.Join(s.Root, pkg.Ecosystem, filepath.FromSlash(pkg.Name), artifactsDir(pkg.Ecosystem))
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasSuffix(e.Name(), sidecarExt) {
				continue
			}
			out = append(out, s.verifyOne(pkg, dir, e.Name()))
		}
	}
	return out, nil
}

func (s *Store) verifyOne(pkg Package, dir, file string) VerifyResult {
	res := VerifyResult{Ecosystem: pkg.Ecosystem, Name: pkg.Name, File: file}
	sidecar, err := os.ReadFile(filepath.Join(dir, file+sidecarExt))
	if err != nil {
		res.Status = "unverified"
		res.Detail = "no checksum sidecar"
		return res
	}
	want, _, _ := strings.Cut(strings.TrimSpace(string(sidecar)), " ")
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		res.Status = "corrupt"
		res.Detail = err.Error()
		return res
	}
	if got := integrity.SHA256Hex(data); got != want {
		res.Status = "corrupt"
		res.Detail = fmt.Sprintf("sha256 mismatch: sidecar %s, actual %s", want, got)
		return res
	}
	res.Status = "ok"
	return res
}
