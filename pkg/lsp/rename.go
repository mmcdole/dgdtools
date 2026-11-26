package lsp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/rename"
	"github.com/mmcdole/dgdtools/pkg/resolve"
)

// prepareRename answers textDocument/prepareRename: the name at the
// position and its range, which the client selects for the new name, or
// an error that says why it cannot be renamed (rename.Prepare, through
// lint.Document.PrepareRename). Nothing named there is null.
func (s *server) prepareRename(ctx context.Context, p *PrepareRenameParams) (any, error) {
	d, err := s.asked(ctx, p.TextDocument.URI, "prepareRename")
	if d == nil || err != nil {
		return nil, err
	}
	m := NewPositionMapper(d.File.Src, s.caps.encoding())
	off := m.Offset(p.Position)
	scanFor(ctx, d, off)
	t, err := d.PrepareRename(ctx, off)
	if err != nil {
		return nil, renameError(ctx, err)
	}
	if t == nil {
		return nil, nil
	}
	return &PrepareRenameResult{Range: m.Range(t.Start, t.End), Placeholder: t.Name}, nil
}

// rename answers textDocument/rename: the edits of rename.Rename's plan
// (lint.Document.Rename), in the document and every other file that
// names the symbol, each file's for the version of it they were made from
// (a file the client has not open: null) when the client reads that
// (documentChanges); or an error that says why the name cannot be
// renamed, or not to the new name.
//
// The certain edits are made. The possible ones (a call on an object
// nobody can tell, a name in a macro's body, code that does not parse)
// go to a client that can confirm edits (changeAnnotationSupport) under
// annotations that need confirmation (annotate); another client gets the
// certain edits only, and is told so. What the plan has to tell is shown
// or logged by planMessage.
func (s *server) rename(ctx context.Context, p *RenameParams) (any, error) {
	uri := p.TextDocument.URI
	d, version, _, err := s.linted(ctx, uri)
	if d == nil || err != nil {
		var modified *rpcError
		if err != nil && !errors.As(err, &modified) {
			s.log.Warn("no rename", "uri", uri, "err", err)
			err = nil
		}
		return nil, err
	}
	m := NewPositionMapper(d.File.Src, s.caps.encoding())
	off := m.Offset(p.Position)
	scanFor(ctx, d, off)
	plan, err := d.Rename(ctx, off, p.NewName, false)
	if err != nil {
		return nil, renameError(ctx, err)
	}
	byPath := map[string][]edit{}
	for _, e := range plan.Edits {
		byPath[e.Path] = append(byPath[e.Path], edit{Edit: e})
	}
	annotations := s.annotate(plan, byPath)
	s.mu.Lock()
	doc := s.docs[uri]
	current := doc != nil && doc.path == d.Path && doc.version == version
	s.mu.Unlock()
	if !current {
		return nil, errContentModified
	}
	result, notUTF8, err := s.workspaceEdit(byPath, annotations, func(path string) ([]byte, error) {
		if path == d.Path {
			return d.File.Src, nil
		}
		return d.Snapshot.ReadFile(path)
	})
	if err != nil {
		return nil, err
	}
	if !d.Snapshot.Current() {
		return nil, errContentModified
	}
	name := func(p string) string { return cmp.Or(d.Snapshot.LibFile(p), p) }
	s.mu.Lock()
	s.planMessage(plan, annotations != nil, name, notUTF8)
	s.mu.Unlock()
	return result, nil
}

