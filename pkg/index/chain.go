package index

import (
	"context"
	"slices"

	"github.com/mmcdole/dgdtools/pkg/config"
)

// Chain is the flattened inherit view of one object.
//
// A chain shares the entries of the chains of what the object inherits:
// an object's chain is mostly its parents' functions, and a whole lib of
// chains would otherwise hold a copy of std/room's in each room. The
// entries are not to be changed.
type Chain struct {
	Funcs map[string]*ChainFunc
	Vars  map[string]*ChainVar
	// Partial: some inherit could not be resolved or loaded, or some
	// program in the chain has an incomplete symbol table. A name absent
	// from a partial chain is Unknown, not Missing.
	Partial bool
	// Unseen: some object of the chain is not known at all: an inherit
	// that could not be resolved or loaded. It is the part of Partial
	// that says a whole program may be missing, with whatever it
	// redefines, rather than an entry of a program that was read.
	Unseen  bool
	Objects []string // lib paths in the chain, leaf last
}

// ChainFunc is a function as an object's chain provides it.
type ChainFunc struct {
	FuncInfo
	DefinedIn string // lib path of the object that provides it
}

// ChainVar is a global variable as an object's chain provides it.
type ChainVar struct {
	VarInfo
	DefinedIn string // lib path of the object that declares it
}

// Chain returns the flattened chain for a lib path.
func (s *Snapshot) Chain(libPath string) *Chain {
	ch, _ := s.chain(libPath, map[string]bool{}, true)
	return ch
}

// ownChain is Chain for the lint of the object at libPath: the chain is
// made for it, and kept for the snapshot only if it is kept already or
// as the chain of what an object inherits. A run over a whole lib lints
// every object, most of which nothing inherits, and keeping each one's
// chain kept a map of its functions for every object of the lib: half
// the heap, on the VikingMUD lib.
func (s *Snapshot) ownChain(libPath string) *Chain {
	ch, _ := s.chain(libPath, map[string]bool{}, false)
	return ch
}

// chain also reports whether an inheritance cycle was met below libPath;
// results computed inside a cycle are not memoized. Without keep, the
// chain made for libPath itself is not kept (ownChain); those of what it
// inherits are.
func (s *Snapshot) chain(libPath string, visiting map[string]bool, keep bool) (*Chain, bool) {
	if ch, ok := s.chains.get(libPath); ok {
		return ch, false
	}
	ch := &Chain{Funcs: map[string]*ChainFunc{}, Vars: map[string]*ChainVar{}}
	if visiting[libPath] {
		ch.Partial, ch.Unseen = true, true // inheritance cycle; the driver rejects it
		return ch, true
	}
	u := s.Unit(libPath)
	if u == nil {
		ch.Partial, ch.Unseen = true, true
		return s.storeChain(libPath, ch, keep), false
	}
	cyc := false
	if u.Partial {
		ch.Partial = true
	}
	visiting[libPath] = true
	for _, ref := range u.Inherits {
		if !ref.Resolved {
			ch.Partial, ch.Unseen = true, true
			continue
		}
		sub, c := s.chain(ref.Path, visiting, true)
		cyc = cyc || c
		if sub.Partial {
			ch.Partial = true
		}
		if sub.Unseen {
			ch.Unseen = true
		}
		// Private functions stay in the chain flagged Private so rules
		// can explain why a call fails. Everything below a `private
		// inherit` is call_other-invisible to inheritors too. A prototype
		// from one sibling never clobbers another sibling's definition.
		for name, fn := range sub.Funcs {
			if fn.PrototypeOnly {
				if old, ok := ch.Funcs[name]; ok && !old.PrototypeOnly {
					continue
				}
			}
			if ref.Private && !fn.Private {
				private := *fn
				private.Private = true
				fn = &private
			}
			ch.Funcs[name] = fn
		}
		for name, v := range sub.Vars {
			if ref.Private && !v.Private {
				private := *v
				private.Private = true
				v = &private
			}
			ch.Vars[name] = v
		}
		ch.Objects = append(ch.Objects, sub.Objects...)
	}
	delete(visiting, libPath)
	for _, fi := range u.Funcs {
		if fi.PrototypeOnly {
			if old, ok := ch.Funcs[fi.Name]; ok && !old.PrototypeOnly {
				continue
			}
		}
		ch.Funcs[fi.Name] = &ChainFunc{FuncInfo: fi, DefinedIn: libPath}
	}
	for _, vi := range u.Vars {
		ch.Vars[vi.Name] = &ChainVar{VarInfo: vi, DefinedIn: libPath}
	}
	ch.Objects = append(ch.Objects, libPath)
	if cyc && len(visiting) > 0 {
		return ch, true
	}
	return s.storeChain(libPath, ch, keep), false
}

