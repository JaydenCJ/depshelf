// Package pypiproto implements the PyPI "simple" index protocols: PEP 503
// project-name normalization, parsing of both PEP 691 JSON and legacy PEP
// 503 HTML upstream responses, and rendering of both formats with file URLs
// rewritten to point back at the mirror.
package pypiproto

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var (
	normalizeRuns = regexp.MustCompile(`[-_.]+`)
	normalizedRe  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
)

// Normalize applies PEP 503 name normalization: lowercase, with every run
// of '-', '_' and '.' collapsed to a single '-'. "Typing_Extensions" and
// "typing.extensions" both become "typing-extensions".
func Normalize(name string) string {
	return strings.ToLower(normalizeRuns.ReplaceAllString(name, "-"))
}

// ValidNormalized reports whether name is a plausible, already-normalized
// project name — the only form the store and the /pypi/ routes accept.
func ValidNormalized(name string) bool {
	return len(name) <= 214 && normalizedRe.MatchString(name)
}

// File is one downloadable distribution inside a project index, mirroring
// the PEP 691 file object shape.
type File struct {
	Filename       string            `json:"filename"`
	URL            string            `json:"url"`
	Hashes         map[string]string `json:"hashes"`
	RequiresPython string            `json:"requires-python,omitempty"`
	// Yanked is either the JSON bool false/true or a string reason,
	// so it is kept as raw JSON for fidelity.
	Yanked json.RawMessage `json:"yanked,omitempty"`
}

// Project is a parsed simple-index project page (the stored form is its
// PEP 691 JSON serialization).
type Project struct {
	Meta struct {
		APIVersion string `json:"api-version"`
	} `json:"meta"`
	Name  string `json:"name"`
	Files []File `json:"files"`
}

// NewProject returns an empty project page for name with the meta header
// filled in.
func NewProject(name string) *Project {
	p := &Project{Name: name}
	p.Meta.APIVersion = "1.0"
	return p
}

// ParseJSON decodes a PEP 691 project page.
func ParseJSON(data []byte) (*Project, error) {
	var p Project
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("invalid PEP 691 JSON: %v", err)
	}
	if p.Meta.APIVersion == "" {
		p.Meta.APIVersion = "1.0"
	}
	return &p, nil
}

// MarshalStored serializes the project into the canonical stored form:
// indented PEP 691 JSON with files sorted by filename, so identical content
// always produces identical bytes on disk.
func (p *Project) MarshalStored() ([]byte, error) {
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Filename < p.Files[j].Filename })
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// FindFile returns the file entry with the given filename, or nil.
func (p *Project) FindFile(filename string) *File {
	for i := range p.Files {
		if p.Files[i].Filename == filename {
			return &p.Files[i]
		}
	}
	return nil
}

// Upsert replaces the entry with the same filename or appends a new one.
// Used by `depshelf import pypi`.
func (p *Project) Upsert(f File) {
	if old := p.FindFile(f.Filename); old != nil {
		*old = f
		return
	}
	p.Files = append(p.Files, f)
}

// fileHref builds the mirror-local download URL for a file, carrying the
// sha256 as a URL fragment so pip verifies what it downloads (PEP 503).
func fileHref(base, project string, f File) string {
	href := base + "/pypi/files/" + project + "/" + url.PathEscape(f.Filename)
	if sha := f.Hashes["sha256"]; sha != "" {
		href += "#sha256=" + sha
	}
	return href
}

// RenderJSON serializes the project as a PEP 691 response with every file
// URL rewritten to point at the mirror (base is scheme://host, no slash).
func (p *Project) RenderJSON(base string) ([]byte, error) {
	out := NewProject(p.Name)
	out.Meta = p.Meta
	out.Files = make([]File, len(p.Files))
	for i, f := range p.Files {
		f.URL = fileHref(base, p.Name, File{Filename: f.Filename}) // no fragment in JSON URLs
		out.Files[i] = f
	}
	return json.MarshalIndent(out, "", "  ")
}
