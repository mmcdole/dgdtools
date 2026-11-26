package index

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unique"

	"github.com/mmcdole/dgdtools/internal/par"
	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/lexer"
	"github.com/mmcdole/dgdtools/pkg/pp"
	"github.com/mmcdole/dgdtools/pkg/syntax"
	"github.com/mmcdole/dgdtools/pkg/token"
)

// Snapshot is one generation of the index: the lib as it was between two
// Invalidate calls. It reads files on first need and remembers what it
// read; an answer, once given, does not change. A Snapshot is safe for
// concurrent use and stays usable after the index has moved on, though
// questions that need the whole lib (who inherits a file) are then
// answered "unknown".
type Snapshot struct {
	*lib
	ix  *Index
	gen uint64
	ext *extent

	headers *memo[*header]
	units   *memo[*Unit]
	base    *lazy[*baseState]
	auto    *lazy[*autoState]
	chains  *table[*Chain]
	// symbols is where a search by name looks, once the whole lib has
	// been scanned. It is not passed on.
	symbols *lazy[*symbolTable]
	// useScope is where a search for references looks, once the whole
	// lib has been scanned. It is not passed on.
	useScope *lazy[*useScope]
	// isFile remembers what exists. It is not passed on: the next
	// generation looks again at whatever it still needs.
	isFile *table[bool]
	// failed holds the files whose analysis crashed, with the panic;
	// guarded by ix.mu.
	failed map[string]string
}

func newSnapshot(ix *Index, gen uint64, ext *extent) *Snapshot {
	return &Snapshot{
		lib: ix.lib, ix: ix, gen: gen, ext: ext,
		headers: newMemo[*header](), units: newMemo[*Unit](),
		base: &lazy[*baseState]{}, auto: &lazy[*autoState]{},
		chains: &table[*Chain]{}, symbols: &lazy[*symbolTable]{}, useScope: &lazy[*useScope]{}, isFile: &table[bool]{}, failed: map[string]string{},
	}
}

// Generation returns the snapshot's generation number.
func (s *Snapshot) Generation() uint64 { return s.gen }

// Index returns the index the snapshot is a generation of.
func (s *Snapshot) Index() *Index { return s.ix }

// Current reports whether the snapshot is still the index's current
// generation: nothing has been invalidated since.
func (s *Snapshot) Current() bool {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	return s.ix.cur == s
}

// Complete reports whether the whole lib is on disk and has everything
// the configuration says it has.
func (s *Snapshot) Complete() bool { return s.ext.complete }

// Notes are the remarks about the lib as a whole.
func (s *Snapshot) Notes() []diag.Note { return slices.Clone(s.ext.notes) }

// InternalError says that the analysis of a file crashed: a bug in the
// tools, not in the file.
type InternalError struct {
	Path string
	// Panic is what the analysis panicked with.
	Panic string
}

func (e *InternalError) Error() string {
	return fmt.Sprintf("internal error analyzing %s (please report): %s", e.Path, e.Panic)
}

// Crashed returns the files whose analysis crashed in this generation,
// in path order. Their facts are unknown, like those of a file that
// cannot be read.
func (s *Snapshot) Crashed() []*InternalError {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	var out []*InternalError
	for _, p := range slices.Sorted(maps.Keys(s.failed)) {
		out = append(out, &InternalError{Path: p, Panic: s.failed[p]})
	}
	return out
}

// ----------------------------------------------------------------- files

// exists reports whether p is a file. The answer is remembered for the
// generation.
func (s *Snapshot) exists(p string) bool {
	if p == "" {
		return false
	}
	if is, ok := s.isFile.get(p); ok {
		return is
	}
	is := s.fs.Exists(p) && !s.fs.IsDir(p)
	return s.isFile.getOrPut(p, func() bool { return is })
}

// ReadFile returns the text of the file at p, a path on disk or the
// location of a stub, as the index reads it now: an editor's buffer, the
// disk, a profile's stub. It is what the offsets of a declaration found
// in this generation count in, as long as the generation is current.
func (s *Snapshot) ReadFile(p string) ([]byte, error) { return s.fs.ReadFile(s.clean(p)) }

