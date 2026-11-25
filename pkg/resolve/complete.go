package resolve

import (
	"cmp"
	"path"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mmcdole/dgdtools/internal/fuzzy"
	"github.com/mmcdole/dgdtools/pkg/calls"
	"github.com/mmcdole/dgdtools/pkg/index"
	"github.com/mmcdole/dgdtools/pkg/kfun"
	"github.com/mmcdole/dgdtools/pkg/pp"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/syntax"
	"github.com/mmcdole/dgdtools/pkg/token"
)

// Context says what kind of place a completion was asked for at.
type Context uint8

const (
	// InCode is a name in the code: in the body of a function, or
	// where declarations stand.
	InCode Context = iota + 1
	// AfterArrow is the function's name in x->name().
	AfterArrow
	// AfterScope is the function's name in ::name() and label::name().
	AfterScope
	// InInclude is the path of an #include, InInherit that of an
	// inherit statement.
	InInclude
	InInherit
	// InCallback is the string that names a function for call_other,
	// call_out or one of the project's registrars.
	InCallback
	// InDirective is a macro's name in an #ifdef, #if, #elif or #undef.
	InDirective
)

// MaxItems is how many items a Completion has at most.
const MaxItems = 2000

// maxDetail is how much of a macro's #define an item shows.
const maxDetail = 120

// Completion is what can be written at a place in a file.
type Completion struct {
	Context Context
	// Start and End are the bytes of the file that an item replaces:
	// the word the cursor is in or behind, the last part of a path being
	// written, the name in a string as far as the cursor; nothing, at
	// the cursor, when nothing is written yet.
	Start, End uint32
	// Items are what fits there, the nearest first (Item.Rank) and by
	// name within that. They are not chosen by what is written already,
	// which whoever shows them does as more is typed, unless there are
	// more than MaxItems: then they are the MaxItems that match it
	// best, and Incomplete says so.
	Items      []Item
	Incomplete bool
	// Called says a '(' follows already: a function's name needs none.
	Called bool
}

// Item is one thing that can be written.
type Item struct {
	// Kind is what the name is: a Function (of the lib's or the
	// driver's), a Variable, a Local, a Parameter, a Macro, a Keyword
	// (a type is one); in a path an Include (a file to include), an
	// Object or a Directory.
	Kind Kind
	// Name is what is written.
	Name string
	// Detail is the declaration: a function's or a kfun's, a variable's
	// with its type, a macro's #define. It is "" for a keyword and in a
	// path.
	Detail string
	// Doc is what the kfun table says of a kfun. The comment above
	// another declaration is not read for a list of hundreds: Path and
	// Off say where the declaration is, for Resolver.Comment, when
	// there is a file to read it in.
	Doc  string
	Path string
	Off  uint32
	Origin
	// Call says the name is called: a function, a macro with
	// parameters; Args that it takes arguments.
	Call, Args bool
	// Rank orders the items by how near what they name is: 0 a
	// parameter or a local variable, 1 what the file itself declares, 2
	// what it inherits, 3 a macro, 4 a function of the auto object, 5 a
	// kfun, 6 a keyword. In a path a header comes before a directory
	// and a directory before another file.
	Rank int
}