// ChainFunc returns the function called name of the chain of the object
// at libPath: what Chain(libPath).Funcs[name] is. Unless that chain has
// been flattened already, it is found from the object's own functions
// and the chains of what it inherits, without flattening the object's:
// a whole lib shares the chains of its parents, and a question asked of
// every object of the lib (a search for references) must not keep a
// chain for each.
func (s *Snapshot) ChainFunc(libPath, name string) (ChainFunc, bool) {
	if ch, ok := s.chains.get(libPath); ok {
		if fn, ok := ch.Funcs[name]; ok {
			return *fn, true
		}
		return ChainFunc{}, false
	}
	u := s.Unit(libPath)
	if u == nil {
		return ChainFunc{}, false
	}
	// As chain merges them: each inherited object in turn, then the
	// object's own; a prototype hides no definition.
	var out ChainFunc
	found := false
	take := func(fn ChainFunc) {
		if fn.PrototypeOnly && found && !out.PrototypeOnly {
			return
		}
		out, found = fn, true
	}
	for _, ref := range u.Inherits {
		if !ref.Resolved {
			continue
		}
		if fn, ok := s.Chain(ref.Path).Funcs[name]; ok {
			f := *fn
			if ref.Private {
				f.Private = true
			}
			take(f)
		}
	}
	if fi, ok := u.Func(name); ok {
		take(ChainFunc{FuncInfo: fi, DefinedIn: libPath})
	}
	return out, found
}

// ChainVar returns the global variable called name of the chain of the
// object at libPath: what Chain(libPath).Vars[name] is, found as
// ChainFunc finds a function, without flattening the object's chain.
func (s *Snapshot) ChainVar(libPath, name string) (ChainVar, bool) {
	if ch, ok := s.chains.get(libPath); ok {
		if v, ok := ch.Vars[name]; ok {
			return *v, true
		}
		return ChainVar{}, false
	}
	u := s.Unit(libPath)
	if u == nil {
		return ChainVar{}, false
	}
	var out ChainVar
	found := false
	for _, ref := range u.Inherits {
		if !ref.Resolved {
			continue
		}
		if v, ok := s.Chain(ref.Path).Vars[name]; ok {
			out, found = *v, true
			if ref.Private {
				out.Private = true
			}
		}
	}
	if vi, ok := u.Var(name); ok {
		out, found = ChainVar{VarInfo: vi, DefinedIn: libPath}, true
	}
	return out, found
}

// ChainUnseen reports whether the chain of the object at libPath has an
// object that is not known at all: what Chain(libPath).Unseen is, found
// as ChainFunc finds a function.
func (s *Snapshot) ChainUnseen(libPath string) bool {
	if ch, ok := s.chains.get(libPath); ok {
		return ch.Unseen
	}
	u := s.Unit(libPath)
	if u == nil {
		return true
	}
	for _, ref := range u.Inherits {
		if !ref.Resolved || s.Chain(ref.Path).Unseen {
			return true
		}
	}
	return false
}

// storeChain keeps ch as libPath's chain, with keep, unless one was
// stored meanwhile.
func (s *Snapshot) storeChain(libPath string, ch *Chain, keep bool) *Chain {
	if !keep {
		return ch
	}
	return s.chains.getOrPut(libPath, func() *Chain { return ch })
}

// autoState is what the configured auto objects give every object.
type autoState struct {
	funcs map[string]ChainFunc
	// unseen says an auto object, or an object of its chain, is not
	// known: what it gives every object is not all in funcs.
	unseen bool
	// unset says no auto_object was given: whether there is one, and
	// what it gives every object, is unknown.
	unset bool
	// members are the lib paths of the auto objects, of the programs they
	// inherit, and of the files those include as code.
	members map[string]bool
}

