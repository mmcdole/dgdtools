package lint

import (
	"strings"

	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/pp"
	"github.com/mmcdole/dgdtools/pkg/syntax"
	"github.com/mmcdole/dgdtools/pkg/token"
)

// The syntax group: constructs the DGD compiler rejects. An object with
// one of these does not load. Every rule here was checked against DGD
// 1.7.9 (see docs/design/architecture.md, "Verification").

var ruleSyntaxError = &Rule{
	Code: "E001", Name: "syntax-error", Group: GroupSyntax,
	Doc: "the file does not parse; DGD will refuse to compile it",
	Explain: "Reported by the linter's own error-tolerant parser, which implements the " +
		"DGD 1.7 grammar on unpreprocessed source. A missing ';' is reported where " +
		"the ';' belongs (DGD reports the token after it), with a fix that inserts it " +
		"when the parser went on as if it were there. Errors that could be an " +
		"artifact of a macro the linter cannot see through are not reported: a " +
		"statement that uses a fragment macro, or, when some #include could not be " +
		"resolved, an error next to an unknown identifier. After an error that " +
		"leaves the file's structure in doubt (unbalanced braces) later errors in " +
		"the file are dropped as likely consequences.",
	Default: true, Severity: diag.Error, CompiledOnly: true,
	Run: func(p *Pass) {
		// A missing #include is fatal to the compile, as in C: errors
		// after it are consequences and are not reported.
		cutoff := ^uint32(0)
		for _, bi := range p.Unit.BadIncludes {
			if bi.Off < cutoff {
				cutoff = bi.Off
			}
		}
		toks := p.File.Tokens
		for _, e := range p.Tree.Errors {
			if e.Tainted {
				continue
			}
			t := toks[e.Tok]
			if t.Off > cutoff {
				break
			}
			start, end := t.Off, t.End
			if e.AtPrevEnd {
				start, end = t.End, t.End
			}
			d := p.Report(start, end, "%s", e.Msg)
			if e.Insert != "" && e.AtPrevEnd && !directiveAfter(toks, e.Tok) {
				// What the parser assumed, where it assumed it: the
				// file then parses as it did.
				d.Fixes = append(d.Fixes, diag.Fix{Title: "insert '" + e.Insert + "'", Applicability: diag.Safe,
					Edits: []diag.TextEdit{{Start: t.End, End: t.End, NewText: e.Insert}}})
			}
			if e.Related >= 0 && e.RelatedMsg != "" {
				rt := toks[e.Related]
				d.Related = append(d.Related, p.Related(rt.Off, rt.End, e.RelatedMsg))
			}
		}
	},
}

// directiveAfter reports whether a preprocessor directive stands between
// the token at index i and the next significant one: an insertion there
// would be in every branch of a conditional, which the parser read one of.
func directiveAfter(toks []token.Token, i int) bool {
	for _, t := range toks[i+1:] {
		switch {
		case t.Kind == token.Directive:
			return true
		case !t.Kind.IsTrivia():
			return false
		}
	}
	return false
}

var ruleAdjacentStrings = &Rule{
	Code: "E002", Name: "adjacent-strings", Group: GroupSyntax,
	Doc: `adjacent string literals ("a" "b"); DGD has no implicit concatenation`,
	Explain: "Older LPmud drivers and C concatenate adjacent string literals. DGD " +
		"does not: it is a syntax error and the object will not compile. Join the " +
		"strings with '+' (DGD folds constant concatenation at compile time). The " +
		"diagnostic carries a fix that inserts the '+'.",
	Default: true, Severity: diag.Error, CompiledOnly: true,
	Run: func(p *Pass) {
		syntax.Inspect(p.Tree, func(n syntax.Node) bool {
			a, ok := n.(*syntax.AdjacentStrings)
			if !ok {
				return true
			}
			d := p.ReportNode(a.Parts[1], "adjacent string literals are a syntax error in DGD; join them with '+'")
			// DGD folds the concatenation of constants when it compiles:
			// the '+' costs nothing and means what adjacency meant.
			fix := diag.Fix{Title: "insert '+' between the strings", Applicability: diag.Safe}
			for _, part := range a.Parts[:len(a.Parts)-1] {
				end := p.Tree.End(part)
				fix.Edits = append(fix.Edits, diag.TextEdit{Start: end, End: end, NewText: " +"})
			}
			d.Fixes = append(d.Fixes, fix)
			return true
		})
	},
}

