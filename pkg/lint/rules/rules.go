// Package rules registers dgdlint's built-in analyzers.
//
// Tier 1 rules see one file at a time. Tier 2 rules additionally see the
// cross-file index and only report what the index can prove: unknown facts
// from unresolvable targets and partial inherit chains are never guessed at.
package rules

import (
	"strings"

	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/format"
	"github.com/mmcdole/dgdtools/pkg/index"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/structure"
	"github.com/mmcdole/dgdtools/pkg/token"
)

func init() {
	lint.Register(rawInheritPath)
	lint.Register(missingVisibility)
	lint.Register(lifecycleChain)
	lint.Register(unformatted)
	lint.Register(callableNotFound)
	lint.Register(staticCrossObj)
	lint.Register(staticAutosaveVar)
	lint.Register(unresolvedInherit)
	lint.Register(undefinedPrototype)
	lint.Register(targetObjectMissing)
}

// --- tier 1 --------------------------------------------------------------

var rawInheritPath = &lint.Analyzer{
	Name: "raw-inherit-path",
	Doc: "inherit uses a literal path string instead of a lib macro; " +
		"configure rules.raw-inherit-path.deny to restrict to given prefixes",
	Tier: 1, Default: false, DefaultSeverity: diag.Warning,
	Run: func(p *lint.Pass) {
		for i := range p.Structure.Items {
			it := &p.Structure.Items[i]
			if it.Kind != structure.Inherit {
				continue
			}
			lit, ok := literalPath(p, it)
			if !ok {
				continue // macro-based: fine
			}
			if len(p.Settings.Deny) > 0 && !matchesAny(lit, p.Settings.Deny) {
				continue
			}
			p.Reportf(p.File.Tokens[it.PathFirst].Off,
				"inherit uses literal path %q; prefer the lib's path macro", lit)
		}
	},
}

// literalPath extracts a pure string-literal inherit path (concatenation
// of literals allowed, macros not).
func literalPath(p *lint.Pass, it *structure.Item) (string, bool) {
	var b strings.Builder
	for i := it.PathFirst; i <= it.PathLast; i++ {
		t := p.File.Tokens[i]
		switch t.Kind {
		case token.StringLit:
			b.WriteString(strings.Trim(string(p.File.Text(t)), `"`))
		case token.Plus, token.LParen, token.RParen:
		default:
			if t.Kind.IsTrivia() {
				continue
			}
			return "", false
		}
	}
	return b.String(), b.Len() > 0
}