// locate returns where libFile (a lib path with its extension) is read
// from: the checkout for the directory it holds, else the lib on disk,
// else a profile's stub. The second result lists the places on disk where
// the file was looked for and not found; a file appearing at one of them
// changes the answer. When nothing has the file the path is where it
// would be on disk ("" if no directory on disk could hold it).
func (s *Snapshot) locate(libFile string) (p string, absent []string) {
	return s.locateIn(libFile, s.exists)
}

// locateIn is locate in a lib whose files exists says.
func (s *Snapshot) locateIn(libFile string, exists func(string) bool) (p string, absent []string) {
	disk, onDisk := s.diskPath(libFile)
	if onDisk && (s.lay.Stubs == "" || s.inCheckout(libFile) || exists(disk)) {
		return disk, nil
	}
	if onDisk {
		absent = []string{disk}
	}
	if stub := s.lay.Stubs + libFile; s.lay.Stubs != "" && !s.inCheckout(libFile) && (!onDisk || exists(stub)) {
		return stub, absent
	}
	return disk, absent
}

// File returns where the lib file libFile ("/include/std.h") is read
// from: a path on disk or a stub's location; "" when nothing could hold
// it.
func (s *Snapshot) File(libFile string) string {
	p, _ := s.locate(path.Clean(libFile))
	return p
}

// ObjectFile returns where the source of the object at libPath is read
// from: a path on disk or a stub's location; "" when nothing could hold
// it. An object's source is the file of its name with ".c" added,
// whatever dots the name has. The lib path of a file that is not an
// object's source (a header linted on its own) is the file's own name,
// and stands for that file when no object is in the way.
func (s *Snapshot) ObjectFile(libPath string) string {
	p, _ := s.locate(objectFile(libPath))
	if path.Ext(libPath) != "" && !s.exists(p) {
		if own, _ := s.locate(libPath); s.exists(own) {
			return own
		}
	}
	return p
}

// ObjectExists reports whether libPath is backed by a file or a stub.
func (s *Snapshot) ObjectExists(libPath string) bool {
	return s.exists(s.ObjectFile(libPath))
}

// ResolveInclude maps an include to the file the driver would read:
// "name" searches the including file's directory then the include dirs,
// <name> the include dirs only. The second result is false when nothing
// was found.
func (s *Snapshot) ResolveInclude(from string, inc pp.Include) (string, bool) {
	p, _ := s.resolveInclude(from, inc)
	return p, p != ""
}

// ResolveIncludeIf is ResolveInclude in the lib as it would be after
// its files changed (moved, created, removed): exists says whether there
// is a file at a path, and decided false leaves the answer to the lib
// as it is. from need not exist.
func (s *Snapshot) ResolveIncludeIf(from string, inc pp.Include, exists func(p string) (is, decided bool)) (string, bool) {
	p, _ := s.resolveIncludeIn(from, inc, func(p string) bool {
		if is, decided := exists(p); decided {
			return is
		}
		return s.exists(p)
	})
	return p, p != ""
}

// resolveInclude also returns the places that were tried before the hit
// (all of them on a miss).
func (s *Snapshot) resolveInclude(from string, inc pp.Include) (hit string, absent []string) {
	return s.resolveIncludeIn(from, inc, s.exists)
}

// resolveIncludeIn is resolveInclude in a lib whose files exists says.
func (s *Snapshot) resolveIncludeIn(from string, inc pp.Include, exists func(string) bool) (hit string, absent []string) {
	try := func(libFile string) bool {
		p, miss := s.locateIn(path.Clean(libFile), exists)
		absent = append(absent, miss...)
		if exists(p) {
			hit = p
			return true
		}
		if len(miss) == 0 && p != "" && !s.isStub(p) {
			absent = append(absent, p)
		}
		return false
	}
	if !inc.Angle {
		switch rel, inLib := s.rel(from); {
		case strings.HasPrefix(inc.Name, "/"):
			if s.HasLib() && try(inc.Name) {
				return hit, absent
			}
		case inLib:
			if try(path.Join("/", path.Dir(rel), inc.Name)) {
				return hit, absent
			}
		default:
			p := filepath.Join(filepath.Dir(from), filepath.FromSlash(inc.Name))
			if exists(p) {
				return p, absent
			}
			absent = append(absent, p)
		}
	}
	if s.HasLib() {
		dirs, _ := s.IncludeDirs(s.LibFile(from))
		for _, dir := range dirs {
			if try(path.Join(dir, inc.Name)) {
				return hit, absent
			}
		}
	}
	return "", absent
}

