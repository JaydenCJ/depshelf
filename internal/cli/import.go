// The import subcommand: put a locally obtained tarball or wheel on the
// shelf without any network — the airgap seeding path. npm imports read
// name/version straight out of package.json inside the tgz; PyPI imports
// infer the project from the wheel/sdist filename.
package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/JaydenCJ/depshelf/internal/integrity"
	"github.com/JaydenCJ/depshelf/internal/npmproto"
	"github.com/JaydenCJ/depshelf/internal/pypiproto"
	"github.com/JaydenCJ/depshelf/internal/store"
)

func runImport(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stdout, "usage: depshelf import npm|pypi [flags] <file>")
		fmt.Fprintln(stdout, "Run 'depshelf import npm -h' or 'depshelf import pypi -h' for flags.")
		return exitOK
	}
	if len(args) == 0 || (args[0] != "npm" && args[0] != "pypi") {
		fmt.Fprintln(stderr, "usage: depshelf import npm|pypi [flags] <file>")
		return exitUsage
	}
	eco := args[0]
	fs := flag.NewFlagSet("import "+eco, flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeDir := fs.String("store", "./depshelf-store", "store directory (plain files)")
	name := fs.String("name", "", "override the package/project name")
	ver := fs.String("version", "", "override the version (npm only)")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: depshelf import npm|pypi [flags] <file>")
		return exitUsage
	}
	srcPath := fs.Arg(0)

	st, err := store.Open(*storeDir)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	file := filepath.Base(srcPath)
	if err := store.ValidateFilename(file); err != nil {
		return runtimeErr(stderr, err)
	}
	if eco == "npm" {
		err = importNpm(st, stdout, data, file, *name, *ver)
	} else {
		err = importPypi(st, stdout, data, file, *name)
	}
	if err != nil {
		return runtimeErr(stderr, err)
	}
	return exitOK
}

func importNpm(st *store.Store, stdout io.Writer, data []byte, file, name, ver string) error {
	if name == "" || ver == "" {
		pkgName, pkgVer, err := readPackageJSON(data)
		if err != nil {
			return fmt.Errorf("%s: %v (pass --name and --version to override)", file, err)
		}
		if name == "" {
			name = pkgName
		}
		if ver == "" {
			ver = pkgVer
		}
	}
	if err := npmproto.ValidateName(name); err != nil {
		return err
	}
	if ver == "" {
		return fmt.Errorf("%s: package.json has no version", file)
	}

	if _, _, _, err := st.WriteArtifact("npm", name, file, bytes.NewReader(data), integrity.Hash{}); err != nil {
		return err
	}

	// Merge a version entry into the (possibly pre-existing) packument.
	doc := map[string]any{}
	if raw, _, err := st.ReadMetadata("npm", name); err == nil {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("existing packument for %s is unreadable: %v", name, err)
		}
	} else if !errors.Is(err, store.ErrNotCached) {
		return err
	}
	doc["name"] = name
	versions, _ := doc["versions"].(map[string]any)
	if versions == nil {
		versions = map[string]any{}
		doc["versions"] = versions
	}
	versions[ver] = map[string]any{
		"name":    name,
		"version": ver,
		"dist": map[string]any{
			// The URL only needs a resolvable basename: serving rewrites
			// it to the mirror, and the artifact is already on disk so
			// this URL is never fetched.
			"tarball":   "imported/" + file,
			"shasum":    integrity.SHA1Hex(data),
			"integrity": integrity.SHA512SRI(data),
		},
	}
	tags, _ := doc["dist-tags"].(map[string]any)
	if tags == nil {
		tags = map[string]any{}
		doc["dist-tags"] = tags
	}
	if latest, _ := tags["latest"].(string); latest == "" || npmproto.CompareVersions(ver, latest) > 0 {
		tags["latest"] = ver
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := st.WriteMetadata("npm", name, append(raw, '\n')); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "imported npm %s@%s (%s, %d bytes, sha256 %s)\n",
		name, ver, file, len(data), integrity.SHA256Hex(data))
	return nil
}

// readPackageJSON extracts name and version from the package.json at the
// top level of the package directory inside an npm tarball (conventionally
// "package/package.json", but the root directory name is not guaranteed).
func readPackageJSON(data []byte) (name, ver string, err error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", "", fmt.Errorf("not a gzipped tarball: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", "", errors.New("no package.json found in tarball")
		}
		if err != nil {
			return "", "", fmt.Errorf("reading tarball: %v", err)
		}
		clean := path.Clean(hdr.Name)
		parts := strings.Split(clean, "/")
		if len(parts) != 2 || parts[1] != "package.json" {
			continue
		}
		var pkg struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if err := json.NewDecoder(tr).Decode(&pkg); err != nil {
			return "", "", fmt.Errorf("invalid package.json: %v", err)
		}
		if pkg.Name == "" || pkg.Version == "" {
			return "", "", errors.New("package.json lacks name or version")
		}
		return pkg.Name, pkg.Version, nil
	}
}

func importPypi(st *store.Store, stdout io.Writer, data []byte, file, project string) error {
	if project == "" {
		inferred, err := inferPypiProject(file)
		if err != nil {
			return fmt.Errorf("%v (pass --name to override)", err)
		}
		project = inferred
	}
	project = pypiproto.Normalize(project)
	if !pypiproto.ValidNormalized(project) {
		return fmt.Errorf("invalid project name %q", project)
	}

	if _, _, _, err := st.WriteArtifact("pypi", project, file, bytes.NewReader(data), integrity.Hash{}); err != nil {
		return err
	}

	p := pypiproto.NewProject(project)
	if raw, _, err := st.ReadMetadata("pypi", project); err == nil {
		if p, err = pypiproto.ParseJSON(raw); err != nil {
			return fmt.Errorf("existing index for %s is unreadable: %v", project, err)
		}
		p.Name = project
	} else if !errors.Is(err, store.ErrNotCached) {
		return err
	}
	p.Upsert(pypiproto.File{
		Filename: file,
		URL:      "imported/" + file, // never fetched; the artifact is already stored
		Hashes:   map[string]string{"sha256": integrity.SHA256Hex(data)},
	})
	stored, err := p.MarshalStored()
	if err != nil {
		return err
	}
	if err := st.WriteMetadata("pypi", project, stored); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "imported pypi %s (%s, %d bytes, sha256 %s)\n",
		project, file, len(data), integrity.SHA256Hex(data))
	return nil
}

// inferPypiProject derives the project name from a distribution filename:
// wheels put it before the first '-'; sdists are "<name>-<version>.<ext>".
func inferPypiProject(file string) (string, error) {
	if strings.HasSuffix(file, ".whl") {
		dist, _, ok := strings.Cut(strings.TrimSuffix(file, ".whl"), "-")
		if !ok || dist == "" {
			return "", fmt.Errorf("cannot infer project from wheel filename %q", file)
		}
		return dist, nil
	}
	base := file
	for _, ext := range []string{".tar.gz", ".tar.bz2", ".zip"} {
		if strings.HasSuffix(base, ext) {
			base = strings.TrimSuffix(base, ext)
			i := strings.LastIndex(base, "-")
			if i <= 0 {
				return "", fmt.Errorf("cannot infer project from sdist filename %q", file)
			}
			return base[:i], nil
		}
	}
	return "", fmt.Errorf("unrecognized distribution filename %q (expected .whl, .tar.gz, .tar.bz2 or .zip)", file)
}
