// Package index builds the cross-file view of an LPC lib that tier-2 lint
// rules need: every object's functions and variables, its resolved inherit
// chain, and every string-referenced call site (call_other, obj->fn(),
// call_out, and configured registrars).
//
// In DGD, every cross-object call is late-bound by name and a missing
// function silently returns nil instead of erroring — the compiler only
// checks names within a file's own inherit chain. The index is what lets a
// linter give LPC the missing-function check other languages get from
// their compiler.
//
// Inherit paths hidden behind macros are resolved with a purpose-built
// preprocessor subset: object-like #define constants from the file, its
// includes (recursively, depth-capped), and the driver's force-included
// file, evaluated over parenthesized string concatenation. Anything beyond
// that marks the chain partial, and tier-2 rules report "cannot verify"
// rather than guessing.
package index

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/fileset"
	"github.com/mmcdole/dgdtools/pkg/lexer"
	"github.com/mmcdole/dgdtools/pkg/structure"
	"github.com/mmcdole/dgdtools/pkg/token"
)

type CallKind uint8

const (
	// CrossObject calls use call_other semantics: static and private
	// functions are invisible and yield nil.
	CrossObject CallKind = iota
	// SelfCallback calls (call_out, registered handlers) can reach static
	// functions of this_object().
	SelfCallback
)

type TargetKind uint8

const (
	TargetUnknown TargetKind = iota
	TargetSelf               // this_object()
	TargetPath               // literal object path
)

// StringCall is one call site that references a function by string name.
type StringCall struct {
	Func       string
	Registrar  string // "->", "call_other", "call_out", or a config name
	Kind       CallKind
	Target     TargetKind
	TargetPath string // lib path, when Target == TargetPath
	// NArgs is the number of arguments the callee will receive, when the
	// call site determines it (->, call_other, call_out); -1 when unknown
	// (config registrars, spread arguments).
	NArgs int
	Off   uint32
}

type FuncInfo struct {
	Static, Private bool
	// PrototypeOnly marks a declared-but-never-defined function: DGD
	// compiles these fine and raises "Undefined function" at call time.
	PrototypeOnly bool
	// MinArgs/MaxArgs bound the accepted argument count. MaxArgs -1 means
	// unbounded (trailing ellipsis). Under non-strict typechecking DGD
	// silently pads missing arguments with nil and drops extras.
	MinArgs, MaxArgs int
	Off              uint32
}

// BadInclude is a #include whose target file could not be found — a
// compile error the moment the object is loaded.
type BadInclude struct {
	Raw string
	Off uint32
}

// PathRef is a literal object path passed to a path-taking function
// (clone_object and friends, per the object registry).
type PathRef struct {
	Path string // normalized lib path
	Via  string // the function it was passed to
	Off  uint32
}

type VarInfo struct {
	Static bool
	Off    uint32
}

type InheritRef struct {
	Raw      string // source text of the path expression
	Path     string // resolved lib path (no .c)
	Resolved bool
	Private  bool // private inherit: hidden from call_other in inheritors
	Off      uint32
}

// ObjectInfo is the indexed view of one .c file.
type ObjectInfo struct {
	LibPath, FSPath string
	File            *token.File
	Structure       *structure.FileStructure
	Funcs           map[string]FuncInfo
	Vars            map[string]VarInfo
	Inherits        []InheritRef
	Calls           []StringCall
	PathRefs        []PathRef
	BadIncludes     []BadInclude
	AutoSave        bool
}

// Index is the whole-lib view.
type Index struct {
	Root    string
	Objects map[string]*ObjectInfo

	// AutoFuncs are functions provided to every object by the configured
	// auto objects (and their chains).
	AutoFuncs map[string]FuncInfo

	cfg       *config.Config
	defCache  sync.Map // fs path -> *defTable
	codeCache sync.Map // fs path -> *codeTable
	chainMu   sync.Mutex
	chains    map[string]*Chain

	usersOnce sync.Once
	users     map[string][]string // lib path -> objects that inherit it

	incMu    sync.Mutex
	incUsers map[string][]string // lib path -> objects that #include it

	virtual   *fileset.Matcher // lib paths served by virtual-object daemons
	statCache sync.Map         // lib path -> bool (file exists under root)
}

// ObjectExists reports whether libPath is backed by an indexed object or a
// file on disk (independent of exclude patterns).
func (ix *Index) ObjectExists(libPath string) bool {
	if ix.Objects[libPath] != nil {
		return true
	}
	if v, ok := ix.statCache.Load(libPath); ok {
		return v.(bool)
	}
	_, err := os.Stat(fileset.FSPath(ix.Root, libPath))
	exists := err == nil
	ix.statCache.Store(libPath, exists)
	return exists
}