// --------------------------------------------------------------- headers

// header is one file as seen through #include: its macro table, and the
// declarations it contributes to the including program.
type header struct {
	// table is nil when the file could not be read.
	table *pp.Table
	// code is nil for a pure header.
	code *Unit
}

func (s *Snapshot) header(p string) *header {
	return s.headers.load(p, func() *header {
		src, err := s.fs.ReadFile(p)
		if err != nil {
			return &header{}
		}
		f := lexer.Lex(p, src, s.dialect)
		// Read on its own: what it defines and declares, not how it reads
		// in any one includer.
		env := pp.NewEnv(s.defined, s.override)
		env.Header = true
		sel := env.Scan(f, nil)
		h := &header{table: env.Own()}
		if len(sel.Sig) == 0 {
			return h
		}
		tree := syntax.Parse(f, syntax.Options{Env: env, Sel: sel})
		u := &Unit{
			Path:    p,
			Partial: tree.Partial || h.table.Partial, Stub: s.isStub(p), Deps: []string{p}, gen: s.gen,
		}
		collectDecls(u, tree)
		sortDecls(u)
		for _, d := range tree.Decls {
			switch d.(type) {
			case *syntax.FuncDecl, *syntax.VarDecl:
				if first, _ := d.Span(); !sel.Guessed(first) {
					u.Declares = true
				}
			}
		}
		h.code = u
		return h
	}, nil)
}

const maxIncludeDepth = 12

// closure is the include graph below a file, walked in preprocessing
// order.
type closure struct {
	order    []*pp.Table // tables in the order their definitions take effect
	deps     []string    // files read
	absent   []string    // places looked at and found empty
	complete bool
	seen     map[string]bool
}

func newClosure(p string) *closure {
	return &closure{complete: true, seen: map[string]bool{p: true}, deps: []string{p}}
}

// includes returns the closure of the file at p, whose own table is t.
func (s *Snapshot) includes(p string, t *pp.Table) *closure {
	c := newClosure(p)
	for _, inc := range t.Includes {
		s.include(c, p, inc, 0)
	}
	return c
}

// include adds what one #include of the file at from brings in: the
// included file and, below it, what that file includes. Each file is read
// once per closure, where it is first met.
func (s *Snapshot) include(c *closure, from string, inc pp.Include, depth int) {
	p, absent := s.resolveInclude(from, inc)
	// The same few headers are in the closures of most units, each
	// path made anew by each resolution: one string of each is kept.
	for _, a := range absent {
		c.absent = append(c.absent, unique.Make(a).Value())
	}
	if p == "" {
		c.complete = false
		return
	}
	if c.seen[p] {
		return
	}
	p = unique.Make(p).Value()
	c.seen[p] = true
	c.deps = append(c.deps, p)
	t := s.header(p).table
	if t == nil {
		c.complete = false
		return
	}
	if depth < maxIncludeDepth {
		for _, sub := range t.Includes {
			s.include(c, p, sub, depth+1)
		}
	} else {
		c.complete = false
	}
	c.order = append(c.order, t)
}

// baseState is the driver's force-included header as one generation sees
// it: the macro layers it gives every file, and the files it was read
// from.
type baseState struct {
	// layers are its tables in the order their definitions take effect.
	layers []*pp.Table
	// ok is true when the header and everything it includes was read.
	ok bool
	// unset says no include_file was given: what the driver forces into
	// every file is unknown, which is all that makes ok false.
	unset bool
	// deps are the files read, absent the places an include was looked
	// for and not found.
	deps, absent []string
}

func (b *baseState) touchedBy(changed map[string]struct{}) bool {
	for _, list := range [][]string{b.deps, b.absent} {
		for _, p := range list {
			if _, hit := changed[p]; hit {
				return true
			}
		}
	}
	return false
}

