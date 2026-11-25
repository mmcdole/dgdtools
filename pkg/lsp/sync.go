package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/editorconfig"
	"github.com/mmcdole/dgdtools/pkg/fileset"
	"github.com/mmcdole/dgdtools/pkg/index"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/session"
)

// document is a document the editor has open: its text as the editor
// has it, which is what the tools read while it is open (the session
// has the same bytes), and where its linting stands.
type document struct {
	uri string
	// path is the file the URI names, "" when it names none; such a
	// document is kept in step and nothing is said about it.
	path    string
	version int
	// text is never changed in place: the session and the lints of
	// earlier versions hold it.
	text  []byte
	lines []uint32

	// ticket is the lint that is scheduled, due when it is to start, and
	// own whether it is for a change to the document itself. running is
	// the lint that is under way, and cancel stops it.
	ticket  *ticket
	due     time.Time
	own     bool
	running *ticket
	cancel  context.CancelFunc

	// linted is the document as last linted, of the version and in the
	// session named, and inputs the files its project is configured by.
	linted        *lint.Document
	lintedVersion int
	lintedIn      *session.Session
	lintedBy      *lint.Linter // the document's scanned when it was linted
	inputs        []string
	// scanned is the linter the document was linted by with the scan of
	// the whole lib complete, and rescanned the generation of the lib a
	// lint after the scan was last asked for.
	scanned   *lint.Linter
	rescanned *index.Snapshot
	// published is what the client was last told of the document, and
	// shown whether that was anything.
	published []byte
	shown     bool
	// tokens are the semantic tokens last sent in full or as a delta.
	tokens *sentTokens
}

// ticket is one scheduled lint.
type ticket struct{ timer *time.Timer }

// change applies one change of a textDocument/didChange.
func (d *document) change(c *TextDocumentContentChangeEvent, encoding string) {
	if c.Range == nil {
		d.text = []byte(c.Text)
	} else {
		// The positions are of the text before the change.
		m := &PositionMapper{src: d.text, lines: d.lines, utf8: encoding == UTF8}
		start, end := m.Offset(c.Range.Start), m.Offset(c.Range.End)
		if end < start {
			start, end = end, start
		}
		text := make([]byte, 0, len(d.text)-int(end-start)+len(c.Text))
		text = append(text, d.text[:start]...)
		text = append(text, c.Text...)
		d.text = append(text, d.text[end:]...)
	}
	d.lines = lineStarts(d.text)
}

// byPath returns the open document of the file at path.
func (s *server) byPath(path string) *document {
	for _, doc := range s.docs {
		if doc.path == path {
			return doc
		}
	}
	return nil
}

func (s *server) didOpen(p *DidOpenTextDocumentParams) {
	uri := p.TextDocument.URI
	if old := s.docs[uri]; old != nil {
		s.unschedule(old) // opened twice: the second is what it is now
	}
	doc := &document{uri: uri, path: pathOf(uri), version: p.TextDocument.Version, text: []byte(p.TextDocument.Text)}
	doc.lines = lineStarts(doc.text)
	s.docs[uri] = doc
	if doc.path == "" {
		return
	}
	s.encodingHint(doc)
	// The editor's text is the file from now on, whether or not it is
	// what the disk has.
	s.changed(doc, s.session.Change(doc.path, doc.text))
}

func (s *server) didChange(p *DidChangeTextDocumentParams) {
	doc := s.docs[p.TextDocument.URI]
	if doc == nil {
		s.log.Warn("change to a document that is not open", "uri", p.TextDocument.URI)
		return
	}
	for i := range p.ContentChanges {
		doc.change(&p.ContentChanges[i], s.caps.encoding())
	}
	doc.version = p.TextDocument.Version
	if doc.path != "" {
		s.changed(doc, s.session.Change(doc.path, doc.text))
	}
}

func (s *server) didClose(p *DidCloseTextDocumentParams) {
	doc := s.docs[p.TextDocument.URI]
	if doc == nil {
		return
	}
	delete(s.docs, doc.uri)
	s.unschedule(doc)
	// Diagnostics are of open documents: nothing would keep those of a
	// closed one true.
	if doc.shown {
		s.notify("textDocument/publishDiagnostics", PublishDiagnosticsParams{URI: doc.uri, Diagnostics: []Diagnostic{}})
	}
	if doc.path != "" {
		// The disk is the file again, which may be another text.
		s.changed(nil, s.session.Change(doc.path, nil))
	}
}