// IsVirtual reports whether libPath matches the configured virtual-object
// path patterns (objects the driver resolves without a backing file).
func (ix *Index) IsVirtual(libPath string) bool {
	return ix.virtual.Match(strings.TrimPrefix(libPath, "/"))
}

// Build indexes every non-excluded .c file under the config root.
func Build(cfg *config.Config) (*Index, error) {
	ix := &Index{
		Root:      cfg.AbsRoot(),
		Objects:   map[string]*ObjectInfo{},
		AutoFuncs: map[string]FuncInfo{},
		cfg:       cfg,
		chains:    map[string]*Chain{},
		incUsers:  map[string][]string{},
		virtual:   fileset.NewMatcher(cfg.Lint.VirtualPaths),
	}

	type job struct{ path, rel string }
	var jobs []job
	err := fileset.Walk(ix.Root, cfg.Exclude, func(p, rel string) {
		if strings.HasSuffix(p, ".c") {
			jobs = append(jobs, job{p, rel})
		}
	})
	if err != nil {
		return nil, err
	}

	results := make([]*ObjectInfo, len(jobs))
	jobCh := make(chan int, 256)
	var wg sync.WaitGroup
	for range runtime.NumCPU() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobCh {
				results[i] = ix.indexFile(jobs[i].path, fileset.LibPath(jobs[i].rel))
			}
		}()
	}
	for i := range jobs {
		jobCh <- i
	}
	close(jobCh)
	wg.Wait()

	for _, obj := range results {
		if obj != nil {
			ix.Objects[obj.LibPath] = obj
		}
	}

	// Auto-object functions are visible everywhere.
	for _, auto := range cfg.Lint.AutoObjects {
		ch := ix.Chain(normalizeLibPath(auto, "/"))
		for name, fi := range ch.Funcs {
			ix.AutoFuncs[name] = FuncInfo{
				Static: fi.Static, Private: fi.Private,
				PrototypeOnly: fi.PrototypeOnly,
				MinArgs:       fi.MinArgs, MaxArgs: fi.MaxArgs,
				Off:           fi.Off,
			}
		}
	}
	return ix, nil
}

func (ix *Index) indexFile(fsPath, libPath string) *ObjectInfo {
	src, err := os.ReadFile(fsPath)
	if err != nil {
		return nil
	}
	f := lexer.Lex(fsPath, src, ix.cfg.TokenDialect())
	fs := structure.Analyze(f, structure.Config{SpecifierMacros: ix.cfg.SpecifierMacroSet()})

	obj := &ObjectInfo{
		LibPath: libPath, FSPath: fsPath,
		File: f, Structure: fs,
		Funcs: map[string]FuncInfo{},
		Vars:  map[string]VarInfo{},
	}

	for i := range fs.Items {
		it := &fs.Items[i]
		switch it.Kind {
		case structure.FuncDef, structure.Prototype:
			if it.NameIdx < 0 {
				continue
			}
			nameTok := f.Tokens[it.NameIdx]
			if nameTok.Kind != token.Ident {
				continue // operator overloads are not name-callable
			}
			name := string(f.Text(nameTok))
			minA, maxA := countParams(f, it)
			fi := FuncInfo{
				Static:        it.Has(f, token.KwStatic),
				Private:       it.Has(f, token.KwPrivate),
				PrototypeOnly: it.Kind == structure.Prototype,
				MinArgs:       minA, MaxArgs: maxA,
				Off: nameTok.Off,
			}
			// A definition always wins over a prototype.
			if _, ok := obj.Funcs[name]; !ok || !fi.PrototypeOnly {
				obj.Funcs[name] = fi
			}
		case structure.VarDecl:
			static := it.Has(f, token.KwStatic)
			private := it.Has(f, token.KwPrivate)
			_ = private
			for _, vi := range it.VarIdxs {
				vt := f.Tokens[vi]
				obj.Vars[string(f.Text(vt))] = VarInfo{Static: static, Off: vt.Off}
			}
		case structure.Inherit:
			obj.Inherits = append(obj.Inherits, ix.resolveInherit(f, it, libPath))
		}
	}

	ix.mergeIncludedCode(obj)
	ix.scanCalls(obj)
	ix.scanBadIncludes(obj)
	return obj
}

// scanBadIncludes records #include directives whose target cannot be
// found — compile errors at load time.
func (ix *Index) scanBadIncludes(obj *ObjectInfo) {
	for _, t := range obj.File.Tokens {
		if t.Kind != token.Directive {
			continue
		}
		text := string(obj.File.Text(t))
		m := includeRe.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		inc := `"` + m[3] + `"`
		if m[2] != "" {
			inc = "<" + m[2] + ">"
		}
		if len(ix.resolveIncludePath(obj.FSPath, inc)) == 0 {
			obj.BadIncludes = append(obj.BadIncludes, BadInclude{Raw: inc, Off: t.Off})
		}
	}
}

