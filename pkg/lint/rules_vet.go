package lint

import (
	"slices"
	"strings"

	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/format"
	"github.com/mmcdole/dgdtools/pkg/syntax"
	"github.com/mmcdole/dgdtools/pkg/token"
)

// ------------------------------------------------------------ suspicious

var ruleAssignInCondition = &Rule{
	Code: "S001", Name: "assignment-in-condition", Group: GroupSuspicious,
	Doc: "condition assigns a bare constant or the result of unparenthesized &&/||",
	Explain: "Assign-and-test (if (x = f())) is an accepted LPC idiom. Two forms " +
		"are almost always bugs: assigning a constant (if (x = 0), a typo for ==) " +
		"and storing the result of an unparenthesized logical expression " +
		"(if (x = f() && ready) gives x 0 or 1). Parenthesizing the assignment or " +
		"the right-hand side states the intent and silences the rule. Assignments " +
		"with an invalid target (!x = f()) are compile errors and left to DGD.",
	Default: true, Severity: diag.Warning,
	Run: func(p *Pass) {
		check := func(cond syntax.Expr, kw string) {
			if cond == nil {
				return
			}
			parts := []syntax.Expr{cond}
			if c, ok := cond.(*syntax.CommaExpr); ok {
				parts = c.List
			}
			for _, e := range parts {
				a, ok := e.(*syntax.AssignExpr)
				if !ok || a.Op != token.Assign {
					continue
				}
				// Walk the chain x = y = rhs; an invalid target anywhere
				// makes the whole thing DGD's to report.
				last := a
				valid := true
				for {
					if !isLvalue(last.L) {
						valid = false
						break
					}
					next, ok := last.R.(*syntax.AssignExpr)
					if !ok || next.Op != token.Assign {
						break
					}
					last = next
				}
				if !valid {
					continue
				}
				opTok := p.File.Tokens[last.OpTok]
				switch rhs := last.R.(type) {
				case *syntax.BasicLit:
					p.Report(opTok.Off, opTok.End, "assignment of a constant in %s condition (did you mean ==?)", kw)
				case *syntax.UnaryExpr:
					if _, lit := rhs.X.(*syntax.BasicLit); lit && (rhs.Op == token.Minus || rhs.Op == token.Not || rhs.Op == token.Tilde) {
						p.Report(opTok.Off, opTok.End, "assignment of a constant in %s condition (did you mean ==?)", kw)
					}
				case *syntax.BinaryExpr:
					if rhs.Op == token.LAnd || rhs.Op == token.LOr {
						p.Report(opTok.Off, opTok.End,
							"assignment in %s condition stores the result of &&/||; parenthesize the assignment or logical RHS to make the intended value explicit", kw)
					}
				}
			}
		}
		syntax.Inspect(p.Tree, func(n syntax.Node) bool {
			switch n := n.(type) {
			case *syntax.IfStmt:
				check(n.Cond, "if")
			case *syntax.WhileStmt:
				check(n.Cond, "while")
			case *syntax.DoStmt:
				check(n.Cond, "while")
			case *syntax.ForStmt:
				check(n.Cond, "for")
			}
			return true
		})
	},
}

func isLvalue(e syntax.Expr) bool {
	switch e.(type) {
	case *syntax.Ident, *syntax.IndexExpr, *syntax.RangeExpr, *syntax.ParenExpr:
		return true
	}
	return false
}

func isComparison(k token.Kind) bool {
	switch k {
	case token.EqEq, token.NotEq, token.Lt, token.Gt, token.LtEq, token.GtEq:
		return true
	}
	return false
}

var ruleNoEffect = &Rule{
	Code: "S002", Name: "no-effect-statement", Group: GroupSuspicious,
	Doc: "comparison used as a statement (x == 1;)",
	Explain: "A statement whose top-level operator is a comparison computes a " +
		"value and discards it; almost always a typo for assignment.",
	Default: true, Severity: diag.Warning,
	Run: func(p *Pass) {
		syntax.Inspect(p.Tree, func(n syntax.Node) bool {
			s, ok := n.(*syntax.ExprStmt)
			if !ok {
				return true
			}
			if b, ok := s.X.(*syntax.BinaryExpr); ok && isComparison(b.Op) {
				t := p.File.Tokens[b.OpTok]
				hint := ""
				if b.Op == token.EqEq {
					hint = " (did you mean =?)"
				}
				p.Report(t.Off, t.End, "comparison has no effect as a statement%s", hint)
			}
			return true
		})
	},
}