func (s *server) didChangeWatchedFiles(p *DidChangeWatchedFilesParams) {
	var paths []string
	for _, c := range p.Changes {
		// A file that is open is its buffer whatever the disk has: for
		// the tools nothing changed. A directory is passed on as the
		// directory.
		if path := pathOf(c.URI); path != "" && s.byPath(path) == nil && !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	if len(paths) > 0 {
		s.log.Debug("files changed", "paths", paths)
		s.changed(nil, s.session.DiskChanged(paths...))
	}
}

// changed schedules the lints a change calls for: of the document that
// was edited, soon; of the other open documents it can have altered the
// diagnostics of, later, so that typing in a header does not lint every
// file that includes it at each key.
func (s *server) changed(edited *document, a session.Affected) {
	if edited != nil {
		s.schedule(edited, s.opt.Debounce, true)
	}
	if a.All {
		// The configuration changed: what was said about the old one is
		// said again about the new when it still holds.
		clear(s.said)
	}
	for _, doc := range s.docs {
		switch {
		case doc == edited:
		case a.All:
			s.schedule(doc, s.opt.Debounce, false)
		default:
			if _, ok := slices.BinarySearch(a.Files, doc.path); ok {
				s.schedule(doc, s.opt.DependentDebounce, false)
			}
		}
	}
}

// encodingHint warns, once, when a file that is not UTF-8 on disk was
// opened as if it were: the editor shows replacement characters, and
// saving from there damages the file. The tools read the text the editor
// has either way.
func (s *server) encodingHint(doc *document) {
	const key = "encoding"
	if s.said[key] || !bytes.ContainsRune(doc.text, utf8.RuneError) {
		return
	}
	if disk, err := s.opt.FS.ReadFile(doc.path); err == nil && !utf8.Valid(disk) {
		s.tell(MessageWarning, fmt.Sprintf("%s is not UTF-8 on disk and was opened as UTF-8: saving it from here would damage it. "+
			"Open it in the encoding it has (Latin-1 for most old mudlibs), and set that as the encoding of the mudlib's files.", doc.path))
		s.said[key] = true
	}
}

// The editor's settings say two things only, each about one workspace
// folder that has no dgdtools.yml: the profile to work against and the
// lib path of the folder. Everything else is the project's own.

type settings struct {
	Profile string `json:"profile"`
	LibPath string `json:"libPath"`
}

// folder is a workspace folder and what the settings say of it.
type folder struct {
	uri, dir string
	set      settings
}

func (s *server) addFolder(uri string) {
	if !slices.ContainsFunc(s.folders, func(f folder) bool { return f.uri == uri }) {
		s.folders = append(s.folders, folder{uri: uri, dir: pathOf(uri), set: s.defaults})
	}
}

func (s *server) didChangeWorkspaceFolders(p *DidChangeWorkspaceFoldersParams) {
	for _, f := range p.Event.Removed {
		s.folders = slices.DeleteFunc(s.folders, func(have folder) bool { return have.uri == f.URI })
	}
	for _, f := range p.Event.Added {
		s.addFolder(f.URI)
	}
	s.settle()
}

func (s *server) didChangeConfiguration(p *DidChangeConfigurationParams) {
	if !s.caps.configuration {
		// A client that cannot be asked says it here, for every folder.
		var all struct {
			Dgdtools settings `json:"dgdtools"`
		}
		if err := json.Unmarshal(p.Settings, &all); err != nil {
			s.log.Warn("settings not understood", "err", err)
			return
		}
		s.defaults = all.Dgdtools
		for i := range s.folders {
			s.folders[i].set = s.defaults
		}
	}
	s.settle()
}

// settle brings the server in line with the folders and their settings
// as they are now: it asks the client for the settings, when the client
// can be asked, and then sees to the session and the watched files.
func (s *server) settle() {
	if !s.caps.configuration || len(s.folders) == 0 {
		s.resettle()
		return
	}
	params := ConfigurationParams{}
	for _, f := range s.folders {
		params.Items = append(params.Items, ConfigurationItem{ScopeURI: f.uri, Section: "dgdtools"})
	}
	s.spawn("workspace/configuration", func() {
		var got []json.RawMessage
		if err := s.conn.Call(s.ctx, "workspace/configuration", params, &got); err != nil {
			s.log.Warn("settings not read", "err", err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, item := range params.Items {
			for j := range s.folders {
				if f := &s.folders[j]; f.uri == item.ScopeURI && i < len(got) {
					// A folder the settings say nothing of has what
					// the client said at the start.
					f.set = s.defaults
					if err := json.Unmarshal(got[i], &f.set); err != nil {
						s.log.Warn("settings not understood", "folder", f.uri, "err", err)
					}
				}
			}
		}
		s.resettle()
	})
}

// resettle makes a new session when the folders' settings are not those
// the current one resolves projects with: a session is made for one set
// of them. The open documents are its buffers again, and all are linted.
func (s *server) resettle() {
	defer s.rewatch()
	opt := s.discover()
	if s.session != nil && slices.Equal(opt.Folders, s.resolved) {
		return
	}
	s.resolved = opt.Folders
	s.session = session.New(s.opt.FS, opt)
	clear(s.scans)
	clear(s.said)
	for _, doc := range s.docs {
		if doc.path != "" {
			s.session.Change(doc.path, doc.text)
			s.schedule(doc, s.opt.Debounce, false)
		}
	}
}

// watched are the files whose changes on disk the client is asked to
// report, wherever they are: sources, and everything a file can be
// configured by.
var watched = []string{
	"**/*.{c,h}",
	"**/" + config.FileName,
	"**/" + config.AltFileName,
	"**/" + editorconfig.FileName,
	"**/*.profile",
	"**/" + lint.BaselineFileName,
}

// covered reports whether the patterns of watched have the file at path.
// A pattern reaches into the workspace folders and no further.
func (s *server) covered(path string) bool {
	in := slices.ContainsFunc(s.folders, func(f folder) bool {
		_, under := fileset.Under(f.dir, path)
		return f.dir != "" && under
	})
	name := filepath.Base(path)
	switch ext := filepath.Ext(name); {
	case !in:
		return false
	case ext == ".c" || ext == ".h" || ext == ".profile":
		return true
	}
	return name == config.FileName || name == config.AltFileName || name == editorconfig.FileName || name == lint.BaselineFileName
}

// watch is what the client was asked to watch beyond the patterns: the
// files the projects in use are configured by that the patterns do not
// have (a profile in the user's directory, the driver's configuration, a
// baseline somewhere else).
type watch struct {
	registered bool
	id         int
	have, want []string
	busy       bool
}

// rewatch registers the file watchers again when what is to be watched
// is not what the client was last asked for.
func (s *server) rewatch() {
	if !s.caps.watch {
		return
	}
	var want []string
	for _, doc := range s.docs {
		for _, in := range doc.inputs {
			if !s.covered(in) && !slices.Contains(want, in) {
				want = append(want, in)
			}
		}
	}
	slices.Sort(want)
	w := &s.watch
	if w.want = want; w.busy || (w.registered && slices.Equal(w.have, want)) {
		return
	}
	w.busy = true
	s.spawn("client/registerCapability", func() {
		const method = "workspace/didChangeWatchedFiles"
		for {
			s.mu.Lock()
			want, was := w.want, w.id
			if w.registered && slices.Equal(w.have, want) {
				w.busy = false
				s.mu.Unlock()
				return
			}
			unregister := w.registered
			w.id++
			id := w.id
			s.mu.Unlock()

			opt := DidChangeWatchedFilesRegistrationOptions{}
			for _, glob := range watched {
				opt.Watchers = append(opt.Watchers, FileSystemWatcher{GlobPattern: glob})
			}
			for _, path := range want {
				var glob any = filepath.ToSlash(path)
				if s.caps.relativeGlob {
					glob = RelativePattern{BaseURI: uriOf(filepath.Dir(path)), Pattern: filepath.Base(path)}
				}
				opt.Watchers = append(opt.Watchers, FileSystemWatcher{GlobPattern: glob})
			}
			name := func(id int) string { return fmt.Sprintf("dgdtools-watch-%d", id) }
			if unregister {
				err := s.conn.Call(s.ctx, "client/unregisterCapability",
					UnregistrationParams{Unregisterations: []Unregistration{{ID: name(was), Method: method}}}, nil)
				if err != nil {
					s.log.Warn("file watchers not unregistered", "err", err)
				}
			}
			err := s.conn.Call(s.ctx, "client/registerCapability",
				RegistrationParams{Registrations: []Registration{{ID: name(id), Method: method, RegisterOptions: opt}}}, nil)
			s.mu.Lock()
			if err != nil {
				// Without watchers the server still has the buffers
				// right; what changes on disk it does not see.
				s.log.Warn("file watchers not registered", "err", err)
				w.busy = false
				s.mu.Unlock()
				return
			}
			w.registered, w.have = true, want
			s.mu.Unlock()
		}
	})
}

// describe says in a line what project a file belongs to.
func describe(prj *config.Project) string {
	var b strings.Builder
	switch {
	case prj.ConfigPath == "":
		b.WriteString("no dgdtools.yml")
	case prj.UserConfig:
		b.WriteString("user configuration " + prj.ConfigPath)
	default:
		b.WriteString("project " + prj.ConfigPath)
	}
	switch {
	case prj.Root != "":
		b.WriteString(": mudlib " + prj.Root)
	case prj.CheckoutDir != "":
		b.WriteString(": " + prj.CheckoutDir + " is " + prj.CheckoutLib)
	case !prj.HasLib():
		b.WriteString(": no mudlib, file-local rules only")
	}
	return b.String()
}