// mergeIncludedCode folds function and variable definitions from #included
// files into the includer — textual inclusion means they belong to the
// including program (Reikland-style `#include "hook.c"` into /std/object.c
// is the motivating case). Own definitions win over included ones.
func (ix *Index) mergeIncludedCode(obj *ObjectInfo) {
	seen := map[string]bool{obj.FSPath: true}
	var walk func(fsPath string, depth int)
	walk = func(fsPath string, depth int) {
		if depth > maxIncludeDepth {
			return
		}
		dt := ix.defTableFor(fsPath)
		if dt == nil {
			return
		}
		for _, inc := range dt.includes {
			for _, p := range ix.resolveIncludePath(fsPath, inc) {
				if seen[p] {
					continue
				}
				seen[p] = true
				ct := ix.codeTableFor(p)
				if len(ct.funcs) > 0 {
					// A code-bearing include is a module; its self-calls
					// must be checked against its includers (UsersOf).
					if rel, ok := strings.CutPrefix(p, ix.Root+"/"); ok {
						ix.incMu.Lock()
						lp := fileset.LibPath(rel)
						ix.incUsers[lp] = append(ix.incUsers[lp], obj.LibPath)
						ix.incMu.Unlock()
					}
				}
				for name, fi := range ct.funcs {
					own, exists := obj.Funcs[name]
					// An included definition also satisfies the includer's
					// own bare prototype.
					if !exists || (own.PrototypeOnly && !fi.PrototypeOnly) {
						obj.Funcs[name] = fi
					}
				}
				for name, vi := range ct.vars {
					if _, own := obj.Vars[name]; !own {
						obj.Vars[name] = vi
					}
				}
				walk(p, depth+1)
			}
		}
	}
	walk(obj.FSPath, 0)
}

type codeTable struct {
	funcs map[string]FuncInfo
	vars  map[string]VarInfo
}

var emptyCode = &codeTable{funcs: map[string]FuncInfo{}, vars: map[string]VarInfo{}}

// codeTableFor extracts (cached) the top-level definitions of an included
// file.
func (ix *Index) codeTableFor(fsPath string) *codeTable {
	if v, ok := ix.codeCache.Load(fsPath); ok {
		return v.(*codeTable)
	}
	src, err := os.ReadFile(fsPath)
	if err != nil {
		ix.codeCache.Store(fsPath, emptyCode)
		return emptyCode
	}
	f := lexer.Lex(fsPath, src, ix.cfg.TokenDialect())
	fs := structure.Analyze(f, structure.Config{SpecifierMacros: ix.cfg.SpecifierMacroSet()})
	ct := &codeTable{funcs: map[string]FuncInfo{}, vars: map[string]VarInfo{}}
	for i := range fs.Items {
		it := &fs.Items[i]
		switch it.Kind {
		case structure.FuncDef, structure.Prototype:
			if it.NameIdx < 0 || f.Tokens[it.NameIdx].Kind != token.Ident {
				continue
			}
			name := string(f.Text(f.Tokens[it.NameIdx]))
			if _, ok := ct.funcs[name]; !ok || it.Kind == structure.FuncDef {
				minA, maxA := countParams(f, it)
				ct.funcs[name] = FuncInfo{
					Static:        it.Has(f, token.KwStatic),
					Private:       it.Has(f, token.KwPrivate),
					PrototypeOnly: it.Kind == structure.Prototype,
					MinArgs:       minA, MaxArgs: maxA,
					Off: f.Tokens[it.NameIdx].Off,
				}
			}
		case structure.VarDecl:
			static := it.Has(f, token.KwStatic)
			for _, vi := range it.VarIdxs {
				ct.vars[string(f.Text(f.Tokens[vi]))] = VarInfo{Static: static, Off: f.Tokens[vi].Off}
			}
		}
	}
	ix.codeCache.Store(fsPath, ct)
	return ct
}

