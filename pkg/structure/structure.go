// Package structure recognizes the top-level shape of a DGD LPC file:
// function definitions, prototypes, global variable declarations, inherits,
// and directives. It is deliberately not a parser — recognition is a small
// deterministic scan over the significant token stream, and anything that
// does not match becomes an Unrecognized item that downstream tools treat
// conservatively (the formatter indents it, the linter skips it). It never
// fails and never panics on malformed input.
package structure

import (
	"github.com/mmcdole/dgdtools/pkg/token"
)

type ItemKind uint8

const (
	Unrecognized ItemKind = iota
	FuncDef
	Prototype
	VarDecl
	Inherit
	Directive
)

// Item is one top-level construct. Index fields refer to positions in
// File.Tokens; -1 means absent.
type Item struct {
	Kind        ItemKind
	First, Last int

	// FuncDef / Prototype / VarDecl:
	SpecIdxs []int // specifier tokens: private/static/atomic/nomask/varargs
	//               keywords, or accepted specifier-macro identifiers
	TypeIdxs []int // type tokens: type keyword [+ "path" after object] [+ *s]

	// FuncDef / Prototype:
	NameIdx          int // Ident, or KwOperator (operator symbols follow)
	ParamsL, ParamsR int
	BodyL, BodyR     int // braces; -1 on prototypes
	HeaderComments   bool // comments/directives between First and ParamsR

	// VarDecl: declared variable name tokens.
	VarIdxs []int

	// Inherit:
	LabelIdx           int // optional inherit label
	PathFirst, PathLast int // token range of the path expression
}

// Has reports whether the item carries the given specifier keyword.
func (it *Item) Has(f *token.File, k token.Kind) bool {
	for _, i := range it.SpecIdxs {
		if f.Tokens[i].Kind == k {
			return true
		}
	}
	return false
}

// SpecMacros reports the accepted specifier-macro identifiers present.
func (it *Item) SpecMacros(f *token.File) []string {
	var out []string
	for _, i := range it.SpecIdxs {
		if f.Tokens[i].Kind == token.Ident {
			out = append(out, string(f.Text(f.Tokens[i])))
		}
	}
	return out
}

// FileStructure is the recognized top level of one file.
type FileStructure struct {
	File  *token.File
	Items []Item
}

// Funcs iterates the FuncDef and Prototype items.
func (fs *FileStructure) Funcs(yield func(*Item) bool) {
	for i := range fs.Items {
		if fs.Items[i].Kind == FuncDef || fs.Items[i].Kind == Prototype {
			if !yield(&fs.Items[i]) {
				return
			}
		}
	}
}

// Config adjusts recognition for mudlib conventions.
type Config struct {
	// SpecifierMacros are identifiers accepted in specifier position —
	// empty-macro visibility markers like "public" (#define public).
	SpecifierMacros map[string]bool
}

// DefaultConfig accepts "public", the near-universal empty visibility macro.
func DefaultConfig() Config {
	return Config{SpecifierMacros: map[string]bool{"public": true}}
}

func isSpecKeyword(k token.Kind) bool {
	switch k {
	case token.KwPrivate, token.KwStatic, token.KwAtomic, token.KwNomask, token.KwVarargs:
		return true
	}
	return false
}

func isTypeKeyword(k token.Kind) bool {
	switch k {
	case token.KwInt, token.KwFloat, token.KwString, token.KwObject,
		token.KwMapping, token.KwMixed, token.KwVoid, token.KwFunction:
		return true
	}
	return false
}

// Analyze recognizes the top level of f.
func Analyze(f *token.File, cfg Config) *FileStructure {
	s := &scanner{f: f, cfg: cfg}
	s.collect()
	fs := &FileStructure{File: f}
	for s.i < len(s.sig) {
		fs.Items = append(fs.Items, s.item())
	}
	return fs
}

type scanner struct {
	f   *token.File
	cfg Config
	sig []int // indexes of significant tokens in f.Tokens
	i   int   // cursor into sig
}

