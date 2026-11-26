package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/session"
)

// schedule arranges for doc to be linted after delay, in place of any
// lint of it that is scheduled or running: that one would be of a text,
// or of a lib, that is no more. own says the document itself changed;
// a lint scheduled for that is not put off by one for something else.
func (s *server) schedule(doc *document, delay time.Duration, own bool) {
	if doc.path == "" {
		return
	}
	if doc.cancel != nil {
		doc.cancel()
	}
	due := time.Now().Add(delay)
	if doc.ticket != nil {
		if !own && doc.own && doc.due.Before(due) {
			return
		}
		s.unschedule(doc)
	}
	if !s.job() {
		return
	}
	s.begin()
	t := &ticket{}
	doc.ticket, doc.due, doc.own = t, due, own
	t.timer = time.AfterFunc(delay, func() {
		defer s.jobs.Done()
		s.guard("lint", func() { s.lint(doc, t) })
		s.mu.Lock()
		s.end()
		s.mu.Unlock()
	})
}

// unschedule takes back the lint that is scheduled for doc and stops the
// one that is running.
func (s *server) unschedule(doc *document) {
	if doc.cancel != nil {
		doc.cancel()
	}
	if t := doc.ticket; t != nil {
		doc.ticket = nil
		// A timer that can no longer be stopped has fired: its function
		// finds the ticket is not the document's and gives up by itself.
		if t.timer.Stop() {
			s.end()
			s.jobs.Done()
		}
	}
}