// Complete returns what can be written at byte offset off of the file,
// nil when that is nothing the tools can say: in a comment, a number or
// a string that names nothing, in code that is not compiled.
//
// Where the cursor is, is read from the tokens to its left, so that the
// answer is there while the statement does not parse:
//
//   - In the body of a function: the parameters and local variables in
//     scope, the global variables and functions of the object (its own,
//     those it inherits and may name, the auto object's), the macros in
//     view, the kfuns where a name is known to mean one (as Hover says
//     it), and the keywords that can stand there.
//   - Where declarations stand: the types and specifiers, and the macros
//     that are one; after `inherit`, the macros that are a path. In the
//     value of a global variable, what an expression can name.
//   - After `::` and `label::`: the functions the file inherits (through
//     that label), and after `::` the auto object's.
//   - After `x->`: the functions that can be called in x when x is
//     this_object() or a constant path, and nothing when it is not
//     known: nothing is guessed about what a variable holds.
//   - In the string that names a function for call_other, call_out or a
//     registrar of the project: the functions of the object it is
//     called in, known as for `->`.
//   - In the path of an #include or an inherit statement: the files and
//     directories there, on disk and in a profile's stubs.
//   - In an #ifdef and the like: the macros.
func (r *Resolver) Complete(off uint32) *Completion {
	f := &r.File
	if f.Tok == nil || f.Tree == nil || f.Tree.Sel == nil || f.Unit == nil || f.Snapshot == nil {
		return nil
	}
	src, toks, sig := f.Tok.Src, f.Tok.Tokens, f.Tree.Sel.Sig
	off = min(off, uint32(len(src)))
	c := &completer{r: r, off: off, out: &Completion{Start: off, End: off}, seen: map[string]uint8{}}

	// The token the cursor is in or directly behind.
	ti := f.Tok.TokenAt(off)
	if ti > 0 && toks[ti].Off == off {
		ti--
	}
	if !r.read(ti) {
		return nil
	}
	if tok := toks[ti]; tok.Off < off {
		switch {
		case tok.Kind == token.Directive:
			return c.directive(ti)
		case tok.Kind == token.LineComment, tok.Kind == token.IntLit, tok.Kind == token.FloatLit:
			return nil
		case tok.Kind == token.BlockComment || tok.Kind == token.CharLit:
			if off < tok.End || tok.Kind == token.BlockComment && !strings.HasSuffix(string(src[tok.Off:tok.End]), "*/") {
				return nil
			}
		case tok.Kind == token.StringLit:
			if off < tok.End {
				return c.str(ti)
			}
		case tok.Kind == token.Illegal:
			// A string that is not closed is one being written.
			if src[tok.Off] == '"' {
				return c.str(ti)
			}
			return nil
		case tok.Kind == token.Ident || tok.Kind.IsKeyword():
			c.out.Start, c.out.End = tok.Off, tok.End
		}
	}

	// The compiled tokens before the word, and the one after it.
	n := sort.Search(len(sig), func(i int) bool { return toks[sig[i]].End > c.out.Start })
	kind := func(i int) token.Kind {
		if i < 0 || i >= len(sig) {
			return token.EOF
		}
		return toks[sig[i]].Kind
	}
	next := sort.Search(len(sig), func(i int) bool { return toks[sig[i]].Off >= c.out.End })
	c.out.Called = kind(next) == token.LParen

	switch kind(n - 1) {
	case token.Arrow:
		c.out.Context = AfterArrow
		if first, ok := r.operand(n - 2); ok {
			c.members(r.target(first, n-2))
		}
		return c.done()
	case token.ColonColon:
		c.out.Context = AfterScope
		label := ""
		if kind(n-2) == token.Ident {
			label = string(f.Tok.Text(toks[sig[n-2]]))
		}
		c.inherited(label)
		return c.done()
	}
	c.out.Context = InCode
	switch sc := r.scope(c.out.Start); {
	case sc != nil:
		c.code(sc, kind(n-1))
	case initializer(n, kind):
		// The value of a global variable is an expression.
		c.names(nil)
		c.keywords(valueWords...)
	default:
		c.declarations(kind(n-1), kind(n-2))
	}
	return c.done()
}

// initializer reports whether the first n compiled tokens, which are
// outside any function's body, end in the value of a global variable:
// an '=' stands before them in the declaration, outside the brackets
// they close, and no ',' at its level begins another declarator.
func initializer(n int, kind func(int) token.Kind) bool {
	level, lowest, comma := 0, 0, false
	for i := n - 1; i >= 0; i-- {
		switch kind(i) {
		case token.RParen, token.RBracket:
			level++
		case token.RBrace:
			if level == lowest {
				return false // the end of a function
			}
			level++
		case token.LParen, token.LBracket, token.LBrace:
			if level--; level < lowest {
				lowest, comma = level, false
			}
		case token.Semicolon:
			return false
		case token.Comma:
			comma = comma || level == lowest
		case token.Assign:
			if level == lowest {
				return !comma
			}
		}
	}
	return false
}

