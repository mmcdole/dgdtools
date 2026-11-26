package rename

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/mmcdole/dgdtools/internal/par"
	"github.com/mmcdole/dgdtools/pkg/index"
	"github.com/mmcdole/dgdtools/pkg/lexer"
	"github.com/mmcdole/dgdtools/pkg/pp"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/resolve"
	"github.com/mmcdole/dgdtools/pkg/token"
)

// The rename of a symbol other files name too: a function that is not
// private (with the functions of the objects that inherit it or that it
// inherits which redefine it: one function by the driver's rules), a
// private function of a file other files include, a global variable that
// is not private or is declared in a file others include, and the macro
// of a header. Its occurrences are found in the reverse index (the Uses
// of the old name in every file of the lib), each looked up where it is
// (resolve.Resolver.Lookup): the symbol, certain; something else,
// excluded; a call on an object nobody can tell, a callback whose object
// is unknown, a name in a macro's body, possible. The files an edit lands
// in are read again for what the index does not keep (code that is not
// compiled or does not parse, the new name where it would change what a
// name means).

// declKey is a declaration as a Lookup has it: its file and the byte of
// its name. A function a header declares is one declaration however many
// objects include the header: its text is in one place.
type declKey struct {
	path string
	off  uint32
}

func defKey(d resolve.Def) declKey { return declKey{d.Path, d.Start} }
func locKey(l index.Loc) declKey   { return declKey{l.Path, l.Off} }

// across is what a rename across files is about, as Prepare decides it.
type across struct {
	// want are the declarations renamed: of a function, its family.
	want map[declKey]bool
	// objects are the lib paths of the objects that have one of them as
	// their own (or from a file they include as code).
	objects map[string]bool
	// private: a private function, which no call made by name reaches.
	private bool
	// auto: the auto object's function is among them, and so every
	// object that has one of the name.
	auto bool
	// forced is the refusal File.Force overruled, "" when none.
	forced string
}

// isHeader reports whether the file at p is read only as part of
// others: not an object.
func isHeader(p string) bool { return filepath.Ext(p) == ".h" }

// objectUnits returns the units of the objects among units.
func objectUnits(units []*index.Unit) []*index.Unit {
	var out []*index.Unit
	for _, u := range units {
		if u.LibPath != "" && !isHeader(u.Path) && !u.Stub {
			out = append(out, u)
		}
	}
	return out
}

// useUnits returns the units a search across files looks at: those of
// the whole-lib scan (Snapshot.UseUnits), the file asked about among
// them; or a refusal when the scan is not complete.
func (rn *renamer) useUnits(ctx context.Context) ([]*index.Unit, error) {
	units, complete := rn.snap.UseUnits(ctx)
	if !complete || ctx.Err() != nil {
		return nil, rn.unknown("every place the name is used")
	}
	if !slices.ContainsFunc(units, func(u *index.Unit) bool { return u.Path == rn.tok.Path }) {
		units = append(slices.Clone(units), rn.unit)
	}
	return units, nil
}

// prepareAcross decides what a rename across files of the symbol renames,
// or refuses it.
func (rn *renamer) prepareAcross(ctx context.Context, t *Target) (*across, error) {
	units, err := rn.useUnits(ctx)
	if err != nil {
		return nil, err
	}
	a := &across{want: map[declKey]bool{defKey(t.Def): true}, objects: map[string]bool{}}
	switch t.Kind {
	case resolve.Function:
		fi := t.Def.Func
		if fi.Private {
			a.private = true
			rn.declaringObjects(a, units, t.Def)
			break
		}
		if err := rn.family(a, units, t); err != nil {
			return nil, err
		}
	case resolve.Variable:
		rn.declaringObjects(a, units, t.Def)
		if !t.Def.Var.Static {
			if err := rn.savedAcross(ctx, a, t.Name); err != nil {
				var no *Refusal
				if !rn.force || !errors.As(err, &no) || !no.Forcible {
					return nil, err
				}
				a.forced = no.Reason
			}
		}
	}
	return a, nil
}

// declaringObjects sets the objects that have the declaration d as their
// own: its object, or the objects that include the file it is in.
func (rn *renamer) declaringObjects(a *across, units []*index.Unit, d resolve.Def) {
	if d.Object != "" && !d.Auto {
		a.objects[d.Object] = true
	}
	for _, u := range objectUnits(units) {
		switch d.Kind {
		case resolve.Function:
			if fi, ok := u.Func(d.Name); ok && locKey(fi.Loc) == defKey(d) {
				a.objects[u.LibPath] = true
			}
		case resolve.Variable:
			if vi, ok := u.Var(d.Name); ok && locKey(vi.Loc) == defKey(d) {
				a.objects[u.LibPath] = true
			}
		}
	}
}

