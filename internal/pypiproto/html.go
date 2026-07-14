// PEP 503 HTML: a hand-rolled anchor scanner for upstream pages (pypi.org
// and most mirrors emit very regular markup) and a renderer for the pages
// depshelf serves to pip. No third-party HTML parser needed.
package pypiproto

import (
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"strings"
)

// ParseHTML extracts file entries from a PEP 503 project page. Relative
// hrefs are resolved against base (the URL the page was fetched from);
// the anchor text is the filename per PEP 503.
func ParseHTML(data []byte, base *url.URL, name string) (*Project, error) {
	p := NewProject(name)
	s := string(data)
	lower := strings.ToLower(s)
	pos := 0
	for {
		i := findAnchorStart(lower[pos:])
		if i < 0 {
			break
		}
		pos += i + 2 // step past "<a"
		attrs, next, ok := parseAttrs(s[pos:])
		if !ok {
			break
		}
		pos = len(s) - len(next)
		end := strings.Index(lower[pos:], "</a>")
		if end < 0 {
			break
		}
		text := html.UnescapeString(strings.TrimSpace(s[pos : pos+end]))
		pos += end + 4
		f, ok := fileFromAnchor(attrs, text, base)
		if ok {
			p.Files = append(p.Files, f)
		}
	}
	return p, nil
}

// fileFromAnchor converts one parsed <a> element into a File entry.
func fileFromAnchor(attrs map[string]string, text string, base *url.URL) (File, bool) {
	href := attrs["href"]
	if href == "" || text == "" {
		return File{}, false
	}
	u, err := url.Parse(href)
	if err != nil {
		return File{}, false
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	f := File{Filename: text, Hashes: map[string]string{}}
	if algo, hexval, ok := strings.Cut(u.Fragment, "="); ok && algo != "" && hexval != "" {
		f.Hashes[algo] = hexval
	}
	u.Fragment = ""
	f.URL = u.String()
	if rp, ok := attrs["data-requires-python"]; ok {
		f.RequiresPython = rp
	}
	if reason, ok := attrs["data-yanked"]; ok {
		if reason == "" {
			f.Yanked = json.RawMessage("true")
		} else {
			raw, _ := json.Marshal(reason)
			f.Yanked = json.RawMessage(raw)
		}
	}
	return f, true
}

// findAnchorStart returns the index of the next "<a" tag opener in the
// (lowercased) input, requiring a tag-ending boundary so "<abbr>" does not
// match.
func findAnchorStart(lower string) int {
	off := 0
	for {
		i := strings.Index(lower[off:], "<a")
		if i < 0 {
			return -1
		}
		at := off + i
		rest := lower[at+2:]
		if rest == "" {
			return -1
		}
		switch rest[0] {
		case ' ', '\t', '\n', '\r', '>', '/':
			return at
		}
		off = at + 2
	}
}

// parseAttrs reads HTML attributes up to the closing '>' of a start tag.
// Returns the attribute map (keys lowercased, values entity-decoded), the
// remaining input after '>', and whether the tag was well formed.
func parseAttrs(s string) (map[string]string, string, bool) {
	attrs := map[string]string{}
	i := 0
	for {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			return nil, "", false
		}
		switch s[i] {
		case '>':
			return attrs, s[i+1:], true
		case '/':
			i++
			continue
		}
		start := i
		for i < len(s) && s[i] != '=' && s[i] != '>' && s[i] != '/' && !isSpace(s[i]) {
			i++
		}
		name := strings.ToLower(s[start:i])
		val := ""
		if i < len(s) && s[i] == '=' {
			i++
			if i < len(s) && (s[i] == '"' || s[i] == '\'') {
				quote := s[i]
				i++
				vstart := i
				for i < len(s) && s[i] != quote {
					i++
				}
				if i >= len(s) {
					return nil, "", false
				}
				val = s[vstart:i]
				i++ // closing quote
			} else {
				vstart := i
				for i < len(s) && s[i] != '>' && !isSpace(s[i]) {
					i++
				}
				val = s[vstart:i]
			}
		}
		if name != "" {
			attrs[name] = html.UnescapeString(val)
		}
	}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// RenderHTML serializes the project as the PEP 503 page depshelf serves to
// pip, with file URLs rewritten to the mirror (base is scheme://host).
func (p *Project) RenderHTML(base string) []byte {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html>\n  <head>\n")
	b.WriteString("    <meta name=\"pypi:repository-version\" content=\"1.0\">\n")
	fmt.Fprintf(&b, "    <title>Links for %s</title>\n  </head>\n  <body>\n", html.EscapeString(p.Name))
	fmt.Fprintf(&b, "    <h1>Links for %s</h1>\n", html.EscapeString(p.Name))
	for _, f := range p.Files {
		fmt.Fprintf(&b, "    <a href=%q", fileHref(base, p.Name, f))
		if f.RequiresPython != "" {
			fmt.Fprintf(&b, " data-requires-python=%q", html.EscapeString(f.RequiresPython))
		}
		if reason, yanked := yankedReason(f.Yanked); yanked {
			fmt.Fprintf(&b, " data-yanked=%q", html.EscapeString(reason))
		}
		fmt.Fprintf(&b, ">%s</a><br/>\n", html.EscapeString(f.Filename))
	}
	b.WriteString("  </body>\n</html>\n")
	return []byte(b.String())
}

// yankedReason decodes the raw yanked value: (reason, true) for yanked
// files — reason may be empty — and ("", false) otherwise.
func yankedReason(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return "", b
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	return "", false
}

// RenderRootHTML renders the PEP 503 index of all locally cached projects.
func RenderRootHTML(names []string) []byte {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html>\n  <head>\n")
	b.WriteString("    <meta name=\"pypi:repository-version\" content=\"1.0\">\n")
	b.WriteString("    <title>Simple index</title>\n  </head>\n  <body>\n")
	for _, n := range names {
		fmt.Fprintf(&b, "    <a href=\"/pypi/simple/%s/\">%s</a><br/>\n", n, html.EscapeString(n))
	}
	b.WriteString("  </body>\n</html>\n")
	return []byte(b.String())
}

// RenderRootJSON renders the PEP 691 index of all locally cached projects.
func RenderRootJSON(names []string) ([]byte, error) {
	type entry struct {
		Name string `json:"name"`
	}
	root := struct {
		Meta struct {
			APIVersion string `json:"api-version"`
		} `json:"meta"`
		Projects []entry `json:"projects"`
	}{}
	root.Meta.APIVersion = "1.0"
	root.Projects = make([]entry, len(names))
	for i, n := range names {
		root.Projects[i] = entry{Name: n}
	}
	return json.MarshalIndent(root, "", "  ")
}