// Comment returns the comment that stands above the declaration at byte
// offset off of the file at path (Comment), for an Item's Path and Off:
// the documentation of one item, read when it is asked for. It is ""
// when there is none.
func (r *Resolver) Comment(path string, off uint32) string {
	if r.Tok == nil || r.s == nil || path == "" {
		return ""
	}
	_, _, stub := profile.SplitLocation(path)
	_, doc := (&texts{r: r}).declared(path, off, stub)
	return doc
}

// completer collects the items of one answer.
type completer struct {
	r   *Resolver
	off uint32
	out *Completion
	// seen says what a name is taken by already: a function, something
	// else (a variable, a keyword), a macro, which is both.
	seen map[string]uint8
}

const (
	asFunction = 1 << iota
	asOther
)

// add adds an item unless its name is taken: a function and a variable
// can have one name, and a macro's name is the macro wherever it
// stands.
func (c *completer) add(it Item) {
	bits := uint8(asOther)
	switch it.Kind {
	case Function:
		bits = asFunction
	case Macro:
		bits = asFunction | asOther
	}
	if c.seen[it.Name]&bits != 0 {
		return
	}
	c.seen[it.Name] |= bits
	it.Name, it.Detail = utf8Text(it.Name), utf8Text(it.Detail)
	c.out.Items = append(c.out.Items, it)
}

// done orders the items and returns the answer.
func (c *completer) done() *Completion {
	out := c.out
	if out.Context == InCallback {
		// A name in a string is not called there.
		for i := range out.Items {
			out.Items[i].Call, out.Items[i].Args = false, false
		}
	}
	order := func(a, b Item) int { return cmp.Or(cmp.Compare(a.Rank, b.Rank), cmp.Compare(a.Name, b.Name)) }
	if len(out.Items) > MaxItems {
		// More than anybody is to be sent: those that match what is
		// written, and the nearest of them.
		m := fuzzy.New(string(c.r.Tok.Src[out.Start:c.off]))
		score := func(it Item) int { return m.Score(it.Name) }
		out.Items = slices.DeleteFunc(out.Items, func(it Item) bool { return score(it) == 0 })
		slices.SortFunc(out.Items, func(a, b Item) int { return cmp.Or(cmp.Compare(score(b), score(a)), order(a, b)) })
		out.Items, out.Incomplete = out.Items[:min(len(out.Items), MaxItems)], true
	}
	slices.SortFunc(out.Items, order)
	return out
}

// ----------------------------------------------------------------- code

// The words of the language, by where they can stand.
var (
	typeWords      = []string{"int", "float", "string", "object", "mapping", "mixed", "void"}
	specifierWords = []string{"private", "static", "atomic", "nomask", "varargs"}
	statementWords = []string{"if", "else", "for", "while", "do", "switch", "case", "default", "break", "continue", "return", "goto", "rlimits", "try", "catch"}
	valueWords     = []string{"nil", "new", "catch"}
)

func (c *completer) keywords(words ...string) {
	for _, w := range words {
		c.add(Item{Kind: Keyword, Name: w, Rank: 6})
	}
}

// types adds the types: the keywords, and "function" where the driver
// has function pointers.
func (c *completer) types() {
	c.keywords(typeWords...)
	if c.r.Tok.Dialect.Closures {
		c.keywords("function")
	}
}

// code adds what can be named in the body of a function, at a place in
// scope sc, after a token of kind prev.
func (c *completer) code(sc *syntax.Scope, prev token.Kind) {
	c.names(sc)

	// A statement begins after what ends one, opens or closes a block,
	// or closes the parentheses of an if or a while; anywhere else an
	// expression goes on.
	switch prev {
	case token.Semicolon, token.LBrace, token.RBrace, token.RParen, token.Colon, token.KwElse, token.KwDo, token.EOF:
		c.keywords(statementWords...)
		c.types()
	case token.LParen:
		c.types() // a cast
	case token.KwNew, token.LArrow:
		c.keywords("object")
	}
	c.keywords(valueWords...)
}

