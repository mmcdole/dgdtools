// Package rename plans the renaming of a name: given a place in a file
// and a new name, every occurrence of what is named there, the edits that
// rename it, or the reason it must not be renamed as asked. Nothing in it
// writes a file; an editor (textDocument/rename) and the command line
// (dgdtools rename) show or apply the plan.
//
// The rule is the linter's: prove it or say so. Every place the old name
// stands is classified (Occurrence): Certain, the symbol; Excluded,
// provably something else; Possible, it may be and nobody can tell. The
// Certain ones are the edits; the Possible ones are edits to confirm
// (Plan.PossibleEdits). A new name that would change what another name
// means (a declaration it would hide or be hidden by, a macro, a
// function it would take the place of) is refused.
//
// What one file decides is renamed in the file: a local variable or a
// parameter (a catch variable among them), a private function and a
// private global variable of the file's object, an inherit label, a macro
// the file defines for itself. There, code the tools cannot read (a
// branch of #if that is not compiled, a statement that does not parse)
// that has the old name makes the rename a refusal.
//
// Everything else is renamed in every file of the lib that names it,
// from the reverse index of the whole-lib scan (across.go): a function
// that is not private with its redefinitions in the objects that inherit
// it and those it inherits, which are one function by the driver's
// rules; a global variable that is not private, in its object and those
// that inherit it; a header's macro, in every file that includes it; what
// a file other files include declares. A call on an object nobody can
// tell, a callback whose object is unknown, a name in a macro's body or
// in code that cannot be read is Possible. A variable save_object writes
// is refused unless File.Force; a declaration in a profile's stub is
// refused.
//
// DGD does not call a private function by name: call_other, call_out
// and every callback a lib's registrar makes look the name up in the
// object's table of functions, which has no private ones (DGD's
// Control::makeSymbols; Frame::call). The string "fn" of
// call_out("fn") and this_object()->fn() never reach a private fn, so
// they are not occurrences of it: they are Excluded, left as they are,
// and reported as a warning, since such a call fails today.
package rename

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/mmcdole/dgdtools/pkg/calls"
	"github.com/mmcdole/dgdtools/pkg/index"
	"github.com/mmcdole/dgdtools/pkg/kfun"
	"github.com/mmcdole/dgdtools/pkg/lexer"
	"github.com/mmcdole/dgdtools/pkg/pp"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/resolve"
	"github.com/mmcdole/dgdtools/pkg/syntax"
	"github.com/mmcdole/dgdtools/pkg/token"
)

// File is the file a rename is asked in, as a lint has it.
type File struct {
	// Resolver answers what the places of the file name
	// (lint.Document's).
	Resolver *resolve.Resolver
	// AutosaveMarkers are the names whose mention says that an object
	// saves itself with save_object (conventions.autosave_markers).
	AutosaveMarkers []string
	// Create is the function the driver calls by name to initialize an
	// object (the configuration's create; "" is "create").
	Create string
	// DriverObject is the lib path of the driver object, whose functions
	// the driver calls by name (the configuration's driver_object; "":
	// not known, and then any object may be it).
	DriverObject string
	// Force overrules a refusal that a person may overrule
	// (Refusal.Forcible: a variable save_object writes), which becomes a
	// warning of the plan.
	Force bool
}

// Symbol is what a rename is about: a declaration.
type Symbol struct {
	// Kind is resolve.Local, Parameter, Function, Variable, Macro or
	// Label, and Name its name.
	Kind resolve.Kind
	Name string
	// Def is the declaration: the file, the bytes of the declared name.
	Def resolve.Def
}

// Target is what can be renamed at a place: the symbol, and the bytes
// of the name there, which an editor selects for the new name.
type Target struct {
	Symbol
	Start, End uint32
	// Across says other files name the symbol too: the rename searches
	// the whole lib.
	Across bool

	across *across
}

// Class says what an occurrence of the old name is.
type Class uint8

const (
	// Certain: the symbol.
	Certain Class = iota + 1
	// Possible: could be the symbol, and nobody can say.
	Possible
	// Excluded: provably something else.
	Excluded
)

func (c Class) String() string {
	switch c {
	case Certain:
		return "certain"
	case Possible:
		return "possible"
	case Excluded:
		return "excluded"
	}
	return "unknown"
}