func (s *Snapshot) baseState() *baseState {
	return s.base.load(func() *baseState {
		inc := s.cfg.IncludeFile
		if !s.HasLib() {
			return &baseState{}
		}
		if inc == "" {
			return &baseState{ok: s.cfg.DriverKnown(config.KeyIncludeFile), unset: !s.cfg.DriverKnown(config.KeyIncludeFile)}
		}
		p, absent := s.locate(path.Clean(inc))
		t := s.header(p).table
		if t == nil {
			if len(absent) == 0 && p != "" && !s.isStub(p) {
				absent = []string{p}
			}
			return &baseState{absent: absent}
		}
		c := s.includes(p, t)
		return &baseState{
			layers: append(c.order, t), ok: c.complete,
			deps: c.deps, absent: append(absent, c.absent...),
		}
	})
}

// ----------------------------------------------------------------- units

// analysis is one file read for linting: tokens, tree, and its #define
// environment.
type analysis struct {
	sum  uint64 // textSum of the source
	tok  *token.File
	sel  *pp.Selection
	env  *pp.Env
	tree *syntax.File
	inc  *closure
	base *baseState
}

// analyze reads and parses p; nil when it cannot be read. An object's
// source is read as the driver compiles it: after the force-included
// header, with each #include taking effect where it stands. A header is
// read the same way, but nothing is concluded from what it does not see
// defined; and a file that is part of the force-included header is not
// read after itself.
func (s *Snapshot) analyze(p string) *analysis {
	src, err := s.fs.ReadFile(p)
	if err != nil {
		return nil
	}
	a := &analysis{sum: textSum(src), tok: lexer.Lex(p, src, s.dialect), base: s.baseState(), inc: newClosure(p)}
	a.env = pp.NewEnv(s.defined, s.override)
	a.env.Header = path.Ext(p) != ".c"
	a.env.Complete = true
	if !slices.Contains(a.base.deps, p) {
		a.env.Add(a.base.layers...)
		a.env.Complete = a.base.ok
	}
	a.sel = a.env.Scan(a.tok, func(inc pp.Include) {
		n := len(a.inc.order)
		s.include(a.inc, p, inc, 0)
		a.env.Add(a.inc.order[n:]...)
		a.env.Complete = a.env.Complete && a.inc.complete
	})
	a.tree = syntax.Parse(a.tok, syntax.Options{Env: a.env, Sel: a.sel})
	return a
}

// facts derives the unit from an analysis.
func (s *Snapshot) facts(a *analysis) *Unit {
	f, tree, p := a.tok, a.tree, a.tok.Path
	deps := make([]string, 0, len(a.inc.deps)+len(a.base.deps))
	deps = append(append(deps, a.inc.deps...), a.base.deps...)
	u := &Unit{
		Path: p, LibPath: s.LibPath(p), Stub: s.isStub(p),
		Partial: tree.Partial || a.env.Own().Partial,
		Deps:    deps,
		gen:     s.gen,
		sum:     a.sum,
		incs:    len(a.inc.deps),
		absent:  slices.Clone(a.inc.absent),
	}
	for _, e := range tree.Errors {
		if e.Severe {
			u.Partial = true
		}
	}

	// Includes: provably missing only where the lib is known completely.
	for _, inc := range a.env.Own().Includes {
		if _, ok := s.ResolveInclude(p, inc); ok {
			continue
		}
		u.Partial = true
		if s.includeProvablyMissing(p, inc) {
			t := f.Tokens[inc.Tok]
			u.BadIncludes = append(u.BadIncludes, BadInclude{Raw: inc.Raw(), Off: t.Off, End: t.End})
		} else {
			u.UnknownIncludes++
		}
	}
	if !a.env.Complete && len(u.BadIncludes) == 0 && u.UnknownIncludes == 0 && !(a.base.unset && a.inc.complete) {
		u.UnknownIncludes = 1 // an include of an include failed
	}

	collectDecls(u, tree)
	for _, d := range tree.Decls {
		if inh, ok := d.(*syntax.InheritDecl); ok {
			u.Inherits = append(u.Inherits, s.resolveInherit(tree, inh, u.LibPath, a.env))
		}
	}
	// The file's own variables, the last declaration of a name winning;
	// then those of included code where the file has none of the name.
	u.Vars = sortVars(u.Vars, true)
	own := len(u.Vars)

	// Code in included files belongs to the including program: a
	// function of the file's own wins, unless it is a prototype only and
	// the included one is not.
	for _, dep := range a.inc.deps[1:] {
		code := s.header(dep).code
		if code == nil {
			continue
		}
		if lp := s.LibPath(dep); len(code.Funcs) > 0 && lp != "" {
			u.code = append(u.code, lp)
		}
		u.Funcs = append(u.Funcs, code.Funcs...)
		u.Vars = append(u.Vars, code.Vars...)
		if code.Partial {
			u.Partial = true
		}
	}
	u.Funcs = sortFuncs(u.Funcs)
	if len(u.Vars) > own {
		u.Vars = sortVars(u.Vars, false)
	}
	u.Inherits = slices.Clone(u.Inherits)
	if s.uses != nil && !u.Stub {
		u.setUses(s.uses(&Parsed{Unit: u, Tok: f, Tree: tree, Env: a.env}, s))
	}
	return u
}

