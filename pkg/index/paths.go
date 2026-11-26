package index

import (
	"context"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/fileset"
	"github.com/mmcdole/dgdtools/pkg/pp"
	"github.com/mmcdole/dgdtools/pkg/token"
)

// A file has two names. Its lib path is what LPC code calls it:
// "/std/room" for an object, "/include/std.h" for anything else. Its
// path is where its text is read from: a path on disk (under the lib
// root, or under the checkout for the lib directory the checkout holds)
// or the location of a profile's stub.

// objectFile is the lib file an object's source is in: an object path
// never has the extension, whatever dots it has ("/obj/sword.v2").
func objectFile(libPath string) string { return libPath + ".c" }

// isStub reports whether p is the location of a profile stub.
func (l *lib) isStub(p string) bool {
	return l.lay.Stubs != "" && strings.HasPrefix(p, l.lay.Stubs+"/")
}

// Shared reports whether a profile has a stub of the lib file libFile
// ("/std/room.c") beside the checkout's own: the file is one the rest of
// the lib uses, which a partial checkout cannot see.
func (l *lib) Shared(libFile string) bool {
	return slices.Contains(l.lay.StubFiles, libFile)
}

// clean normalizes a path given by a caller.
func (l *lib) clean(p string) string {
	if l.isStub(p) {
		return p
	}
	return filepath.Clean(p)
}

// diskPath is where libFile (a lib path with its extension) would be on
// disk; false when no directory on disk holds that part of the lib.
func (l *lib) diskPath(libFile string) (string, bool) { return l.lay.Path(libFile) }

// inCheckout reports whether libFile lies in the lib directory the
// checkout holds; nothing but the checkout speaks for those.
func (l *lib) inCheckout(libFile string) bool { return l.lay.InCheckout(libFile) }

// rel returns p relative to the lib root, slash-separated and with its
// extension ("players/x/room.c"); false when p is not a file of the lib.
func (l *lib) rel(p string) (string, bool) {
	if l.isStub(p) {
		return strings.TrimPrefix(p, l.lay.Stubs+"/"), true
	}
	return l.lay.Rel(p)
}

// LibPath maps a file's path to its lib path ("/std/room" for
// std/room.c), or "" when the file is not part of the lib.
func (l *lib) LibPath(p string) string {
	rel, ok := l.rel(p)
	if !ok {
		return ""
	}
	return fileset.LibPath(rel)
}

// LibFile maps a file's path to the file's name in the lib, with its
// extension ("/std/room.c"), or "" when the file is not part of the lib.
// A profile's stub has the name of the file it stands for.
func (l *lib) LibFile(p string) string {
	rel, ok := l.rel(p)
	if !ok {
		return ""
	}
	return "/" + strings.Trim(filepath.ToSlash(rel), "/")
}

// Covers reports whether the index knows everything under libPath's
// location, so that "not found" there means "does not exist". With a whole
// lib that is the lib's completeness; with a partial checkout it is the
// checkout itself and the directories the profile captured completely.
func (s *Snapshot) Covers(libPath string) bool {
	l := s.lib
	if !l.partial {
		return s.ext.complete
	}
	if l.inCheckout(libPath) {
		return true
	}
	for _, c := range l.lay.Covered {
		if strings.HasPrefix(libPath, c) {
			// An object path has no extension; holes are file globs.
			rel := strings.TrimPrefix(libPath, "/")
			return !l.holes.Match(rel) && !l.holes.Match(rel+".c")
		}
	}
	return false
}

// Dialect returns the dialect the lib's files are lexed in.
func (l *lib) Dialect() token.Dialect { return l.dialect }

// IsVirtual reports whether libPath is served by a virtual-object daemon.
func (l *lib) IsVirtual(libPath string) bool {
	return l.virtual.Match(strings.TrimPrefix(libPath, "/"))
}

// IncludeDirs returns the lib directories an #include in the file at lib
// path fromLib searches, in order (the configuration's include_dirs), and
// whether some are left out because only the lib can say where they are.
//
// DGD hands each include path to the driver object's include_file, and
// by default resolves it as it does a "..." include: an absolute one from
// the lib root, any other from the including file's directory. One that
// starts with ~ (the kernel library's ~/include, the directory of the
// file's owner) means what the lib's driver object makes of it.
func (l *lib) IncludeDirs(fromLib string) (dirs []string, unknown bool) {
	for _, d := range l.cfg.IncludeDirs {
		switch {
		case strings.HasPrefix(d, "/"):
			dirs = append(dirs, path.Clean(d))
		case strings.HasPrefix(d, "~") || fromLib == "":
			unknown = true
		default:
			dirs = append(dirs, path.Join(path.Dir(fromLib), d))
		}
	}
	return dirs, unknown
}