func (s *Snapshot) autoState() *autoState {
	return s.auto.load(func() *autoState {
		a := &autoState{funcs: map[string]ChainFunc{}, members: map[string]bool{}}
		a.unset = !s.cfg.DriverKnown(config.KeyAutoObject)
		for _, auto := range s.cfg.AutoObjects {
			ch := s.Chain(NormalizeLibPath(auto, "/"))
			a.unseen = a.unseen || ch.Unseen
			for name, fn := range ch.Funcs {
				f := *fn
				f.DefinedIn = "(auto object)"
				a.funcs[name] = f
			}
			for _, o := range ch.Objects {
				a.members[o] = true
				if u := s.Unit(o); u != nil {
					for _, c := range u.code {
						a.members[c] = true
					}
				}
			}
		}
		return a
	})
}

// AutoFunc returns the function called name that the configured auto
// objects give every object, when they have one. Its DefinedIn is not a
// lib path; the declaration's own location (Loc.Path) is where it is.
func (s *Snapshot) AutoFunc(name string) (ChainFunc, bool) {
	f, ok := s.autoState().funcs[name]
	return f, ok
}

// AutoFuncs returns the functions that the configured auto objects give
// every object, by name. The map is the index's own: it is not to be
// changed.
func (s *Snapshot) AutoFuncs() map[string]ChainFunc { return s.autoState().funcs }

// AutoUnset reports whether no auto_object was given, so that any
// function may be the auto object's. What the auto object gives is then
// left out of what is known (AutoFunc has none), and a function looked
// for by name is not known to be missing.
func (s *Snapshot) AutoUnset() bool { return s.autoState().unset }

// AutoUnseen reports whether a configured auto object, or an object it
// inherits, is not known at all (Chain.Unseen): it may give every object
// functions that AutoFunc does not have, and a function of the driver's
// that it redefines is then not known to be redefined.
func (s *Snapshot) AutoUnseen() bool { return s.autoState().unseen }

// AutoMember reports whether the object at libPath is part of what the
// auto objects are made of: an auto object, a program one inherits, a
// file one includes as code. Such a program does not inherit the auto
// object: what AutoFunc gives every other object is not given to it.
func (s *Snapshot) AutoMember(libPath string) bool { return s.autoState().members[libPath] }

// usersOf returns the objects whose program includes libPath's: those
// that inherit it, however indirectly, and those that #include it as
// code. It needs the whole-lib scan and runs it if it has not completed.
// The second result is false when the answer cannot be had: the scan was
// cancelled, or the index has moved on to a newer generation.
func (s *Snapshot) usersOf(ctx context.Context, libPath string) ([]string, bool) {
	if !s.Scan(ctx) {
		return nil, false
	}
	return s.users(libPath)
}

// Users returns the objects whose program includes libPath's: those
// that inherit it, however indirectly, and those that #include it as
// code, sorted. It needs the whole-lib scan and runs it if it has not
// completed. The second result is false when the answer cannot be had:
// the scan was cancelled, or the index has moved on to a newer
// generation. In a partial checkout the answer is the checkout's.
func (s *Snapshot) Users(ctx context.Context, libPath string) ([]string, bool) {
	return s.usersOf(ctx, libPath)
}

// Includers returns the files that #include the file at path (a path on
// disk), directly or through other files, sorted: those whose facts were
// built from it. It needs the whole-lib scan and runs it if it has not
// completed; the second result is false when the answer cannot be had,
// as for Users.
func (s *Snapshot) Includers(ctx context.Context, path string) ([]string, bool) {
	if !s.Scan(ctx) {
		return nil, false
	}
	ix := s.ix
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if ix.cur != s || !ix.walked || len(ix.todo) > 0 {
		return nil, false
	}
	var out []string
	for key := range ix.builtFrom[path] {
		if u, _ := s.units.get(key); u != nil && u.Path != path {
			out = append(out, u.Path)
		}
	}
	slices.Sort(out)
	return out, true
}

// users answers usersOf from the index's reverse edges, which are those
// of this generation only while it is the current one.
func (s *Snapshot) users(libPath string) ([]string, bool) {
	ix := s.ix
	ix.mu.RLock()
	if ix.cur != s || !ix.walked || len(ix.todo) > 0 {
		ix.mu.RUnlock()
		return nil, false
	}
	var out []string
	seen := map[string]bool{libPath: true}
	for queue := []string{libPath}; len(queue) > 0; queue = queue[1:] {
		for key := range ix.children[queue[0]] {
			if u, _ := s.units.get(key); u != nil && !seen[u.LibPath] {
				seen[u.LibPath] = true
				out = append(out, u.LibPath)
				queue = append(queue, u.LibPath)
			}
		}
	}
	ix.mu.RUnlock()
	slices.Sort(out)
	return out, true
}