// collect gathers significant tokens, taking the FIRST branch of every
// preprocessor conditional and suppressing #else/#elif branches. Code that
// splits brackets across branches (`if (x &&` under #ifdef, `if (y &&`
// under #else, shared continuation after #endif) is balanced within any
// single branch but not across both; counting one branch keeps the
// bracket-matching sound. On DGD the first branch of the ubiquitous
// `#ifdef __DGD__` is also the branch that actually compiles.
func (s *scanner) collect() {
	depth := 0     // nesting of preprocessor conditionals
	suppress := -1 // depth at which an inactive branch started; -1 = none
	for i, t := range s.f.Tokens {
		if t.Kind == token.Directive {
			switch directiveKind(s.f.Text(t)) {
			case "if":
				depth++
			case "else":
				if suppress < 0 {
					suppress = depth
				}
			case "endif":
				if suppress == depth {
					suppress = -1
				}
				if depth > 0 {
					depth--
				}
			}
			if suppress >= 0 {
				continue
			}
			s.sig = append(s.sig, i)
			continue
		}
		if suppress >= 0 || t.Kind.IsTrivia() || t.Kind == token.EOF {
			continue
		}
		s.sig = append(s.sig, i)
	}
}

// directiveKind classifies a preprocessor directive's conditional role:
// "if" (#if/#ifdef/#ifndef), "else" (#else/#elif), "endif", or "".
func directiveKind(text []byte) string {
	i := 1 // past '#'
	for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
		i++
	}
	j := i
	for j < len(text) && text[j] >= 'a' && text[j] <= 'z' {
		j++
	}
	switch string(text[i:j]) {
	case "if", "ifdef", "ifndef":
		return "if"
	case "else", "elif":
		return "else"
	case "endif":
		return "endif"
	}
	return ""
}

// kind returns the kind of the significant token at cursor offset d.
func (s *scanner) kind(d int) token.Kind {
	if s.i+d < len(s.sig) {
		return s.f.Tokens[s.sig[s.i+d]].Kind
	}
	return token.EOF
}

// idx returns the f.Tokens index at cursor offset d (-1 past the end).
func (s *scanner) idx(d int) int {
	if s.i+d < len(s.sig) {
		return s.sig[s.i+d]
	}
	return -1
}

func (s *scanner) text(d int) string {
	if i := s.idx(d); i >= 0 {
		return string(s.f.Text(s.f.Tokens[i]))
	}
	return ""
}

// item recognizes one top-level item starting at the cursor.
func (s *scanner) item() Item {
	first := s.idx(0)

	if s.kind(0) == token.Directive {
		s.i++
		return Item{Kind: Directive, First: first, Last: first, NameIdx: -1, BodyL: -1, BodyR: -1, LabelIdx: -1}
	}

	it := Item{First: first, NameIdx: -1, ParamsL: -1, ParamsR: -1, BodyL: -1, BodyR: -1, LabelIdx: -1}

	// Specifiers: keywords, plus accepted macro identifiers (an identifier
	// counts only when what follows still looks like a declaration —
	// otherwise it is a K&R-style function name).
	for {
		k := s.kind(0)
		if isSpecKeyword(k) {
			it.SpecIdxs = append(it.SpecIdxs, s.idx(0))
			s.i++
			continue
		}
		if k == token.Ident && s.cfg.SpecifierMacros[s.text(0)] && s.kind(1) != token.LParen {
			it.SpecIdxs = append(it.SpecIdxs, s.idx(0))
			s.i++
			continue
		}
		break
	}

	// Inherit (possibly private-prefixed).
	if s.kind(0) == token.KwInherit {
		return s.inherit(it)
	}

	// Type.
	if isTypeKeyword(s.kind(0)) {
		it.TypeIdxs = append(it.TypeIdxs, s.idx(0))
		obj := s.kind(0) == token.KwObject
		s.i++
		if obj && s.kind(0) == token.StringLit { // typed object: object "/path"
			it.TypeIdxs = append(it.TypeIdxs, s.idx(0))
			s.i++
		}
		for s.kind(0) == token.Star {
			it.TypeIdxs = append(it.TypeIdxs, s.idx(0))
			s.i++
		}
	}

	switch {
	case s.kind(0) == token.Ident && s.kind(1) == token.LParen:
		it.NameIdx = s.idx(0)
		s.i++
		return s.function(it)
	case s.kind(0) == token.KwOperator:
		it.NameIdx = s.idx(0)
		s.i++
		for s.kind(0) != token.LParen && s.kind(0) != token.EOF {
			// operator symbol tokens: + - [ ] = .. etc.
			if k := s.kind(0); k == token.Semicolon || k == token.LBrace {
				return s.bail(it)
			}
			s.i++
		}
		return s.function(it)
	case s.kind(0) == token.Ident || s.kind(0) == token.Star:
		return s.varDecl(it)
	}
	return s.bail(it)
}