var ruleSscanfFormat = &Rule{
	Code: "S003", Name: "sscanf-format", Group: GroupSuspicious,
	Doc: "more variables supplied than %-conversions in the scan format",
	Explain: "Fewer variables than conversions is the legal idiom for discarding " +
		"trailing matches; more variables than conversions means some are never " +
		"assigned. The function checked is sscanf.",
	Default: true, Severity: diag.Warning,
	Run: func(p *Pass) {
		const fmtIdx = 1 // sscanf(str, format, ...)
		for _, dc := range p.calls().Direct {
			c := dc.Call
			if c.Fun.Name != "sscanf" || c.Scoped || c.Spread || fmtIdx >= len(c.Args) {
				continue
			}
			lit, ok := c.Args[fmtIdx].(*syntax.BasicLit)
			if !ok || lit.Kind != token.StringLit {
				continue
			}
			convs := countConversions(p.Tree.Text(lit))
			supplied := len(c.Args) - fmtIdx - 1
			if supplied > convs {
				p.ReportNode(lit, "format has only %d conversion(s) but %d variable(s) are supplied", convs, supplied)
			}
		}
	},
}

// countConversions counts % conversions in a format literal, ignoring %%
// and counting %*x (assignment-suppressed) as zero.
func countConversions(lit string) int {
	n := 0
	for i := 0; i+1 < len(lit); i++ {
		if lit[i] != '%' {
			continue
		}
		switch lit[i+1] {
		case '%', '*':
			i++
		default:
			n++
		}
	}
	return n
}

var ruleUnreachable = &Rule{
	Code: "S004", Name: "unreachable-code", Group: GroupSuspicious,
	Doc: "statement directly after return, break, continue or goto in the same block",
	Explain: "Code after an unconditional jump in the same block never runs. " +
		"Labels and case labels restart reachability. Off by default: legal, and " +
		"common as leftover debugging in old code.",
	Default: false, Severity: diag.Warning, Tags: []diag.Tag{diag.Unnecessary},
	Run: func(p *Pass) {
		syntax.Inspect(p.Tree, func(n syntax.Node) bool {
			b, ok := n.(*syntax.BlockStmt)
			if !ok {
				return true
			}
			for i := 0; i+1 < len(b.Stmts); i++ {
				// A jump may sit under case labels: `case 1: return x;`.
				s := b.Stmts[i]
				for {
					if c, ok := s.(*syntax.CaseStmt); ok && c.Body != nil {
						s = c.Body
					} else if l, ok := s.(*syntax.LabelStmt); ok && l.Body != nil {
						s = l.Body
					} else {
						break
					}
				}
				switch s.(type) {
				case *syntax.ReturnStmt, *syntax.BreakStmt, *syntax.ContinueStmt, *syntax.GotoStmt:
				default:
					continue
				}
				switch next := b.Stmts[i+1].(type) {
				case *syntax.CaseStmt, *syntax.LabelStmt, *syntax.EmptyStmt, *syntax.BadStmt, *syntax.MacroStmt:
				default:
					d := p.ReportNode(next, "unreachable code")
					d.Related = append(d.Related, p.Related(p.Tree.Off(b.Stmts[i]), p.Tree.End(b.Stmts[i]), "control leaves here"))
				}
			}
			return true
		})
	},
}

// ------------------------------------------------------------ convention

var ruleRawInheritPath = &Rule{
	Code: "C001", Name: "raw-inherit-path", Group: GroupConvention,
	Doc: "inherit uses a literal path string where the mudlib mandates macros",
	Explain: "For mudlibs that name their inheritable objects with macros (inherit " +
		"ROOM;), so that a moved file is one changed #define. The option deny " +
		"restricts the rule to the parts of the mudlib that have such macros.",
	Default: false, Severity: diag.Warning,
	Options: func() any { return &inheritPathOptions{} },
	Run: func(p *Pass) {
		deny := p.Options.(*inheritPathOptions).Deny
		for _, d := range p.Tree.Decls {
			inh, ok := d.(*syntax.InheritDecl)
			if !ok {
				continue
			}
			lit, ok := literalPath(p, inh.Path)
			if !ok {
				continue
			}
			if len(deny) > 0 && !hasAnyPrefix(lit, deny) {
				continue
			}
			p.ReportNode(inh.Path, "inherit uses literal path %q; prefer the mudlib's path macro", lit)
		}
	},
}

// inheritPathOptions are the options of raw-inherit-path.
type inheritPathOptions struct {
	Deny []string `yaml:"deny" doc:"report only literal paths that begin with one of these (default: all of them)"`
}

