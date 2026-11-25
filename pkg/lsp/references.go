package lsp

import (
	"context"
	"fmt"

	"github.com/mmcdole/dgdtools/pkg/resolve"
)

// references answers textDocument/references: the places that name what
// the token at the position names, which is lint.Document.References'
// answer (pkg/resolve): in the document for a local, a parameter or a
// label; in every file of the lib for a function, a global variable, a
// macro, an object or an included file. With includeDeclaration the
// declarations are among them, the one in a profile's stub at the stub's
// URI.
//
// The lib must have been scanned for that. The scan is made here when
// nothing has made it yet, for as long as scanBudget allows; an answer
// from a lib not yet indexed in full is what was found in what is
// indexed, and the log says so. So does it for a partial checkout, where
// the rest of the lib cannot be searched, and for calls on objects that
// cannot be told, which may call a function and are not listed. A name
// that is declared nowhere the tools see (a function of the driver's, a
// call on an unknown receiver) has no references: null.
func (s *server) references(ctx context.Context, p *ReferenceParams) (any, error) {
	d, err := s.asked(ctx, p.TextDocument.URI, "references")
	if d == nil || err != nil {
		return nil, err
	}
	texts := &texts{doc: d, encoding: s.caps.encoding(), mappers: map[string]*PositionMapper{}}
	off := texts.mapper(d.Path).Offset(p.Position)
	ref := d.Resolve(off)
	if ref == nil || len(ref.Defs) == 0 {
		return nil, nil
	}
	scanFor(ctx, d, off)
	refs := d.References(ctx, off, p.Context.IncludeDeclaration)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if refs == nil {
		return nil, nil
	}
	out := make([]Location, 0, len(refs.Sites))
	uri := s.uris()
	for _, site := range refs.Sites {
		if rng, ok := siteRange(texts, site); ok {
			out = append(out, Location{URI: uri(site.Path), Range: rng})
		}
	}
	if note := referencesNote(refs); note != "" {
		s.mu.Lock()
		s.logMessage(MessageInfo, note)
		s.mu.Unlock()
	}
	return out, nil
}

// referencesNote says what an answer to references is not: complete for
// the whole lib, or all the calls there may be. It is "" when it is all
// of both.
func referencesNote(refs *resolve.References) string {
	what := fmt.Sprintf("references to %s %s: %d", refs.Ref.Kind, refs.Ref.Name, len(refs.Sites))
	note := ""
	if refs.Possible > 0 {
		note += fmt.Sprintf("; %d calls of %s on objects that cannot be told (in %d files) may call it too and are not listed",
			refs.Possible, refs.Ref.Name, refs.PossibleFiles)
	}
	switch {
	case refs.Ref.Kind == resolve.Local, refs.Ref.Kind == resolve.Parameter, refs.Ref.Kind == resolve.Label:
	case refs.Partial:
		note += fmt.Sprintf("; complete for this checkout only (%d files searched): the rest of the mudlib is known from a profile, which has no code to search", refs.Files)
	case !refs.Complete:
		note += fmt.Sprintf("; the mudlib is not indexed in full yet: %d files searched", refs.Files)
	}
	if note == "" {
		return ""
	}
	return what + note
}

// siteRange returns the range of a place: from its line and column when
// the index has them and they count the same in every encoding (a line
// of ASCII), else from the text of its file, which is read for it. False
// when the file cannot be read any more.
func siteRange(texts *texts, site resolve.Site) (Range, bool) {
	if site.Line > 0 && site.ASCII {
		start := Position{Line: int(site.Line) - 1, Character: int(site.Col) - 1}
		return Range{Start: start, End: Position{Line: start.Line, Character: start.Character + int(site.End-site.Start)}}, true
	}
	m := texts.mapper(site.Path)
	if m == nil {
		return Range{}, false
	}
	return m.Range(site.Start, site.End), true
}

// documentHighlight answers textDocument/documentHighlight: the places of
// the document that name what the token at the position names
// (lint.Document.Highlights), the declarations among them. A global
// variable, a local and a parameter are read or written at each (written
// where declared, assigned, incremented or decremented); anything else is
// text. Nothing named is null.
func (s *server) documentHighlight(ctx context.Context, p *DocumentHighlightParams) (any, error) {
	d, err := s.asked(ctx, p.TextDocument.URI, "highlights")
	if d == nil || err != nil {
		return nil, err
	}
	m := NewPositionMapper(d.File.Src, s.caps.encoding())
	sites := d.Highlights(m.Offset(p.Position))
	if sites == nil {
		return nil, nil
	}
	out := make([]DocumentHighlight, 0, len(sites))
	for _, site := range sites {
		kind := HighlightText
		switch site.Access {
		case resolve.Read:
			kind = HighlightRead
		case resolve.Write:
			kind = HighlightWrite
		}
		out = append(out, DocumentHighlight{Range: m.Range(site.Start, site.End), Kind: kind})
	}
	return out, nil
}