var ruleLegacyClosure = &Rule{
	Code: "E003", Name: "legacy-closure", Group: GroupSyntax,
	Doc: `(: ob, "fn" :) function-pointer literal; not DGD syntax`,
	Explain: "The (: ... :) literal comes from other LPC dialects. DGD rejects it " +
		"as a syntax error. Use the mudlib's function-pointer constructor instead.",
	Default: true, Severity: diag.Error, CompiledOnly: true,
	Run: func(p *Pass) {
		syntax.Inspect(p.Tree, func(n syntax.Node) bool {
			if c, ok := n.(*syntax.LegacyClosure); ok {
				p.ReportNode(c, "'(: ... :)' is not DGD syntax; the object will not compile")
			}
			return true
		})
	},
}

var ruleDeclInit = &Rule{
	Code: "E004", Name: "declaration-initializer", Group: GroupSyntax,
	Doc: "variable initialized in its declaration (int x = 5;); not DGD syntax",
	Explain: "DGD 1.7 declarations cannot carry an initializer, for globals or " +
		"locals. It is a syntax error. Declare first and assign in a statement " +
		"(globals: in create()).",
	Default: true, Severity: diag.Error, CompiledOnly: true,
	Run: func(p *Pass) {
		syntax.Inspect(p.Tree, func(n syntax.Node) bool {
			if d, ok := n.(*syntax.Declarator); ok && d.Init != nil {
				p.Report(p.Tree.Off(d.Name), p.Tree.End(d.Init),
					"'%s' is initialized in its declaration, which is a syntax error in DGD; assign it in a statement", d.Name.Name)
			}
			return true
		})
	},
}

var ruleDeclAfterStmt = &Rule{
	Code: "E005", Name: "declaration-after-statement", Group: GroupSyntax,
	Doc: "local declaration after a statement in the same block; a DGD compile error",
	Explain: "DGD requires every local declaration of a block to come before the " +
		"block's first statement (an empty statement ';' counts). Violations are " +
		"the compile error \"declaration after statement\". Blocks in which a " +
		"macro stands as a statement before the declaration are not reported, " +
		"since the macro may expand to declarations.",
	Default: true, Severity: diag.Error, CompiledOnly: true,
	Run: func(p *Pass) {
		syntax.Inspect(p.Tree, func(n syntax.Node) bool {
			b, ok := n.(*syntax.BlockStmt)
			if !ok {
				return true
			}
			var first syntax.Stmt
			for _, s := range b.Stmts {
				switch s := s.(type) {
				case *syntax.DeclStmt:
					if first != nil {
						d := p.ReportNode(s, "declaration after statement; DGD requires declarations at the start of the block")
						d.Related = append(d.Related, p.Related(p.Tree.Off(first), p.Tree.End(first), "first statement of the block"))
					}
				case *syntax.MacroStmt, *syntax.BadStmt:
					first = nil // unknown expansion: start over
				default:
					if first == nil {
						first = s
					}
				}
			}
			return true
		})
	},
}

var ruleIncludeNotFound = &Rule{
	Code: "E006", Name: "include-not-found", Group: GroupSyntax,
	Doc: "#include target not found in the file's directory or the include dirs",
	Explain: "A missing #include is a compile error when the object loads. Only " +
		"reported when the mudlib is complete (the configured include dirs and " +
		"force-included header exist); in a partial checkout the target is " +
		"unknown, not missing. Like a C compiler's fatal \"file not found\", it " +
		"stops syntax reporting for the rest of the file.",
	Default: true, Severity: diag.Error, CrossFile: true, CompiledOnly: true,
	Run: func(p *Pass) {
		for _, bi := range p.Unit.BadIncludes {
			p.Report(bi.Off, bi.End, "cannot find include file %s", bi.Raw)
		}
	},
}