// Occurrence is a place the old name stands at.
type Occurrence struct {
	Path       string
	Start, End uint32
	// Line and Col are where Start is, 1-based, Col counting bytes; 0
	// when not known (the file's text says).
	Line, Col uint32
	Class     Class
	// Decl says the place declares the symbol.
	Decl bool
	// Reason says why an occurrence is Possible or Excluded.
	Reason string
}

// Edit replaces the bytes [Start, End) of the file at Path.
type Edit struct {
	Path       string
	Start, End uint32
	NewText    string
}

// Plan is a rename as it is to be made.
type Plan struct {
	Symbol Symbol
	New    string
	// Occurrences are the places of the old name, in the order of their
	// files and of the files: the Certain ones are renamed.
	Occurrences []Occurrence
	// Edits are those of the Certain occurrences, in the same order.
	Edits []Edit
	// PossibleEdits are those of the Possible occurrences, which are made
	// only when the person renaming asks for them: a call on an object
	// nobody can tell, a name in a macro's body, code that does not
	// parse. A rename is complete when there are none.
	PossibleEdits []Edit
	// Warnings are what the person renaming should know that does not
	// stop the rename.
	Warnings []string
	// Dynamic are the calls of the lib that name a function with a name
	// made when they run, of a function's rename: those that may call
	// the renamed function (Dynamic.Matches) first.
	Dynamic []Dynamic
	// Files is how many files were searched; Partial says the lib is a
	// checkout of part of a mudlib, whose rest was not.
	Files   int
	Partial bool
	// Moves are the files moved, of a plan of moved files (rename.Files):
	// a directory's files each.
	Moves []Move
}

// Count returns how many occurrences of the class the plan has.
func (p *Plan) Count(c Class) int {
	n := 0
	for _, o := range p.Occurrences {
		if o.Class == c {
			n++
		}
	}
	return n
}

// Refusal is the reason a name cannot be renamed, or not to the name
// asked for.
type Refusal struct {
	Reason string
	// Forcible says File.Force overrules it: the rename is safe as far as
	// the code goes, and what it breaks is data (a variable save_object
	// writes).
	Forcible bool
}

func (r *Refusal) Error() string { return r.Reason }

func refuse(format string, args ...any) error {
	return &Refusal{Reason: fmt.Sprintf(format, args...)}
}

// Prepare says what can be renamed at byte offset off of the file: the
// symbol and the name's place, or a *Refusal saying why it cannot be. It
// is nil, nil where nothing is named (a keyword, a comment, a number).
//
// A symbol that other files could name too (anything but a local, a
// parameter or a label) needs the whole-lib scan, to tell that no other
// file includes this one; Prepare runs it, for as long as ctx allows.
func Prepare(ctx context.Context, f File, off uint32) (*Target, error) {
	rn := newRenamer(f)
	if rn == nil {
		return nil, nil
	}
	return rn.prepare(ctx, off)
}

// Rename plans the renaming of what is named at byte offset off of the
// file to newName: Prepare's symbol, every occurrence of it and the
// edits; or a *Refusal. A new name that is the old one is a plan without
// edits.
func Rename(ctx context.Context, f File, off uint32, newName string) (*Plan, error) {
	rn := newRenamer(f)
	if rn == nil {
		return nil, refuse("nothing here can be renamed")
	}
	t, err := rn.prepare(ctx, off)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, refuse("nothing here can be renamed")
	}
	plan := &Plan{Symbol: t.Symbol, New: newName, Files: 1}
	if newName == t.Name {
		return plan, nil
	}
	switch {
	case t.across != nil:
		err = rn.planAcross(ctx, plan, t.across)
	case t.Kind == resolve.Macro:
		if err = rn.valid(t.Symbol, newName); err == nil {
			err = rn.planMacro(plan)
		}
	default:
		if err = rn.valid(t.Symbol, newName); err == nil && t.Kind == resolve.Function {
			err = rn.calledByName(newName, rn.unit != nil && rn.unit.LibPath == rn.driver)
		}
		if err == nil {
			err = rn.planNames(plan)
		}
	}
	if err != nil {
		return nil, err
	}
	rn.fillLines(plan)
	for _, o := range plan.Occurrences {
		e := Edit{Path: o.Path, Start: o.Start, End: o.End, NewText: newName}
		switch o.Class {
		case Certain:
			plan.Edits = append(plan.Edits, e)
		case Possible:
			plan.PossibleEdits = append(plan.PossibleEdits, e)
		}
	}
	return plan, nil
}

