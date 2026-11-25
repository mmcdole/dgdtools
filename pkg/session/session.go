// Package session is what a long-lived front end (an editor server)
// holds: the texts of the documents an editor has open, the projects of
// the files it is asked about, and a linter for each project, kept in
// step with one another.
//
// Everything that changes goes through the session: a document's buffer
// (Change) and what a file watcher reports (DiskChanged). One call applies
// one change to all of it — the text the tools read, the projects
// resolved from a configuration file, each linter's index — and returns
// whose diagnostics it can have altered. The tools then read through the
// session (FS, Project, Lint), never around it.
//
//	s := session.New(vfs.OS{}, config.DiscoverOptions{WorkDir: folder, ...})
//	affected := s.Change(path, text)             // didOpen, didChange
//	doc, err := s.Lint(ctx, path, lint.Options{NoScan: true})
//	if err == nil && s.Current(doc) { publish(doc.Diagnostics) }
//	affected = s.Change(path, nil)               // didClose
//	affected = s.DiskChanged(paths...)           // didChangeWatchedFiles
//
// A Session is safe for concurrent use. Changes are applied one at a
// time; lints run beside them and beside each other, each against one
// generation of its linter's index. A lint a change overtook is not
// Current, or fails as lint.Stale; either way it is made again.
package session

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"sync"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// Session is the documents, projects and linters of one front end.
type Session struct {
	fs *vfs.Overlay
	ws *config.Workspace

	// mu makes a change one step, and guards linters.
	mu      sync.Mutex
	linters map[*config.Project]*linter
}

// linter is a project's linter, or why it has none.
type linter struct {
	l   *lint.Linter
	err error
}

// New returns a session over the files of base. Projects are resolved
// with opt, as config.NewWorkspace resolves them.
func New(base vfs.FS, opt config.DiscoverOptions) *Session {
	fs := vfs.NewOverlay(base)
	return &Session{fs: fs, ws: config.NewWorkspace(fs, opt), linters: map[*config.Project]*linter{}}
}

// FS returns the files as the session's tools read them: the buffers of
// the open documents over the base file system.
func (s *Session) FS() vfs.FS { return s.fs }

// Affected says whose diagnostics a change can have altered.
type Affected struct {
	// Files are the files whose diagnostics may differ now, in sorted
	// order: the changed ones and what depends on them
	// (index.Index.Invalidate). A front end lints again those it shows.
	Files []string
	// All is set when the change was to what files are configured by: a
	// dgdtools.yml, an .editorconfig, a profile, a driver configuration
	// or a baseline. Projects were resolved again and their linters
	// replaced as needed; the settings and diagnostics of any file may
	// differ.
	All bool
}

// Change sets the buffer of the document at path (absolute) to text, or,
// with a nil text, drops it: the document was closed and the file is
// read from disk again.
func (s *Session) Change(path string, text []byte) Affected {
	return s.apply([]string{path}, func() {
		if text == nil {
			s.fs.Delete(path)
		} else {
			s.fs.Set(path, text)
		}
	})
}

// DiskChanged tells the session that the files or directories at paths
// (absolute) changed on disk, were created or were deleted. A file with
// a buffer still reads as its buffer.
func (s *Session) DiskChanged(paths ...string) Affected {
	return s.apply(paths, func() {})
}

// apply makes one change: set changes the texts, and paths are what
// changed.
func (s *Session) apply(paths []string, set func()) Affected {
	paths = slices.Clone(paths)
	for i, p := range paths {
		paths[i] = filepath.Clean(p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set()
	var a Affected
	for _, p := range paths {
		dropped, changed := s.ws.Invalidate(p)
		for _, prj := range dropped {
			delete(s.linters, prj)
		}
		a.All = a.All || changed
	}
	// A file may be part of more than one project's lib, so every linter
	// hears of every change; one that never read the file and has no
	// place for it is not disturbed.
	files := map[string]struct{}{}
	for _, e := range s.linters {
		if e.l == nil {
			continue
		}
		for _, p := range paths {
			a.All = a.All || e.l.BaselineChanged(p)
		}
		for _, f := range e.l.Invalidate(paths...) {
			files[f] = struct{}{}
		}
	}
	for _, p := range paths {
		files[p] = struct{}{}
	}
	a.Files = slices.Sorted(maps.Keys(files))
	return a
}

// Project returns the project of the file or directory at path
// (absolute), as config.Workspace.Project does. It is a new value after
// a change to what the project was resolved from.
func (s *Session) Project(path string) (*config.Project, error) {
	return s.ws.Project(path)
}

// LibPath returns the path the tools know the file or directory at path
// (absolute) by. It is path itself, unless path is in a directory outside
// any lib that a lib has a link to, named by its real path (as an editor
// that resolves links names it): then it is the path through the link,
// which is what the driver sees (config.Project.LibPath). The libs asked
// are those of the projects of dirs (an editor's workspace folders) and
// those of the projects resolved so far.
func (s *Session) LibPath(path string, dirs ...string) string {
	path = filepath.Clean(path)
	var prjs []*config.Project
	for _, d := range dirs {
		if prj, err := s.ws.Project(d); err == nil {
			prjs = append(prjs, prj)
		}
	}
	prjs = append(prjs, s.ws.Projects()...)
	// A file of a lib by its own name is that, and no lib's links need
	// be looked for.
	if slices.ContainsFunc(prjs, func(prj *config.Project) bool { return inLib(prj, path) }) {
		return path
	}
	for _, prj := range prjs {
		if p, ok := prj.LibPath(path); ok {
			// A directory with a lib of its own is that lib's,
			// whoever links to it.
			if own, err := s.ws.Project(path); err == nil && inLib(own, path) {
				return path
			}
			return p
		}
	}
	return path
}

// inLib reports whether path is in prj's lib on disk.
func inLib(prj *config.Project, path string) bool {
	_, in := prj.Tree().Rel(path)
	return in && prj.HasLib()
}

// Linter returns the linter of the project the file at path (absolute)
// belongs to. It lives as long as its project: until a change to the
// project's configuration replaces both.
func (s *Session) Linter(path string) (*lint.Linter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prj, err := s.ws.Project(path)
	if err != nil {
		return nil, err
	}
	e := s.linters[prj]
	if e == nil {
		e = &linter{}
		e.l, e.err = lint.New(prj, s.fs)
		s.linters[prj] = e
	}
	return e.l, e.err
}

// Lint lints the file at path with its project's linter; see
// lint.Linter.Lint.
func (s *Session) Lint(ctx context.Context, path string, opt lint.Options) (*lint.Document, error) {
	l, err := s.Linter(path)
	if err != nil {
		return nil, err
	}
	return l.Lint(ctx, path, opt)
}

// Current reports whether doc is still what linting its file gives: no
// change has reached its project or the lib since it was linted. A
// front end publishes a document that is current and lints again one
// that is not.
func (s *Session) Current(doc *lint.Document) bool {
	l, err := s.Linter(doc.Path)
	return err == nil && l.Index() == doc.Snapshot.Index() && doc.Snapshot.Current()
}
