package resolve

import (
	"bytes"
	"cmp"
	"slices"

	"github.com/mmcdole/dgdtools/pkg/index"
	"github.com/mmcdole/dgdtools/pkg/pp"
	"github.com/mmcdole/dgdtools/pkg/syntax"
	"github.com/mmcdole/dgdtools/pkg/token"
)

// TokenType is what a semantic token says a name is: the type of the
// Language Server Protocol's semantic tokens, by the same name
// (TokenTypes), so that an editor's theme colours it unconfigured.
type TokenType uint8

const (
	// TokenNamespace is an inherit statement's label.
	TokenNamespace TokenType = iota
	// TokenType is a macro the parser takes for a type (status).
	TokenTypeName
	TokenParameter
	// TokenVariable is a global variable or a local one (TokenLocal).
	TokenVariable
	TokenFunction
	// TokenMethod is a function named in a call that is bound when it
	// runs: x->name().
	TokenMethod
	TokenMacro
	// TokenKeyword is `function`, which only a lib with closures
	// reserves, and a grammar cannot tell.
	TokenKeyword
	// TokenModifier is a macro the parser takes for specifiers, or for
	// nothing (public).
	TokenModifier
)

// TokenTypes are the names of the token types, in the order of their
// values: the legend of the protocol's semantic tokens.
var TokenTypes = []string{"namespace", "type", "parameter", "variable", "function", "method", "macro", "keyword", "modifier"}

func (t TokenType) String() string { return TokenTypes[t] }

// TokenMods are a semantic token's modifiers, a bit each.
type TokenMods uint16

const (
	// ModDeclaration: the name is declared here (a prototype, a
	// variable, a parameter, a label, a macro's #define); ModDefinition:
	// a function is defined here, with its body.
	ModDeclaration TokenMods = 1 << iota
	ModDefinition
	// ModStatic: the function or the global variable is static.
	ModStatic
	// ModDefaultLibrary: a function of the driver's (a kfun) or one the
	// auto object gives every object.
	ModDefaultLibrary
	// ModInherited: a function or a global variable the object inherits.
	ModInherited
	// ModLocal: a local variable of a function.
	ModLocal
	// ModMacro: a macro that stands for a type or for specifiers.
	ModMacro
)

// TokenModNames are the names of the modifiers, in the order of their
// bits: the legend of the protocol's semantic tokens. The first four are
// the protocol's own, the others dgdtools'.
var TokenModNames = []string{"declaration", "definition", "static", "defaultLibrary", "inherited", "local", "macro"}