var knownDirectives = func() map[string]bool {
	m := make(map[string]bool, len(pp.Directives))
	for _, name := range pp.Directives {
		m[name] = true
	}
	return m
}()

var ruleBadDirective = &Rule{
	Code: "E007", Name: "bad-directive", Group: GroupSyntax,
	Doc: "malformed preprocessor directive (text after #else/#endif, unknown directive)",
	Explain: "DGD's preprocessor rejects tokens after #else and #endif (\"bad token " +
		"in control\"; write them as a comment) and unknown directive names " +
		"(\"undefined control\"), even inside a skipped #if block.",
	Default: true, Severity: diag.Error,
	Run: func(p *Pass) {
		active := map[uint32]bool{}
		for _, i := range p.Tree.Sel.Dirs {
			active[p.File.Tokens[i].Off] = true
		}
		for i, t := range p.File.Tokens {
			if t.Kind != token.Directive {
				continue
			}
			name, rest := pp.SplitDirective(p.File.Text(t))
			switch {
			case name == "":
			case !knownDirectives[name]:
				p.Report(t.Off, t.End, "unknown preprocessor directive '#%s'", name)
			case (name == "else" || name == "endif") && strings.TrimSpace(rest) != "":
				p.Report(t.Off, t.End, "text after '#%s' is a preprocessor error in DGD; put it in a comment", name)
			case name == "include" && active[t.Off] && !p.Tree.Sel.Guessed(i):
				// The rest of an #include line is compiled as source:
				// `#include "x.h";` leaves a stray ';' at top level.
				if tail := includeTail(rest); tail != "" {
					p.Report(t.Off, t.End, "'%s' after the #include target is an error in DGD; remove it", tail)
				}
			}
		}
	},
}

// includeTail returns what follows the target of an #include.
func includeTail(rest string) string {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return ""
	}
	close := byte('"')
	if rest[0] == '<' {
		close = '>'
	} else if rest[0] != '"' {
		return ""
	}
	end := strings.IndexByte(rest[1:], close)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[end+2:])
}

var ruleInheritOrder = &Rule{
	Code: "E008", Name: "inherit-after-declaration", Group: GroupSyntax,
	Doc: "inherit statement after a variable or function declaration",
	Explain: "DGD requires every inherit to precede all declarations (\"inherit " +
		"must precede all declarations\"). That includes declarations that " +
		"arrive through an #include placed before the inherit: a header with a " +
		"prototype or a variable must be included after the inherit statements.",
	Default: true, Severity: diag.Error, CompiledOnly: true,
	Run: func(p *Pass) {
		var first syntax.Decl
		// A header included before an inherit that declares something.
		var lastInherit *syntax.InheritDecl
		for _, d := range p.Tree.Decls {
			if inh, ok := d.(*syntax.InheritDecl); ok {
				lastInherit = inh
			}
		}
		if lastInherit != nil {
			for _, inc := range p.Env.Own().Includes {
				if inc.Off > p.Tree.Off(lastInherit) || p.Tree.Sel.Guessed(inc.Tok) {
					continue
				}
				if file, ok := p.Index.IncludeDeclares(inc); ok {
					var target *syntax.InheritDecl
					for _, d := range p.Tree.Decls {
						if inh, ok := d.(*syntax.InheritDecl); ok && p.Tree.Off(inh) > inc.Off {
							target = inh
							break
						}
					}
					if target != nil {
						r := p.ReportNode(target, "inherit must precede all declarations, but %s (included above) declares functions or variables; include it after the inherits", inc.Raw())
						it := p.File.Tokens[inc.Tok]
						r.Related = append(r.Related, p.Related(it.Off, it.End, "included here"),
							diag.Related{Path: file, Line: 1, Col: 1, EndLine: 1, EndCol: 1, Message: "declares functions or variables"})
						return
					}
				}
			}
		}
		for _, d := range p.Tree.Decls {
			switch d := d.(type) {
			case *syntax.FuncDecl, *syntax.VarDecl:
				if a, _ := d.Span(); first == nil && !p.Tree.Sel.Guessed(a) {
					first = d
				}
			case *syntax.InheritDecl:
				if first != nil {
					r := p.ReportNode(d, "inherit must precede all declarations")
					a, _ := first.Span()
					ft := p.File.Tokens[a]
					r.Related = append(r.Related, p.Related(ft.Off, ft.End, "first declaration"))
				}
			}
		}
	},
}

