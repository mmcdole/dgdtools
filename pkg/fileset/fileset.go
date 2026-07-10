// Package fileset walks an LPC source tree with exclude globs and maps
// between filesystem paths and lib-absolute object paths.
package fileset

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// Matcher matches slash-separated relative paths against ** globs.
type Matcher struct {
	res []*regexp.Regexp
}

// NewMatcher compiles exclude patterns. Supported syntax: `*` (within one
// path segment), `?`, and `**` (across segments). A pattern matches if it
// matches the whole relative path or any prefix ending at a segment
// boundary (so "players/**" and "**/old/**" behave as expected).
func NewMatcher(patterns []string) *Matcher {
	m := &Matcher{}
	for _, p := range patterns {
		p = strings.Trim(p, "/")
		if p == "" {
			continue
		}
		m.res = append(m.res, regexp.MustCompile("^"+globToRe(p)+"$"))
	}
	return m
}

func globToRe(glob string) string {
	var b strings.Builder
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				// "**/" or trailing "**": any number of segments
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
					b.WriteString(`([^/]+/)*`)
				} else {
					b.WriteString(`.*`)
				}
			} else {
				b.WriteString(`[^/]*`)
			}
		case '?':
			b.WriteString(`[^/]`)
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// Match reports whether rel (slash-separated, no leading /) is excluded.
func (m *Matcher) Match(rel string) bool {
	rel = strings.Trim(rel, "/")
	for _, re := range m.res {
		if re.MatchString(rel) {
			return true
		}
	}
	return false
}

// MatchDir reports whether the directory rel can be pruned: everything
// under it would be excluded (e.g. "players" under pattern "players/**").
func (m *Matcher) MatchDir(rel string) bool {
	rel = strings.Trim(rel, "/") + "/"
	for _, re := range m.res {
		if re.MatchString(rel) {
			return true
		}
	}
	return false
}

// Walk visits every .c and .h file under root that is not excluded.
// rel is slash-separated and relative to root.
func Walk(root string, exclude []string, visit func(path, rel string)) error {
	m := NewMatcher(exclude)
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (m.Match(rel) || m.MatchDir(rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(path); ext != ".c" && ext != ".h" {
			return nil
		}
		if m.Match(rel) {
			return nil
		}
		visit(path, rel)
		return nil
	})
}

// LibPath converts a root-relative file path to a lib-absolute object path
// ("/std/room" for "std/room.c"). The .c extension is dropped; .h files
// keep theirs.
func LibPath(rel string) string {
	rel = "/" + strings.Trim(filepath.ToSlash(rel), "/")
	return strings.TrimSuffix(rel, ".c")
}

// FSPath converts a lib-absolute path to a filesystem path under root.
// The .c extension is appended when the path has no extension.
func FSPath(root, libPath string) string {
	p := strings.TrimPrefix(libPath, "/")
	if filepath.Ext(p) == "" {
		p += ".c"
	}
	return filepath.Join(root, filepath.FromSlash(p))
}