// names adds the names an expression can have at a place in scope sc
// (nil outside a function): the macros, the parameters and local
// variables in scope, the global variables, and the functions the
// object can call.
func (c *completer) names(sc *syntax.Scope) {
	r := c.r
	c.macros(func(*pp.Macro) bool { return true })
	for _, l := range sc.Visible(c.out.Start) {
		kind := Local
		if l.Param {
			kind = Parameter
		}
		o := r.origin(r.Tok.Path)
		o.Line = r.Tok.Pos(r.Tree.Off(l.Ident)).Line
		c.add(Item{Kind: kind, Name: l.Name, Detail: join(l.Type, l.Name), Origin: o})
	}
	names := map[string]bool{}
	for _, v := range r.u.Vars {
		names[v.Name] = true
	}
	for _, in := range r.u.Inherits {
		if in.Resolved {
			for name := range r.s.Chain(in.Path).Vars {
				names[name] = true
			}
		}
	}
	for name := range names {
		if defs := r.variable(name); len(defs) > 0 && defs[0].Var != nil {
			d := &defs[0]
			it := Item{Kind: Variable, Name: name, Detail: variableCode(d), Origin: r.origin(d.Path), Rank: c.rank(d)}
			it.Path, it.Off = d.Path, d.Start
			if it.Profile == "" {
				it.Line = d.Var.Line
			}
			c.add(it)
		}
	}
	c.callables(true)
}

// declarations adds what can be written where declarations stand: after
// `inherit` the macros that are a path, anywhere else the types, the
// specifiers and the macros that are one or the other.
func (c *completer) declarations(prev, before token.Kind) {
	if prev == token.KwInherit || before == token.KwInherit && (prev == token.Ident || prev == token.KwObject) {
		c.macros(func(m *pp.Macro) bool { return m.Class == pp.ClassString || m.Class == pp.ClassExpr })
		return
	}
	c.macros(func(m *pp.Macro) bool {
		return m.Class == pp.ClassType || m.Class == pp.ClassSpecifier || m.Class == pp.ClassEmpty || m.Class == pp.ClassFragment
	})
	c.types()
	c.keywords(specifierWords...)
	c.keywords("inherit")
}

// macros adds the macros in view that keep says to: those a file read
// defines, each with its #define, and those the project's configuration
// gives a class.
func (c *completer) macros(keep func(*pp.Macro) bool) {
	r := c.r
	for _, name := range r.Env.Names() {
		m := r.Env.Lookup(name)
		if m == nil || !keep(m) {
			continue
		}
		it := Item{Kind: Macro, Name: name, Detail: "#define " + name, Origin: Origin{Configured: true}, Rank: 3}
		if def := r.Env.Definition(name); def != nil {
			it.Detail, it.Origin = brief(define(def)), r.origin(def.Path)
			it.Call, it.Args = def.FuncLike, len(def.Params) > 0
			if it.Profile == "" {
				it.Path, it.Off = def.Path, def.Off
			}
		}
		c.add(it)
	}
}

// brief cuts a text that is long for a line of a list.
func brief(s string) string {
	if len(s) <= maxDetail {
		return s
	}
	cut := maxDetail
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + " ..."
}

// rank says how near a declaration is: the file's own, the auto
// object's, or one that is inherited.
func (c *completer) rank(d *Def) int {
	switch {
	case d.Auto:
		return 4
	case d.Path == c.r.Tok.Path:
		return 1
	}
	return 2
}

// function is the item of a function of the lib's.
func (c *completer) function(d *Def) Item {
	it := Item{Kind: Function, Name: d.Name, Origin: c.r.origin(d.Path), Call: true, Rank: c.rank(d)}
	it.Auto = d.Auto
	if d.Func != nil {
		it.Detail, it.Args = d.Func.Decl, d.Func.MaxArgs != 0
		it.Path, it.Off = d.Path, d.Start
		if it.Profile == "" {
			it.Line = d.Func.Line
		}
	}
	return it
}

// callables adds the functions the object's own code can call by name:
// the file's, those it inherits and may name, those the auto object
// gives it, and, with kfuns, the kfuns where a name is known to mean
// one (Resolver.kfun). They are what this_object()->name() reaches too,
// but for the kfuns.
func (c *completer) callables(kfuns bool) {
	r := c.r
	names := map[string]bool{}
	for _, fn := range r.u.Funcs {
		names[fn.Name] = true
	}
	for _, in := range r.u.Inherits {
		if in.Resolved {
			for name := range r.s.Chain(in.Path).Funcs {
				names[name] = true
			}
		}
	}
	for name := range names {
		if defs := r.function(name); len(defs) > 0 {
			c.add(c.function(&defs[0]))
		}
	}
	c.auto(r.u.LibPath, false)
	if kfuns {
		c.kfuns()
	}
}

