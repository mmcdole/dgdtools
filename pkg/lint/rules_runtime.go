package lint

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mmcdole/dgdtools/pkg/calls"
	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/index"
	"github.com/mmcdole/dgdtools/pkg/syntax"
)

// The runtime group: code that compiles and then fails, or silently
// returns nil, when it runs. All of these need the lib index and report
// only what the index can prove.

// definedAt builds the related location of the definition of the
// function called name: its name there.
func definedAt(fn index.ChainFunc, name, msg string) []diag.Related {
	if fn.Path == "" {
		return nil
	}
	n := len(name)
	return []diag.Related{{
		Path: fn.Path, Line: fn.Line, Col: fn.Col, EndLine: fn.Line, EndCol: fn.Col + n,
		Start: fn.Off, End: fn.Off + uint32(n), Message: msg,
	}}
}

var ruleCallableNotFound = &Rule{
	Code: "R001", Name: "callable-not-found", Group: GroupRuntime,
	Doc: "string-referenced function (obj->fn(), call_other, call_out, registrars) missing from the target's inherit chain",
	Explain: "In DGD a call_other to a function that does not exist silently " +
		"returns nil. Reported only when the receiver is known (a string constant, " +
		"a macro that evaluates to one, or this_object()) and the target's whole " +
		"inherit chain was resolved and parsed; for a file that other objects " +
		"inherit or #include, the function is also looked for in those users.",
	Default: true, Severity: diag.Error, CrossFile: true, CompiledOnly: true,
	Run: func(p *Pass) {
		for i, c := range p.calls().Named {
			lk, desc := p.lookup(i)
			if lk.State != index.LookupMissing {
				continue
			}
			where := ""
			switch lk.Searched {
			case index.SearchedStub:
				where = " or is private there (a profile lists public functions only)"
			case index.SearchedCheckout:
				where = " (nor in this checkout; inheritors elsewhere in the mudlib cannot be seen)"
			case index.SearchedUsers:
				if lk.Users > 0 {
					where = fmt.Sprintf(" (nor in any of its %d inheritors)", lk.Users)
				}
			}
			p.Report(c.Start, c.End, "function '%s' is not defined in %s%s (called via %s)",
				c.Func, desc, where, c.Via)
		}
	},
}

var ruleStaticCrossObj = &Rule{
	Code: "R002", Name: "static-crossobj", Group: GroupRuntime,
	Doc: "call_other to an unreachable function: private always, static from another object",
	Explain: "call_other cannot reach private functions at all, and reaches static " +
		"functions only from the same object. The call silently returns nil. " +
		"On this_object() a private function is reported only when the name is " +
		"private in the whole chain and in every object that inherits the file, " +
		"as private-callback has it: DGD keeps an inherited function that a " +
		"private one redefines, and this_object() may be an inheritor that has it.",
	Default: true, Severity: diag.Error, CrossFile: true, CompiledOnly: true,
	Run: func(p *Pass) {
		for i, c := range p.calls().Named {
			if c.Kind != calls.CrossObject {
				continue
			}
			lk, _ := p.lookup(i)
			if lk.State != index.LookupFound {
				continue
			}
			same := c.Target == calls.TargetSelf || c.TargetPath == p.LibPath
			var d *diag.Diagnostic
			switch {
			case lk.Fn.Private && c.Target == calls.TargetSelf:
				// this_object() may be an object that inherits the file,
				// and DGD keeps the inherited function a private one
				// redefines: private-callback's question.
				if fn, _, ok := p.Index.PrivateOnly(p.Ctx, c.Func); ok {
					d = p.Report(c.Start, c.End, "'%s' in %s is private — call_other cannot reach it and returns nil",
						c.Func, fn.DefinedIn)
				}
			case lk.Fn.Private:
				d = p.Report(c.Start, c.End, "'%s' in %s is private — call_other cannot reach it and returns nil",
					c.Func, lk.Fn.DefinedIn)
			case lk.Fn.Static && !same:
				d = p.Report(c.Start, c.End, "'%s' in %s is static — call_other from another object returns nil",
					c.Func, lk.Fn.DefinedIn)
			}
			if d != nil {
				d.Related = definedAt(lk.Fn, c.Func, "defined here")
			}
		}
	},
}