// literalPath extracts a pure string-literal path (concatenation of
// literals allowed, macros not).
func literalPath(p *Pass, e syntax.Expr) (string, bool) {
	var b strings.Builder
	ok := true
	syntax.Inspect(e, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.BasicLit:
			if n.Kind != token.StringLit {
				ok = false
			}
			b.WriteString(strings.Trim(p.Tree.Text(n), `"`))
		case *syntax.BinaryExpr:
			if n.Op != token.Plus {
				ok = false
			}
		case *syntax.ParenExpr, *syntax.AdjacentStrings:
		default:
			ok = false
		}
		return ok
	})
	return b.String(), ok && b.Len() > 0
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

var ruleMissingVisibility = &Rule{
	Code: "C002", Name: "missing-visibility", Group: GroupConvention,
	Doc: "function with no visibility specifier",
	Explain: "Flags functions declared without private, static, nomask or one " +
		"of the mudlib's specifier macros (a macro the file's headers define as " +
		"empty or as specifiers, or one conventions.macros classes as specifier).",
	Default: false, Severity: diag.Warning,
	Run: func(p *Pass) {
		p.Tree.Funcs(func(fn *syntax.FuncDecl) bool {
			if len(fn.Specs) == 0 && fn.Name != nil {
				p.ReportNode(fn.Name, "function '%s' has no visibility specifier", fn.Name.Name)
			}
			return true
		})
	},
}

var ruleUnformatted = &Rule{
	Code: "C003", Name: "unformatted", Group: GroupConvention,
	Doc: "file is not formatted as `dgdtools format` would format it",
	Explain: "Formats the file exactly as `dgdtools format` does, with the format settings of that file, and reports a difference. " +
		"What dgdtools-ignore-start format comments protect is left alone here too. " +
		"A file `dgdtools format` refuses to change (it does not tokenize, or the result could not be proven) is not reported: that command says why.",
	Default: false, Severity: diag.Info,
	Run: func(p *Pass) {
		out, err := format.Format(p.File, p.Format)
		if err == nil && string(out) != string(p.File.Src) {
			p.Report(0, 0, "file is not formatted (run `dgdtools format`)")
		}
	},
}

var ruleStaticAutosaveVar = &Rule{
	Code: "C004", Name: "static-autosave-var", Group: GroupConvention,
	Doc: "static global in an object that saves itself; excluded from save_object",
	Explain: "Static variables are not saved by save_object. Off by default: " +
		"static is also the deliberate idiom for runtime-only state, so this is a " +
		"review aid for specifier changes, not a standing audit. An object saves " +
		"itself when its source mentions one of conventions.autosave_markers (the " +
		"mudlib's function or macro that turns saving on); without markers the rule " +
		"reports nothing.",
	Default: false, Severity: diag.Warning,
	Run: func(p *Pass) {
		if !mentions(p, p.Config.Conventions.AutosaveMarkers) {
			return
		}
		for _, d := range p.Tree.Decls {
			vd, ok := d.(*syntax.VarDecl)
			if !ok || !vd.Has(token.KwStatic) {
				continue
			}
			for _, v := range vd.Vars {
				p.ReportNode(v.Name, "static variable '%s' will not be saved by this auto-saving object", v.Name.Name)
			}
		}
	},
}

// mentions reports whether one of names stands as an identifier in the
// file's compiled code.
func mentions(p *Pass, names []string) bool {
	if len(names) == 0 {
		return false
	}
	for _, i := range p.Tree.Sel.Sig {
		t := p.File.Tokens[i]
		if t.Kind != token.Ident {
			continue
		}
		text := p.File.Text(t)
		if slices.ContainsFunc(names, func(n string) bool { return n == string(text) }) {
			return true
		}
	}
	return false
}

var ruleUnresolvedInherit = &Rule{
	Code: "C005", Name: "unresolved-inherit", Group: GroupConvention,
	Doc: "inherit path the macro evaluator could not resolve",
	Explain: "Informational: cross-file rules cannot verify anything involving " +
		"this object's inherit chain. Usually a path built from a function-like " +
		"macro or a macro from an unresolved #include.",
	Default: false, Severity: diag.Info, CrossFile: true,
	Run: func(p *Pass) {
		for _, ref := range p.Unit.Inherits {
			if !ref.Resolved {
				p.Report(ref.Off, ref.End, "cannot resolve inherit path %q", ref.Raw)
			}
		}
	},
}