// auto adds the functions the auto object gives the object at libPath,
// which stand behind a function of the same name the object has. They
// are there whatever the object inherits that the index cannot see: one
// of them may be redefined there, and is then still a function of that
// name. public says the call comes from outside: a static function is
// not reached then.
func (c *completer) auto(libPath string, public bool) {
	r := c.r
	if libPath == "" || r.s.AutoMember(libPath) {
		return
	}
	for name, fn := range r.s.AutoFuncs() {
		if fn.Private || public && fn.Static {
			continue
		}
		d := funcDef(name, fn)
		d.Object, d.Auto = r.s.LibPath(fn.Path), true
		c.add(c.function(&d))
	}
}

// kfuns adds the kernel functions that a call by name in the file's
// code is known to reach.
func (c *completer) kfuns() {
	for _, name := range c.r.kfuns().Names() {
		if kf := c.r.kfun(name); kf != nil {
			c.add(kfunItem(kf))
		}
	}
}

func kfunItem(kf *kfun.Func) Item {
	return Item{Kind: Function, Name: kf.Name, Detail: kf.Decl, Doc: kf.Doc, Origin: Origin{Kfun: true, Driver: kf.Origin},
		Call: true, Args: len(index.ParamSpans(kf.Decl)) > 0, Rank: 5}
}

// members adds the functions that a call bound when it runs reaches in
// the object target says: the object's own for this_object(), and for a
// constant path the functions of that object's chain that are neither
// private nor static, which is what a call from outside finds, and the
// auto object's that are so. Nothing for an object that is not known,
// or not there.
func (c *completer) members(target calls.Target, libPath string) {
	r := c.r
	switch target {
	case calls.TargetSelf:
		c.callables(false)
	case calls.TargetPath:
		if r.s.Unit(libPath) == nil {
			return
		}
		ch := r.s.Chain(libPath)
		for name, fn := range ch.Funcs {
			// The function of the name in the chain is the one, whether
			// it can be called or not: the auto object's is not.
			if fn.Private || fn.Static {
				c.seen[name] |= asFunction
				continue
			}
			d := funcDef(name, *fn)
			it := c.function(&d)
			if it.Rank = 2; fn.DefinedIn == libPath {
				it.Rank = 1
			}
			c.add(it)
		}
		c.auto(libPath, true)
	}
}

// inherited adds the functions that ::name() can name, or label::name()
// with a label: those of the inherited objects (of that label) that are
// not private there, and, without a label, the auto object's; in the
// auto object itself and what it is made of, the kfuns, which are what
// it has above it.
func (c *completer) inherited(label string) {
	r := c.r
	names := map[string]bool{}
	for _, in := range r.u.Inherits {
		if in.Resolved && (label == "" || in.Label == label) {
			for name := range r.s.Chain(in.Path).Funcs {
				names[name] = true
			}
		}
	}
	for name := range names {
		if defs := r.defs(name, r.inherited(name, label)); len(defs) > 0 {
			c.add(c.function(&defs[0]))
		}
	}
	if label != "" {
		return
	}
	c.auto(r.u.LibPath, false)
	if r.u.LibPath != "" && r.s.AutoMember(r.u.LibPath) {
		c.kfuns()
	}
}

// -------------------------------------------------------------- strings