// fillLines sets the lines and columns of the occurrences that have
// none: from the file's tokens, or its text.
func (rn *renamer) fillLines(plan *Plan) {
	var src []byte
	srcOf := ""
	for i := range plan.Occurrences {
		o := &plan.Occurrences[i]
		if o.Line != 0 {
			continue
		}
		if o.Path == rn.tok.Path {
			pos := rn.tok.Pos(o.Start)
			o.Line, o.Col = uint32(pos.Line), uint32(pos.Col)
			continue
		}
		if srcOf != o.Path {
			src, _ = rn.snap.ReadFile(o.Path)
			srcOf = o.Path
		}
		if int(o.Start) <= len(src) {
			before := src[:o.Start]
			o.Line = uint32(1 + bytes.Count(before, []byte("\n")))
			o.Col = uint32(int(o.Start) - (bytes.LastIndexByte(before, '\n') + 1) + 1)
		}
	}
}

// renamer is a rename in one file.
// badSpan is code that does not parse, tokens [first, last], and why
// what it names cannot be told.
type badSpan struct {
	first, last int
	why         string
}

const notParsed = "in code that does not parse"

type renamer struct {
	r       *resolve.Resolver
	tok     *token.File
	tree    *syntax.File
	env     *pp.Env
	unit    *index.Unit
	snap    *index.Snapshot
	kfuns   *kfun.Table
	markers []string
	create  string
	driver  string
	force   bool
	scopes  []*syntax.Scope
	// bad are the spans of the code that does not parse: what it names
	// cannot be told.
	bad []badSpan
}

func newRenamer(f File) *renamer {
	r := f.Resolver
	if r == nil || r.Tok == nil || r.Tree == nil || r.Tree.Sel == nil || r.Unit == nil || r.Snapshot == nil {
		return nil
	}
	rn := &renamer{r: r, tok: r.Tok, tree: r.Tree, env: r.Env, unit: r.Unit, snap: r.Snapshot,
		kfuns: r.Kfuns, markers: f.AutosaveMarkers, create: f.Create, driver: f.DriverObject, force: f.Force}
	if rn.create == "" {
		rn.create = "create"
	}
	if rn.kfuns == nil {
		rn.kfuns = kfun.DGD()
	}
	rn.scopes = r.Tree.Scopes()
	add := func(n syntax.Node, why string) {
		first, last := n.Span()
		rn.bad = append(rn.bad, badSpan{first, last, why})
	}
	syntax.Inspect(r.Tree, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.BadDecl:
			add(n, rn.opaque(n, n.Opaque))
		case *syntax.BadExpr:
			add(n, notParsed)
		case *syntax.BadStmt:
			if !n.Missing {
				add(n, rn.opaque(n, n.Opaque))
			}
			if n.Resumed != nil {
				// Read after a skip, and evidence of nothing.
				add(n.Resumed, notParsed)
			}
		case *syntax.CallExpr:
			if n.Opaque && n.Fun != nil {
				// A macro's arguments that are no expressions.
				why := "in the arguments of the macro " + n.Fun.Name + ", which are not read as code"
				if m := rn.env.Lookup(n.Fun.Name); m != nil && strings.Contains(m.Body, "#") {
					why = "an argument of the macro " + n.Fun.Name + ", which makes a string of its arguments or pastes them (# or ##)"
				}
				add(n, why)
			}
		}
		return true
	})
	return rn
}

// opaque says why what code that does not parse names cannot be told:
// of code a macro made opaque, which macro, and whether it makes strings
// of its arguments.
func (rn *renamer) opaque(n syntax.Node, opaque bool) string {
	if !opaque {
		return notParsed
	}
	first, last := n.Span()
	toks := rn.tok.Tokens
	for ti := first; ti <= last && ti < len(toks); ti++ {
		if toks[ti].Kind != token.Ident {
			continue
		}
		name := rn.text(ti)
		m := rn.env.Lookup(name)
		switch {
		case m == nil:
		case m.FuncLike && strings.Contains(m.Body, "#"):
			return "an argument of the macro " + name + ", which makes a string of its arguments or pastes them (# or ##)"
		case m.Class == pp.ClassStatement || m.Class == pp.ClassFragment:
			return "in code the macro " + name + " makes unreadable"
		}
	}
	return notParsed
}