func (s *Snapshot) resolveInherit(tree *syntax.File, d *syntax.InheritDecl, libPath string, env *pp.Env) InheritRef {
	ref := InheritRef{Raw: tree.Text(d.Path), Off: tree.Off(d.Path), End: tree.End(d.Path), Private: d.Private}
	if d.Label != nil {
		ref.Label = d.Label.Name
	}
	if libPath == "" {
		return ref
	}
	first, last := d.Path.Span()
	val, ok := env.EvalString(pp.FileTokens(tree.Tok, first, last))
	if !ok || val == "" {
		return ref
	}
	ref.Path = s.ObjectPath(val, libPath)
	ref.Resolved = true
	return ref
}

// guard runs f, which analyzes p. A bug in the analysis of one file must
// not take down a run over a hundred thousand: the panic is recorded and
// the file is treated as unreadable.
func (s *Snapshot) guard(p string, f func()) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			s.ix.mu.Lock()
			s.failed[p] = fmt.Sprint(r)
			s.ix.mu.Unlock()
			ok = false
		}
	}()
	f()
	return true
}

// unit returns the facts for the file at p. If this call is the one that
// builds them, the analysis they were derived from is left in *a.
func (s *Snapshot) unit(p string, a **analysis) *Unit {
	return s.units.load(p, func() (u *Unit) {
		s.guard(p, func() {
			if *a = s.analyze(p); *a != nil {
				u = s.facts(*a)
			}
		})
		if u == nil {
			*a = nil
		}
		return u
	}, func(u *Unit, complete func()) {
		// A unit becomes complete and known to the reverse edges in one
		// step, so Invalidate never sees one without the other.
		s.ix.mu.Lock()
		complete()
		s.ix.built(s, p, u)
		s.ix.mu.Unlock()
	})
}

// Unit returns the indexed facts for the object at a lib path, or nil
// when no file backs it.
func (s *Snapshot) Unit(libPath string) *Unit {
	if !s.HasLib() || libPath == "" {
		return nil
	}
	return s.UnitAt(s.ObjectFile(libPath))
}

// UnitAt returns the indexed facts for the file at p, or nil if it
// cannot be read.
func (s *Snapshot) UnitAt(p string) *Unit {
	if p == "" {
		return nil
	}
	var a *analysis
	return s.unit(s.clean(p), &a)
}

// Parsed is a Unit together with its token stream, tree and #define
// environment, for the file being linted.
type Parsed struct {
	Unit *Unit
	Tok  *token.File
	Tree *syntax.File
	Env  *pp.Env
}

// ErrChanged says a file's text is no longer the text this generation
// of the index read: it changed, and the index has not been told
// (Invalidate) or has moved on since.
var ErrChanged = errors.New("the file changed since it was indexed")