// inherit recognizes: inherit [label] [object] pathexpr ';'
func (s *scanner) inherit(it Item) Item {
	it.Kind = Inherit
	s.i++ // 'inherit'
	// A bare identifier is a label only when a path follows it; a lone
	// identifier (or one followed by an operator) is a macro path itself.
	if s.kind(0) == token.Ident {
		switch s.kind(1) {
		case token.StringLit, token.KwObject, token.Ident:
			it.LabelIdx = s.idx(0)
			s.i++
		}
	}
	if s.kind(0) == token.KwObject {
		s.i++
	}
	it.PathFirst = s.idx(0)
	for s.kind(0) != token.Semicolon && s.kind(0) != token.EOF {
		it.PathLast = s.idx(0)
		s.i++
	}
	if s.kind(0) == token.Semicolon {
		it.Last = s.idx(0)
		s.i++
	} else {
		it.Last = it.PathLast
	}
	return it
}

// function recognizes params and body/semicolon; cursor is at '('.
func (s *scanner) function(it Item) Item {
	it.ParamsL = s.idx(0)
	if !s.skipBalanced(token.LParen, token.RParen) {
		return s.bail(it)
	}
	it.ParamsR = s.sig[s.i-1]

	switch s.kind(0) {
	case token.LBrace:
		it.Kind = FuncDef
		it.BodyL = s.idx(0)
		if !s.skipBalanced(token.LBrace, token.RBrace) {
			return s.bail(it)
		}
		it.BodyR = s.sig[s.i-1]
		it.Last = it.BodyR
	case token.Semicolon:
		it.Kind = Prototype
		it.Last = s.idx(0)
		s.i++
	default:
		return s.bail(it)
	}

	it.HeaderComments = s.headerHasComments(it.First, it.ParamsR)
	return it
}

// varDecl recognizes: [*]* name (, [*]* name)* ';'
func (s *scanner) varDecl(it Item) Item {
	it.Kind = VarDecl
	expectName := true
	for {
		switch s.kind(0) {
		case token.Star:
			s.i++
		case token.Ident:
			if expectName {
				it.VarIdxs = append(it.VarIdxs, s.idx(0))
				expectName = false
				s.i++
			} else {
				return s.bail(it)
			}
		case token.Comma:
			expectName = true
			s.i++
		case token.Semicolon:
			it.Last = s.idx(0)
			s.i++
			return it
		default:
			return s.bail(it)
		}
	}
}

// bail turns the current item into Unrecognized and skips to a recovery
// point: past the next top-level ';' or matched '{...}'.
func (s *scanner) bail(it Item) Item {
	it.Kind = Unrecognized
	it.SpecIdxs, it.TypeIdxs, it.VarIdxs = nil, nil, nil
	it.NameIdx, it.ParamsL, it.ParamsR, it.BodyL, it.BodyR = -1, -1, -1, -1, -1
	last := it.First
	for s.kind(0) != token.EOF {
		switch s.kind(0) {
		case token.Semicolon:
			it.Last = s.idx(0)
			s.i++
			return it
		case token.LBrace:
			s.skipBalanced(token.LBrace, token.RBrace)
			if s.i > 0 {
				it.Last = s.sig[s.i-1]
			}
			return it
		case token.Directive:
			// A directive recovers the scan; do not consume it.
			it.Last = last
			return it
		}
		last = s.idx(0)
		s.i++
	}
	it.Last = last
	return it
}

// skipBalanced advances past a balanced open..close region; the cursor must
// be at an `open` token. All bracket kinds nest.
func (s *scanner) skipBalanced(open, close token.Kind) bool {
	depth := 0
	for s.kind(0) != token.EOF {
		switch s.kind(0) {
		case token.LParen, token.LBracket, token.LBrace:
			depth++
		case token.RParen, token.RBracket, token.RBrace:
			depth--
			if depth < 0 {
				return false
			}
		}
		s.i++
		if depth == 0 {
			return true
		}
	}
	return false
}

// headerHasComments reports whether any comment or directive token lies
// between token indexes a and b — headers containing them are not reflowed.
func (s *scanner) headerHasComments(a, b int) bool {
	for i := a; i <= b && i < len(s.f.Tokens); i++ {
		switch s.f.Tokens[i].Kind {
		case token.LineComment, token.BlockComment, token.Directive:
			return true
		}
	}
	return false
}