var rulePrivateCallback = &Rule{
	Code: "R007", Name: "private-callback", Group: GroupRuntime,
	Doc: "callback by name (call_out, a registrar of the lib) to a private function, which DGD never calls",
	Explain: "call_out(\"fn\") and every callback the lib's registrars make by name " +
		"(conventions.call_registry) find the function in the object's table of " +
		"functions, and DGD leaves private functions out of that table, and the " +
		"functions of an object inherited privately: the callback silently never " +
		"happens. A static function is what such a callback can reach and no other " +
		"object can call. Reported only when the receiver is this_object() and the " +
		"name is a string constant, and when the name is private in the object's " +
		"whole inherit chain, as known in full, and in every object that inherits " +
		"or includes the file; not in a partial checkout or in the auto object. " +
		"The ->fn and call_other forms are static-crossobj's. There is no fix: " +
		"making the function static is what is usually meant, but a function that " +
		"is no longer private redefines and clashes with the functions of that " +
		"name of the objects it is inherited with, which only its author can weigh.",
	Default: true, Severity: diag.Error, CrossFile: true, CompiledOnly: true,
	Run: func(p *Pass) {
		for _, c := range p.calls().Named {
			if c.Kind != calls.SelfCallback || c.Target != calls.TargetSelf {
				continue
			}
			fn, inherited, ok := p.Index.PrivateOnly(p.Ctx, c.Func)
			if !ok {
				continue
			}
			how := "private in " + fn.DefinedIn
			if inherited {
				how = "inherited privately from " + fn.DefinedIn
			}
			d := p.Report(c.Start, c.End, "%s() never calls '%s', which is %s: DGD calls no private function by name",
				c.Via, c.Func, how)
			d.Related = definedAt(fn, c.Func, "defined here")
		}
	},
}

var ruleUndefinedPrototype = &Rule{
	Code: "R003", Name: "undefined-prototype", Group: GroupRuntime,
	Doc: "function declared but never defined anywhere in the chain, and called",
	Explain: "DGD compiles a call to a function that only has a prototype and " +
		"raises \"Undefined function\" when the call runs. Not reported when an " +
		"inheritor or includer of the file provides the definition.",
	Default: true, Severity: diag.Error, CrossFile: true, CompiledOnly: true,
	Run: func(p *Pass) {
		for i, c := range p.calls().Named {
			lk, _ := p.lookup(i)
			if lk.State != index.LookupFound || !lk.Fn.PrototypeOnly {
				continue
			}
			if c.Target == calls.TargetSelf && p.Index.ProvidedByUsers(p.Ctx, c.Func) {
				continue
			}
			d := p.Report(c.Start, c.End, "'%s' in %s is declared but never defined — calling it is a runtime error",
				c.Func, lk.Fn.DefinedIn)
			d.Related = definedAt(lk.Fn, c.Func, "prototype")
		}

		ch := p.Index.Chain(p.LibPath)
		if ch.Partial {
			return // the definition may live where we cannot see
		}
		for _, dc := range p.calls().Direct {
			if dc.Call.Scoped {
				continue
			}
			name := dc.Call.Fun.Name
			fn, ok := ch.Funcs[name]
			if !ok || !fn.PrototypeOnly {
				continue
			}
			if !p.Index.ProvidedByUsers(p.Ctx, name) {
				d := p.ReportNode(dc.Call.Fun, "'%s' is declared but never defined (prototype in %s) — calling it is a runtime error",
					name, fn.DefinedIn)
				d.Related = definedAt(*fn, name, "prototype")
			}
		}
	},
}

var ruleTargetObjectMissing = &Rule{
	Code: "R004", Name: "target-object-missing", Group: GroupRuntime,
	Doc: "literal object path (inherit, clone_object, call target) with no backing file",
	Explain: "Loading an object whose file does not exist is a runtime error (for " +
		"an inherit, a compile error). Paths served by virtual-object daemons " +
		"belong in conventions.virtual_paths. Not reported when the mudlib on disk is " +
		"incomplete.",
	Default: true, Severity: diag.Error, CrossFile: true, CompiledOnly: true,
	Run: func(p *Pass) {
		missing := func(lib string) bool {
			return p.Index.Covers(lib) && !p.Index.IsVirtual(lib) && !p.Index.ObjectExists(lib)
		}
		cf := p.calls()
		for _, ref := range cf.Paths {
			if missing(ref.Path) {
				p.Report(ref.Start, ref.End, "object %s does not exist (via %s)", ref.Path, ref.Via)
			}
		}
		for _, c := range cf.Named {
			if c.Target == calls.TargetPath && missing(c.TargetPath) {
				p.Report(c.Start, c.End, "object %s does not exist (called via %s)", c.TargetPath, c.Via)
			}
		}
		for _, ref := range p.Unit.Inherits {
			if ref.Resolved && missing(ref.Path) {
				p.Report(ref.Off, ref.End, "inherited object %s does not exist", ref.Path)
			}
		}
	},
}

var ruleCallbackArity = &Rule{
	Code: "R005", Name: "callback-arity", Group: GroupRuntime,
	Doc: "dispatched call passing an argument count the target cannot accept",
	Explain: "Under non-strict typechecking DGD silently pads missing call_other " +
		"arguments with nil and drops extras; under strict typechecking it is a " +
		"runtime error.",
	Default: true, Severity: diag.Warning, CrossFile: true, CompiledOnly: true,
	Run: func(p *Pass) {
		for i, c := range p.calls().Named {
			if c.NArgs < 0 {
				continue
			}
			lk, _ := p.lookup(i)
			if lk.State != index.LookupFound || lk.Fn.PrototypeOnly {
				continue
			}
			var d *diag.Diagnostic
			switch {
			case c.NArgs < lk.Fn.MinArgs:
				d = p.Report(c.Start, c.End,
					"'%s' in %s needs at least %d argument(s); this call passes %d (missing ones become nil)",
					c.Func, lk.Fn.DefinedIn, lk.Fn.MinArgs, c.NArgs)
			case lk.Fn.MaxArgs >= 0 && c.NArgs > lk.Fn.MaxArgs:
				d = p.Report(c.Start, c.End,
					"'%s' in %s takes at most %d argument(s); this call passes %d (extras are dropped)",
					c.Func, lk.Fn.DefinedIn, lk.Fn.MaxArgs, c.NArgs)
			}
			if d != nil {
				d.Related = definedAt(lk.Fn, c.Func, "defined here")
			}
		}
	},
}

