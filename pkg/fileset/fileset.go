// Package fileset knows where a lib's files are: it maps between paths on
// disk and lib paths, matches the globs that select files, and walks a
// source tree with the project's excludes.
package fileset

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/mmcdole/dgdtools/pkg/vfs"
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

// CheckGlob reports a pattern NewMatcher would read otherwise than its
// writer likely meant: one with the brackets, braces or backslashes other
// glob dialects give a meaning, which here would be matched as they are.
func CheckGlob(p string) error {
	if i := strings.IndexAny(p, `[]{}\`); i >= 0 {
		return fmt.Errorf("%q: %q is not part of a glob here (only *, ? and ** are)", p, p[i:i+1])
	}
	return nil
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

// Under returns the path of p from dir, when p is dir or lies below it.
func Under(dir, p string) (rel string, ok bool) {
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// Tree says where a lib is on disk: all of it under Root, or, for a
// partial checkout, its directory CheckoutLib ("/usr/name") under
// CheckoutDir. With neither there is no lib.
type Tree struct {
	Root                     string
	CheckoutDir, CheckoutLib string
}

// Rel returns the path p as a file of the lib: relative to the lib root,
// slash-separated and with its extension ("players/x/room.c"). It is what
// exclude and override globs are matched against. The second result is
// false when p is not in the lib.
func (t Tree) Rel(p string) (string, bool) {
	if t.CheckoutDir != "" {
		if r, ok := Under(t.CheckoutDir, p); ok {
			return strings.TrimPrefix(path.Join(t.CheckoutLib, filepath.ToSlash(r)), "/"), true
		}
	}
	if t.Root == "" {
		return "", false
	}
	r, ok := Under(t.Root, p)
	return filepath.ToSlash(r), ok
}

// Path is where libFile (a lib path with its extension) would be on disk;
// false when no directory on disk holds that part of the lib.
func (t Tree) Path(libFile string) (string, bool) {
	if t.CheckoutDir != "" {
		if libFile == t.CheckoutLib {
			return t.CheckoutDir, true
		}
		if rest, ok := strings.CutPrefix(libFile, t.CheckoutLib+"/"); ok {
			return filepath.Join(t.CheckoutDir, filepath.FromSlash(rest)), true
		}
	}
	if t.Root == "" {
		return "", false
	}
	return filepath.Join(t.Root, filepath.FromSlash(strings.TrimPrefix(libFile, "/"))), true
}

// InCheckout reports whether libFile lies in the lib directory the
// checkout holds.
func (t Tree) InCheckout(libFile string) bool {
	return t.CheckoutDir != "" &&
		(libFile == t.CheckoutLib || strings.HasPrefix(libFile, t.CheckoutLib+"/"))
}

// Walk visits every file under dir (a directory on disk) that has one of
// the extensions exts and that exclude does not take. The globs are
// matched against lib-relative names, so they mean the same wherever the
// walk starts; a file outside the lib is never excluded. Walk stops with
// ctx's error when ctx is cancelled.
//
// A link to a directory outside the lib is walked as a directory of the
// lib, its files named by their path through the link: that is how the
// driver sees them. A link to a directory inside the lib is not entered,
// since the walk has those files by their own names already, nor is one
// to a directory above the lib, or a loop (vfs.FS.WalkDir). dir itself
// is walked whatever it is.
func (t Tree) Walk(ctx context.Context, fsys vfs.FS, dir string, exts []string, exclude *Matcher, visit func(path string)) error {
	return t.walk(ctx, fsys, dir, exts, exclude, visit, nil)
}

// walk is Walk, telling link of each link it enters.
func (t Tree) walk(ctx context.Context, fsys vfs.FS, dir string, exts []string, exclude *Matcher, visit func(path string), link func(Link)) error {
	err := fsys.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return fs.SkipAll
		}
		if err != nil {
			return nil
		}
		rel, inLib := t.Rel(p)
		if d.IsDir() {
			if p != dir && inLib && (exclude.Match(rel) || exclude.MatchDir(rel)) {
				return filepath.SkipDir
			}
			if p != dir && d.Type()&fs.ModeSymlink != 0 {
				target, ok := t.Enters(fsys, p)
				if !ok {
					return filepath.SkipDir
				}
				if link != nil {
					link(Link{Target: target, Dir: p})
				}
			}
			return nil
		}
		if slices.Contains(exts, filepath.Ext(p)) && !(inLib && exclude.Match(rel)) {
			visit(p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return ctx.Err()
}

// Enters reports whether a walk of the lib enters the link to a
// directory at p, and returns the directory's real path: not when that
// is in the lib, which the walk has by its own name, or above it, which
// would walk the lib again.
func (t Tree) Enters(fsys vfs.FS, p string) (target string, ok bool) {
	target, err := fsys.RealPath(p)
	if err != nil {
		return "", false
	}
	for _, d := range []string{t.Root, t.CheckoutDir} {
		if d == "" {
			continue
		}
		lib, err := fsys.RealPath(d)
		if err != nil {
			continue
		}
		_, into := Under(lib, target)
		_, above := Under(target, lib)
		if into || above {
			return "", false
		}
	}
	return target, true
}

// Link is a link in the lib to a directory outside it, which makes that
// a directory of the lib: Dir, the link's path, has the files of Target,
// the directory's real path.
type Link struct {
	Target, Dir string
}

// Links are the links of a lib.
type Links []Link

// Links returns the links to directories outside the lib that a walk of
// the whole lib enters, as Walk enters them: not those exclude takes,
// and each directory by the first link the walk comes to, in name order
// (a directory two links lead to is walked through both, and is named
// by the first). A link inside a linked directory is one too.
func (t Tree) Links(ctx context.Context, fsys vfs.FS, exclude *Matcher) (Links, error) {
	dir := t.Root
	if dir == "" {
		dir = t.CheckoutDir
	}
	if dir == "" {
		return nil, nil
	}
	var out Links
	err := t.walk(ctx, fsys, dir, nil, exclude, func(string) {}, func(l Link) {
		if !slices.ContainsFunc(out, func(have Link) bool { return have.Target == l.Target }) {
			out = append(out, l)
		}
	})
	return out, err
}

// Path returns the path in the lib of the file or directory at p, a path
// with no links in it: p by way of the link to the directory it is in,
// the innermost such directory when they nest. False when p is in none.
func (ls Links) Path(p string) (string, bool) {
	best := -1
	for i, l := range ls {
		if _, in := Under(l.Target, p); in && (best < 0 || len(l.Target) > len(ls[best].Target)) {
			best = i
		}
	}
	if best < 0 {
		return "", false
	}
	rel, _ := Under(ls[best].Target, p)
	return filepath.Join(ls[best].Dir, rel), true
}

// LibPath converts a root-relative file path to a lib-absolute object path
// ("/std/room" for "std/room.c"). The .c extension is dropped; .h files
// keep theirs.
func LibPath(rel string) string {
	rel = "/" + strings.Trim(filepath.ToSlash(rel), "/")
	return strings.TrimSuffix(rel, ".c")
}