// countParams derives the accepted argument-count range of a function
// item: a per-parameter `varargs` keyword makes that and later parameters
// optional, a class-level `varargs` makes all optional, and a trailing
// `...` collects unbounded extras.
func countParams(f *token.File, it *structure.Item) (minArgs, maxArgs int) {
	if it.ParamsL < 0 || it.ParamsR < 0 {
		return 0, -1
	}
	nparams := 0
	firstOptional := -1
	ellipsis := false
	depth := 0
	sawTokens := false
	for i := it.ParamsL; i <= it.ParamsR; i++ {
		t := f.Tokens[i]
		switch t.Kind {
		case token.LParen, token.LBracket, token.LBrace:
			depth++
		case token.RParen, token.RBracket, token.RBrace:
			depth--
		case token.Comma:
			if depth == 1 {
				nparams++
			}
		case token.KwVarargs:
			if depth == 1 && firstOptional < 0 {
				firstOptional = nparams
			}
		case token.Ellipsis:
			if depth == 1 {
				ellipsis = true
			}
		default:
			if !t.Kind.IsTrivia() && depth == 1 {
				if t.Kind != token.KwVoid || sawTokens {
					sawTokens = true
				}
			}
		}
	}
	if sawTokens {
		nparams++ // params = commas + 1 when the list is non-empty
	}
	minArgs, maxArgs = nparams, nparams
	if firstOptional >= 0 {
		minArgs = firstOptional
	}
	if it.Has(f, token.KwVarargs) {
		minArgs = 0
	}
	if ellipsis {
		maxArgs = -1 // the collector param absorbs any extras
		if minArgs == nparams {
			minArgs = nparams - 1 // the collector itself is optional
		}
	}
	if minArgs < 0 {
		minArgs = 0
	}
	return minArgs, maxArgs
}

// normalizeLibPath strips .c and makes the path absolute relative to the
// directory of the referencing object.
func normalizeLibPath(p, fromDir string) string {
	p = strings.TrimSuffix(strings.TrimSpace(p), ".c")
	if !strings.HasPrefix(p, "/") {
		p = path.Join(fromDir, p)
	}
	return path.Clean(p)
}

var autosaveDefaults = []string{"set_auto_save"}

func (ix *Index) autosaveMarkers() []string {
	if len(ix.cfg.Lint.AutosaveMarkers) > 0 {
		return ix.cfg.Lint.AutosaveMarkers
	}
	return autosaveDefaults
}

// registrar describes one call-registry entry.
type registrar struct {
	arg        int
	kind       CallKind
	targetArg0 bool // target object comes from argument 0 (call_other)
}

func (ix *Index) registrars() map[string]registrar {
	m := map[string]registrar{
		"call_other": {arg: 1, kind: CrossObject, targetArg0: true},
		"call_out":   {arg: 0, kind: SelfCallback},
	}
	for name, arg := range ix.cfg.Lint.CallRegistry {
		m[name] = registrar{arg: arg, kind: SelfCallback}
	}
	return m
}

// objectRegistrars maps path-taking functions (DGD kfuns by default) to the
// argument index holding an object path.
func (ix *Index) objectRegistrars() map[string]int {
	m := map[string]int{
		"clone_object":   0,
		"compile_object": 0,
		"find_object":    0,
	}
	for name, arg := range ix.cfg.Lint.ObjectRegistry {
		m[name] = arg
	}
	return m
}