// LookupState classifies a callable lookup.
type LookupState uint8

const (
	LookupUnknown     LookupState = iota // cannot verify: never report
	LookupFound                          // in the target's own chain (Fn valid)
	LookupFoundInUser                    // provided by an inheriting object
	LookupMissing                        // provably absent
)

// Lookup is the result of resolving one name in one object.
type Lookup struct {
	State LookupState
	Fn    ChainFunc // valid when State == LookupFound
	// Searched says how far a LookupMissing answer reaches, and Users
	// how many inheritors were searched (SearchedUsers).
	Searched Searched
	Users    int
}

// Searched says where a function that is LookupMissing was looked for.
type Searched uint8

const (
	// SearchedChain: the object's own inherit chain, which is all there
	// is to it.
	SearchedChain Searched = iota
	// SearchedStub: the object is known from a profile's stub, which
	// lists public functions only: the function may be private there.
	SearchedStub
	// SearchedUsers: the chain and every object that inherits or
	// includes the file.
	SearchedUsers
	// SearchedCheckout: the chain and the checkout, which has no
	// inheritors of the file; inheritors elsewhere in the lib cannot be
	// seen.
	SearchedCheckout
)

// maxUserChains bounds the fallback search through inheritors' chains.
const maxUserChains = 20

// usersFunc answers usersOf, or declines to.
type usersFunc func(ctx context.Context, libPath string) ([]string, bool)

// chainFunc returns the chain of the object at a lib path, as Chain
// does (Query.chain).
type chainFunc func(libPath string) *Chain

// lookupCallable resolves function name fn in the object at target.
//
// self says the receiver is this_object() in code that lives in target:
// the running object may then be any inheritor (or includer) of target,
// so a name missing from target's own chain is looked for in its users
// too — the callback a module registers is routinely provided by a
// sibling module or the leaf. When the receiver is the named object
// itself ("/path"->fn()), only its own chain counts.
//
// A lookup made after ctx was cancelled reads nothing and is unknown.
func (s *Snapshot) lookupCallable(ctx context.Context, target, fn string, self bool, usersOf usersFunc, chainOf chainFunc) Lookup {
	if !s.HasLib() || target == "" || ctx.Err() != nil {
		return Lookup{State: LookupUnknown}
	}
	ch := chainOf(target)
	if f, ok := ch.Funcs[fn]; ok {
		return Lookup{State: LookupFound, Fn: *f}
	}
	auto := s.autoState()
	if f, ok := auto.funcs[fn]; ok {
		return Lookup{State: LookupFound, Fn: f}
	}
	if ch.Partial || auto.unset {
		return Lookup{State: LookupUnknown}
	}
	if !self {
		// A profile stub lists an object's public interface only.
		if u := s.Unit(target); u != nil && u.Stub {
			return Lookup{State: LookupMissing, Searched: SearchedStub}
		}
		return Lookup{State: LookupMissing}
	}
	// Code in the auto object's own chain runs as every object in the
	// lib: this_object() there can be anything.
	if auto.members[target] {
		return Lookup{State: LookupUnknown}
	}
	users, ok := usersOf(ctx, target)
	if !ok {
		return Lookup{State: LookupUnknown}
	}
	if len(users) == 0 {
		if s.partial {
			// Only the checkout could be searched for inheritors.
			return Lookup{State: LookupMissing, Searched: SearchedCheckout}
		}
		return Lookup{State: LookupMissing, Searched: SearchedUsers}
	}
	for _, u := range users {
		if unit := s.Unit(u); unit != nil {
			if _, ok := unit.Func(fn); ok {
				return Lookup{State: LookupFoundInUser}
			}
		}
	}
	checked := 0
	for _, u := range users {
		if checked >= maxUserChains {
			return Lookup{State: LookupUnknown} // bounded: cannot verify
		}
		uch := chainOf(u)
		checked++
		if uch.Partial {
			return Lookup{State: LookupUnknown}
		}
		if _, ok := uch.Funcs[fn]; ok {
			return Lookup{State: LookupFoundInUser}
		}
	}
	return Lookup{State: LookupMissing, Searched: SearchedUsers, Users: len(users)}
}