// family finds the functions renamed with the one t names: a function
// redefined in an object that inherits it is the same function by the
// driver's rules (a call in the inherited program reaches the
// redefinition), so a rename takes the definitions it redefines and those
// that redefine it, through the whole hierarchy, as an IDE renames a
// virtual method with its overrides. Two declarations are joined where an
// object declares the name and what it inherits has a function of the
// name (or the auto object has, and nothing it inherits does); the family
// is what is joined to the symbol.
func (rn *renamer) family(a *across, units []*index.Unit, t *Target) error {
	name := t.Name
	parent := map[declKey]declKey{}
	var find func(k declKey) declKey
	find = func(k declKey) declKey {
		p, ok := parent[k]
		if !ok || p == k {
			parent[k] = k
			return k
		}
		r := find(p)
		parent[k] = r
		return r
	}
	union := func(x, y declKey) { parent[find(x)] = find(y) }
	owners := map[declKey]map[string]bool{}
	own := func(k declKey, lp string) {
		if owners[k] == nil {
			owners[k] = map[string]bool{}
		}
		owners[k][lp] = true
	}
	autoFn, hasAuto := rn.snap.AutoFunc(name)
	if hasAuto && autoFn.Private {
		hasAuto = false
	}
	autoKey := locKey(autoFn.Loc)
	find(defKey(t.Def))
	byPath := make(map[string]*index.Unit, len(units))
	for _, u := range units {
		byPath[u.Path] = u
	}
	for _, u := range objectUnits(units) {
		fi, ok := u.Func(name)
		if !ok || fi.Private {
			continue
		}
		k := locKey(fi.Loc)
		find(k)
		own(k, u.LibPath)
		// The declarations of the name in the files the program is made
		// of, read each on its own (a prototype in a header, a definition
		// in a file included as code): one function of the program.
		var deps map[string]bool
		for _, d := range u.Deps[1:] {
			du := byPath[d]
			if du == nil {
				continue
			}
			df, ok := du.Func(name)
			if !ok || df.Private || locKey(df.Loc) == k {
				continue
			}
			if deps == nil {
				deps = map[string]bool{}
				for _, p := range u.Deps {
					deps[p] = true
				}
			}
			if deps[df.Path] {
				union(k, locKey(df.Loc))
				own(locKey(df.Loc), u.LibPath)
			}
		}
		inherited := false
		for _, in := range u.Inherits {
			if !in.Resolved {
				continue
			}
			if cf, ok := rn.snap.Chain(in.Path).Funcs[name]; ok && !cf.Private {
				union(k, locKey(cf.Loc))
				own(locKey(cf.Loc), cf.DefinedIn)
				inherited = true
			}
		}
		if !inherited && hasAuto && !rn.snap.AutoMember(u.LibPath) {
			union(k, autoKey)
		}
	}
	root := find(defKey(t.Def))
	for k := range parent {
		if find(k) != root {
			continue
		}
		a.want[k] = true
		if hasAuto && k == autoKey {
			a.auto = true
		}
		for lp := range owners[k] {
			a.objects[lp] = true
		}
	}
	if t.Def.Object != "" && !t.Def.Auto {
		a.objects[t.Def.Object] = true
	}
	// What cannot be renamed in the family.
	switch {
	case driverCalls[name] && rn.driver == "":
		// With no driver_object given, any object may be it.
		return refuse("'%s' may be a function of the driver object, which the driver calls by name: driver_object is not set", name)
	case driverCalls[name]:
		if fn, ok := rn.snap.ChainFunc(rn.driver, name); ok && a.want[locKey(fn.Loc)] {
			return refuse("'%s' is a function of the driver object (%s), which the driver calls by name", name, rn.driver)
		}
	}
	for _, k := range slices.SortedFunc(maps.Keys(a.want), cmpKey) {
		if _, file, ok := splitStub(k.path); ok {
			if k == defKey(t.Def) {
				return refuse("'%s' is defined in the mudlib profile, not in your checkout", name)
			}
			return refuse("'%s' redefines, or is redefined by, the function of %s, which is defined in the mudlib profile, not in your checkout: the two are one function and are renamed together", name, file)
		}
	}
	return nil
}

// driverCalls are the functions DGD calls by name in the driver object
// (DGD::callDriver, Frame::callCritical).
var driverCalls = map[string]bool{
	"initialize": true, "restored": true, "interrupt": true, "path_read": true, "path_write": true,
	"include_file": true, "inherit_program": true, "object_type": true, "compile_rlimits": true,
	"runtime_rlimits": true, "compile_error": true, "runtime_error": true, "atomic_error": true,
	"remove_program": true, "recompile": true, "telnet_connect": true, "binary_connect": true,
	"datagram_connect": true, "touch": true, "call_object": true,
}

// connectionCalls are the functions DGD calls by name in an object that
// is a connection (comm.cpp): which objects those are, the lib decides.
var connectionCalls = map[string]bool{
	"open": true, "close": true, "receive_message": true, "message_done": true,
	"unconnected": true, "receive_datagram": true, "datagram_attach": true,
}

func cmpKey(a, b declKey) int { return cmp.Or(cmp.Compare(a.path, b.path), cmp.Compare(a.off, b.off)) }

// sees reports, for each object, whether its program has one of the
// objects of a: it is one, or inherits one, however indirectly. With
// a.auto every object but the auto object's own sees it.
type seer struct {
	snap *index.Snapshot
	a    *across
	memo map[string]bool
}

func (rn *renamer) seer(a *across) *seer {
	return &seer{snap: rn.snap, a: a, memo: map[string]bool{}}
}

func (s *seer) sees(lp string) bool {
	if lp == "" {
		return false
	}
	if s.a.objects[lp] || s.a.auto && !s.snap.AutoMember(lp) {
		return true
	}
	if v, ok := s.memo[lp]; ok {
		return v
	}
	s.memo[lp] = false // a cycle sees nothing through itself
	u := s.snap.Unit(lp)
	v := false
	if u != nil {
		for _, in := range u.Inherits {
			if in.Resolved && s.sees(in.Path) {
				v = true
				break
			}
		}
	}
	s.memo[lp] = v
	return v
}