// scanCalls extracts string-referenced call sites and autosave markers.
func (ix *Index) scanCalls(obj *ObjectInfo) {
	f := obj.File
	var sig []int
	for i, t := range f.Tokens {
		if !t.Kind.IsTrivia() && t.Kind != token.EOF {
			sig = append(sig, i)
		}
	}
	kind := func(j int) token.Kind {
		if j < 0 || j >= len(sig) {
			return token.EOF
		}
		return f.Tokens[sig[j]].Kind
	}
	text := func(j int) string {
		if j < 0 || j >= len(sig) {
			return ""
		}
		return string(f.Text(f.Tokens[sig[j]]))
	}
	regs := ix.registrars()
	objRegs := ix.objectRegistrars()
	markers := map[string]bool{}
	for _, m := range ix.autosaveMarkers() {
		markers[m] = true
	}

	for j := 0; j < len(sig); j++ {
		switch kind(j) {
		case token.Arrow:
			// <target> -> ident (
			if kind(j+1) != token.Ident || kind(j+2) != token.LParen {
				continue
			}
			call := StringCall{
				Func: text(j + 1), Registrar: "->", Kind: CrossObject,
				NArgs: countArgs(f, splitArgs(f, sig, j+2)),
				Off:   f.Tokens[sig[j+1]].Off,
			}
			call.Target, call.TargetPath = ix.targetBefore(f, sig, j, obj.LibPath)
			obj.Calls = append(obj.Calls, call)

		case token.Ident:
			name := text(j)
			if markers[name] {
				// Markers match bare too: daemon macros appear as
				// MACRO->register(...), not calls.
				obj.AutoSave = true
			}
			if argIdx, ok := objRegs[name]; ok && kind(j+1) == token.LParen && kind(j-1) != token.Arrow {
				args := splitArgs(f, sig, j+1)
				if argIdx < len(args) && len(args[argIdx]) == 1 {
					pt := f.Tokens[args[argIdx][0]]
					if pt.Kind == token.StringLit {
						if p, ok := unquote(string(f.Text(pt))); ok && strings.Contains(p, "/") {
							obj.PathRefs = append(obj.PathRefs, PathRef{
								Path: normalizeLibPath(p, path.Dir(obj.LibPath)),
								Via:  name, Off: pt.Off,
							})
						}
					}
				}
			}
			reg, ok := regs[name]
			if !ok || kind(j+1) != token.LParen {
				continue
			}
			// A registrar name used as obj->call_out(...) target is the
			// arrow case; skip if preceded by '->'.
			if kind(j-1) == token.Arrow {
				continue
			}
			args := splitArgs(f, sig, j+1)
			if reg.arg >= len(args) || len(args[reg.arg]) != 1 {
				continue
			}
			fnTok := f.Tokens[args[reg.arg][0]]
			if fnTok.Kind != token.StringLit {
				continue
			}
			fn, ok := unquote(string(f.Text(fnTok)))
			if !ok {
				continue
			}
			call := StringCall{
				Func: fn, Registrar: name, Kind: reg.kind,
				Target: TargetSelf, NArgs: -1, Off: fnTok.Off,
			}
			if reg.targetArg0 {
				call.Target, call.TargetPath = classifyTarget(f, args[0], obj.LibPath)
			}
			// call_other(obj, "fn", rest...) and call_out("fn", delay,
			// rest...) both hand exactly the rest to the callee.
			if name == "call_other" || name == "call_out" {
				if len(args) >= 2 {
					call.NArgs = countArgs(f, args[2:])
				} else {
					call.NArgs = 0
				}
			}
			if !isIdentName(fn) {
				// Two-argument registrar form: fp("/obj/path", "fn") binds
				// fn on another object — a cross-object call in disguise.
				if strings.Contains(fn, "/") && reg.arg+1 < len(args) && len(args[reg.arg+1]) == 1 {
					fnTok2 := f.Tokens[args[reg.arg+1][0]]
					if fnTok2.Kind == token.StringLit {
						if fn2, ok := unquote(string(f.Text(fnTok2))); ok && isIdentName(fn2) {
							obj.Calls = append(obj.Calls, StringCall{
								Func: fn2, Registrar: name, Kind: CrossObject,
								Target: TargetPath, Off: fnTok2.Off,
								TargetPath: normalizeLibPath(fn, path.Dir(obj.LibPath)),
							})
						}
					}
				}
				continue // never record a non-identifier as a function name
			}
			obj.Calls = append(obj.Calls, call)
		}
	}
}

// targetBefore classifies the receiver expression ending just before the
// '->' at sig[j]: this_object() → Self, "literal" → Path, else Unknown.
func (ix *Index) targetBefore(f *token.File, sig []int, j int, fromLib string) (TargetKind, string) {
	if j == 0 {
		return TargetUnknown, ""
	}
	prev := f.Tokens[sig[j-1]]
	switch prev.Kind {
	case token.StringLit:
		if p, ok := unquote(string(f.Text(prev))); ok {
			return TargetPath, normalizeLibPath(p, path.Dir(fromLib))
		}
	case token.RParen:
		if j >= 3 &&
			f.Tokens[sig[j-2]].Kind == token.LParen &&
			f.Tokens[sig[j-3]].Kind == token.Ident &&
			string(f.Text(f.Tokens[sig[j-3]])) == "this_object" {
			return TargetSelf, ""
		}
	}
	return TargetUnknown, ""
}

// classifyTarget classifies a call_other first argument.
func classifyTarget(f *token.File, arg []int, fromLib string) (TargetKind, string) {
	if len(arg) == 1 && f.Tokens[arg[0]].Kind == token.StringLit {
		if p, ok := unquote(string(f.Text(f.Tokens[arg[0]]))); ok {
			return TargetPath, normalizeLibPath(p, path.Dir(fromLib))
		}
	}
	if len(arg) == 3 &&
		f.Tokens[arg[0]].Kind == token.Ident &&
		string(f.Text(f.Tokens[arg[0]])) == "this_object" &&
		f.Tokens[arg[1]].Kind == token.LParen &&
		f.Tokens[arg[2]].Kind == token.RParen {
		return TargetSelf, ""
	}
	return TargetUnknown, ""
}

// countArgs counts call arguments; -1 when a spread ("expr...") makes the
// count unknowable statically.
func countArgs(f *token.File, args [][]int) int {
	for _, arg := range args {
		for _, i := range arg {
			if f.Tokens[i].Kind == token.Ellipsis {
				return -1
			}
		}
	}
	return len(args)
}

