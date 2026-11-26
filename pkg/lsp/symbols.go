package lsp

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/mmcdole/dgdtools/pkg/index"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/resolve"
)

// The kinds of symbol the server has (SymbolKind).
const (
	SymbolClass    = 5
	SymbolFunction = 12
	SymbolVariable = 13
	SymbolConstant = 14
)

// SymbolKindOf returns the protocol's kind for a declaration: a
// function, a variable, a constant for a macro, and a class for an
// inherit statement, the inherited program being the nearest thing LPC
// has to one.
func SymbolKindOf(k resolve.Kind) int {
	switch k {
	case resolve.Function:
		return SymbolFunction
	case resolve.Macro:
		return SymbolConstant
	case resolve.Object:
		return SymbolClass
	}
	return SymbolVariable
}

// documentSymbol answers textDocument/documentSymbol with the outline
// of the document, which is lint.Document.Symbols' (pkg/resolve): its
// inherit statements, functions, global variables and macros in the
// order they stand, each with the range of the whole declaration and
// that of its name. A client that reads the hierarchy gets
// DocumentSymbols (LPC has one level); another gets SymbolInformation.
func (s *server) documentSymbol(ctx context.Context, p *DocumentSymbolParams) (any, error) {
	d, err := s.asked(ctx, p.TextDocument.URI, "symbols")
	if d == nil || err != nil {
		return nil, err
	}
	m := NewPositionMapper(d.File.Src, s.caps.encoding())
	syms := d.Symbols()
	if !s.caps.hierarchy {
		out := make([]SymbolInformation, 0, len(syms))
		for _, sym := range syms {
			out = append(out, SymbolInformation{Name: sym.Name, Kind: SymbolKindOf(sym.Kind),
				Location: Location{URI: p.TextDocument.URI, Range: m.Range(sym.Start, sym.End)}})
		}
		return out, nil
	}
	out := make([]DocumentSymbol, 0, len(syms))
	for _, sym := range syms {
		out = append(out, DocumentSymbol{Name: sym.Name, Detail: sym.Detail, Kind: SymbolKindOf(sym.Kind),
			Range: m.Range(sym.Start, sym.End), SelectionRange: m.Range(sym.NameStart, sym.NameEnd)})
	}
	return out, nil
}

// MaxWorkspaceSymbols is how many symbols a workspace/symbol answer has
// at most: the best matches.
const MaxWorkspaceSymbols = 200

// scanBudget is how long a search by name waits for the lib to be
// indexed before it answers from what is indexed so far.
const scanBudget = 2 * time.Second

// workspaceSymbol answers workspace/symbol: the functions, global
// variables and header macros whose names match the query, in the libs
// of the open documents and of the workspace folders
// (index.Snapshot.FindSymbols). The whole lib is searched once it has
// been scanned; the scan is made here when nothing made it yet, for as
// long as scanBudget allows, and goes on from where it was with the next
// query. What a profile's stub declares is at the stub's URI, and its
// container says so.
func (s *server) workspaceSymbol(ctx context.Context, p *WorkspaceSymbolParams) (any, error) {
	s.mu.Lock()
	sess := s.session
	var docs, folders []string
	for _, doc := range s.docs {
		if doc.path != "" {
			docs = append(docs, doc.path)
		}
	}
	for _, f := range s.folders {
		if f.dir != "" {
			folders = append(folders, f.dir)
		}
	}
	s.mu.Unlock()
	slices.Sort(docs)

	var (
		found   []index.Symbol
		snaps   []*index.Snapshot
		linters []*lint.Linter
	)
	for _, path := range append(docs, folders...) {
		l, err := sess.Linter(path)
		if err != nil || slices.Contains(linters, l) {
			continue
		}
		linters = append(linters, l)
		snap := l.Index().Snapshot()
		if !snap.Scanned() {
			scanning, cancel := context.WithTimeout(ctx, scanBudget)
			snap.Scan(scanning)
			cancel()
		}
		syms, _ := snap.FindSymbols(p.Query, MaxWorkspaceSymbols)
		for _, sym := range syms {
			found = append(found, sym)
			snaps = append(snaps, snap)
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// The best of all the libs, each lib's being in order already.
	order := make([]int, len(found))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(found[b].Score, found[a].Score) })
	out := []SymbolInformation{}
	uri := s.uris()
	mappers := map[string]*PositionMapper{}
	seen := map[Location]bool{}
	for _, i := range order {
		sym := &found[i]
		start := Position{Line: sym.Line - 1, Character: sym.Col - 1}
		// A column is a byte offset for a client that counts so, and
		// then only a macro, of which the index has no line, needs the
		// text.
		if !s.caps.utf8 || sym.Line == 0 {
			m, read := mappers[sym.Path]
			if !read {
				if src, err := snaps[i].ReadFile(sym.Path); err == nil {
					m = NewPositionMapper(src, s.caps.encoding())
				}
				mappers[sym.Path] = m
			}
			if m == nil {
				continue // gone since it was indexed
			}
			start = m.Position(sym.Off)
		}
		width := len(sym.Name)
		if !s.caps.utf8 {
			width = utf16Len(sym.Name)
		}
		loc := Location{URI: uri(sym.Path), Range: Range{Start: start, End: Position{Line: start.Line, Character: start.Character + width}}}
		if seen[loc] {
			continue
		}
		seen[loc] = true
		kind := map[index.SymbolKind]int{index.SymbolFunction: SymbolFunction, index.SymbolVariable: SymbolVariable, index.SymbolMacro: SymbolConstant}[sym.Kind]
		in := sym.In
		if name, _, stub := profile.SplitLocation(sym.Path); stub {
			in += " (profile " + name + ")"
		}
		if out = append(out, SymbolInformation{Name: sym.Name, Kind: kind, Location: loc, ContainerName: in}); len(out) == MaxWorkspaceSymbols {
			break
		}
	}
	return out, nil
}