// ancestors adds to out the lib paths of the programs the object at lp
// inherits, however indirectly.
func (rn *renamer) ancestors(lp string, out map[string]bool) {
	u := rn.snap.Unit(lp)
	if u == nil {
		return
	}
	for _, in := range u.Inherits {
		if in.Resolved && !out[in.Path] {
			out[in.Path] = true
			rn.ancestors(in.Path, out)
		}
	}
}

// savedAcross refuses a global variable that save_object writes: of an
// object that has it, of an object that inherits one of those, or of an
// object made of programs one of which names save_object,
// restore_object or an autosave marker. The refusal can be forced.
func (rn *renamer) savedAcross(ctx context.Context, a *across, name string) error {
	for _, lp := range slices.Sorted(maps.Keys(a.objects)) {
		if err := rn.savedIn(ctx, lp, name, false); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- the plan

// planAcross plans the rename across files: the occurrences in every file
// of the lib, the checks of the new name, the warnings.
func (rn *renamer) planAcross(ctx context.Context, plan *Plan, a *across) error {
	units, err := rn.useUnits(ctx)
	if err != nil {
		return err
	}
	sym, n := plan.Symbol, plan.New
	if err := rn.validName(sym, n); err != nil {
		return err
	}
	if sym.Kind == resolve.Function {
		if err := rn.calledByName(n, a.objects[rn.driver]); err != nil {
			return err
		}
	}
	plan.Files, plan.Partial = len(units), rn.snap.Partial()
	s := rn.seer(a)
	switch sym.Kind {
	case resolve.Function:
		err = rn.functionClashesAcross(a, s, units, n)
	case resolve.Variable:
		err = rn.variableClashesAcross(a, s, units, n)
	}
	if err != nil {
		return err
	}

	occ := rn.collect(ctx, units, sym, a)
	if ctx.Err() != nil {
		return refuse("cancelled before every file was searched")
	}

	// The files to read again: those an edit lands in; for a macro, every
	// file it reaches that has either name.
	files := map[string]bool{}
	for _, o := range occ {
		if o.Class == Certain {
			files[o.Path] = true
		}
	}
	if sym.Kind == resolve.Macro {
		reach, err := rn.macroReach(ctx, sym, n)
		if err != nil {
			return err
		}
		for p := range reach {
			files[p] = true
		}
	} else {
		// The headers (the bodies of their macros), the objects that have
		// the symbol and those that inherit them, those of them that have
		// either name in their text: code there that is not compiled or
		// does not parse, a macro's body, a call the new name would
		// capture.
		var maybe []string
		for _, u := range units {
			if !files[u.Path] && (isHeader(u.Path) || s.sees(u.LibPath)) {
				maybe = append(maybe, u.Path)
			}
		}
		having, err := rn.having(ctx, maybe, sym.Name, n)
		if err != nil {
			return err
		}
		for _, p := range having {
			files[p] = true
		}
	}
	more, err := rn.readAgain(ctx, plan, a, s, slices.Sorted(maps.Keys(files)), occ)
	if err != nil {
		return err
	}
	occ = append(occ, more...)
	if sym.Kind == resolve.Function && !a.private {
		var byMacro []Occurrence
		plan.Dynamic, byMacro = rn.dynamic(units, sym.Name)
		occ = append(occ, byMacro...)
	}
	slices.SortFunc(occ, func(x, y Occurrence) int {
		return cmp.Or(cmp.Compare(x.Path, y.Path), cmp.Compare(x.Start, y.Start))
	})
	occ = slices.CompactFunc(occ, func(x, y Occurrence) bool { return x.Path == y.Path && x.Start == y.Start })
	plan.Occurrences = occ
	rn.warnAcross(ctx, plan, a)
	return nil
}

// calledByName refuses a function's new name that the driver calls by
// name: the create function, in every object, and in the driver object
// (inDriver: the function is one of it) the functions the driver calls
// there. The renamed function would be called when nothing called it.
// With no driver object given, any object may be it.
func (rn *renamer) calledByName(n string, inDriver bool) error {
	switch {
	case n == rn.create:
		return refuse("'%s' is the function the driver calls by name to initialize every object (create in the configuration): renamed so, the function is called when an object is made", n)
	case rn.driver == "" && driverCalls[n]:
		return refuse("'%s' is a function the driver calls by name in the driver object, and driver_object is not set: renamed so, the function may be called by the driver", n)
	case inDriver && driverCalls[n]:
		return refuse("'%s' is a function the driver calls by name in the driver object (%s): renamed so, the function is called by the driver", n, rn.driver)
	}
	return nil
}

// validName refuses a new name that is no name or a keyword; whether it
// is a macro is asked in each file an edit lands in.
func (rn *renamer) validName(sym Symbol, n string) error {
	if !token.IsIdent(n) {
		return refuse("'%s' is not a name: a name is letters, digits and '_', and does not begin with a digit", n)
	}
	if token.Keywords[n] != 0 || n == "defined" && sym.Kind == resolve.Macro {
		return refuse("'%s' is a keyword", n)
	}
	return nil
}

// collect classifies the Uses of the old name in every file of the lib.
func (rn *renamer) collect(ctx context.Context, units []*index.Unit, sym Symbol, a *across) []Occurrence {
	var (
		mu  sync.Mutex
		out []Occurrence
	)
	par.Each(ctx, units, func(u *index.Unit) {
		uses := u.Uses(sym.Name)
		if len(uses) == 0 {
			return
		}
		lr := rn.r
		if u.Path != rn.tok.Path {
			lr = resolve.New(resolve.File{Unit: u, Snapshot: rn.snap, Registry: rn.r.Registry, Kfuns: rn.r.Kfuns})
		}
		var mine []Occurrence
		for _, use := range uses {
			class, reason, ok := rn.classifyUse(u, lr, use, sym, a)
			if !ok {
				continue
			}
			mine = append(mine, Occurrence{Path: u.Path, Start: use.Start, End: use.End, Line: use.Line, Col: use.Col,
				Class: class, Decl: use.Decl && class == Certain, Reason: reason})
		}
		mu.Lock()
		out = append(out, mine...)
		mu.Unlock()
	})
	return out
}

const (
	unknownReceiver = "a call on an object nobody can tell, which may be one that has it"
	privateByName   = "DGD does not call a private function by name"
)

// classifyUse says what a Use of the old name is to the rename; false
// when it is no occurrence (another kind of name).
func (rn *renamer) classifyUse(u *index.Unit, lr *resolve.Resolver, use index.Use, sym Symbol, a *across) (Class, string, bool) {
	switch sym.Kind {
	case resolve.Function:
		if use.Kind != index.UseFunction {
			return 0, "", false
		}
		late := use.Form == index.FormSelf || use.Form == index.FormPath || use.Form == index.FormUnknown
		if use.Form == index.FormUnknown {
			if a.private {
				return 0, "", false
			}
			return Possible, unknownReceiver, true
		}
		defs := lr.Lookup(use)
		hits := 0
		for _, d := range defs {
			if a.want[defKey(d)] {
				hits++
			}
		}
		switch {
		case hits > 0 && a.private && late:
			return Excluded, privateByName, true
		case hits > 0 && hits == len(defs):
			return Certain, "", true
		case hits > 0:
			return Possible, "ambiguous here: more than one inherited object has a function of the name", true
		case len(defs) == 0:
			return rn.unresolved(u, use)
		}
		return Excluded, rn.describe(&resolve.Ref{Kind: resolve.Function, Name: sym.Name, Defs: defs}), true
	case resolve.Variable:
		if use.Kind != index.UseVariable {
			return 0, "", false
		}
	case resolve.Macro:
		if use.Kind != index.UseMacro {
			return 0, "", false
		}
	default:
		return 0, "", false
	}
	defs := lr.Lookup(use)
	for _, d := range defs {
		if a.want[defKey(d)] {
			return Certain, "", true
		}
	}
	if len(defs) == 0 {
		if sym.Kind == resolve.Variable && isHeader(u.Path) {
			return Possible, "in a header, where what it names depends on the file that includes it", true
		}
		return Excluded, "names nothing the tools know of", true
	}
	return Excluded, rn.describe(&resolve.Ref{Kind: sym.Kind, Name: sym.Name, Defs: defs}), true
}

// unresolved classifies a Use of a function's name that names no
// declaration where it is.
func (rn *renamer) unresolved(u *index.Unit, use index.Use) (Class, string, bool) {
	switch {
	case isHeader(u.Path):
		return Possible, "in a header, where what it names depends on the file that includes it", true
	case use.Form == index.FormSelf:
		return Possible, "a call in this_object(), which may be an object that inherits this one and has it", true
	case use.Form == index.FormPath:
		if !rn.snap.ObjectExists(use.Aux) && !rn.snap.IsVirtual(use.Aux) {
			return Excluded, "a call in " + use.Aux + ", which does not exist", true
		}
		return Possible, "a call in " + use.Aux + ", whose functions cannot be told", true
	case u.LibPath != "" && rn.snap.ChainUnseen(u.LibPath):
		return Possible, "the object inherits a file the tools cannot find, which may have it", true
	}
	return Excluded, "names no function the tools know of", true
}

// macroReach returns the files a header's macro reaches: the header, the
// files that include it and what they include, those of them that have
// the old name or the new one in their text.
func (rn *renamer) macroReach(ctx context.Context, sym Symbol, n string) (map[string]bool, error) {
	h := sym.Def.Path
	includers, ok := rn.snap.Includers(ctx, h)
	if !ok {
		return nil, rn.unknown("which files include " + rn.libName(h))
	}
	set := map[string]bool{h: true}
	for _, p := range includers {
		set[p] = true
		if u := rn.snap.UnitAt(p); u != nil {
			for _, d := range u.Deps {
				if _, _, stub := splitStub(d); !stub {
					set[d] = true
				}
			}
		}
	}
	return rn.havingSet(ctx, slices.Sorted(maps.Keys(set)), sym.Name, n)
}

// having returns those of the files whose text has one of the names.
func (rn *renamer) having(ctx context.Context, paths []string, names ...string) ([]string, error) {
	set, err := rn.havingSet(ctx, paths, names...)
	return slices.Sorted(maps.Keys(set)), err
}

func (rn *renamer) havingSet(ctx context.Context, paths []string, names ...string) (map[string]bool, error) {
	var mu sync.Mutex
	out := map[string]bool{}
	par.Each(ctx, paths, func(p string) {
		src, err := rn.snap.ReadFile(p)
		if err != nil || !slices.ContainsFunc(names, func(n string) bool { return bytes.Contains(src, []byte(n)) }) {
			return
		}
		mu.Lock()
		out[p] = true
		mu.Unlock()
	})
	if ctx.Err() != nil {
		return nil, refuse("cancelled before every file was searched")
	}
	return out, nil
}

// readAgain reads the files again, each with its own resolver: the
// occurrences the index does not have (in code that is not compiled or
// does not parse, in the bodies of macros), and the checks of the new
// name there. The refusal is that of the first file, in path order.
func (rn *renamer) readAgain(ctx context.Context, plan *Plan, a *across, s *seer, files []string, occ []Occurrence) ([]Occurrence, error) {
	byFile := map[string][]Occurrence{}
	for _, o := range occ {
		byFile[o.Path] = append(byFile[o.Path], o)
	}
	// Whether the new name would mean the symbol in a file: in an object
	// that sees it.
	seen := map[string]bool{}
	for _, p := range files {
		if u := rn.snap.UnitAt(p); u != nil {
			seen[p] = s.sees(u.LibPath)
		}
	}
	type result struct {
		occ []Occurrence
		err error
	}
	results := make([]result, len(files))
	idx := make([]int, len(files))
	for i := range idx {
		idx[i] = i
	}
	par.Each(ctx, idx, func(i int) {
		occ, err := rn.readFile(plan, a, files[i], byFile[files[i]], seen[files[i]])
		results[i] = result{occ, err}
	})
	if ctx.Err() != nil {
		return nil, refuse("cancelled before every file was read")
	}
	var out []Occurrence
	for _, r := range results {
		if r.err != nil {
			return nil, r.err
		}
		out = append(out, r.occ...)
	}
	return out, nil
}

// readFile is readAgain of one file.
func (rn *renamer) readFile(plan *Plan, a *across, path string, known []Occurrence, sees bool) ([]Occurrence, error) {
	sym, old, n := plan.Symbol, plan.Symbol.Name, plan.New
	var frn *renamer
	if path == rn.tok.Path {
		frn = rn
	} else {
		p, err := rn.snap.Parse(path)
		if err != nil {
			if errors.Is(err, index.ErrChanged) {
				return nil, rn.unknown("what " + rn.libName(path) + " has")
			}
			var perr *fs.PathError
			if errors.As(err, &perr) {
				return nil, refuse("%s cannot be read: %v", rn.libName(path), perr.Err)
			}
			return nil, refuse("%s cannot be read: %v", rn.libName(path), err)
		}
		r := resolve.New(resolve.File{Tok: p.Tok, Tree: p.Tree, Env: p.Env, Unit: p.Unit, Snapshot: rn.snap,
			Registry: rn.r.Registry, Kfuns: rn.r.Kfuns})
		frn = newRenamer(File{Resolver: r, AutosaveMarkers: rn.markers})
		if frn == nil {
			return nil, refuse("%s cannot be read", rn.libName(path))
		}
	}
	where := func(off uint32) string { return fmt.Sprintf("%s:%d", rn.libName(path), frn.line(off)) }
	have := map[uint32]bool{}
	certain := false
	for _, o := range known {
		have[o.Start] = true
		certain = certain || o.Class == Certain
	}
	var out []Occurrence
	add := func(start uint32, class Class, reason string) {
		pos := frn.tok.Pos(start)
		have[start] = true
		out = append(out, Occurrence{Path: path, Start: start, End: start + uint32(len(old)),
			Line: uint32(pos.Line), Col: uint32(pos.Col), Class: class, Reason: reason})
	}

	// The new name: a macro here; a name the renamed symbol would take
	// the place of.
	if certain || sym.Kind == resolve.Macro {
		if err := frn.macroHere(n, path); err != nil {
			return nil, err
		}
	}
	toks := frn.tok.Tokens
	if sym.Kind == resolve.Macro {
		for ti, tok := range toks {
			if tok.Kind == token.Ident && frn.text(ti) == n {
				return nil, refuse("'%s' is used at %s: the renamed macro would replace it", n, where(tok.Off))
			}
		}
		for _, w := range frn.directiveWords() {
			if w.name == n {
				return nil, refuse("'%s' is used at %s: the renamed macro would replace it", n, where(w.off))
			}
		}
	}

	// The old name where the index does not see it.
	for ti, tok := range toks {
		if tok.Kind != token.Ident || have[tok.Off] || frn.text(ti) != old {
			continue
		}
		switch {
		case !frn.compiled(ti):
			add(tok.Off, Possible, "in code that is not compiled here (a branch of #if that is not taken)")
		case frn.unparsed(ti) != "":
			add(tok.Off, Possible, frn.unparsed(ti))
		default:
			at := frn.r.At(tok.Off)
			switch {
			case at == nil:
			case at.Kind == sym.Kind && slices.ContainsFunc(at.Defs, func(d resolve.Def) bool { return a.want[defKey(d)] }):
				if a.private && at.Call != nil {
					add(tok.Off, Excluded, privateByName)
				} else {
					add(tok.Off, Certain, "")
					certain = true
				}
			default:
				add(tok.Off, Excluded, frn.describe(at))
			}
		}
	}
	if sym.Kind == resolve.Macro {
		for _, w := range frn.directiveWords() {
			if w.name != old || have[w.off] {
				continue
			}
			at := frn.r.At(w.off)
			switch {
			case at == nil:
				add(w.off, Possible, "in a directive that is not read here (a branch of #if that is not taken)")
			case at.Kind == resolve.Macro && slices.ContainsFunc(at.Defs, func(d resolve.Def) bool { return a.want[defKey(d)] }):
				add(w.off, Certain, "")
			default:
				add(w.off, Excluded, frn.describe(at))
			}
		}
	} else {
		for _, b := range frn.macroBodies(old) {
			if !have[b.off] {
				add(b.off, Possible, "in the body of the macro "+b.macro+", where what it names depends on where the macro is used")
			}
		}
	}

	// What the renamed name would change here.
	mine := append(slices.Clone(known), out...)
	if err := frn.stringizedAt(old, path, mine); err != nil {
		return nil, err
	}
	if sees {
		if sym.Kind == resolve.Function || sym.Kind == resolve.Variable {
			if err := frn.expandsTo(sym, n, where); err != nil {
				return nil, err
			}
		}
		switch sym.Kind {
		case resolve.Function:
			if err := frn.calledHere(n, where); err != nil {
				return nil, err
			}
		case resolve.Variable:
			if err := frn.capturedHere(sym, n, path, mine, where); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// expandsTo refuses a new name that the body of a macro used in the
// file's code names: the macro would name the renamed symbol there.
func (rn *renamer) expandsTo(sym Symbol, n string, where func(uint32) string) error {
	if rn.env == nil {
		return nil
	}
	done := map[string]bool{}
	for _, ti := range rn.tree.Sel.Sig {
		tok := rn.tok.Tokens[ti]
		if tok.Kind != token.Ident {
			continue
		}
		name := rn.text(ti)
		if done[name] || rn.env.Lookup(name) == nil {
			continue
		}
		done[name] = true
		exp := map[string]bool{}
		rn.expansion(name, map[string]bool{}, exp)
		if exp[n] {
			return refuse("the macro %s, used at %s, names '%s' in its body: it would name the renamed %s there", name, where(tok.Off), n, what(sym.Kind))
		}
	}
	return nil
}

// macroHere refuses a new name that is a macro in the file.
func (rn *renamer) macroHere(n, path string) error {
	if rn.env == nil {
		return nil
	}
	if def := rn.env.Definition(n); def != nil && !def.Undefined {
		return refuse("'%s' is a macro in %s (#define in %s)", n, rn.libName(path), rn.libName(def.Path))
	}
	if defined, _ := rn.env.Defined(n); defined || rn.env.Lookup(n) != nil || rn.env.Outside(n) {
		return refuse("'%s' is a macro in %s (the driver's or the project's configuration's)", n, rn.libName(path))
	}
	return nil
}

// calledHere refuses a new name for a function that the file calls
// already, where the call names nothing: it would reach the renamed
// function.
func (rn *renamer) calledHere(n string, where func(uint32) string) error {
	toks := rn.tok.Tokens
	for ti, tok := range toks {
		if tok.Kind != token.Ident || rn.text(ti) != n || !rn.compiled(ti) {
			continue
		}
		at := rn.r.At(tok.Off)
		prev, next := rn.neighbours(ti)
		called := at != nil && at.Kind == resolve.Function && at.Call == nil && len(at.Defs) == 0 && prev != token.ColonColon
		if at == nil && next == token.LParen && prev != token.Arrow && prev != token.ColonColon {
			called = true
		}
		if called {
			return refuse("'%s' is called at %s: the call would reach the renamed function", n, where(tok.Off))
		}
	}
	return nil
}

// capturedHere refuses a new name for a global variable that a local
// of the file would hide where the variable is used, or that the file
// uses as a name that is no variable it can see now.
func (rn *renamer) capturedHere(sym Symbol, n, path string, occ []Occurrence, where func(uint32) string) error {
	for _, o := range occ {
		if o.Path != path || o.Class != Certain || o.Decl {
			continue
		}
		if sc := rn.funcScope(o.Start); sc != nil {
			if m := sc.Lookup(n, o.Start); m != nil {
				return refuse("at %s the renamed variable would be hidden by the %s '%s' declared at line %d", where(o.Start), localWhat(m), n, rn.localLine(m))
			}
		}
	}
	toks := rn.tok.Tokens
	for ti, tok := range toks {
		if tok.Kind != token.Ident || rn.text(ti) != n || !rn.compiled(ti) {
			continue
		}
		at := rn.r.At(tok.Off)
		if at != nil {
			switch at.Kind {
			case resolve.Function, resolve.Label, resolve.Macro, resolve.Local, resolve.Parameter:
				continue
			}
		}
		if prev, _ := rn.neighbours(ti); prev == token.Arrow || prev == token.ColonColon {
			continue
		}
		return refuse("'%s' at %s %s: it would name the renamed variable", n, where(tok.Off), rn.describe(at))
	}
	return nil
}

// stringizedAt refuses when an occurrence to rename in the file is an
// argument of a macro that makes a string of its arguments or pastes
// them.
func (rn *renamer) stringizedAt(old, path string, occ []Occurrence) error {
	var mine []Occurrence
	for _, o := range occ {
		if o.Path == path {
			mine = append(mine, o)
		}
	}
	err := rn.stringized(&Plan{Symbol: Symbol{Name: old}, Occurrences: mine})
	var no *Refusal
	if errors.As(err, &no) && path != rn.tok.Path {
		no.Reason = rn.libName(path) + ": " + no.Reason
	}
	return err
}

// body is a name in the body of a macro.
type body struct {
	off   uint32
	macro string
}

// macroBodies returns where name stands in the bodies of the file's
// #defines, but as a parameter.
func (rn *renamer) macroBodies(name string) []body {
	var out []body
	for _, tok := range rn.tok.Tokens {
		if tok.Kind != token.Directive {
			continue
		}
		text := rn.tok.Text(tok)
		if !bytes.Contains(text, []byte(name)) {
			continue
		}
		if dir, _ := pp.SplitDirective(text); dir != "define" {
			continue
		}
		arg := pp.ArgOffset(text)
		words := lexer.Lex("", text[arg:], rn.tok.Dialect).Tokens
		var macro string
		var params []string
		inParams := false
		for i, w := range words {
			if w.Kind.IsTrivia() {
				continue
			}
			word := string(text[arg:][w.Off:w.End])
			switch {
			case macro == "":
				if w.Kind != token.Ident {
					break
				}
				macro = word
				// A '(' right after the name opens the parameters.
				if i+1 < len(words) && words[i+1].Kind == token.LParen && words[i+1].Off == w.End {
					inParams = true
				}
				continue
			case inParams:
				if w.Kind == token.RParen {
					inParams = false
				} else if w.Kind == token.Ident {
					params = append(params, word)
				}
				continue
			}
			if w.Kind == token.Ident && word == name && !slices.Contains(params, word) {
				out = append(out, body{off: tok.Off + uint32(arg) + w.Off, macro: macro})
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- clashes

// functionClashesAcross refuses a new name for a function that an object
// which has the renamed one, or inherits it, has already (its own, an
// inherited one, the auto object's), or that is a kfun.
func (rn *renamer) functionClashesAcross(a *across, s *seer, units []*index.Unit, n string) error {
	if rn.kfuns.Lookup(n) != nil {
		return refuse("'%s' is a kfun: calls of it in the objects that have the renamed function would reach the renamed function instead", n)
	}
	if fn, ok := rn.snap.AutoFunc(n); ok && !fn.Private {
		return refuse("'%s' is a function of the auto object, which every object has", n)
	}
	objects := slices.Sorted(maps.Keys(a.objects))
	for _, own := range []bool{true, false} {
		for _, lp := range objects {
			if fn, ok := rn.snap.ChainFunc(lp, n); ok && (fn.DefinedIn == lp) == own {
				return refuse("%s has a function '%s' already (%s)", lp, n, rn.fileLine(fn.Path, fn.Line))
			}
		}
	}
	for _, u := range objectUnits(units) {
		if a.objects[u.LibPath] || !s.sees(u.LibPath) {
			continue
		}
		if fn, ok := rn.snap.ChainFunc(u.LibPath, n); ok {
			return refuse("%s, which has the renamed function, has a function '%s' already (%s)", u.LibPath, n, rn.fileLine(fn.Path, fn.Line))
		}
	}
	return nil
}

// variableClashesAcross refuses a new name for a global variable that an
// object which has the renamed one, or inherits it, has already.
func (rn *renamer) variableClashesAcross(a *across, s *seer, units []*index.Unit, n string) error {
	objects := slices.Sorted(maps.Keys(a.objects))
	for _, own := range []bool{true, false} {
		for _, lp := range objects {
			if v, ok := rn.snap.ChainVar(lp, n); ok && (v.DefinedIn == lp) == own {
				return refuse("%s has a global variable '%s' already (%s)", lp, n, rn.fileLine(v.Path, v.Line))
			}
		}
	}
	for _, u := range objectUnits(units) {
		if a.objects[u.LibPath] || !s.sees(u.LibPath) {
			continue
		}
		if v, ok := rn.snap.ChainVar(u.LibPath, n); ok {
			return refuse("%s, which has the renamed variable, has a global variable '%s' already (%s)", u.LibPath, n, rn.fileLine(v.Path, v.Line))
		}
	}
	return nil
}

// fileLine names a place for a message about more than one file: the
// file's lib name and the line.
func (rn *renamer) fileLine(p string, line int) string {
	return fmt.Sprintf("%s:%d", rn.libName(p), line)
}

// ---------------------------------------------------------------- reported beside

// Dynamic is a call that names a function with a name made when it runs
// (call_other(ob, fn), call_out("do_" + verb)): no search can tell
// whether it calls the renamed function.
type Dynamic struct {
	Path       string
	Start, End uint32
	// Line and Col are where Start is, 1-based, Col counting bytes; 0
	// when not known.
	Line, Col uint32
	// Pattern is the name as far as it is known: the constant parts
	// around a '*' ("do_*", "*").
	Pattern string
	// Matches says the old name begins with the constant part before the
	// '*' or ends with the one after it.
	Matches bool
}

// dynamic returns the calls of the lib that name a function with a name
// made when they run: those whose constant parts the old name begins or
// ends with first. The calls that name it by a macro whose body is the
// old name as a string are possible occurrences instead: the edit is the
// macro's, which other places may use too.
func (rn *renamer) dynamic(units []*index.Unit, old string) ([]Dynamic, []Occurrence) {
	var out []Dynamic
	var byMacro []Occurrence
	for _, u := range units {
		for _, use := range u.Uses(index.DynamicName) {
			if use.Kind != index.UseDynamic {
				continue
			}
			prefix, suffix, pattern := strings.Cut(use.Aux, "*")
			if !pattern {
				if use.Aux != old {
					continue
				}
				if o, ok := rn.macroString(u.Path, use, old); ok {
					byMacro = append(byMacro, o)
				} else {
					out = append(out, Dynamic{Path: u.Path, Start: use.Start, End: use.End, Line: use.Line, Col: use.Col, Pattern: old, Matches: true})
				}
				continue
			}
			out = append(out, Dynamic{Path: u.Path, Start: use.Start, End: use.End, Line: use.Line, Col: use.Col,
				Pattern: use.Aux,
				Matches: prefix != "" && strings.HasPrefix(old, prefix) || suffix != "" && strings.HasSuffix(old, suffix)})
		}
	}
	slices.SortStableFunc(out, func(x, y Dynamic) int {
		if x.Matches != y.Matches {
			if x.Matches {
				return -1
			}
			return 1
		}
		return 0
	})
	return out, byMacro
}

// macroString returns the string in the body of the macro that a call of
// the file at path names a function by (the Use), as a possible
// occurrence: what the call names is the old name, and renaming it
// renames it for every place that uses the macro. False when the macro's
// #define cannot be read in the lib.
func (rn *renamer) macroString(path string, use index.Use, old string) (Occurrence, bool) {
	src, err := rn.snap.ReadFile(path)
	if err != nil || int(use.End) > len(src) {
		return Occurrence{}, false
	}
	name := string(src[use.Start:use.End])
	p, err := rn.snap.Parse(path)
	if err != nil || p.Env == nil {
		return Occurrence{}, false
	}
	m := p.Env.Lookup(name)
	if m == nil {
		return Occurrence{}, false
	}
	if _, _, stub := splitStub(m.Path); stub {
		return Occurrence{}, false
	}
	def, err := rn.snap.ReadFile(m.Path)
	if err != nil || int(m.NameOff) > len(def) {
		return Occurrence{}, false
	}
	line := def[m.NameOff:]
	if i := bytes.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	k := bytes.Index(line, []byte(`"`+old+`"`))
	if k < 0 {
		return Occurrence{}, false
	}
	start := m.NameOff + uint32(k) + 1
	return Occurrence{Path: m.Path, Start: start, End: start + uint32(len(old)), Class: Possible,
		Reason: fmt.Sprintf("the body of the macro %s, by which %s:%d names it; other places may use the macro too", name, rn.libName(path), use.Line)}, true
}

// warnAcross adds the warnings of a rename across files: a variable
// renamed though save_object writes it, a checkout that is part of the
// lib, base files that other objects inherit or include, calls by name
// of a private function.
func (rn *renamer) warnAcross(ctx context.Context, plan *Plan, a *across) {
	if a.forced != "" {
		plan.Warnings = append(plan.Warnings, "renamed although "+a.forced+" (forced): convert the save files, or the values saved under the old name are lost")
	}
	if a.private {
		for _, o := range plan.Occurrences {
			if o.Reason == privateByName {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s:%d: the string or ->%s cannot reach the private function '%s' (DGD calls no private function by name), and is left as it is",
					rn.libName(o.Path), o.Line, plan.Symbol.Name, plan.Symbol.Name))
			}
		}
	}
	if connectionCalls[plan.Symbol.Name] && !a.private {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("the driver calls '%s' by name in an object that is a connection: if one of the objects that have it is, the renamed function is no longer called", plan.Symbol.Name))
	}
	if plan.Partial && !a.private {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("this checkout is part of the mudlib: the places outside it that name '%s' are not renamed, and cannot be seen from here", plan.Symbol.Name))
	}
	// The files edited that other objects inherit or include.
	type base struct {
		path string
		n    int
	}
	var bases []base
	seen := map[string]bool{}
	for _, e := range plan.Occurrences {
		if e.Class != Certain || seen[e.Path] {
			continue
		}
		seen[e.Path] = true
		var n int
		if isHeader(e.Path) {
			inc, _ := rn.snap.Includers(ctx, e.Path)
			n = len(inc)
		} else if lp := rn.snap.LibPath(e.Path); lp != "" {
			users, _ := rn.snap.Users(ctx, lp)
			n = len(users)
		}
		if n > 0 {
			bases = append(bases, base{e.Path, n})
		}
	}
	if len(bases) == 0 {
		return
	}
	slices.SortStableFunc(bases, func(x, y base) int { return cmp.Or(cmp.Compare(y.n, x.n), cmp.Compare(x.path, y.path)) })
	var list []string
	for i, b := range bases {
		if i == 3 {
			list = append(list, fmt.Sprintf("and %d more", len(bases)-3))
			break
		}
		what := "objects inherit"
		switch {
		case isHeader(b.path) && b.n == 1:
			what = "file includes"
		case isHeader(b.path):
			what = "files include"
		case b.n == 1:
			what = "object inherits"
		}
		list = append(list, fmt.Sprintf("%s (%d %s it)", rn.libName(b.path), b.n, what))
	}
	plan.Warnings = append(plan.Warnings, "edits in base files: "+strings.Join(list, ", ")+
		": an object keeps the program it was compiled with, so the objects that inherit or include them must be recompiled for the rename to take effect")
}

// splitStub reports whether p is the location of a profile's stub, and
// of which file.
func splitStub(p string) (name, file string, ok bool) { return profile.SplitLocation(p) }