// splitArgs returns the token indexes of each top-level argument of the
// call whose '(' is at sig[open]. Empty result on malformed input.
func splitArgs(f *token.File, sig []int, open int) [][]int {
	var args [][]int
	var cur []int
	depth := 0
	for j := open; j < len(sig); j++ {
		k := f.Tokens[sig[j]].Kind
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
	return nil // unbalanced
}

// isIdentName reports whether s is a plausible LPC function name.
func isIdentName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// unquote extracts a string literal's value. Literals with escapes beyond
// \\ and \" are rejected (never seen in function names or object paths).
func unquote(lit string) (string, bool) {
	if len(lit) < 2 || lit[0] != '"' || lit[len(lit)-1] != '"' {
		return "", false
	}
	body := lit[1 : len(lit)-1]
	if !strings.ContainsRune(body, '\\') {
		return body, true
	}
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			b.WriteByte(body[i])
			continue
		}
		i++
		if i >= len(body) {
			return "", false
		}
		switch body[i] {
		case '\\', '"':
			b.WriteByte(body[i])
		default:
			return "", false
		}
	}
	return b.String(), true
}

// --- inherit path resolution -------------------------------------------

var defineRe = regexp.MustCompile(`^#[ \t]*define[ \t]+([A-Za-z_][A-Za-z0-9_]*)([ \t(]|$)`)
var includeRe = regexp.MustCompile(`^#[ \t]*include[ \t]+(<([^>]+)>|"([^"]+)")`)

type defTable struct {
	defines  map[string]string // object-like defines: name -> value text
	includes []string          // raw include targets, <...> or "..."
}

// resolveInherit evaluates one inherit item's path expression.
func (ix *Index) resolveInherit(f *token.File, it *structure.Item, libPath string) InheritRef {
	raw := string(f.Src[f.Tokens[it.PathFirst].Off:f.Tokens[it.PathLast].End])
	ref := InheritRef{
		Raw: raw, Off: f.Tokens[it.PathFirst].Off,
		Private: it.Has(f, token.KwPrivate),
	}

	defs := ix.fileDefines(f.Path)
	val, ok := ix.evalString(raw, defs, 0)
	if !ok {
		return ref
	}
	ref.Path = normalizeLibPath(val, path.Dir(libPath))
	ref.Resolved = true
	return ref
}

// fileDefines returns the merged define table visible in fsPath: the
// driver's force-included file, then includes (recursively), then the
// file's own defines.
func (ix *Index) fileDefines(fsPath string) map[string]string {
	merged := map[string]string{}
	seen := map[string]bool{}
	if inc := ix.cfg.Lint.IncludeFile; inc != "" {
		ix.mergeDefines(fileset.FSPath(ix.Root, inc), merged, seen, 0)
	}
	ix.mergeDefines(fsPath, merged, seen, 0)
	return merged
}

const maxIncludeDepth = 6

func (ix *Index) mergeDefines(fsPath string, merged map[string]string, seen map[string]bool, depth int) {
	if depth > maxIncludeDepth || seen[fsPath] {
		return
	}
	seen[fsPath] = true
	dt := ix.defTableFor(fsPath)
	if dt == nil {
		return
	}
	for _, inc := range dt.includes {
		for _, p := range ix.resolveIncludePath(fsPath, inc) {
			ix.mergeDefines(p, merged, seen, depth+1)
		}
	}
	for k, v := range dt.defines {
		merged[k] = v // the including file's own defines win
	}
}

// defTableFor lexes fsPath (cached) and extracts its directives.
func (ix *Index) defTableFor(fsPath string) *defTable {
	if v, ok := ix.defCache.Load(fsPath); ok {
		return v.(*defTable)
	}
	var dt *defTable
	if src, err := os.ReadFile(fsPath); err == nil {
		dt = &defTable{defines: map[string]string{}}
		f := lexer.Lex(fsPath, src, ix.cfg.TokenDialect())
		for _, t := range f.Tokens {
			if t.Kind != token.Directive {
				continue
			}
			text := string(f.Text(t))
			if m := defineRe.FindStringSubmatch(text); m != nil {
				if m[2] == "(" {
					continue // function-like macro: out of scope
				}
				body := text[len(m[0]):]
				if m[2] != "" {
					body = m[2] + body // keep first byte of value if consumed
				}
				dt.defines[m[1]] = strings.TrimSpace(body)
			} else if m := includeRe.FindStringSubmatch(text); m != nil {
				if m[2] != "" {
					dt.includes = append(dt.includes, "<"+m[2]+">")
				} else {
					dt.includes = append(dt.includes, `"`+m[3]+`"`)
				}
			}
		}
	}
	ix.defCache.Store(fsPath, dt)
	return dt
}

// resolveIncludePath maps an include target to candidate fs paths:
// "file" searches the including file's directory then include_dirs;
// <file> searches include_dirs only.
func (ix *Index) resolveIncludePath(fromFS, inc string) []string {
	var cands []string
	name := inc[1 : len(inc)-1]
	if strings.HasPrefix(inc, `"`) {
		if strings.HasPrefix(name, "/") {
			cands = append(cands, fileset.FSPath(ix.Root, name))
		} else {
			cands = append(cands, path.Join(path.Dir(fromFS), name))
		}
	}
	for _, dir := range ix.cfg.Lint.IncludeDirs {
		cands = append(cands, fileset.FSPath(ix.Root, path.Join(dir, name)))
	}
	var out []string
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			out = append(out, c)
			break // first hit wins, like the compiler
		}
	}
	return out
}

