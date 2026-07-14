// Package allowlist restricts which packages the mirror will fetch or
// serve. Rules live in a plain text file, one "<ecosystem>:<pattern>" per
// line; patterns are simple globs where '*' matches any run of characters
// (including '/', so "npm:@babel/*" covers the whole scope) and '?' matches
// exactly one character.
package allowlist

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/JaydenCJ/depshelf/internal/pypiproto"
)

// Rule is a single parsed allowlist entry.
type Rule struct {
	Eco     string // "npm" or "pypi"
	Pattern string
}

// List is a parsed allowlist. A nil *List allows everything (no file was
// configured); a non-nil empty List denies everything — a present-but-empty
// file is treated as an explicit lockdown, not an accident.
type List struct {
	Rules []Rule
}

// Load reads and parses the allowlist file at path.
func Load(path string) (*List, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	l, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return l, nil
}

// Parse reads allowlist rules from r. Blank lines and '#' comments are
// ignored. Malformed lines are hard errors: a typo in an allowlist must not
// silently widen or narrow what the mirror serves.
func Parse(r io.Reader) (*List, error) {
	l := &List{}
	sc := bufio.NewScanner(r)
	lineno := 0
	for sc.Scan() {
		lineno++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eco, pattern, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("line %d: expected \"<ecosystem>:<pattern>\", got %q", lineno, line)
		}
		eco = strings.TrimSpace(eco)
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			return nil, fmt.Errorf("line %d: empty pattern", lineno)
		}
		switch eco {
		case "npm":
			// npm names are matched verbatim (they are already lowercase).
		case "pypi":
			// PyPI names compare in PEP 503 normalized form, so normalize
			// the pattern too: "Typing_Extensions*" and "typing-extensions*"
			// must behave identically.
			pattern = pypiproto.Normalize(pattern)
		default:
			return nil, fmt.Errorf("line %d: unknown ecosystem %q (want npm or pypi)", lineno, eco)
		}
		l.Rules = append(l.Rules, Rule{Eco: eco, Pattern: pattern})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return l, nil
}

// Allowed reports whether name in ecosystem eco passes the list. PyPI names
// must be passed in normalized form (the server normalizes before routing).
func (l *List) Allowed(eco, name string) bool {
	if l == nil {
		return true
	}
	for _, r := range l.Rules {
		if r.Eco == eco && match(r.Pattern, name) {
			return true
		}
	}
	return false
}

// match implements the two-metacharacter glob. Unlike path.Match, '*'
// crosses '/' so scope-wide npm rules work naturally.
func match(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	switch pattern[0] {
	case '*':
		for i := len(s); i >= 0; i-- {
			if match(pattern[1:], s[i:]) {
				return true
			}
		}
		return false
	case '?':
		return s != "" && match(pattern[1:], s[1:])
	default:
		return s != "" && s[0] == pattern[0] && match(pattern[1:], s[1:])
	}
}