// ---------------------------------------------------------------- the place

// tokenAt returns the index of the token at off: a name that ends at off
// is at it, as an editor's cursor at the end of a word is.
func (rn *renamer) tokenAt(off uint32) int {
	toks := rn.tok.Tokens
	ti := rn.tok.TokenAt(off)
	if toks[ti].Kind != token.Ident && ti > 0 && toks[ti].Off == off && toks[ti-1].Kind == token.Ident {
		ti--
	}
	return ti
}

func (rn *renamer) compiled(ti int) bool {
	_, ok := slices.BinarySearch(rn.tree.Sel.Sig, ti)
	return ok
}

// unparsed says why the token at index ti is in code that does not
// parse, the innermost first; "" when it is not.
func (rn *renamer) unparsed(ti int) string {
	why := ""
	for _, b := range rn.bad {
		if b.first <= ti && ti <= b.last {
			why = b.why
		}
	}
	return why
}

func (rn *renamer) text(ti int) string { return string(rn.tok.Text(rn.tok.Tokens[ti])) }

func (rn *renamer) line(off uint32) int { return rn.tok.Pos(off).Line }

// where names a place of a declaration for a message: "line 12" in the
// file, else the file's lib path or its path and the line.
func (rn *renamer) where(d resolve.Def) string {
	if d.Path == rn.tok.Path {
		return fmt.Sprintf("line %d", rn.line(d.Start))
	}
	if _, file, ok := profile.SplitLocation(d.Path); ok {
		return file + " (the mudlib profile)"
	}
	if lf := rn.snap.LibFile(d.Path); lf != "" {
		return lf
	}
	return d.Path
}

func (rn *renamer) libName(p string) string {
	if lf := rn.snap.LibFile(p); lf != "" {
		return lf
	}
	return p
}