// providedByUsers reports whether any inheritor/includer of libPath
// supplies a real definition of fn. It returns true (assume provided)
// whenever the bounded search cannot decide.
func (s *Snapshot) providedByUsers(ctx context.Context, libPath, fn string, usersOf usersFunc, chainOf chainFunc) bool {
	users, ok := usersOf(ctx, libPath)
	if !ok {
		return true
	}
	if len(users) == 0 {
		return false
	}
	for _, u := range users {
		if unit := s.Unit(u); unit != nil {
			if f, ok := unit.Func(fn); ok && !f.PrototypeOnly {
				return true
			}
		}
	}
	checked := 0
	for _, u := range users {
		if checked >= maxUserChains {
			return true
		}
		uch := chainOf(u)
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

// symbolState says what a call by name finds in an object's table of
// functions (DGD's symbol table, which call_out, call_other and every
// callback search): a function that is not private, nothing, or that it
// cannot be told.
type symbolState uint8

const (
	symNone    symbolState = iota // no function of the name is in the table
	symFound                      // a function that is not private is
	symUnknown                    // some program of the chain is not known in full
)

// PrivateOnly reports whether a call by name of fn made on this_object()
// in the file itself (call_out("fn"), a registrar's callback) finds
// nothing but a private function: DGD's table of functions, which such
// a call searches, has no private function in it (Control::makeSymbols),
// nor any function of an object inherited privately, and a private
// redefinition of an inherited function leaves the inherited one there.
// It returns the private function the file's chain has under the name,
// and whether it is private there because it is inherited privately.
//
// It reports false whenever that is not certain: the chain or one of the
// objects that inherit or include the file (as which this_object() may
// run) is not known in full, has a function of the name that is not
// private or declarations of it that DGD refuses (FuncInfo.Refused), or
// cannot be listed; the file is part of the auto object, or
// the auto object has a function of the name; the lib is a partial
// checkout, whose inheritors elsewhere cannot be seen.
func (q *Query) PrivateOnly(ctx context.Context, fn string) (f ChainFunc, inherited, ok bool) {
	s, libPath := q.s, q.unit.LibPath
	if !s.HasLib() || s.partial || libPath == "" || ctx.Err() != nil {
		return ChainFunc{}, false, false
	}
	cf, found := q.chain(libPath).Funcs[fn]
	if !found || !cf.Private || cf.Refused {
		return ChainFunc{}, false, false
	}
	auto := s.autoState()
	if _, has := auto.funcs[fn]; has || auto.unseen || auto.unset || auto.members[libPath] {
		return ChainFunc{}, false, false
	}
	memo := map[string]symbolState{}
	if s.inSymbols(libPath, fn, memo) != symNone {
		return ChainFunc{}, false, false
	}
	users, known := q.usersOf(ctx, libPath)
	if !known {
		return ChainFunc{}, false, false
	}
	for _, u := range users {
		if ctx.Err() != nil || s.inSymbols(u, fn, memo) != symNone {
			return ChainFunc{}, false, false
		}
	}
	if u := s.Unit(cf.DefinedIn); u != nil {
		if own, has := u.Func(fn); has && !own.Private {
			inherited = true
		}
	}
	return *cf, inherited, true
}

// inSymbols says what a call by name of fn finds in the table of functions
// of the object at libPath: its own functions that are not private, and
// those of what it inherits that are not inherited privately. memo holds
// the answers of the programs already seen; one inherited twice is read
// once, and a cycle is unknown.
func (s *Snapshot) inSymbols(libPath, fn string, memo map[string]symbolState) symbolState {
	if r, ok := memo[libPath]; ok {
		return r
	}
	memo[libPath] = symUnknown
	u := s.Unit(libPath)
	if u == nil || u.Partial || u.Stub {
		return symUnknown
	}
	if f, ok := u.Func(fn); ok && f.Refused {
		return symUnknown
	} else if ok && !f.Private {
		memo[libPath] = symFound
		return symFound
	}
	r := symNone
	for _, ref := range u.Inherits {
		switch {
		case !ref.Resolved:
			r = symUnknown
		case ref.Private:
			// Not in the table, whatever the object has.
		default:
			if sub := s.inSymbols(ref.Path, fn, memo); sub != symNone {
				r = sub
			}
		}
		if r == symFound {
			break
		}
	}
	memo[libPath] = r
	return r
}
