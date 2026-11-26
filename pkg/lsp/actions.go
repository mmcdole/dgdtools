package lsp

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/format"
	"github.com/mmcdole/dgdtools/pkg/lexer"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/session"
)

// linted returns the document at uri as linted in its current version,
// which is returned too: from the last lint when that is still good, and
// by linting it now when not. It is nil for a document nothing is said
// about.
func (s *server) linted(ctx context.Context, uri string) (*lint.Document, int, *session.Session, error) {
	s.mu.Lock()
	doc, sess := s.docs[uri], s.session
	if doc == nil || doc.path == "" {
		s.mu.Unlock()
		return nil, 0, nil, nil
	}
	version, scanned := doc.version, doc.scanned
	if d := doc.linted; d != nil && doc.lintedVersion == version && doc.lintedIn == sess && sess.Current(d) {
		s.mu.Unlock()
		return d, version, sess, nil
	}
	s.mu.Unlock()

	d, _, err := lintWith(ctx, sess, doc.path, scanned)
	s.mu.Lock()
	defer s.mu.Unlock()
	var fe *lint.FileError
	switch {
	case errors.As(err, &fe) && fe.Kind == lint.Stale, doc.version != version, s.session != sess:
		return nil, 0, nil, errContentModified
	case err != nil:
		return nil, 0, nil, err
	case !sess.Current(d):
		return nil, 0, nil, errContentModified
	}
	doc.linted, doc.lintedVersion, doc.lintedIn, doc.lintedBy = d, version, sess, scanned
	return d, version, sess, nil
}

// wants reports whether a request that asks for the kinds only (for any,
// when it names none) asks for kind: a kind stands for those below it.
func wants(only []string, kind string) bool {
	return len(only) == 0 || slices.ContainsFunc(only, func(o string) bool {
		return kind == o || strings.HasPrefix(kind, o+".")
	})
}

// codeAction answers textDocument/codeAction with what can be done about
// the findings in the range: each fix a finding carries (a safe one is
// the preferred action), the path of the file a missing #include or
// object path may have meant (lint.Document.PathFixes), the comments
// that suppress it (lint.Suppress); at the start of the range, the
// prototype of a function called before the file defines it
// (Document.Prototype); and, for the document as a whole, every safe fix
// at once.
func (s *server) codeAction(ctx context.Context, p *CodeActionParams) ([]CodeAction, error) {
	uri := p.TextDocument.URI
	d, version, sess, err := s.linted(ctx, uri)
	if d == nil || err != nil {
		var modified *rpcError
		if err != nil && !errors.As(err, &modified) {
			s.log.Warn("no code actions", "uri", uri, "err", err)
			err = nil // the lint of the document says why, where it is seen
		}
		return nil, err
	}
	c := &converter{caps: s.caps, sess: sess, doc: d, uri: s.uris(),
		m: NewPositionMapper(d.File.Src, s.caps.encoding())}
	edit := func(edits []diag.TextEdit) *WorkspaceEdit {
		out := make([]TextEdit, 0, len(edits))
		for _, e := range edits {
			out = append(out, TextEdit{Range: c.m.Range(e.Start, e.End), NewText: e.NewText})
		}
		if s.caps.documentChanges {
			return &WorkspaceEdit{DocumentChanges: []TextDocumentEdit{{
				TextDocument: OptionalVersionedTextDocumentIdentifier{URI: uri, Version: &version}, Edits: out}}}
		}
		return &WorkspaceEdit{Changes: map[string][]TextEdit{uri: out}}
	}

	actions := []CodeAction{}
	start, end := c.m.Offset(p.Range.Start), c.m.Offset(p.Range.End)
	seen := map[string]bool{}
	if wants(p.Context.Only, KindQuickFix) {
		for i := range d.Diagnostics {
			f := &d.Diagnostics[i]
			if f.Start > end || f.End < start {
				continue
			}
			about := []Diagnostic{c.diagnostic(f)}
			for _, fix := range append(slices.Clone(f.Fixes), d.PathFixes(ctx, *f)...) {
				actions = append(actions, CodeAction{Title: fix.Title, Kind: KindQuickFix, Diagnostics: about,
					IsPreferred: fix.Applicability == diag.Safe, Edit: edit(fix.Edits)})
			}
			for _, fix := range lint.Suppress(d.File, *f) {
				// Two findings of a rule on a line have one comment.
				if key := fix.Title + "\x00" + fix.Edits[0].NewText; !seen[key] {
					seen[key] = true
					actions = append(actions, CodeAction{Title: fix.Title, Kind: KindQuickFix, Diagnostics: about, Edit: edit(fix.Edits)})
				}
			}
		}
	}
	if wants(p.Context.Only, KindRefactorRewrite) {
		if fix := d.Prototype(start); fix != nil {
			actions = append(actions, CodeAction{Title: fix.Title, Kind: KindRefactorRewrite, Edit: edit(fix.Edits)})
		}
	}
	if wants(p.Context.Only, KindFixAll) {
		if all, _ := diag.SafeEdits(d.Diagnostics); len(all) > 0 {
			actions = append(actions, CodeAction{Title: "Fix all safely fixable problems", Kind: KindFixAll, Edit: edit(all)})
		}
	}
	return actions, nil
}

// formatting answers textDocument/formatting and, with a range,
// textDocument/rangeFormatting: the edits that make the document's text
// what format.Source makes of it, with the settings of its project and
// not the editor's. A text the formatter refuses gets no edits, and a
// line in the log.
func (s *server) formatting(uri string, rng *Range) ([]TextEdit, error) {
	s.mu.Lock()
	doc, sess := s.docs[uri], s.session
	if doc == nil || doc.path == "" {
		s.mu.Unlock()
		return nil, nil
	}
	text, lines, version := doc.text, doc.lines, doc.version
	s.mu.Unlock()

	refused := func(err error) ([]TextEdit, error) {
		s.mu.Lock()
		s.logMessage(MessageInfo, "not formatted: "+err.Error())
		s.mu.Unlock()
		return nil, nil
	}
	prj, err := sess.Project(doc.path)
	if err != nil {
		return refused(err)
	}
	m := &PositionMapper{src: text, lines: lines, utf8: s.caps.utf8}
	var only []format.LineRange
	if rng != nil {
		only = []format.LineRange{lineRange(*rng)}
	}
	out, err := format.Source(prj, doc.path, text, only)
	if err != nil {
		return refused(err)
	}
	edits := []TextEdit{}
	for _, e := range format.Edits(lexer.Lex(doc.path, text, prj.Config.TokenDialect()), out) {
		edits = append(edits, TextEdit{Range: m.Range(e.Start, e.End), NewText: e.NewText})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if doc.version != version {
		return nil, errContentModified
	}
	return edits, nil
}

// lineRange returns the lines a range of the protocol selects, as the
// formatter counts them: a range that ends at the start of a line does
// not include that line.
func lineRange(r Range) format.LineRange {
	last := r.End.Line + 1
	if r.End.Character == 0 && r.End.Line > r.Start.Line {
		last--
	}
	return format.LineRange{First: r.Start.Line + 1, Last: last}
}