// prepare is Prepare.
func (rn *renamer) prepare(ctx context.Context, off uint32) (*Target, error) {
	ti := rn.tokenAt(off)
	ref := rn.r.At(off)
	if ref == nil {
		if rn.tok.Tokens[ti].Kind != token.Ident {
			return nil, nil
		}
		name := rn.text(ti)
		switch {
		case token.Keywords[name] != 0:
			return nil, nil
		case !rn.compiled(ti):
			return nil, refuse("'%s' is in code that is not compiled here (a branch of #if that is not taken): what it names cannot be told", name)
		case rn.unparsed(ti) != "":
			return nil, refuse("'%s' is %s: what it names cannot be told", name, rn.unparsed(ti))
		}
		return nil, refuse("'%s' names nothing the tools know of here", name)
	}
	t := &Target{Symbol: Symbol{Kind: ref.Kind, Name: ref.Name}, Start: ref.Start, End: ref.End}
	acrossFiles := false
	var err error
	switch ref.Kind {
	case resolve.Include, resolve.Object:
		return nil, refuse("'%s' names a file: rename the file itself (in the editor, or with dgdtools rename FILE NEW), and the paths that name it are changed with it", ref.Name)
	case resolve.Local, resolve.Parameter:
		t.Def = ref.Defs[0]
		return t, nil
	case resolve.Label:
		t.Def = ref.Defs[0]
		by, err := rn.includedBy(ctx)
		if err != nil {
			return nil, err
		}
		if by != "" {
			return nil, refuse("this file is included by %s, which has its inherit statements too: an inherit label of such a file is not renamed", by)
		}
		return t, nil
	case resolve.Function:
		acrossFiles, err = rn.function(ref)
	case resolve.Variable:
		acrossFiles, err = rn.variable(ref)
	case resolve.Macro:
		acrossFiles, err = rn.macro(ctx, ref)
	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Def = ref.Defs[0]
	if !acrossFiles {
		// What a file another includes declares is theirs too.
		by, err := rn.includedBy(ctx)
		if err != nil {
			return nil, err
		}
		acrossFiles = by != ""
	}
	if acrossFiles {
		a, err := rn.prepareAcross(ctx, t)
		if err != nil {
			return nil, err
		}
		t.Across, t.across = true, a
		return t, nil
	}
	if ref.Kind == resolve.Variable && !t.Def.Var.Static {
		if err := rn.savedIn(ctx, rn.unit.LibPath, ref.Name, true); err != nil {
			var no *Refusal
			if !rn.force || !errors.As(err, &no) || !no.Forcible {
				return nil, err
			}
			// Planned as a rename across files, which warns of it.
			a, err := rn.prepareAcross(ctx, t)
			if err != nil {
				return nil, err
			}
			a.forced = no.Reason
			t.Across, t.across = true, a
		}
	}
	return t, nil
}

// stub reports whether a declaration is in a profile's stub.
func stub(d resolve.Def) bool {
	_, _, ok := profile.SplitLocation(d.Path)
	return ok
}

// function refuses a function that cannot be renamed, and says whether
// its rename reaches other files: all but a private function of the
// file's own do.
func (rn *renamer) function(ref *resolve.Ref) (bool, error) {
	name := ref.Name
	switch {
	case len(ref.Defs) == 0 && ref.Call != nil && ref.Call.Target == calls.TargetUnknown:
		return false, refuse("'%s' is called in an object that cannot be told: what it names is not known", name)
	case len(ref.Defs) == 0 && rn.kfuns.Lookup(name) != nil:
		return false, refuse("'%s' is a kfun, a function of the driver", name)
	case len(ref.Defs) == 0:
		return false, refuse("no file the tools see declares a function '%s'", name)
	case len(ref.Defs) > 1:
		return false, refuse("'%s' is ambiguous here: more than one inherited object has it", name)
	}
	d := ref.Defs[0]
	fi := d.Func
	switch {
	case stub(d):
		return false, refuse("'%s' is defined in the mudlib profile, not in your checkout", name)
	case fi == nil:
		return false, refuse("nothing is known of the function '%s'", name)
	case fi.Private && ref.Call != nil:
		return false, refuse("DGD does not call a private function by name (%s): this is not a call of '%s'; rename it where it is declared or called", via(ref), name)
	case fi.Private:
		return d.Object != rn.unit.LibPath || d.Path != rn.tok.Path, nil
	case name == rn.create:
		return false, refuse("'%s' is the function the driver calls by name to initialize every object (create in the configuration)", name)
	case rn.kfuns.Lookup(name) != nil:
		return false, refuse("'%s' is a kfun too: the function takes the kfun's place in the objects that have it, and a call the rename cannot see would reach the driver's once renamed", name)
	}
	return true, nil
}

// via says how a late-bound call is made, for a message.
func via(ref *resolve.Ref) string {
	if ref.Call.Via == "->" {
		return "->"
	}
	return ref.Call.Via + "()"
}

// variable refuses a global variable that cannot be renamed, and says
// whether its rename reaches other files: all but a private variable of
// the file's own do.
func (rn *renamer) variable(ref *resolve.Ref) (bool, error) {
	name := ref.Name
	switch {
	case len(ref.Defs) == 0:
		return false, refuse("no file the tools see declares a variable '%s'", name)
	case len(ref.Defs) > 1:
		return false, refuse("'%s' is ambiguous here: more than one inherited object has it", name)
	}
	d := ref.Defs[0]
	switch {
	case stub(d):
		return false, refuse("'%s' is declared in the mudlib profile, not in your checkout", name)
	case d.Var == nil:
		return false, refuse("nothing is known of the variable '%s'", name)
	}
	return !d.Var.Private || d.Object != rn.unit.LibPath || d.Path != rn.tok.Path, nil
}

// macro refuses a macro that cannot be renamed, and says whether its
// rename reaches other files: a header's does, and a macro of a file
// another includes; a macro the file defines for itself is renamed in the
// file, when nothing outside it knows the name.
func (rn *renamer) macro(ctx context.Context, ref *resolve.Ref) (bool, error) {
	name := ref.Name
	if len(ref.Defs) == 0 {
		return false, refuse("'%s' is defined by the driver or the project's configuration, not in a file", name)
	}
	d := ref.Defs[0]
	switch {
	case stub(d):
		return false, refuse("'%s' is defined in the mudlib profile, not in your checkout", name)
	case d.Path != rn.tok.Path:
		return true, nil
	}
	if by, err := rn.includedBy(ctx); err != nil || by != "" {
		return by != "", err
	}
	if rn.env.Outside(name) {
		return false, refuse("'%s' is defined outside this file too (by a header it includes, the driver or the project's configuration)", name)
	}
	if h := rn.mentionedInHeaders(name); h != "" {
		return false, refuse("'%s' is used in %s, which this file includes, where it is no macro of this file's", name, h)
	}
	if what := rn.outsideName(name); what != "" {
		// The name means something else where the macro is not defined.
		if own := rn.env.Own(); own != nil && own.Undefs[name] {
			return false, refuse("'%s' is #undef'd in this file, and is %s where it is not a macro", name, what)
		}
		first := ^uint32(0)
		for _, m := range rn.ownDefines(name) {
			first = min(first, m)
		}
		for ti, tok := range rn.tok.Tokens {
			if tok.Kind == token.Ident && tok.Off < first && rn.text(ti) == name {
				return false, refuse("'%s' at line %d, before its #define, is %s", name, rn.line(tok.Off), what)
			}
		}
	}
	return false, nil
}

// outsideName says what else name is in the file's object, for a
// message: a function or a global variable it inherits, the auto
// object's function, a kfun; "" when nothing.
func (rn *renamer) outsideName(name string) string {
	lp := rn.unit.LibPath
	if lp != "" {
		if fn, ok := rn.snap.ChainFunc(lp, name); ok && fn.DefinedIn != lp && !fn.Private {
			return "a function inherited from " + fn.DefinedIn
		}
		if v, ok := rn.snap.ChainVar(lp, name); ok && v.DefinedIn != lp && !v.Private {
			return "a global variable inherited from " + v.DefinedIn
		}
	}
	if _, ok := rn.snap.AutoFunc(name); ok {
		return "a function of the auto object"
	}
	if rn.kfuns.Lookup(name) != nil {
		return "a kfun"
	}
	return ""
}

// ownDefines returns where the file's #defines of name have the name.
func (rn *renamer) ownDefines(name string) []uint32 {
	var out []uint32
	for _, w := range rn.directiveWords() {
		if w.name == name && w.define {
			out = append(out, w.off)
		}
	}
	return out
}

// includedBy returns the first file of the lib that includes this one,
// "" when none does.
func (rn *renamer) includedBy(ctx context.Context) (string, error) {
	includers, ok := rn.snap.Includers(ctx, rn.tok.Path)
	if !ok {
		return "", rn.unknown("whether another file includes this one")
	}
	if len(includers) > 0 {
		return rn.libName(includers[0]), nil
	}
	return "", nil
}

// unknown refuses for want of an answer from the whole lib: the scan is
// not complete, or files changed since the question was asked.
func (rn *renamer) unknown(what string) error {
	if !rn.snap.Current() {
		return refuse("files changed while the rename was being planned, so it cannot be told %s: try again", what)
	}
	return refuse("the mudlib is not indexed in full yet, so it cannot be told %s: try again in a moment", what)
}

// savedIn refuses a global variable of the object at lp that save_object
// writes: one of an object that saves itself, or of an object that
// inherits it, or is made of programs one of which does. The object saves
// itself when one of its programs names save_object, restore_object or
// one of the conventions' autosave markers (with own, the file asked
// about is read for those, and for a macro of the file that expands to
// one); the auto object is taken to wrap them, not to call them on its
// own. The refusal is Forcible.
func (rn *renamer) savedIn(ctx context.Context, lp, name string, own bool) error {
	names := map[string]bool{"save_object": true, "restore_object": true}
	for _, m := range rn.markers {
		names[m] = true
	}
	if own && rn.env != nil {
		for _, m := range rn.env.Names() {
			if names[m] {
				continue
			}
			exp := map[string]bool{}
			rn.expansion(m, map[string]bool{}, exp)
			for n := range exp {
				if names[n] {
					names[m] = true
					break
				}
			}
		}
	}
	refuseSaved := func(who string) error {
		return &Refusal{Forcible: true, Reason: fmt.Sprintf("'%s' is written by save_object to the save files under its name (%s): renamed, restore_object no longer finds the saved value", name, who)}
	}
	if own {
		for _, ti := range rn.tree.Sel.Sig {
			if tok := rn.tok.Tokens[ti]; tok.Kind == token.Ident && names[rn.text(ti)] {
				return refuseSaved(fmt.Sprintf("this file names %s at line %d", rn.text(ti), rn.line(tok.Off)))
			}
		}
	}
	if lp == "" {
		return nil
	}
	if rn.snap.Partial() && rn.snap.Shared(rn.snap.LibFile(rn.snap.ObjectFile(lp))) {
		return &Refusal{Forcible: true, Reason: fmt.Sprintf("'%s' may be written by save_object: objects of the mudlib outside this checkout inherit %s, and cannot be seen from here", name, lp)}
	}
	users, ok := rn.snap.Users(ctx, lp)
	if !ok {
		return rn.unknown(fmt.Sprintf("whether an object that inherits this one saves '%s'", name))
	}
	programs := map[string]bool{lp: true}
	rn.ancestors(lp, programs)
	for _, u := range users {
		programs[u] = true
		rn.ancestors(u, programs)
	}
	if own {
		delete(programs, lp)
	}
	for _, p := range slices.Sorted(maps.Keys(programs)) {
		if ctx.Err() != nil {
			return refuse("cancelled before it could be told whether an object that inherits this one saves '%s'", name)
		}
		if rn.snap.AutoMember(p) {
			continue
		}
		u := rn.snap.Unit(p)
		if u == nil {
			continue
		}
		for _, n := range slices.Sorted(maps.Keys(names)) {
			if len(u.Uses(n)) > 0 {
				return refuseSaved(fmt.Sprintf("%s names %s", p, n))
			}
		}
	}
	return nil
}

// expansion adds to out the identifiers the macro called name expands
// to, through the macros of its body, but for its parameters.
func (rn *renamer) expansion(name string, seen, out map[string]bool) {
	if rn.env == nil || seen[name] {
		return
	}
	seen[name] = true
	m := rn.env.Lookup(name)
	if m == nil {
		return
	}
	for _, t := range m.Toks {
		if t.Kind != token.Ident || slices.Contains(m.Params, t.Text) {
			continue
		}
		out[t.Text] = true
		rn.expansion(t.Text, seen, out)
	}
}

// ---------------------------------------------------------------- headers

// mentionedInHeaders returns the first of the files the file includes
// (directly or not, and the header read before it) that has name as an
// identifier, in its code or its directives; "" when none has.
func (rn *renamer) mentionedInHeaders(name string) string {
	if len(rn.unit.Deps) < 2 {
		return ""
	}
	for _, p := range rn.unit.Deps[1:] {
		src, err := rn.snap.ReadFile(p)
		if err != nil {
			continue
		}
		if !strings.Contains(string(src), name) {
			continue
		}
		f := lexer.Lex(p, src, rn.tok.Dialect)
		for _, tok := range f.Tokens {
			switch tok.Kind {
			case token.Ident:
				if string(f.Text(tok)) == name {
					return rn.libName(p)
				}
			case token.Directive:
				text := f.Text(tok)
				arg := pp.ArgOffset(text)
				for _, w := range lexer.Lex("", text[arg:], rn.tok.Dialect).Tokens {
					if w.Kind == token.Ident && string(text[arg:][w.Off:w.End]) == name {
						return rn.libName(p)
					}
				}
			}
		}
	}
	return ""
}

// word is an identifier in a directive of the file that names a macro
// or may: in #define, #undef, #if, #ifdef, #ifndef and #elif, read or
// not.
type word struct {
	name   string
	off    uint32
	define bool // the name a #define defines
	tok    int  // the directive's token
}

func (rn *renamer) directiveWords() []word {
	var out []word
	for ti, tok := range rn.tok.Tokens {
		if tok.Kind != token.Directive {
			continue
		}
		text := rn.tok.Text(tok)
		dir, _ := pp.SplitDirective(text)
		switch dir {
		case "define", "undef", "if", "ifdef", "ifndef", "elif":
		default:
			continue
		}
		arg := pp.ArgOffset(text)
		first := true
		for _, w := range lexer.Lex("", text[arg:], rn.tok.Dialect).Tokens {
			if w.Kind.IsTrivia() {
				continue
			}
			if w.Kind == token.Ident {
				out = append(out, word{name: string(text[arg:][w.Off:w.End]), off: tok.Off + uint32(arg) + w.Off,
					define: first && dir == "define", tok: ti})
			}
			first = false
		}
	}
	return out
}
