package rules

// The rules in this file cover the "compiles clean, fails or misbehaves at
// runtime" constructs the DGD compiler does not diagnose — especially
// under non-strict typechecking (typechecking = 1), where argument-count
// mismatches on dispatched calls are silently padded with nil or dropped.

import (

	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/index"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/token"
)

func init() {
	lint.Register(callbackArity)
	lint.Register(includeNotFound)
	lint.Register(assignInCondition)
	lint.Register(noEffectStatement)
	lint.Register(sscanfFormat)
}

// --- tier 2 --------------------------------------------------------------

var callbackArity = &lint.Analyzer{
	Name: "callback-arity",
	Doc: "a dispatched call (obj->fn(), call_other, call_out) passes an " +
		"argument count the target cannot accept — under non-strict " +
		"typechecking DGD silently pads missing arguments with nil and " +
		"drops extras; under strict typechecking it is a runtime error",
	Tier: 2, Default: true, DefaultSeverity: diag.Warning,
	Run: func(p *lint.Pass) {
		forEachCall(p, func(call index.StringCall, lk index.Lookup, targetDesc string) {
			if lk.State != index.LookupFound || lk.Fn.PrototypeOnly || call.NArgs < 0 {
				return
			}
			switch {
			case call.NArgs < lk.Fn.MinArgs:
				p.Reportf(call.Off,
					"'%s' in %s needs at least %d argument(s); this call passes %d (missing ones become nil)",
					call.Func, lk.Fn.DefinedIn, lk.Fn.MinArgs, call.NArgs)
			case lk.Fn.MaxArgs >= 0 && call.NArgs > lk.Fn.MaxArgs:
				p.Reportf(call.Off,
					"'%s' in %s takes at most %d argument(s); this call passes %d (extras are dropped)",
					call.Func, lk.Fn.DefinedIn, lk.Fn.MaxArgs, call.NArgs)
			}
		})
	},
}

var includeNotFound = &lint.Analyzer{
	Name: "include-not-found",
	Doc: "a #include target could not be found in the file's directory or " +
		"the configured include_dirs — a compile error when the object loads",
	Tier: 2, Default: true, DefaultSeverity: diag.Error,
	Run: func(p *lint.Pass) {
		if p.Object == nil {
			return
		}
		for _, bi := range p.Object.BadIncludes {
			p.Reportf(bi.Off, "cannot find include file %s", bi.Raw)
		}
	},
}

// --- tier 1 --------------------------------------------------------------

// sigTokens returns the indexes of significant tokens.
func sigTokens(p *lint.Pass) []int {
	var sig []int
	for i, t := range p.File.Tokens {
		if !t.Kind.IsTrivia() && t.Kind != token.EOF {
			sig = append(sig, i)
		}
	}
	return sig
}

var assignInCondition = &lint.Analyzer{
	Name: "assignment-in-condition",
	Doc: "a suspicious assignment inside an if/while/for condition: " +
		"assigning a bare literal (a typo for ==) or mixing assignment " +
		"with && or || (`=` binds last, so the variable gets the boolean). " +
		"The assign-and-test idiom `if (sz = sizeof(x))` is accepted",
	Tier: 1, Default: true, DefaultSeverity: diag.Warning,
	Run: func(p *lint.Pass) {
		sig := sigTokens(p)
		for j := 0; j+1 < len(sig); j++ {
			k := p.File.Tokens[sig[j]].Kind
			if (k != token.KwIf && k != token.KwWhile && k != token.KwFor) ||
				p.File.Tokens[sig[j+1]].Kind != token.LParen {
				continue
			}
			depth := 0
			clause := 0 // for(init; COND; update): only the middle clause
			for i := j + 1; i < len(sig); i++ {
				switch p.File.Tokens[sig[i]].Kind {
				case token.LParen, token.LBracket, token.LBrace:
					depth++
				case token.RParen, token.RBracket, token.RBrace:
					depth--
				case token.Semicolon:
					if depth == 1 {
						clause++
					}
				case token.Assign:
					// depth 1 = directly in the condition; ((x = y))
					// sits at depth 2 and is always accepted.
					if depth == 1 && (k != token.KwFor || clause == 1) {
						switch classifyCondAssign(p, sig, i, k) {
						case "literal":
							p.Reportf(p.File.Tokens[sig[i]].Off,
								"assignment of a constant in %s condition (did you mean ==?)", k)
						case "precedence":
							p.Reportf(p.File.Tokens[sig[i]].Off,
								"assignment mixed with &&/|| in %s condition: = binds last, the variable gets the boolean", k)
						}
					}
				}
				if depth == 0 {
					break
				}
			}
		}
	},
}

// classifyCondAssign inspects the right-hand side of an assignment found
// at condition depth 1. "literal" means the RHS is a bare constant (the ==
// typo class); "precedence" means the condition mixes the assignment with
// && or || at the same level (the variable receives the boolean); ""
// means the assign-and-test idiom, which is accepted.
func classifyCondAssign(p *lint.Pass, sig []int, i int, kw token.Kind) string {
	depth := 1
	var rhs []token.Kind
	for j := i + 1; j < len(sig); j++ {
		k := p.File.Tokens[sig[j]].Kind
		switch k {
		case token.LParen, token.LBracket, token.LBrace:
			depth++
		case token.RParen, token.RBracket, token.RBrace:
			depth--
			if depth == 0 {
				return classifyRHS(rhs)
			}
		case token.Semicolon:
			if depth == 1 && kw == token.KwFor {
				return classifyRHS(rhs) // end of the for's middle clause
			}
		case token.LAnd, token.LOr:
			if depth == 1 {
				return "precedence"
			}
		case token.Comma:
			if depth == 1 {
				return classifyRHS(rhs) // comma operator ends this RHS
			}
		}
		rhs = append(rhs, k)
	}
	return classifyRHS(rhs)
}