// lint is a scheduled lint coming due: the document is linted as it is
// now, and what was found is published if, when the lint is done, it is
// still of the document's current version and of the lib as it is.
// Otherwise the document is linted again.
func (s *server) lint(doc *document, t *ticket) {
	s.mu.Lock()
	if doc.ticket != t || s.closed {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	doc.ticket, doc.running, doc.cancel = nil, t, cancel
	sess, version, scanned := s.session, doc.version, doc.scanned
	// A request may have linted the document already, as it is now and
	// as this lint would (a client asks for semantic tokens at once
	// after a change): that lint is this one's, unless it left questions
	// for the scan.
	reuse := doc.linted
	if reuse != nil && (doc.lintedVersion != version || doc.lintedIn != sess || doc.lintedBy != scanned ||
		reuse.NeedsScan || !sess.Current(reuse)) {
		reuse = nil
	}
	s.mu.Unlock()

	start := time.Now()
	var (
		d   *lint.Document
		l   *lint.Linter
		err error
	)
	if reuse != nil {
		d = reuse
		l, err = sess.Linter(doc.path)
	} else {
		d, l, err = lintWith(ctx, sess, doc.path, scanned)
	}
	var (
		found  []Diagnostic
		tokens []uint32
		prj    *config.Project
		inputs []string
	)
	if err == nil {
		found = s.diagnostics(sess, d)
		if s.caps.tokens {
			// Made here, beside the diagnostics, so that a request for
			// them finds them made.
			tokens = EncodeTokens(NewPositionMapper(d.File.Src, s.caps.encoding()), d.Tokens())
		}
		prj = l.Project()
		inputs = prj.Inputs()
		if b := l.BaselinePath(); b != "" {
			inputs = append(inputs, b)
		}
	}
	s.log.Debug("lint", "path", doc.path, "version", version, "took", time.Since(start), "reused", reuse != nil, "err", err)

	s.mu.Lock()
	defer s.mu.Unlock()
	if doc.running == t {
		doc.running, doc.cancel = nil, nil
	}
	if s.docs[doc.uri] != doc || ctx.Err() != nil {
		return // closed, or overtaken by a change that scheduled its own
	}
	var fe *lint.FileError
	stale := errors.As(err, &fe) && fe.Kind == lint.Stale
	if stale || doc.version != version || s.session != sess || (err == nil && !sess.Current(d)) {
		// Something changed under the lint that did not schedule this
		// document: it is of a lib that is no more.
		if doc.ticket == nil {
			s.schedule(doc, s.opt.Debounce, false)
		}
		return
	}
	if err != nil {
		// Nothing can be said about the document; what was said before
		// is not kept as if it still held.
		if fe != nil {
			s.logMessage(MessageError, fe.Error())
		} else {
			s.tellOnce(MessageError, "dgdtools: "+err.Error())
		}
		s.publish(doc, version, nil)
		return
	}
	for _, e := range d.Errors {
		s.logMessage(MessageError, e.Error())
	}
	doc.linted, doc.lintedVersion, doc.lintedIn, doc.lintedBy, doc.inputs = d, version, sess, scanned, inputs
	s.publish(doc, version, found)
	if tokens != nil {
		s.tokensChanged(doc, version, tokens)
	}
	s.announce(prj, d)
	s.rewatch()
	switch {
	case !d.NeedsScan:
		if d.Snapshot.Scanned() {
			doc.scanned = l
		}
	case !d.Snapshot.Scanned():
		s.scan(sess, l, doc)
	case doc.rescanned != d.Snapshot:
		// The scan was completed while the document was being linted.
		doc.rescanned = d.Snapshot
		s.schedule(doc, 0, false)
	}
}

// lintWith lints the file at path by its linter in sess, which is
// returned too. The first scan of a lib takes seconds and is made beside
// the lints, which say what they can without it (lint.Options.NoScan).
// Once a document has been linted with the scan complete, by the linter
// scanned, its lints keep it complete themselves, a file or two after
// each change, rather than say less for a moment than was said before.
func lintWith(ctx context.Context, sess *session.Session, path string, scanned *lint.Linter) (*lint.Document, *lint.Linter, error) {
	l, err := sess.Linter(path)
	if err != nil {
		return nil, nil, err
	}
	d, err := l.Lint(ctx, path, lint.Options{NoScan: l != scanned})
	return d, l, err
}

// publish tells the client the diagnostics of doc at the given version,
// unless that is what it was last told.
func (s *server) publish(doc *document, version int, found []Diagnostic) {
	if found == nil {
		found = []Diagnostic{}
	}
	data, err := json.Marshal(PublishDiagnosticsParams{URI: doc.uri, Version: &version, Diagnostics: found})
	if err != nil || bytes.Equal(data, doc.published) {
		return
	}
	doc.published, doc.shown = data, len(found) > 0
	s.notify("textDocument/publishDiagnostics", json.RawMessage(data))
}

// announce logs, once for each project, what project a document was
// found to belong to and the remarks about it.
func (s *server) announce(prj *config.Project, d *lint.Document) {
	what := describe(prj)
	if s.said[what] {
		return
	}
	s.said[what] = true
	s.logMessage(MessageInfo, what)
	for _, n := range append(prj.Notes, d.Snapshot.Notes()...) {
		s.logMessage(MessageInfo, "note: "+n.Message)
	}
}

// scan is a whole-lib scan under way, and the documents to lint again
// when it is done: those whose lints had questions only it answers.
type scan struct {
	docs map[*document]bool
}

// progressDelay is how long a scan runs before the client is told of it;
// in a checkout it is over long before.
var progressDelay = 300 * time.Millisecond

// scan runs the whole-lib scan of l beside everything else, unless it is
// running already, and has doc linted again when it is done.
func (s *server) scan(sess *session.Session, l *lint.Linter, doc *document) {
	if sc := s.scans[l]; sc != nil {
		sc.docs[doc] = true
		return
	}
	sc := &scan{docs: map[*document]bool{doc: true}}
	s.scans[l] = sc
	s.spawn("scan", func() {
		done := make(chan struct{})
		var progress sync.WaitGroup
		if s.caps.progress {
			progress.Add(1)
			go func() {
				defer progress.Done()
				s.progress(done, fmt.Sprintf("dgdtools-scan-%p", sc), "dgdtools: indexing the mudlib")
			}()
		}
		// A scan that a change overtakes stops, having kept what it
		// read; the next one goes on from there.
		start := time.Now()
		ok := false
		for {
			gen := l.Generation()
			if ok = l.Scan(s.ctx); ok || s.ctx.Err() != nil || l.Generation() == gen {
				break
			}
		}
		took := time.Since(start).Seconds()
		close(done)
		progress.Wait()

		s.mu.Lock()
		defer s.mu.Unlock()
		if s.scans[l] == sc {
			delete(s.scans, l)
		}
		root := l.Project().Root
		if l.Project().Partial {
			root = l.Project().Base
		}
		switch {
		case ok && !s.said["scanned "+root]:
			// Once: the scans after a change read only what it touched.
			s.said["scanned "+root] = true
			n := l.Index().Snapshot().ScannedFiles()
			s.logMessage(MessageInfo, fmt.Sprintf("mudlib scanned: %d files in %.1f s (%s)", n, took, root))
		case !ok && s.ctx.Err() != nil:
			s.log.Info("mudlib scan stopped", "root", root, "seconds", took)
		case !ok:
			s.logMessage(MessageWarning, fmt.Sprintf("mudlib scan did not complete after %.1f s (%s)", took, root))
		}
		if !ok || s.session != sess {
			return
		}
		for doc := range sc.docs {
			if s.docs[doc.uri] == doc && doc.ticket == nil {
				s.schedule(doc, 0, false)
			}
		}
	})
}

// progress shows the client that something is going on until done is
// closed, when that takes long enough to be worth showing.
func (s *server) progress(done <-chan struct{}, token, title string) {
	select {
	case <-done:
		return
	case <-time.After(progressDelay):
	}
	if err := s.conn.Call(s.ctx, "window/workDoneProgress/create", WorkDoneProgressCreateParams{Token: token}, nil); err != nil {
		return
	}
	s.mu.Lock()
	s.notify("$/progress", ProgressParams{Token: token, Value: WorkDoneProgress{Kind: "begin", Title: title}})
	s.mu.Unlock()
	<-done
	s.mu.Lock()
	s.notify("$/progress", ProgressParams{Token: token, Value: WorkDoneProgress{Kind: "end"}})
	s.mu.Unlock()
}

// diagnostics returns the findings of d as the protocol has them.
func (s *server) diagnostics(sess *session.Session, d *lint.Document) []Diagnostic {
	c := &converter{caps: s.caps, version: s.opt.Version, sess: sess, doc: d,
		uri: s.uris(),
		m:   NewPositionMapper(d.File.Src, s.caps.encoding())}
	out := make([]Diagnostic, 0, len(d.Diagnostics))
	for i := range d.Diagnostics {
		out = append(out, c.diagnostic(&d.Diagnostics[i]))
	}
	return out
}

// converter turns the findings of one document into the protocol's.
type converter struct {
	caps    capabilities
	version string // the server's, for the address of a rule's page
	sess    *session.Session
	doc     *lint.Document
	m       *PositionMapper
	// uri names a file to the client (server.uris).
	uri func(string) string
	// others are the mappers of the other files related locations are
	// in, nil for one that cannot be read.
	others map[string]*PositionMapper
}

func (c *converter) diagnostic(d *diag.Diagnostic) Diagnostic {
	out := Diagnostic{
		Range:    c.m.Range(d.Start, d.End),
		Severity: SeverityInformation,
		Code:     d.Code,
		Source:   "dgdtools",
		Message:  d.Message,
	}
	switch d.Severity {
	case diag.Error:
		out.Severity = SeverityError
	case diag.Warning:
		out.Severity = SeverityWarning
	}
	if r := lint.RuleByName(d.Rule); r != nil && c.caps.href {
		out.CodeDescription = &CodeDescription{Href: r.URL(c.version)}
	}
	if c.caps.tags {
		for _, t := range d.Tags {
			out.Tags = append(out.Tags, int(t)) // a diag.Tag has the protocol's value
		}
	}
	if c.caps.related {
		for i := range d.Related {
			r := &d.Related[i]
			out.RelatedInformation = append(out.RelatedInformation, DiagnosticRelatedInformation{
				Location: Location{URI: c.uri(r.Path), Range: c.related(r)},
				Message:  r.Message,
			})
		}
	}
	return out
}

// related returns the range of a related location, which may be in
// another file than the document: one the editor has open, whose text is
// then the editor's, a file on disk, or a stub of a profile. Of a file
// that is not open the bytes are as on disk, Latin-1 or whatever they
// are, and a byte that is no UTF-8 counts as one unit, which is how an
// editor that reads the file in a single-byte encoding counts it.
func (c *converter) related(r *diag.Related) Range {
	if r.Path == c.doc.Path {
		return c.m.Range(r.Start, r.End)
	}
	bytes := Range{
		Start: Position{Line: r.Line - 1, Character: r.Col - 1},
		End:   Position{Line: r.EndLine - 1, Character: r.EndCol - 1},
	}
	if r.EndLine == 0 {
		bytes.End = bytes.Start
	}
	if c.caps.utf8 {
		return bytes // a column is a byte offset: there is nothing to convert
	}
	m, read := c.others[r.Path]
	if !read {
		if src, err := c.sess.FS().ReadFile(r.Path); err == nil {
			m = NewPositionMapper(src, UTF16)
		}
		if c.others == nil {
			c.others = map[string]*PositionMapper{}
		}
		c.others[r.Path] = m
	}
	if m == nil {
		return bytes
	}
	return m.Range(r.Start, r.End)
}