func matchesAny(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

var missingVisibility = &lint.Analyzer{
	Name: "missing-visibility",
	Doc:  "function has no visibility specifier (private/static/nomask or a specifier macro like public)",
	Tier: 1, Default: false, DefaultSeverity: diag.Warning,
	Run: func(p *lint.Pass) {
		p.Structure.Funcs(func(it *structure.Item) bool {
			if len(it.SpecIdxs) == 0 && it.NameIdx >= 0 {
				name := p.File.Text(p.File.Tokens[it.NameIdx])
				p.Reportf(p.File.Tokens[it.NameIdx].Off,
					"function '%s' has no visibility specifier", name)
			}
			return true
		})
	},
}

var unformatted = &lint.Analyzer{
	Name: "unformatted",
	Doc:  "file is not dgdfmt-formatted",
	Tier: 1, Default: false, DefaultSeverity: diag.Info,
	Run: func(p *lint.Pass) {
		opts := format.Options{
			Indent:        p.Config.Format.Indent,
			MaxBlankLines: p.Config.Format.MaxBlankLines,
		}
		switch p.Config.Format.LineEndings {
		case "lf":
			opts.LineEndings = format.LF
		case "crlf":
			opts.LineEndings = format.CRLF
		}
		if p.Config.Format.FunctionHeaders == "joined" {
			opts.FuncHeaders = format.HeadersJoined
		}
		out, err := format.Format(p.File, opts)
		if err != nil {
			return // unlexable files are dgdfmt's problem, not a finding
		}
		if string(out) != string(p.File.Src) {
			p.Reportf(0, "file is not dgdfmt-formatted (run dgdfmt -w)")
		}
	},
}

// --- tier 2 --------------------------------------------------------------

var lifecycleChain = &lint.Analyzer{
	Name: "lifecycle-chain",
	Doc: "a local lifecycle function (default: create) omits calls to " +
		"resolved explicit parent implementations; multiple parents require " +
		"labeled calls, with suppressions available for intentional diamonds; " +
		"configure rules.lifecycle-chain.names",
	Tier: 2, Default: true, DefaultSeverity: diag.Warning,
	Run: func(p *lint.Pass) {
		if p.Index == nil || p.Object == nil {
			return
		}
		names := p.Settings.Names
		if len(names) == 0 {
			names = []string{"create"}
		}
		p.Structure.Funcs(func(it *structure.Item) bool {
			if it.Kind != structure.FuncDef || it.NameIdx < 0 {
				return true
			}
			name := string(p.File.Text(p.File.Tokens[it.NameIdx]))
			if !contains(names, name) {
				return true
			}
			checkLifecycleChain(p, it, name)
			return true
		})
	},
}

type lifecycleParent struct {
	labels []string
	path   string
}

func lifecycleParents(p *lint.Pass, name string) []lifecycleParent {
	var parents []lifecycleParent
	seen := map[string]int{}
	for _, ref := range p.Object.Inherits {
		if !ref.Resolved {
			continue
		}
		fn, ok := p.Index.Chain(ref.Path).Funcs[name]
		if !ok || fn.PrototypeOnly || fn.Private {
			continue
		}
		if i, ok := seen[ref.Path]; ok {
			if ref.Label != "" {
				parents[i].labels = append(parents[i].labels, ref.Label)
			}
			continue // the same inherited program is one implementation
		}
		parent := lifecycleParent{path: ref.Path}
		if ref.Label != "" {
			parent.labels = []string{ref.Label}
		}
		seen[ref.Path] = len(parents)
		parents = append(parents, parent)
	}
	return parents
}

type lifecycleCalls struct {
	unqualified bool
	labels      map[string]bool
	labelOrder  []string
}

// collectLifecycleCalls recognizes actual ::name() and label::name() calls;
// a name reference without the following '(' does not satisfy the rule.
func collectLifecycleCalls(f *token.File, it *structure.Item, name string) lifecycleCalls {
	calls := lifecycleCalls{labels: map[string]bool{}}
	for i := it.BodyL; i <= it.BodyR && i < len(f.Tokens); i++ {
		if f.Tokens[i].Kind != token.ColonColon {
			continue
		}
		nameIdx := nextSignificant(f, i+1, it.BodyR)
		if nameIdx < 0 || f.Tokens[nameIdx].Kind != token.Ident ||
			string(f.Text(f.Tokens[nameIdx])) != name {
			continue
		}
		callIdx := nextSignificant(f, nameIdx+1, it.BodyR)
		if callIdx < 0 || f.Tokens[callIdx].Kind != token.LParen {
			continue
		}
		prevIdx := previousSignificant(f, i-1, it.BodyL)
		if prevIdx >= 0 && f.Tokens[prevIdx].Kind == token.Ident {
			label := string(f.Text(f.Tokens[prevIdx]))
			if !calls.labels[label] {
				calls.labelOrder = append(calls.labelOrder, label)
			}
			calls.labels[label] = true
		} else {
			calls.unqualified = true
		}
	}
	return calls
}

func nextSignificant(f *token.File, start, end int) int {
	for i := start; i <= end && i < len(f.Tokens); i++ {
		if !f.Tokens[i].Kind.IsTrivia() {
			return i
		}
	}
	return -1
}

func previousSignificant(f *token.File, start, end int) int {
	for i := start; i >= end && i >= 0; i-- {
		if !f.Tokens[i].Kind.IsTrivia() {
			return i
		}
	}
	return -1
}

func checkLifecycleChain(p *lint.Pass, it *structure.Item, name string) {
	parents := lifecycleParents(p, name)
	if len(parents) == 0 {
		return
	}
	calls := collectLifecycleCalls(p.File, it, name)
	if len(parents) == 1 {
		parent := parents[0]
		if calls.unqualified || anyLifecycleLabelCalled(calls, parent.labels) ||
			(len(parent.labels) == 0 && len(calls.labelOrder) > 0) {
			return
		}
		suggestion := "::" + name + "()"
		for _, label := range parent.labels {
			suggestion += " or " + label + "::" + name + "()"
		}
		p.Reportf(p.File.Tokens[it.NameIdx].Off,
			"%s() overrides inherited %s::%s() but does not chain it; call %s",
			name, parent.path, name, suggestion)
		return
	}

	knownLabels := map[string]bool{}
	for _, parent := range parents {
		for _, label := range parent.labels {
			knownLabels[label] = true
		}
	}
	var fallbackLabels []string
	for _, label := range calls.labelOrder {
		if !knownLabels[label] {
			fallbackLabels = append(fallbackLabels, label)
		}
	}

	var unlabeled, missing []string
	for _, parent := range parents {
		if len(parent.labels) == 0 {
			unlabeled = append(unlabeled, parent.path)
			continue
		}
		if !anyLifecycleLabelCalled(calls, parent.labels) {
			missing = append(missing, parent.labels[0]+"::"+name+"()")
		}
	}
	// The structure scanner takes one preprocessor branch. In old portable
	// source that branch may contain unlabeled MudOS inherits while the body
	// uses labels declared in the DGD alternative. Match otherwise-unclaimed
	// label calls to those parents in source order; DGD itself validates that
	// the labels are real.
	if len(fallbackLabels) >= len(unlabeled) {
		unlabeled = nil
	} else {
		unlabeled = unlabeled[len(fallbackLabels):]
	}
	if len(unlabeled) == 0 && len(missing) == 0 {
		return
	}
	var details []string
	if len(unlabeled) > 0 {
		details = append(details, "missing labeled "+name+"() call for "+strings.Join(unlabeled, ", "))
	}
	if len(missing) > 0 {
		details = append(details, "missing "+strings.Join(missing, ", "))
	}
	p.Reportf(p.File.Tokens[it.NameIdx].Off,
		"%s() does not chain all inherited implementations: %s",
		name, strings.Join(details, "; "))
}

func anyLifecycleLabelCalled(calls lifecycleCalls, labels []string) bool {
	for _, label := range labels {
		if calls.labels[label] {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

var callableNotFound = &lint.Analyzer{
	Name: "callable-not-found",
	Doc: "a string-referenced function (call_other, obj->fn(), call_out, " +
		"configured registrars) does not exist in the target's inherit " +
		"chain — the call silently returns nil at runtime",
	Tier: 2, Default: true, DefaultSeverity: diag.Error,
	Run: func(p *lint.Pass) {
		forEachCall(p, func(call index.StringCall, lk index.Lookup, targetDesc string) {
			if lk.State != index.LookupMissing {
				return
			}
			p.Reportf(call.Off, "function '%s' is not defined in %s%s (called via %s)",
				call.Func, targetDesc, lk.Note, call.Registrar)
		})
	},
}

var staticCrossObj = &lint.Analyzer{
	Name: "static-crossobj",
	Doc: "a call_other-style call targets an unreachable function and " +
		"silently returns nil: private functions are never call_other-able; " +
		"static functions are call_other-able only by the same object " +
		"(DGD interpret.cpp Frame::call semantics)",
	Tier: 2, Default: true, DefaultSeverity: diag.Error,
	Run: func(p *lint.Pass) {
		forEachCall(p, func(call index.StringCall, lk index.Lookup, targetDesc string) {
			if call.Kind != index.CrossObject || lk.State != index.LookupFound {
				return
			}
			sameObject := call.Target == index.TargetSelf || call.TargetPath == p.LibPath
			switch {
			case lk.Fn.Private:
				p.Reportf(call.Off, "'%s' in %s is private — call_other cannot reach it and returns nil",
					call.Func, lk.Fn.DefinedIn)
			case lk.Fn.Static && !sameObject:
				p.Reportf(call.Off, "'%s' in %s is static — call_other from another object returns nil",
					call.Func, lk.Fn.DefinedIn)
			}
		})
	},
}

// forEachCall resolves every string call in the current object through the
// index and hands the lookup to fn. Unknown/unverifiable lookups are
// reported by no rule.
func forEachCall(p *lint.Pass, fn func(index.StringCall, index.Lookup, string)) {
	if p.Index == nil || p.Object == nil {
		return
	}
	for _, call := range p.Object.Calls {
		lk := p.Index.LookupCallable(p.LibPath, call)
		desc := call.TargetPath
		if call.Target == index.TargetSelf {
			desc = p.LibPath + " (this_object)"
		}
		fn(call, lk, desc)
	}
}

var staticAutosaveVar = &lint.Analyzer{
	Name: "static-autosave-var",
	Doc: "a static global variable in an auto-saving object — static " +
		"variables are excluded from save_object/restore_object, so this " +
		"state silently does not persist. Off by default: static is also " +
		"the deliberate idiom for runtime-only state, so enable this when " +
		"reviewing specifier changes, not as a standing audit",
	Tier: 2, Default: false, DefaultSeverity: diag.Warning,
	Run: func(p *lint.Pass) {
		if p.Object == nil || !p.Object.AutoSave {
			return
		}
		for name, v := range p.Object.Vars {
			if v.Static {
				p.Reportf(v.Off,
					"static variable '%s' will not be saved by this auto-saving object", name)
			}
		}
	},
}

var undefinedPrototype = &lint.Analyzer{
	Name: "undefined-prototype",
	Doc: "a function is declared (prototype) but never defined anywhere in " +
		"the inherit chain, and it is called — DGD compiles this fine and " +
		"raises a runtime error (\"Undefined function\") at the call",
	Tier: 2, Default: true, DefaultSeverity: diag.Error,
	Run: func(p *lint.Pass) {
		if p.Index == nil || p.Object == nil {
			return
		}
		// String-referenced calls (call_other, ->, call_out, registrars).
		forEachCall(p, func(call index.StringCall, lk index.Lookup, targetDesc string) {
			if lk.State != index.LookupFound || !lk.Fn.PrototypeOnly {
				return
			}
			if call.Target == index.TargetSelf && p.Index.ProvidedByUsers(p.LibPath, call.Func) {
				return // a leaf or sibling module defines it
			}
			p.Reportf(call.Off,
				"'%s' in %s is declared but never defined — calling it is a runtime error",
				call.Func, lk.Fn.DefinedIn)
		})

		// Direct local calls: ident( where ident resolves to a
		// prototype-only chain entry.
		ch := p.Index.Chain(p.LibPath)
		if ch.Partial {
			return // the definition may live in an unresolved parent
		}
		defNames := map[int]bool{}
		p.Structure.Funcs(func(it *structure.Item) bool {
			defNames[it.NameIdx] = true
			return true
		})
		var sig []int
		for i, t := range p.File.Tokens {
			if !t.Kind.IsTrivia() && t.Kind != token.EOF {
				sig = append(sig, i)
			}
		}
		for j := 0; j+1 < len(sig); j++ {
			t := p.File.Tokens[sig[j]]
			if t.Kind != token.Ident || p.File.Tokens[sig[j+1]].Kind != token.LParen {
				continue
			}
			if defNames[sig[j]] { // a declaration/definition header, not a call
				continue
			}
			if j > 0 && p.File.Tokens[sig[j-1]].Kind == token.Arrow {
				continue // method calls are handled via the index above
			}
			name := string(p.File.Text(t))
			fn, ok := ch.Funcs[name]
			if !ok || !fn.PrototypeOnly {
				continue
			}
			// Module escape hatch: an inheritor or includer may provide
			// the definition (directly or through its own chain).
			if !p.Index.ProvidedByUsers(p.LibPath, name) {
				p.Reportf(t.Off,
					"'%s' is declared but never defined (prototype in %s) — calling it is a runtime error",
					name, fn.DefinedIn)
			}
		}
	},
}

var targetObjectMissing = &lint.Analyzer{
	Name: "target-object-missing",
	Doc: "a literal object path (inherit, clone_object, call_other target, " +
		"...) has no backing file — loading it is a runtime error; paths " +
		"served by virtual-object daemons belong in lint.virtual_paths",
	Tier: 2, Default: true, DefaultSeverity: diag.Error,
	Run: func(p *lint.Pass) {
		if p.Index == nil || p.Object == nil {
			return
		}
		missing := func(lib string) bool {
			return !p.Index.IsVirtual(lib) && !p.Index.ObjectExists(lib)
		}
		for _, ref := range p.Object.PathRefs {
			if missing(ref.Path) {
				p.Reportf(ref.Off, "object %s does not exist (via %s)", ref.Path, ref.Via)
			}
		}
		for _, call := range p.Object.Calls {
			if call.Target == index.TargetPath && missing(call.TargetPath) {
				p.Reportf(call.Off, "object %s does not exist (called via %s)",
					call.TargetPath, call.Registrar)
			}
		}
		for _, ref := range p.Object.Inherits {
			if ref.Resolved && missing(ref.Path) {
				p.Reportf(ref.Off, "inherited object %s does not exist", ref.Path)
			}
		}
	},
}

var unresolvedInherit = &lint.Analyzer{
	Name: "unresolved-inherit",
	Doc: "an inherit path could not be resolved by the macro evaluator; " +
		"tier-2 rules cannot verify calls involving this chain",
	Tier: 2, Default: false, DefaultSeverity: diag.Info,
	Run: func(p *lint.Pass) {
		if p.Object == nil {
			return
		}
		for _, ref := range p.Object.Inherits {
			if !ref.Resolved {
				p.Reportf(ref.Off, "cannot resolve inherit path %q", ref.Raw)
			}
		}
	},
}