// annotate adds the plan's possible edits to byPath, which has its
// certain ones, when the client can confirm edits (changeAnnotationSupport):
// the certain edits under one annotation, the possible ones under
// annotations that need confirmation, one for each reason, so that the
// client's preview shows them unticked. It returns the annotations; nil,
// and nothing added, when the client cannot confirm edits or the plan has
// no possible edits.
func (s *server) annotate(plan *rename.Plan, byPath map[string][]edit) map[string]ChangeAnnotation {
	if !s.caps.annotations || len(plan.PossibleEdits) == 0 {
		return nil
	}
	certain := "Certain: the renamed " + plan.What()
	if plan.Moves != nil {
		certain = "Certain: the paths that name the moved files"
	}
	annotations := map[string]ChangeAnnotation{"certain": {Label: certain}}
	for k := range byPath {
		for i := range byPath[k] {
			byPath[k][i].annotation = "certain"
		}
	}
	ids := map[string]string{}
	reasons := map[[2]any]string{}
	for _, o := range plan.Occurrences {
		if o.Class == rename.Possible {
			reasons[[2]any{o.Path, o.Start}] = o.Reason
		}
	}
	desc := "may be the renamed " + plan.What() + ", and nobody can tell"
	if plan.Moves != nil {
		desc = "may name a moved file, and nobody can tell"
	}
	for _, e := range plan.PossibleEdits {
		reason := reasons[[2]any{e.Path, e.Start}]
		id, ok := ids[reason]
		if !ok {
			id = fmt.Sprintf("possible-%d", len(ids)+1)
			ids[reason] = id
			annotations[id] = ChangeAnnotation{Label: "Possible: " + reason, NeedsConfirmation: true, Description: desc}
		}
		byPath[e.Path] = append(byPath[e.Path], edit{Edit: e, annotation: id})
	}
	return annotations
}

// planMessage tells the user what the plan has to tell beside its edits
// (rename.Plan.Notes), in one message: by window/showMessage when there is
// something to look at (possible places, ticked or not, calls that may
// call the renamed function, an edited file that is not UTF-8), else only
// in the log (window/logMessage). annotated says the possible edits are
// in the edit, unticked; name says how a file is named; notUTF8 are the
// edited files whose text is not valid UTF-8. Called with s.mu held.
func (s *server) planMessage(plan *rename.Plan, annotated bool, name func(string) string, notUTF8 []string) {
	var parts []string
	look := false
	possible := plan.Count(rename.Possible)
	for _, n := range plan.Notes(false) {
		text := n.Text
		if n.Kind == rename.NotePossible && annotated && len(plan.PossibleEdits) == possible {
			text = plan.PossiblePlaces() + ", and nobody can tell: " + verb(possible, "it is", "they are") + " in the preview, not ticked"
		}
		if len(n.Places) > 0 {
			var at []string
			for _, p := range n.Places[:min(len(n.Places), 5)] {
				at = append(at, fmt.Sprintf("%s:%d (%s)", name(p.Path), p.Line, p.Detail))
			}
			text += ": " + strings.Join(at, ", ")
			if more := len(n.Places) - len(at); more > 0 {
				text += fmt.Sprintf(" and %d more", more)
			}
		}
		if n.Kind == rename.NotePossible && len(plan.PossibleEdits) > 0 {
			switch {
			case annotated && len(plan.PossibleEdits) < possible:
				text += "; those that can be edited are in the preview, not ticked"
			case !annotated && plan.Moves == nil:
				text += "; `dgdtools rename --include-possible` renames " + verb(possible, "it", "them") + " too"
			}
		}
		look = look || n.Look(false)
		parts = append(parts, text)
	}
	if len(notUTF8) > 0 {
		look = true
		parts = append(parts, fmt.Sprintf("%s not valid UTF-8 (%s): its edits are right for an editor that reads it as Latin-1, as the driver does, and an editor that reads it as UTF-8 to apply them may change its other characters",
			verbFiles(len(notUTF8)), strings.Join(names(notUTF8[:min(len(notUTF8), 5)], name), ", ")))
	}
	if len(parts) == 0 {
		return
	}
	msg := plan.Title() + ": " + strings.Join(parts, "; ")
	if look {
		s.tell(MessageWarning, msg)
	} else {
		s.logMessage(MessageInfo, msg)
	}
}

func verb(n int, one, more string) string {
	if n == 1 {
		return one
	}
	return more
}