// typechecked reports whether the driver compiles typeless functions with
// type checking (config typechecking >= 1). Unknown means no.
func (p *Pass) typechecked() bool {
	tc := p.Config.Dialect.Typechecking
	return tc != nil && *tc >= 1
}

// guessedSpec reports whether fn has a specifier that the parser only
// assumed to be one (an identifier it has no definition for).
func (p *Pass) guessedSpec(fn *syntax.FuncDecl) bool {
	for _, s := range fn.Specs {
		if s.Kind == token.Ident && p.Env.Lookup(string(p.File.Text(p.File.Tokens[s.Tok]))) == nil {
			return true
		}
	}
	return false
}

var ruleParamType = &Rule{
	Code: "E009", Name: "missing-parameter-type", Group: GroupSyntax,
	Doc: "untyped parameter in a typeless function, under driver typechecking",
	Explain: "With typechecking >= 1 in the driver config, a function written " +
		"without a return type is still type-checked, and a parameter without a " +
		"type is the compile error \"missing type for parameter\". Silent when the " +
		"typechecking level is unknown (set driver_config).",
	Default: true, Severity: diag.Error, CompiledOnly: true,
	Run: func(p *Pass) {
		if !p.typechecked() {
			return
		}
		p.Tree.Funcs(func(fn *syntax.FuncDecl) bool {
			if fn.Type != nil || fn.Body == nil || p.guessedSpec(fn) {
				return true
			}
			for _, par := range fn.Params {
				if par.Type == nil {
					p.ReportNode(par.Name, "missing type for parameter '%s' (typechecking is on; give it a type)", par.Name.Name)
				}
			}
			return true
		})
	},
}

var ruleReturnValue = &Rule{
	Code: "E010", Name: "return-value", Group: GroupSyntax,
	Doc: "value returned from a void (or typeless) function, or bare return in a typed one",
	Explain: "DGD rejects 'return expr;' in a void function (\"value returned from " +
		"void function\") and 'return;' in a function with a non-void type " +
		"(\"function must return value\"). Under typechecking >= 1 a function " +
		"declared without a return type is void, which makes this the most common " +
		"reason old LPmud code does not compile on DGD: give the function a type.",
	Default: true, Severity: diag.Error, CompiledOnly: true,
	Run: func(p *Pass) {
		p.Tree.Funcs(func(fn *syntax.FuncDecl) bool {
			if fn.Body == nil || fn.Name == nil {
				return true
			}
			void, typed := false, false
			switch {
			case fn.Type == nil:
				void = p.typechecked() && !p.guessedSpec(fn)
			case fn.Type.Kind == token.KwVoid && fn.Stars == 0:
				void = true
			case fn.Type.Kind != token.Ident:
				typed = true
			}
			if !void && !typed {
				return true
			}
			syntax.Inspect(fn.Body, func(n syntax.Node) bool {
				r, ok := n.(*syntax.ReturnStmt)
				if !ok {
					return true
				}
				switch {
				case void && r.X != nil && fn.Type == nil:
					p.ReportNode(r, "value returned from '%s', which has no return type and is void under typechecking; declare its type", fn.Name.Name)
				case void && r.X != nil:
					p.ReportNode(r, "value returned from void function")
				case typed && r.X == nil:
					p.ReportNode(r, "function must return a value")
				}
				return true
			})
			return true
		})
	},
}