// Names returns the names of the modifiers set.
func (m TokenMods) Names() []string {
	var out []string
	for i, n := range TokenModNames {
		if m&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	return out
}

// SemanticToken is a name of the file and what it is the name of, as
// far as analysis knows what a grammar cannot: the bytes [Start, End),
// which are on one line.
type SemanticToken struct {
	Start, End uint32
	Type       TokenType
	Mods       TokenMods
}

// Tokens returns the semantic tokens of the file, in the order of the
// file and none overlapping another: a name of the compiled code, or of
// a directive the preprocessor reads, that At says is a macro, a label,
// a function the file names or calls, a global variable, a local or a
// parameter, and `function` where it is a keyword. A name that names
// nothing known has none, as At has nothing to say of it, and so have
// strings, comments, numbers and the other keywords: the grammar of the
// editor colours them.
//
// What a token says is what At says, without the cost of asking it at
// every offset: one walk of the tree, and a lookup in the lib for each
// name, function or variable, once.
func (r *Resolver) Tokens() []SemanticToken {
	f := &r.File
	if f.Tok == nil || f.Tree == nil || f.Tree.Sel == nil || f.Unit == nil || f.Snapshot == nil {
		return nil
	}
	t := &tokenizer{r: r, looked: map[lookupKey]lookedUp{}}
	toks := f.Tok.Tokens
	var names []int
	for _, ti := range f.Tree.Sel.Sig {
		switch toks[ti].Kind {
		case token.Ident:
			names = append(names, ti)
		case token.KwFunction:
			t.add(toks[ti].Off, toks[ti].End, TokenKeyword, 0)
		}
	}
	syntax.PathsAt(f.Tree, names, func(ti int, path []syntax.Node) { t.ident(ti, path) })
	for ti, tok := range toks {
		if tok.Kind != token.Directive {
			continue
		}
		text := f.Tok.Text(tok)
		switch name, _ := pp.SplitDirective(text); name {
		case "define", "undef", "if", "ifdef", "ifndef", "elif":
			arg := pp.ArgOffset(text)
			for _, w := range lexWords(text[arg:], f.Tok.Dialect) {
				if ref, _ := r.directive(ti, tok.Off+uint32(arg)+w); ref != nil && ref.Kind == Macro {
					mods := TokenMods(0)
					if ref.Decl {
						mods = ModDeclaration
					}
					t.add(ref.Start, ref.End, TokenMacro, mods)
				}
			}
		}
	}
	slices.SortFunc(t.out, func(a, b SemanticToken) int { return cmp.Compare(a.Start, b.Start) })
	return slices.CompactFunc(t.out, func(a, b SemanticToken) bool { return a.Start == b.Start })
}

// tokenizer makes the tokens of one file.
type tokenizer struct {
	r      *Resolver
	out    []SemanticToken
	looked map[lookupKey]lookedUp
}

// lookupKey is what the lookup of a Use depends on beyond the file.
type lookupKey struct {
	kind index.UseKind
	form index.UseForm
	name string
	aux  string
}

// lookedUp is what a token makes of a lookup: whether it found anything,
// and the modifiers of what it found.
type lookedUp struct {
	found bool
	mods  TokenMods
}

// add keeps a token, unless its bytes are not on one line (a name split
// by a backslash and a line break).
func (t *tokenizer) add(start, end uint32, typ TokenType, mods TokenMods) {
	if end <= start || bytes.ContainsAny(t.r.Tok.Src[start:end], "\r\n") {
		return
	}
	t.out = append(t.out, SemanticToken{Start: start, End: end, Type: typ, Mods: mods})
}

// ident makes the token of an identifier of the compiled code, path
// being the nodes around it.
func (t *tokenizer) ident(ti int, path []syntax.Node) {
	r := t.r
	tok := r.Tok.Tokens[ti]
	ref, u := r.ident(ti, path)
	if ref == nil {
		return
	}
	switch ref.Kind {
	case Object:
		// `inherit ROOM;` names the room, and ROOM is a macro all the
		// same; any other word of the statement is the grammar's.
		if m, _ := r.macro(string(r.Tok.Text(tok)), tok.Off); m != nil {
			t.add(m.Start, m.End, TokenMacro, 0)
		}
	case Macro:
		typ, mods := TokenMacro, TokenMods(0)
		if m := r.Env.Lookup(ref.Name); m != nil {
			switch m.Class {
			case pp.ClassType:
				typ, mods = TokenTypeName, ModMacro
			case pp.ClassSpecifier, pp.ClassEmpty:
				// An empty macro in code stands where a specifier
				// does, as does one conventions.macros classes so.
				typ, mods = TokenModifier, ModMacro
			}
		}
		t.add(ref.Start, ref.End, typ, mods)
	case Label:
		mods := TokenMods(0)
		if ref.Decl {
			mods = ModDeclaration
		}
		t.add(ref.Start, ref.End, TokenNamespace, mods)
	case Local, Parameter:
		typ, mods := TokenParameter, TokenMods(0)
		if ref.Kind == Local {
			typ, mods = TokenVariable, ModLocal
		}
		if ref.Decl {
			mods |= ModDeclaration
		}
		t.add(ref.Start, ref.End, typ, mods)
	case Variable:
		got := t.lookup(u)
		if !got.found {
			return // no variable the file can see: At says nothing
		}
		mods := got.mods
		if ref.Decl {
			mods |= ModDeclaration
		}
		t.add(ref.Start, ref.End, TokenVariable, mods)
	case Function:
		t.function(ref, u, path)
	}
}

// function makes the token of a function's name: at its declaration, in
// a call, after ->.
func (t *tokenizer) function(ref *Ref, u *index.Use, path []syntax.Node) {
	if ref.Decl {
		mods := ModDeclaration
		if fd, ok := path[0].(*syntax.FuncDecl); ok {
			if fd.Body != nil {
				mods = ModDefinition
			}
			if fd.Has(token.KwStatic) {
				mods |= ModStatic
			}
		}
		t.add(ref.Start, ref.End, TokenFunction, mods)
		return
	}
	typ := TokenFunction
	if ref.Call != nil {
		// Bound when it runs: a function's name all the same, whatever
		// the object is.
		typ = TokenMethod
	}
	got := t.lookup(u)
	switch {
	case got.found:
		mods := got.mods
		if u.Form == index.FormPath {
			mods &^= ModInherited // of another object, which is not this one's parent
		}
		t.add(ref.Start, ref.End, typ, mods)
	case ref.Call != nil:
		t.add(ref.Start, ref.End, typ, 0)
	case t.r.kfun(ref.Name) != nil:
		t.add(ref.Start, ref.End, typ, ModDefaultLibrary)
	}
}

// lookup looks a Use up as At does, once for each name and form.
func (t *tokenizer) lookup(u *index.Use) lookedUp {
	k := lookupKey{u.Kind, u.Form, u.Name, u.Aux}
	if got, ok := t.looked[k]; ok {
		return got
	}
	defs := t.r.Lookup(*u)
	got := lookedUp{found: len(defs) > 0}
	if got.found {
		got.mods = ModStatic | ModDefaultLibrary | ModInherited
		for _, d := range defs {
			var m TokenMods
			switch {
			case d.Auto:
				m |= ModDefaultLibrary
			case d.Object != t.r.u.LibPath:
				m |= ModInherited
			}
			if d.Func != nil && d.Func.Static || d.Var != nil && d.Var.Static {
				m |= ModStatic
			}
			got.mods &= m // what all of them are
		}
	}
	t.looked[k] = got
	return got
}