// Parse lexes and parses the file at p for linting and returns its tree
// together with its facts. The tree is not kept by the index. The facts
// are built at most once per generation: a file this generation already
// knows is parsed for its tree only, and the tree is of the same text as
// the facts or there is none. It fails when the file cannot be read, with
// the file system's error; when its text is not what the generation read,
// with ErrChanged; and when its analysis crashes, with an
// *InternalError.
func (s *Snapshot) Parse(p string) (*Parsed, error) {
	p = s.clean(p)
	var a *analysis
	u := s.unit(p, &a)
	if u != nil && a == nil {
		s.guard(p, func() { a = s.analyze(p) })
		if a != nil && a.sum != u.sum {
			return nil, &fs.PathError{Op: "read", Path: p, Err: ErrChanged}
		}
	}
	if u != nil && a != nil {
		return &Parsed{Unit: u, Tok: a.tok, Tree: a.tree, Env: a.env}, nil
	}
	s.ix.mu.RLock()
	crash, crashed := s.failed[p]
	s.ix.mu.RUnlock()
	if crashed {
		return nil, &InternalError{Path: p, Panic: crash}
	}
	if _, err := s.fs.ReadFile(p); err != nil {
		return nil, err
	}
	return nil, &fs.PathError{Op: "read", Path: p, Err: ErrChanged}
}

// IncludeDeclares reports whether the file an #include resolves to, or
// anything it includes in turn, declares a function or a variable. It
// returns the path of the first such file.
func (s *Snapshot) IncludeDeclares(from string, inc pp.Include) (string, bool) {
	p, ok := s.ResolveInclude(from, inc)
	if !ok {
		return "", false
	}
	t := s.header(p).table
	if t == nil {
		return "", false
	}
	for _, dep := range s.includes(p, t).deps {
		if code := s.header(dep).code; code != nil && code.Declares {
			return dep, true
		}
	}
	return "", false
}

// ------------------------------------------------------------------ scan

// Files lists the .c files under dir (a directory on disk) that the
// configuration does not exclude. A listing of the whole lib also serves
// the whole-lib scan, which then need not list it again.
func (s *Snapshot) Files(ctx context.Context, dir string) ([]string, error) {
	var files []string
	if err := s.Walk(ctx, dir, func(p string) { files = append(files, s.clean(p)) }); err != nil {
		return nil, err
	}
	if ix := s.ix; s.clean(dir) == s.scanRoot() {
		for _, p := range files {
			s.covered.getOrPut(p, func() bool { return true }) // what inScope decides
		}
		ix.mu.Lock()
		if ix.cur == s && !ix.walked {
			ix.walked, ix.todo = true, map[string]struct{}{}
			for _, p := range files {
				if _, ok := s.units.get(p); !ok {
					ix.todo[p] = struct{}{}
				}
			}
		}
		ix.mu.Unlock()
	}
	return files, nil
}

// Scanned reports whether the whole-lib scan is complete for this
// generation: who inherits a file is then answered without reading
// anything.
func (s *Snapshot) Scanned() bool {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	return s.ix.cur == s && s.ix.walked && len(s.ix.todo) == 0
}

// ScannedFiles returns the number of files the whole-lib scan covers when
// it is complete for this generation, and 0 when it is not.
func (s *Snapshot) ScannedFiles() int {
	if !s.Scanned() {
		return 0
	}
	n := 0
	for p := range s.units.complete() {
		if s.inScope(p) {
			n++
		}
	}
	return n
}

// Scan indexes every .c file of the lib that the configuration does not
// exclude, in parallel. Only questions about who inherits a file need it.
// What an earlier scan indexed and no change has touched is not read
// again, so after the first scan this is as cheap as the edits since.
//
// It reports whether the scan is complete for this generation. It is not
// when there is no lib, when ctx was cancelled with files still to index,
// or when the index has moved on to a newer generation. A scan that did
// not complete is not an answer; it leaves what it did index for the next
// one.
func (s *Snapshot) Scan(ctx context.Context) bool {
	ix := s.ix
	root := s.scanRoot()
	if root == "" {
		return false
	}
	if s.Scanned() {
		return true
	}
	select {
	case ix.scanning <- struct{}{}:
		defer func() { <-ix.scanning }()
	case <-ctx.Done():
		return false
	}

	if !s.Current() {
		return false
	}
	ix.mu.RLock()
	walked := ix.walked
	ix.mu.RUnlock()
	if !walked {
		if _, err := s.Files(ctx, root); err != nil {
			return false
		}
	}

	ix.mu.RLock()
	todo := slices.Collect(maps.Keys(ix.todo))
	ix.mu.RUnlock()
	// A scan the index has moved on from indexes nothing more: nobody
	// will ask this generation who inherits what.
	par.Each(ctx, todo, func(p string) {
		if s.Current() {
			s.UnitAt(p)
		}
	})
	return s.Scanned()
}