// ObjectPath turns an object path as written in fromLib into an absolute
// lib path. A path without a leading slash is resolved from the lib root
// unless the config says the lib resolves it from the file
// (relative_object_paths).
func (l *lib) ObjectPath(p, fromLib string) string {
	if l.cfg.RelativeObjectPaths == config.ObjectPathsFromFile {
		return NormalizeLibPath(p, fromLib)
	}
	return NormalizeLibPath(p, "/")
}

// NormalizeLibPath strips .c and makes p absolute relative to fromLib's
// directory.
func NormalizeLibPath(p, fromLib string) string {
	p = strings.TrimSuffix(strings.TrimSpace(p), ".c")
	if !strings.HasPrefix(p, "/") {
		p = path.Join(path.Dir(fromLib), p)
	}
	return path.Clean(p)
}

// excluded reports whether the config's exclude globs take the file or
// directory rel (lib-relative).
func (l *lib) excluded(rel string, dir bool) bool {
	return l.exclude.Match(rel) || (dir && l.exclude.MatchDir(rel))
}

// Walk visits every .c file under dir (a directory on disk) that the
// configuration does not exclude. It stops with ctx's error when ctx is
// cancelled.
func (l *lib) Walk(ctx context.Context, dir string, visit func(path string)) error {
	return l.lay.Walk(ctx, l.fs, dir, []string{".c"}, l.exclude, visit)
}

// scanRoot is the directory the whole-lib scan covers. With a partial
// checkout the checkout is all there is to scan: who else in the lib
// inherits these files cannot be known, and a wizard's own tree is taken
// to be self-contained.
func (l *lib) scanRoot() string {
	if l.partial {
		return l.lay.CheckoutDir
	}
	return l.lay.Root
}

// inScope reports whether Walk(scanRoot) visits p.
func (l *lib) inScope(p string) bool {
	return filepath.Ext(p) == ".c" && l.covers(p)
}

// covers reports whether p lies under the scan's root, in no directory
// the configuration excludes, and is not excluded itself or a stub. The
// answer depends on the configuration alone, and is remembered.
func (l *lib) covers(p string) bool {
	if in, ok := l.covered.get(p); ok {
		return in
	}
	in := l.decideCovers(p)
	return l.covered.getOrPut(p, func() bool { return in })
}

func (l *lib) decideCovers(p string) bool {
	root := l.scanRoot()
	if root == "" || l.isStub(p) {
		return false
	}
	below, ok := fileset.Under(root, p)
	if !ok {
		return false
	}
	rel, ok := l.rel(p)
	if !ok {
		return false
	}
	if l.excluded(rel, false) {
		return false
	}
	// The directories between the scan root and the file.
	for n := strings.Count(filepath.ToSlash(below), "/"); n > 0; n-- {
		rel = path.Dir(rel)
		if l.excluded(rel, true) {
			return false
		}
	}
	return true
}

// includeProvablyMissing reports whether an unresolved include is known
// not to exist: every place the driver would look is covered.
func (s *Snapshot) includeProvablyMissing(from string, inc pp.Include) bool {
	l := s.lib
	if !l.cfg.DriverKnown(config.KeyIncludeDirs) {
		// Nothing said where the driver looks: the include dirs are
		// unknown, not none. A "..." include not found as written is
		// looked for there too (Preproc::do_include).
		return false
	}
	if !l.partial {
		_, unknown := l.IncludeDirs(l.LibFile(from))
		return s.ext.complete && !unknown
	}
	var cands []string
	if !inc.Angle {
		if strings.HasPrefix(inc.Name, "/") {
			cands = append(cands, inc.Name)
		} else {
			rel, ok := l.rel(from)
			if !ok {
				return false
			}
			cands = append(cands, path.Join("/", path.Dir(rel), inc.Name))
		}
	}
	dirs, unknown := l.IncludeDirs(l.LibFile(from))
	if unknown {
		return false
	}
	for _, dir := range dirs {
		cands = append(cands, path.Join(dir, inc.Name))
	}
	for _, c := range cands {
		if !s.Covers(path.Clean(c)) {
			return false
		}
	}
	return len(cands) > 0
}