var ruleLifecycleChain = &Rule{
	Code: "R006", Name: "lifecycle-chain", Group: GroupRuntime,
	Doc: "lifecycle function (the create function) does not call the inherited implementations",
	Explain: "A create function that overrides an inherited one without calling " +
		"it (::create()) leaves the parent uninitialized. With several defining " +
		"parents, label each inherit and chain each one. Proof-based: files with " +
		"no explicit inherit, parents without the function, and unresolved " +
		"parents are skipped. The function is the project's create function, " +
		"conventions.create (\"create\" when it is not set; a mudlib whose auto object " +
		"receives the driver's call and passes it on under another name sets the " +
		"name its objects define). The option names lists other functions to hold " +
		"to the same rule.",
	Default: true, Severity: diag.Warning, CrossFile: true, CompiledOnly: true,
	Options: func() any { return &lifecycleOptions{} },
	Run: func(p *Pass) {
		names := p.Options.(*lifecycleOptions).Names
		if len(names) == 0 {
			names = []string{p.Config.Create()}
		}
		for _, d := range p.Tree.Decls {
			fn, ok := d.(*syntax.FuncDecl)
			if !ok || fn.Body == nil || fn.Name == nil {
				continue
			}
			if slices.Contains(names, fn.Name.Name) {
				checkLifecycleChain(p, fn)
			}
		}
	},
}

// lifecycleOptions are the options of lifecycle-chain.
type lifecycleOptions struct {
	Names []string `yaml:"names" doc:"the functions that must call what they override (default: the project's create function)"`
}

type lifecycleParent struct {
	labels []string
	path   string
}

func lifecycleParents(p *Pass, name string) []lifecycleParent {
	var parents []lifecycleParent
	seen := map[string]int{}
	for _, ref := range p.Unit.Inherits {
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

func checkLifecycleChain(p *Pass, fn *syntax.FuncDecl) {
	name := fn.Name.Name
	parents := lifecycleParents(p, name)
	if len(parents) == 0 {
		return
	}
	// ::name() and label::name() calls in the body.
	unqualified := false
	labels := map[string]bool{}
	var labelOrder []string
	opaque := false
	syntax.Inspect(fn.Body, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.BadStmt, *syntax.MacroStmt:
			opaque = true // a chained call may hide in what we could not parse
		case *syntax.CallExpr:
			if n.Opaque {
				opaque = true
			}
			if !n.Scoped || n.Fun.Name != name {
				return true
			}
			if n.Label == nil {
				unqualified = true
			} else if !labels[n.Label.Name] {
				labels[n.Label.Name] = true
				labelOrder = append(labelOrder, n.Label.Name)
			}
		}
		return true
	})
	if opaque {
		return
	}
	called := func(ls []string) bool {
		for _, l := range ls {
			if labels[l] {
				return true
			}
		}
		return false
	}

	if len(parents) == 1 {
		parent := parents[0]
		if unqualified || called(parent.labels) || (len(parent.labels) == 0 && len(labelOrder) > 0) {
			return
		}
		suggestion := "::" + name + "()"
		for _, label := range parent.labels {
			suggestion += " or " + label + "::" + name + "()"
		}
		p.ReportNode(fn.Name, "%s() overrides inherited %s::%s() but does not chain it; call %s",
			name, parent.path, name, suggestion)
		return
	}

	known := map[string]bool{}
	for _, parent := range parents {
		for _, label := range parent.labels {
			known[label] = true
		}
	}
	var fallback []string
	for _, label := range labelOrder {
		if !known[label] {
			fallback = append(fallback, label)
		}
	}
	var unlabeled, missing []string
	for _, parent := range parents {
		if len(parent.labels) == 0 {
			unlabeled = append(unlabeled, parent.path)
			continue
		}
		if !called(parent.labels) {
			missing = append(missing, parent.labels[0]+"::"+name+"()")
		}
	}
	// One preprocessor branch is linted. In old portable source that
	// branch may hold unlabeled inherits while the body uses labels
	// declared in the other branch; match otherwise-unclaimed label calls
	// to those parents in order. DGD validates that the labels are real.
	if len(fallback) >= len(unlabeled) {
		unlabeled = nil
	} else {
		unlabeled = unlabeled[len(fallback):]
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
	p.ReportNode(fn.Name, "%s() does not chain all inherited implementations: %s",
		name, strings.Join(details, "; "))
}
