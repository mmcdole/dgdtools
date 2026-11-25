package lsp

import (
	"context"

	"github.com/mmcdole/dgdtools/pkg/lint"
)

// definition answers textDocument/definition: where what the token at
// the position names is declared, which is lint.Document.Resolve's
// answer (pkg/resolve) and nothing of the server's own. A client that
// reads links gets them, with the range of the name asked about; another
// gets locations. A name nothing is known of, and one that is known but
// declared nowhere the tools can see (a function of the driver's, a call
// on an unknown receiver), has no definition: the answer is null.
//
// A declaration in a profile's stub is at the stub's URI
// (dgdtools-profile://NAME/std/room.c), whose text the client asks for
// with workspace/textDocumentContent.
func (s *server) definition(ctx context.Context, p *DefinitionParams) (any, error) {
	d, err := s.asked(ctx, p.TextDocument.URI, "definition")
	if d == nil || err != nil {
		return nil, err
	}
	texts := &texts{doc: d, encoding: s.caps.encoding(), mappers: map[string]*PositionMapper{}}
	here := texts.mapper(d.Path)
	ref := d.Resolve(here.Offset(p.Position))
	if ref == nil {
		return nil, nil
	}
	origin := here.Range(ref.Start, ref.End)
	uri := s.uris()
	var links []LocationLink
	var locations []Location
	for _, def := range ref.Defs {
		m := texts.mapper(def.Path)
		if m == nil {
			continue // gone since the document was linted
		}
		target := m.Range(def.Start, def.End)
		links = append(links, LocationLink{OriginSelectionRange: origin,
			TargetURI: uri(def.Path), TargetRange: target, TargetSelectionRange: target})
		locations = append(locations, Location{URI: uri(def.Path), Range: target})
	}
	switch {
	case len(links) == 0:
		return nil, nil
	case s.caps.links:
		return links, nil
	}
	return locations, nil
}

// texts are the position mappers of the files one answer has places in:
// the document's own text, and of another file the text the document's
// generation of the index reads, which is an editor's buffer for a file
// that is open, the disk, or a profile's stub.
type texts struct {
	doc      *lint.Document
	encoding string
	mappers  map[string]*PositionMapper
}

// mapper returns the mapper of the file at path, nil when it cannot be
// read.
func (t *texts) mapper(path string) *PositionMapper {
	if m, ok := t.mappers[path]; ok {
		return m
	}
	var m *PositionMapper
	if path == t.doc.Path {
		m = NewPositionMapper(t.doc.File.Src, t.encoding)
	} else if src, err := t.doc.Snapshot.ReadFile(path); err == nil {
		m = NewPositionMapper(src, t.encoding)
	}
	t.mappers[path] = m
	return m
}