// names names the files.
func names(paths []string, name func(string) string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = name(p)
	}
	return out
}

// verbFiles says how many files are something: "1 edited file is", "2
// edited files are".
func verbFiles(n int) string {
	if n == 1 {
		return "1 edited file is"
	}
	return fmt.Sprintf("%d edited files are", n)
}

// edit is an edit of a plan, with the id of its change annotation ("":
// none).
type edit struct {
	rename.Edit
	annotation string
}

// workspaceEdit lays the edits of each file out as a WorkspaceEdit: in
// documentChanges, each file's for the version of it they were made from
// (an open document's; null for a file the client has not open, whose
// text is the disk's), when the client reads that, else in changes. src
// returns the text a file's edits were made in. notUTF8 are the files
// whose text is not valid UTF-8: the edits' positions count each byte
// that is not as a character, as an editor that reads the file as
// Latin-1 does, and one that reads it as UTF-8 may damage it.
func (s *server) workspaceEdit(byPath map[string][]edit, annotations map[string]ChangeAnnotation, src func(path string) ([]byte, error)) (result *WorkspaceEdit, notUTF8 []string, err error) {
	paths := slices.Sorted(maps.Keys(byPath))
	result = &WorkspaceEdit{ChangeAnnotations: annotations}
	if !s.caps.documentChanges {
		result.Changes = map[string][]TextEdit{}
	}
	uri := s.uris()
	s.mu.Lock()
	versions := map[string]int{}
	for _, doc := range s.docs {
		if _, edited := byPath[doc.path]; edited && doc.uri == uri(doc.path) {
			versions[doc.path] = doc.version
		}
	}
	s.mu.Unlock()
	for _, path := range paths {
		text, err := src(path)
		if err != nil {
			return nil, nil, &rpcError{Code: codeRequestFailed, Message: "the edits reach " + path + ", which cannot be read: " + err.Error()}
		}
		if !utf8.Valid(text) {
			notUTF8 = append(notUTF8, path)
		}
		pm := NewPositionMapper(text, s.caps.encoding())
		es := byPath[path]
		slices.SortFunc(es, func(a, b edit) int { return cmp.Compare(a.Start, b.Start) })
		out := make([]TextEdit, 0, len(es))
		for _, e := range es {
			out = append(out, TextEdit{Range: pm.Range(e.Start, e.End), NewText: e.NewText, AnnotationID: e.annotation})
		}
		u := uri(path)
		if !s.caps.documentChanges {
			result.Changes[u] = out
			continue
		}
		td := OptionalVersionedTextDocumentIdentifier{URI: u}
		if v, open := versions[path]; open {
			td.Version = &v
		}
		result.DocumentChanges = append(result.DocumentChanges, TextDocumentEdit{TextDocument: td, Edits: out})
	}
	return result, notUTF8, nil
}

// renameError is the protocol's error for a rename's: a refusal as a
// request that failed, with its reason for the user to read.
func renameError(ctx context.Context, err error) error {
	var no *rename.Refusal
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(err, &no) && no.Forcible:
		return &rpcError{Code: codeRequestFailed, Message: no.Reason + " (`dgdtools rename --force` renames it all the same)"}
	case errors.As(err, &no):
		return &rpcError{Code: codeRequestFailed, Message: no.Reason}
	}
	return err
}

// scanFor makes the whole-lib scan for a question about the place at off
// of the document that needs it, when nothing has made it yet, for as
// long as scanBudget allows: what a local, a parameter or a label is
// named, the document alone says.
func scanFor(ctx context.Context, d *lint.Document, off uint32) {
	ref := d.Resolve(off)
	if ref == nil {
		return
	}
	switch ref.Kind {
	case resolve.Local, resolve.Parameter, resolve.Label:
		return
	}
	if snap := d.Snapshot; !snap.Scanned() {
		scanning, cancel := context.WithTimeout(ctx, scanBudget)
		snap.Scan(scanning)
		cancel()
	}
}