// str answers for a cursor in the string at token ti: the path of an
// inherit statement, or the name of a function handed to call_other,
// call_out or a registrar of the project. Any other string has nothing.
func (c *completer) str(ti int) *Completion {
	r := c.r
	toks, sig := r.Tok.Tokens, r.Tree.Sel.Sig
	tok := toks[ti]
	si, compiled := slices.BinarySearch(sig, ti)
	if !compiled {
		return nil
	}
	kind := func(i int) token.Kind {
		if i < 0 {
			return token.EOF
		}
		return toks[sig[i]].Kind
	}
	typed := string(r.Tok.Src[tok.Off+1 : c.off])
	if strings.ContainsAny(typed, "\"\n\\") {
		return nil
	}
	// inherit [label] [object] "path
	for i := si - 1; i >= si-3; i-- {
		if k := kind(i); k == token.KwInherit {
			return c.objects(typed)
		} else if k != token.Ident && k != token.KwObject {
			break
		}
	}
	body := r.bodyAround(ti)
	if body < 0 || typed != "" && !token.IsIdent(typed) {
		return nil
	}
	open, commas, ok := r.callAround(si, body)
	if !ok || kind(open-2) == token.Arrow || kind(open-2) == token.ColonColon {
		return nil
	}
	// The first form of the function called that takes a function's
	// name at this argument says whose function it is.
	arg := len(commas)
	for _, form := range r.registry().Forms(string(r.Tok.Text(toks[sig[open-1]]))) {
		if form.Function != arg || form.Object != nil && *form.Object >= arg {
			continue
		}
		c.out.Context, c.out.Start, c.out.End = InCallback, tok.Off+1, c.off
		if form.Object == nil {
			c.callables(false)
			return c.done()
		}
		first := open + 1
		if *form.Object > 0 {
			first = commas[*form.Object-1] + 1
		}
		if last := commas[*form.Object] - 1; first <= last {
			c.members(r.target(first, last))
		}
		return c.done()
	}
	return nil
}

// split parts a path that is being written into the directory that is
// complete ("" for none, "/" for the root) and what follows its last
// '/'.
func split(typed string) (dir, last string) {
	i := strings.LastIndexByte(typed, '/')
	switch {
	case i < 0:
		return "", typed
	case i == 0:
		return "/", typed[1:]
	}
	return typed[:i], typed[i+1:]
}

// objects answers in the path of an inherit statement, of which typed
// is written: the objects and directories of the directory it has so
// far.
func (c *completer) objects(typed string) *Completion {
	dir, last := split(typed)
	c.out.Context, c.out.Start, c.out.End = InInherit, c.off-uint32(len(last)), c.off
	for _, e := range c.r.s.ObjectEntries(dir, c.r.u.LibPath) {
		it := Item{Kind: Object, Name: e.Name}
		if e.Dir {
			it.Kind, it.Rank = Directory, 1
		}
		c.out.Items = append(c.out.Items, it)
	}
	return c.done()
}

// ----------------------------------------------------------- directives

// directive answers for a cursor in the directive at token ti: the
// files and directories an #include can name, in the directory written
// so far, and the macros in an #ifdef, #ifndef, #if, #elif or #undef.
func (c *completer) directive(ti int) *Completion {
	r := c.r
	tok := r.Tok.Tokens[ti]
	text := r.Tok.Text(tok)
	name, _ := pp.SplitDirective(text)
	arg, at := pp.ArgOffset(text), int(c.off-tok.Off)
	switch name {
	case "include":
		if at <= arg {
			return nil
		}
		open := text[arg]
		if open != '"' && open != '<' {
			return nil
		}
		typed := string(text[arg+1 : at])
		if strings.ContainsAny(typed, "\">\n\\") {
			return nil // behind the path
		}
		dir, last := split(typed)
		c.out.Context, c.out.Start = InInclude, c.off-uint32(len(last))
		for _, e := range r.s.IncludeEntries(r.Tok.Path, dir, open == '<') {
			it := Item{Kind: Include, Name: e.Name, Rank: 2}
			switch {
			case e.Dir:
				it.Kind, it.Rank = Directory, 1
			case path.Ext(e.Name) == ".h":
				it.Rank = 0
			}
			c.out.Items = append(c.out.Items, it)
		}
		return c.done()
	case "if", "ifdef", "ifndef", "elif", "undef":
		if at < arg {
			return nil
		}
		start := at
		for start > arg && token.IsIdentByte(text[start-1]) {
			start--
		}
		c.out.Context, c.out.Start = InDirective, tok.Off+uint32(start)
		c.macros(func(*pp.Macro) bool { return true })
		for i := range c.out.Items {
			c.out.Items[i].Call, c.out.Items[i].Args = false, false
		}
		return c.done()
	}
	return nil
}