// classifyRHS reports "literal" for a bare (optionally negated) constant.
func classifyRHS(rhs []token.Kind) string {
	if len(rhs) > 0 && (rhs[0] == token.Minus || rhs[0] == token.Not || rhs[0] == token.Tilde) {
		rhs = rhs[1:]
	}
	if len(rhs) != 1 {
		return ""
	}
	switch rhs[0] {
	case token.IntLit, token.FloatLit, token.StringLit, token.CharLit, token.KwNil:
		return "literal"
	}
	return ""
}

var noEffectStatement = &lint.Analyzer{
	Name: "no-effect-statement",
	Doc: "a comparison used as a statement (`x == 1;`) — computes a value " +
		"and throws it away; almost always a typo for assignment",
	Tier: 1, Default: true, DefaultSeverity: diag.Warning,
	Run: func(p *lint.Pass) {
		sig := sigTokens(p)
		braceDepth := 0 // block braces only
		litDepth := 0   // ({ ... }) / ([ ... ]) literal braces
		stmtStart := -1
		var prev token.Kind
		var hasCompare, poisoned bool
		var compareOff uint32

		reset := func(next int) {
			stmtStart = next
			hasCompare, poisoned = false, false
		}

		for j := 0; j < len(sig); j++ {
			t := p.File.Tokens[sig[j]]
			k := t.Kind
			switch k {
			case token.LBrace:
				if prev == token.LParen { // "({" literal: part of the stmt
					litDepth++
					poisoned = true
				} else {
					braceDepth++
					reset(j + 1)
				}
			case token.RBrace:
				if litDepth > 0 {
					litDepth--
					poisoned = true
				} else {
					braceDepth--
					reset(j + 1)
				}
			case token.LParen, token.LBracket, token.RParen, token.RBracket:
				// Any call, grouping, or indexing poisons the pattern.
				poisoned = true
			case token.Semicolon:
				if braceDepth > 0 && stmtStart >= 0 && j > stmtStart &&
					hasCompare && !poisoned {
					p.Reportf(compareOff,
						"comparison has no effect as a statement (did you mean =?)")
				}
				reset(j + 1)
			case token.Colon:
				reset(j + 1) // case labels / labels start a fresh statement
			case token.EqEq, token.NotEq:
				if !hasCompare {
					hasCompare = true
					compareOff = t.Off
				}
			case token.Assign, token.PlusEq, token.MinusEq, token.StarEq,
				token.SlashEq, token.PercentEq, token.AmpEq, token.PipeEq,
				token.CaretEq, token.ShlEq, token.ShrEq,
				token.Inc, token.Dec, token.Arrow, token.ColonColon:
				poisoned = true
			default:
				if j == stmtStart && k.IsKeyword() {
					poisoned = true // if/return/for/... own their statements
				}
			}
			prev = k
		}
	},
}

var sscanfFormat = &lint.Analyzer{
	Name: "sscanf-format",
	Doc: "the number of %-conversions in a scan format string does not " +
		"match the variables supplied; configure format_registry for " +
		"mudlib printf-family functions",
	Tier: 1, Default: true, DefaultSeverity: diag.Warning,
	Run: func(p *lint.Pass) {
		registry := map[string]int{"sscanf": 1}
		for name, idx := range p.Config.Lint.FormatRegistry {
			registry[name] = idx
		}
		sig := sigTokens(p)
		for j := 0; j+1 < len(sig); j++ {
			t := p.File.Tokens[sig[j]]
			if t.Kind != token.Ident {
				continue
			}
			fmtIdx, ok := registry[string(p.File.Text(t))]
			if !ok || p.File.Tokens[sig[j+1]].Kind != token.LParen {
				continue
			}
			if j > 0 && p.File.Tokens[sig[j-1]].Kind == token.Arrow {
				continue
			}
			args := splitArgsLocal(p, sig, j+1)
			if fmtIdx >= len(args) || len(args[fmtIdx]) != 1 {
				continue
			}
			ft := p.File.Tokens[args[fmtIdx][0]]
			if ft.Kind != token.StringLit {
				continue
			}
			convs := countConversions(string(p.File.Text(ft)))
			supplied := len(args) - fmtIdx - 1
			// Fewer variables than conversions is the legal DGD idiom for
			// discarding trailing matches; only oversupply is a bug.
			if supplied > convs {
				p.Reportf(ft.Off,
					"format has only %d conversion(s) but %d variable(s) are supplied",
					convs, supplied)
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

// splitArgsLocal mirrors the index package's argument splitter for tier-1
// rules that run without an index.
func splitArgsLocal(p *lint.Pass, sig []int, open int) [][]int {
	var args [][]int
	var cur []int
	depth := 0
	for j := open; j < len(sig); j++ {
		k := p.File.Tokens[sig[j]].Kind
		switch k {
		case token.LParen, token.LBracket, token.LBrace:
			depth++
			if depth > 1 {
				cur = append(cur, sig[j])
			}
			continue
		case token.RParen, token.RBracket, token.RBrace:
			depth--
			if depth == 0 {
				if len(cur) > 0 || len(args) > 0 {
					args = append(args, cur)
				}
				return args
			}
			cur = append(cur, sig[j])
			continue
		case token.Comma:
			if depth == 1 {
				args = append(args, cur)
				cur = nil
				continue
			}
		}
		if depth >= 1 {
			cur = append(cur, sig[j])
		}
	}
	return nil
}