const maxMacroDepth = 6

// evalString evaluates a parenthesized string-concatenation expression
// with macro substitution: ("/d/Empire/" + SUBDIR).
func (ix *Index) evalString(expr string, defs map[string]string, depth int) (string, bool) {
	if depth > maxMacroDepth {
		return "", false
	}
	f := lexer.Lex("<expr>", []byte(expr), ix.cfg.TokenDialect())
	var b strings.Builder
	expectOperand := true
	for _, t := range f.Significant() {
		switch t.Kind {
		case token.LParen, token.RParen:
			// grouping only
		case token.StringLit:
			if !expectOperand {
				return "", false
			}
			s, ok := unquote(string(f.Text(t)))
			if !ok {
				return "", false
			}
			b.WriteString(s)
			expectOperand = false
		case token.Ident:
			if !expectOperand {
				return "", false
			}
			val, ok := defs[string(f.Text(t))]
			if !ok {
				return "", false
			}
			s, ok := ix.evalString(val, defs, depth+1)
			if !ok {
				return "", false
			}
			b.WriteString(s)
			expectOperand = false
		case token.Plus:
			if expectOperand {
				return "", false
			}
			expectOperand = true
		default:
			return "", false
		}
	}
	if expectOperand { // empty or trailing '+'
		return "", false
	}
	return b.String(), true
}

// --- inherit chains ------------------------------------------------------

// UsersOf returns the objects whose inherit closure includes libPath
// (excluding libPath itself). Built lazily over the whole index.
func (ix *Index) UsersOf(libPath string) []string {
	ix.usersOnce.Do(func() {
		ix.users = map[string][]string{}
		closures := map[string][]string{}
		var closure func(p string, visiting map[string]bool) []string
		closure = func(p string, visiting map[string]bool) []string {
			if c, ok := closures[p]; ok {
				return c
			}
			if visiting[p] {
				return nil
			}
			visiting[p] = true
			var out []string
			if obj := ix.Objects[p]; obj != nil {
				for _, ref := range obj.Inherits {
					if !ref.Resolved {
						continue
					}
					out = append(out, ref.Path)
					out = append(out, closure(ref.Path, visiting)...)
				}
			}
			delete(visiting, p)
			closures[p] = out
			return out
		}
		for p := range ix.Objects {
			seen := map[string]bool{}
			for _, anc := range closure(p, map[string]bool{}) {
				if anc != p && !seen[anc] {
					seen[anc] = true
					ix.users[anc] = append(ix.users[anc], p)
				}
			}
		}
	})
	users := ix.users[libPath]
	ix.incMu.Lock()
	users = append(users, ix.incUsers[libPath]...)
	ix.incMu.Unlock()
	return users
}

// LookupState classifies a callable lookup.
type LookupState uint8

const (
	LookupUnknown LookupState = iota // cannot verify: skip, never report
	LookupFound                      // in the target's own chain (Fn valid)
	LookupFoundInUser                // provided by an inheriting object
	LookupMissing                    // provably absent (Note explains scope)
)

// Lookup is the result of resolving one string call.
type Lookup struct {
	State LookupState
	Fn    ChainFunc // valid when State == LookupFound
	Note  string    // extra context for LookupMissing
}

// maxUserChains bounds the fallback search through inheritors' full chains.
const maxUserChains = 20

// LookupCallable resolves a string call from the given object. Self-calls
// in module files (files that others inherit) are checked against their
// inheritors too: the callback a module registers is routinely provided by
// a sibling module or the leaf.
func (ix *Index) LookupCallable(fromLib string, call StringCall) Lookup {
	var target string
	switch call.Target {
	case TargetSelf:
		target = fromLib
	case TargetPath:
		target = call.TargetPath
	default:
		return Lookup{State: LookupUnknown}
	}

	ch := ix.Chain(target)
	if fn, ok := ch.Funcs[call.Func]; ok {
		return Lookup{State: LookupFound, Fn: fn}
	}
	if fn, ok := ix.AutoFuncs[call.Func]; ok {
		return Lookup{State: LookupFound, Fn: ChainFunc{
			Static: fn.Static, Private: fn.Private,
			MinArgs: fn.MinArgs, MaxArgs: fn.MaxArgs,
			Off:     fn.Off, DefinedIn: "(auto object)",
		}}
	}
	if ch.Partial {
		return Lookup{State: LookupUnknown}
	}

	users := ix.UsersOf(target)
	if len(users) == 0 {
		return Lookup{State: LookupMissing}
	}
	// Module file: any inheritor may provide the function.
	for _, u := range users {
		if obj := ix.Objects[u]; obj != nil {
			if _, ok := obj.Funcs[call.Func]; ok {
				return Lookup{State: LookupFoundInUser}
			}
		}
	}
	checked := 0
	for _, u := range users {
		if checked >= maxUserChains {
			return Lookup{State: LookupUnknown} // bounded: cannot verify
		}
		uch := ix.Chain(u)
		checked++
		if uch.Partial {
			return Lookup{State: LookupUnknown}
		}
		if _, ok := uch.Funcs[call.Func]; ok {
			return Lookup{State: LookupFoundInUser}
		}
	}
	return Lookup{State: LookupMissing,
		Note: fmt.Sprintf(" (nor in any of its %d inheritors)", len(users))}
}

