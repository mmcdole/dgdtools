package index

import (
	"cmp"
	"context"
	"io/fs"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/mmcdole/dgdtools/pkg/fileset"
)

// DirEntry is a file or a directory in a directory of the lib.
type DirEntry struct {
	// Name is its name in the directory.
	Name string
	Dir  bool
	// Stub says it is known from a profile's stubs and is not on disk.
	Stub bool
}

// entries collects the entries of directories, each name once: the
// first that has it.
type entries struct {
	list []DirEntry
	seen map[string]bool
}

func (e *entries) add(name string, dir, stub bool) {
	if name == "" || strings.HasPrefix(name, ".") || e.seen[name] {
		return
	}
	if e.seen == nil {
		e.seen = map[string]bool{}
	}
	e.seen[name] = true
	e.list = append(e.list, DirEntry{Name: name, Dir: dir, Stub: stub})
}

func (e *entries) sorted() []DirEntry {
	slices.SortFunc(e.list, byName)
	return e.list
}

// disk adds the entries of the directory at dir on disk.
func (s *Snapshot) disk(e *entries, dir string) {
	if !s.fs.IsDir(dir) {
		return
	}
	s.fs.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		rel, ok := fileset.Under(dir, p)
		if !ok || rel == "." {
			return nil
		}
		// A file system that walks files only has the directory in the
		// file's path.
		name, _, deeper := strings.Cut(filepath.ToSlash(rel), "/")
		e.add(name, deeper || d.IsDir(), false)
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
}

// libDir adds the entries of the lib directory libDir ("/std"): what is
// on disk there, the directories that lead to a checkout, and what a
// profile's stubs have that the checkout does not speak for.
func (s *Snapshot) libDir(e *entries, libDir string) {
	libDir = path.Clean("/" + libDir)
	if p, ok := s.diskPath(libDir); ok {
		s.disk(e, p)
	}
	prefix := strings.TrimSuffix(libDir, "/") + "/"
	if rest, ok := strings.CutPrefix(s.lay.CheckoutLib, prefix); ok && s.lay.CheckoutDir != "" {
		name, _, _ := strings.Cut(rest, "/")
		e.add(name, true, false)
	}
	stubs := s.lay.StubFiles
	for i := sort.SearchStrings(stubs, prefix); i < len(stubs) && strings.HasPrefix(stubs[i], prefix); i++ {
		if s.inCheckout(stubs[i]) {
			continue
		}
		name, _, deeper := strings.Cut(stubs[i][len(prefix):], "/")
		e.add(name, deeper, true)
	}
}

// IncludeEntries lists what an #include in the file at from can name in
// the directory dir, which is the path written so far ("" for none,
// "sys" in `#include <sys/`): the entries of that directory beside the
// file, unless the include is of the <...> form, and in each include
// directory, sorted by name. It is the places ResolveInclude looks in.
func (s *Snapshot) IncludeEntries(from, dir string, angle bool) []DirEntry {
	e := &entries{}
	if !angle {
		switch rel, inLib := s.rel(from); {
		case strings.HasPrefix(dir, "/"):
			// A path from the lib's root is that directory and no other.
			if s.HasLib() {
				s.libDir(e, dir)
			}
			return e.sorted()
		case inLib:
			s.libDir(e, path.Join("/", path.Dir(rel), dir))
		default:
			s.disk(e, filepath.Join(filepath.Dir(from), filepath.FromSlash(dir)))
		}
	}
	if s.HasLib() {
		dirs, _ := s.IncludeDirs(s.LibFile(from))
		for _, inc := range dirs {
			s.libDir(e, path.Join(inc, dir))
		}
	}
	return e.sorted()
}

// ObjectEntries lists the objects and directories in the lib directory
// that the path dir, written in the object at fromLib, names (ObjectPath
// reads it): each directory, and each object by its name, without the
// ".c" of its file. Sorted by name; nothing without a lib.
func (s *Snapshot) ObjectEntries(dir, fromLib string) []DirEntry {
	if !s.HasLib() || fromLib == "" {
		return nil
	}
	all := &entries{}
	s.libDir(all, path.Dir(s.ObjectPath(path.Join(dir, "x"), fromLib)))
	var out []DirEntry
	for _, e := range all.sorted() {
		if name, ok := strings.CutSuffix(e.Name, ".c"); ok && !e.Dir {
			e.Name = name
		} else if !e.Dir {
			continue
		}
		out = append(out, e)
	}
	slices.SortFunc(out, byName)
	return out
}

// byName orders entries by name, a file before the directory of its
// name.
func byName(a, b DirEntry) int {
	if a.Name != b.Name {
		return cmp.Compare(a.Name, b.Name)
	}
	switch {
	case a.Dir == b.Dir:
		return 0
	case b.Dir:
		return -1
	}
	return 1
}

// FilesNamed returns the lib files ("/std/room.c") whose base name is
// name: under the lib directories dirs, on disk and among a profile's
// stubs, and among every file the snapshot has read (the whole-lib
// scan's, the headers they include). Sorted, each once; what was found
// when ctx ends.
func (s *Snapshot) FilesNamed(ctx context.Context, name string, dirs []string) []string {
	found := map[string]bool{}
	add := func(p string) {
		if lf := s.LibFile(p); lf != "" {
			found[lf] = true
		}
	}
	for p, u := range s.units.complete() {
		if u != nil && filepath.Base(p) == name {
			add(p)
		}
	}
	for _, dir := range dirs {
		dir = path.Clean("/" + dir)
		if root, ok := s.diskPath(dir); ok && s.fs.IsDir(root) {
			s.fs.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				switch {
				case ctx.Err() != nil:
					return fs.SkipAll
				case err != nil || d == nil:
					return nil
				case d.IsDir() && strings.HasPrefix(d.Name(), ".") && filepath.Clean(p) != filepath.Clean(root):
					return fs.SkipDir
				case d.IsDir() && d.Type()&fs.ModeSymlink != 0 && filepath.Clean(p) != filepath.Clean(root):
					if _, ok := s.lay.Enters(s.fs, p); !ok {
						return fs.SkipDir // as a walk of the lib does
					}
				case !d.IsDir() && d.Name() == name:
					add(p)
				}
				return nil
			})
		}
		prefix := strings.TrimSuffix(dir, "/") + "/"
		stubs := s.lay.StubFiles
		for i := sort.SearchStrings(stubs, prefix); i < len(stubs) && strings.HasPrefix(stubs[i], prefix); i++ {
			if path.Base(stubs[i]) == name {
				found[stubs[i]] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(found))
}