// ProvidedByUsers reports whether any inheritor/includer of libPath
// supplies a real (non-prototype) definition of fn — the module escape
// hatch for mixins whose callbacks and prototypes are satisfied by
// siblings or leaves. Returns true (assume provided) when the bounded
// search cannot decide.
func (ix *Index) ProvidedByUsers(libPath, fn string) bool {
	users := ix.UsersOf(libPath)
	if len(users) == 0 {
		return false
	}
	for _, u := range users {
		if obj := ix.Objects[u]; obj != nil {
			if f, ok := obj.Funcs[fn]; ok && !f.PrototypeOnly {
				return true
			}
		}
	}
	checked := 0
	for _, u := range users {
		if checked >= maxUserChains {
			return true // undecided: never report on a bounded search
		}
		uch := ix.Chain(u)
		checked++
		if uch.Partial {
			return true
		}
		if f, ok := uch.Funcs[fn]; ok && !f.PrototypeOnly {
			return true
		}
	}
	return false
}

// Chain is the flattened inherit view of one object.
type Chain struct {
	Funcs   map[string]ChainFunc
	Partial bool     // some inherit could not be resolved or loaded
	Objects []string // lib paths in the chain, leaf last
}

type ChainFunc struct {
	Static, Private  bool
	PrototypeOnly    bool
	MinArgs, MaxArgs int
	Off              uint32
	DefinedIn        string
}

// Chain returns the flattened chain for a lib path, caching results.
func (ix *Index) Chain(libPath string) *Chain {
	ix.chainMu.Lock()
	defer ix.chainMu.Unlock()
	return ix.chainLocked(libPath, map[string]bool{})
}

func (ix *Index) chainLocked(libPath string, visiting map[string]bool) *Chain {
	if ch, ok := ix.chains[libPath]; ok {
		return ch
	}
	ch := &Chain{Funcs: map[string]ChainFunc{}}
	if visiting[libPath] {
		ch.Partial = true // inheritance cycle; the driver would reject it
		return ch
	}
	obj := ix.Objects[libPath]
	if obj == nil {
		ch.Partial = true
		ix.chains[libPath] = ch
		return ch
	}
	visiting[libPath] = true
	for _, ref := range obj.Inherits {
		if !ref.Resolved {
			ch.Partial = true
			continue
		}
		sub := ix.chainLocked(ref.Path, visiting)
		if sub.Partial {
			ch.Partial = true
		}
		// Private functions stay in the chain flagged Private so rules
		// can explain *why* a call fails instead of just "not defined".
		// Everything below a `private inherit` is call_other-invisible
		// to inheritors too (DGD excludes it from the symbol table).
		// A prototype from one sibling module never clobbers another
		// sibling's definition — any definition in the composed program
		// satisfies every prototype of the same name.
		for name, fn := range sub.Funcs {
			if ref.Private {
				fn.Private = true
			}
			if fn.PrototypeOnly {
				if old, ok := ch.Funcs[name]; ok && !old.PrototypeOnly {
					continue
				}
			}
			ch.Funcs[name] = fn
		}
		ch.Objects = append(ch.Objects, sub.Objects...)
	}
	delete(visiting, libPath)
	for name, fi := range obj.Funcs {
		if fi.PrototypeOnly {
			// An inherited definition satisfies a local prototype.
			if old, ok := ch.Funcs[name]; ok && !old.PrototypeOnly {
				continue
			}
		}
		ch.Funcs[name] = ChainFunc{
			Static: fi.Static, Private: fi.Private,
			PrototypeOnly: fi.PrototypeOnly,
			MinArgs:       fi.MinArgs, MaxArgs: fi.MaxArgs,
			Off: fi.Off, DefinedIn: libPath,
		}
	}
	ch.Objects = append(ch.Objects, libPath)
	ix.chains[libPath] = ch
	return ch
}
