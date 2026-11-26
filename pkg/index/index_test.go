package index

import (
	"context"
	"fmt"
	"io/fs"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/fileset"
	"github.com/mmcdole/dgdtools/pkg/pp"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// probeFS counts reads and walks, and can hold reads open.
type probeFS struct {
	vfs.FS
	mu    sync.Mutex
	reads map[string]int
	total int
	walks int
	// hold, when set, picks the reads that block, by path and by how many
	// reads came before: entered is closed when the first of them arrives
	// and they all continue when release is closed.
	hold             func(name string, before int) bool
	entered, release chan struct{}
	arrived          bool
}

// holdReads arms hold.
func (p *probeFS) holdReads(hold func(name string, before int) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hold, p.entered, p.release, p.arrived = hold, make(chan struct{}), make(chan struct{}), false
}

func (p *probeFS) ReadFile(name string) ([]byte, error) {
	p.mu.Lock()
	if p.reads == nil {
		p.reads = map[string]int{}
	}
	p.reads[name]++
	held := p.hold != nil && p.hold(name, p.total)
	p.total++
	release := p.release
	if held && !p.arrived {
		p.arrived = true
		close(p.entered)
	}
	p.mu.Unlock()
	if held {
		<-release
	}
	return p.FS.ReadFile(name)
}

func (p *probeFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	p.mu.Lock()
	p.walks++
	p.mu.Unlock()
	return p.FS.WalkDir(root, fn)
}

func (p *probeFS) count(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reads[name]
}

func (p *probeFS) reset() {
	p.mu.Lock()
	p.reads, p.total, p.walks = nil, 0, 0
	p.mu.Unlock()
}

// lib_ is a small whole lib under /lib: files on "disk", an overlay for
// edits, and a probe on what the index reads.
type lib_ struct {
	ix   *Index
	disk vfs.Mem
	ov   *vfs.Overlay
	fs   *probeFS
}

func newLib(t *testing.T, files map[string]string, tweak ...func(*config.Config)) *lib_ {
	t.Helper()
	m := vfs.Mem{
		"/lib/include/std.h": "#define ROOM \"/std/room\"\n#define status int\n",
		"/lib/sys/auto.c":    "string object_name() { return \"x\"; }\nstatic void hidden_auto() { }\n",
		"/lib/sys/forced.h":  "#define TRUE 1\n",
	}
	for k, v := range files {
		m["/lib/"+k] = v
	}
	cfg := &config.Config{}
	cfg.Root = "/lib"
	cfg.IncludeDirs = []string{"/include"}
	cfg.IncludeFile = "/sys/forced.h"
	cfg.AutoObjects = []string{"/sys/auto"}
	for _, f := range tweak {
		f(cfg)
	}
	ov := vfs.NewOverlay(m)
	pfs := &probeFS{FS: ov}
	return &lib_{ix: New(cfg, Layout{Tree: fileset.Tree{Root: "/lib"}}, pfs), disk: m, ov: ov, fs: pfs}
}

func (l *lib_) set(t *testing.T, rel, src string) []string {
	t.Helper()
	l.ov.Set("/lib/"+rel, []byte(src))
	return l.ix.Invalidate("/lib/" + rel)
}

var bg = context.Background()

// ------------------------------------------------------------- facts

func TestUnitFacts(t *testing.T) {
	l := newLib(t, map[string]string{
		"std/room.c": "#include <std.h>\n" +
			"inherit \"/std/base\";\n" +
			"private inherit helper \"/std/helper\";\n" +
			"inherit BROKEN;\n" +
			"string *names;\nprivate static int count;\n" +
			"static varargs status query(string what, mixed extra...) { return 1; }\n" +
			"void proto(int a);\n",
		"std/base.c":   "void create() { }\n",
		"std/helper.c": "void help() { }\n",
	})
	s := l.ix.Snapshot()
	u := s.Unit("/std/room")
	require.NotNil(t, u)
	assert.Equal(t, "/lib/std/room.c", u.Path)
	assert.Equal(t, "/std/room", u.LibPath)
	assert.False(t, u.Stub)
	assert.Equal(t, []string{"/lib/std/room.c", "/lib/include/std.h", "/lib/sys/forced.h"}, u.Deps)

	q := funcOf(u, "query")
	assert.True(t, q.Static)
	assert.Equal(t, "static varargs status query(string what, mixed extra...)", q.Decl)
	assert.Equal(t, "status", q.Type)
	assert.Equal(t, 0, q.MinArgs)
	assert.Equal(t, -1, q.MaxArgs)
	assert.Equal(t, Loc{Path: "/lib/std/room.c", Off: q.Off, Line: 7, Col: 23}, q.Loc)
	assert.Equal(t, []string{"string what", "mixed extra..."}, Params(q.Decl))
	assert.True(t, funcOf(u, "proto").PrototypeOnly)

	names := varOf(u, "names")
	assert.Equal(t, "string *", names.Type)
	assert.Equal(t, Loc{Path: "/lib/std/room.c", Off: names.Off, Line: 5, Col: 9}, names.Loc)
	assert.True(t, varOf(u, "count").Private)
	assert.True(t, varOf(u, "count").Static)

	require.Len(t, u.Inherits, 3)
	assert.Equal(t, InheritRef{Raw: `"/std/base"`, Path: "/std/base", Resolved: true, Off: u.Inherits[0].Off, End: u.Inherits[0].End}, u.Inherits[0])
	assert.True(t, u.Inherits[1].Private)
	assert.Equal(t, "helper", u.Inherits[1].Label)
	assert.False(t, u.Inherits[2].Resolved, "a macro nobody defines")

	assert.Nil(t, s.Unit("/std/none"))
	assert.Nil(t, s.UnitAt("/lib/std/none.c"))
	assert.Same(t, u, s.UnitAt("/lib/std/../std/room.c"), "paths are cleaned")
}

// Of the declarations of a name, a unit keeps what the driver compiles:
// the first definition of a function, else its first prototype; the
// last declaration of a variable; the code of an included file where
// the file has nothing of the name, and a function of it where the file
// has a prototype only. Sorted by name, each once.
func TestUnitDeclsOfAName(t *testing.T) {
	l := newLib(t, map[string]string{
		"include/code.h": "int both;\nint hv;\nvoid hdef() { }\nvoid own_def() { }\nvoid hproto();\n",
		"obj/a.c": "#include <code.h>\n" +
			"void f();\nvoid f(int a) { }\nvoid f(int b) { }\n" +
			"void g() { }\nvoid g();\n" +
			"int v;\nstring v;\n" +
			"string both;\nvoid hdef();\nvoid own_def() { }\nvoid hproto() { }\n",
	})
	u := l.ix.Snapshot().Unit("/obj/a")
	require.NotNil(t, u)
	assert.Equal(t, []string{"f", "g", "hdef", "hproto", "own_def"}, funcNames(u))
	assert.Equal(t, []string{"both", "hv", "v"}, varNames(u))
	assert.Equal(t, "void f(int a)", funcOf(u, "f").Decl, "the first definition")
	assert.False(t, funcOf(u, "g").PrototypeOnly)
	assert.Equal(t, "string", varOf(u, "v").Type, "the last declaration")
	assert.Equal(t, "string", varOf(u, "both").Type, "the file's own")
	assert.Equal(t, "/lib/include/code.h", varOf(u, "hv").Path)
	assert.Equal(t, "/lib/include/code.h", funcOf(u, "hdef").Path, "a definition over the file's prototype")
	assert.Equal(t, "/lib/obj/a.c", funcOf(u, "own_def").Path, "the file's own definition")
	assert.Equal(t, "/lib/obj/a.c", funcOf(u, "hproto").Path)
	_, ok := u.Func("none")
	assert.False(t, ok)
	_, ok = u.Var("none")
	assert.False(t, ok)
}

func TestIncludes(t *testing.T) {
	l := newLib(t, map[string]string{
		"obj/defs.h":     "#define WHERE \"/std/here\"\n",
		"include/defs.h": "#define WHERE \"/std/there\"\n",
		"include/code.h": "int shared;\nvoid from_header() { }\n",
		"obj/a.c":        "#include \"defs.h\"\n#include <code.h>\ninherit WHERE;\n",
		"obj/b.c":        "#include <defs.h>\ninherit WHERE;\n",
		"obj/c.c":        "#include \"/obj/defs.h\"\n#include \"gone.h\"\n#include <gone2.h>\ninherit WHERE;\n",
		"obj/d.c":        "#include \"nest.h\"\n",
		"obj/nest.h":     "#include \"nest2.h\"\n",
		"std/here.c":     "", "std/there.c": "",
	})
	s := l.ix.Snapshot()
	a, b, c := s.Unit("/obj/a"), s.Unit("/obj/b"), s.Unit("/obj/c")
	assert.Equal(t, "/std/here", a.Inherits[0].Path, `"name" looks next to the file first`)
	assert.Equal(t, "/std/there", b.Inherits[0].Path, "<name> looks in the include dirs only")
	assert.Equal(t, "/std/here", c.Inherits[0].Path, "an absolute name is a lib path")

	assert.Contains(t, funcNames(a), "from_header", "code in a header belongs to the includer")
	assert.Contains(t, varNames(a), "shared")
	assert.Equal(t, "/lib/include/code.h", funcOf(a, "from_header").Path)
	assert.Equal(t, []string{"/include/code.h"}, a.code)
	assert.False(t, a.Partial)

	require.Len(t, c.BadIncludes, 2, "the lib is complete: a missing include is provably missing")
	assert.Equal(t, `"gone.h"`, c.BadIncludes[0].Raw)
	assert.Equal(t, `<gone2.h>`, c.BadIncludes[1].Raw)
	assert.True(t, c.Partial)
	assert.Equal(t, []string{"/lib/obj/defs.h", "/lib/obj/gone.h", "/lib/include/gone.h", "/lib/include/gone2.h"}, slices.Collect(c.watched),
		"the places an include was looked for are watched too")

	d := s.Unit("/obj/d")
	assert.Empty(t, d.BadIncludes)
	assert.Equal(t, 1, d.UnknownIncludes, "an include of an include failed")

	p, ok := s.ResolveInclude("/lib/obj/a.c", pp.Include{Name: "defs.h"})
	assert.True(t, ok)
	assert.Equal(t, "/lib/obj/defs.h", p)
	_, ok = s.ResolveInclude("/lib/obj/a.c", pp.Include{Name: "nope.h"})
	assert.False(t, ok)
	file, ok := s.IncludeDeclares("/lib/obj/a.c", pp.Include{Name: "code.h", Angle: true})
	assert.True(t, ok)
	assert.Equal(t, "/lib/include/code.h", file)
	_, ok = s.IncludeDeclares("/lib/obj/a.c", pp.Include{Name: "defs.h"})
	assert.False(t, ok)

	assert.Equal(t, 1, l.fs.count("/lib/include/code.h"), "a header is read once for its macros and its code")
	assert.Equal(t, 1, l.fs.count("/lib/sys/forced.h"))
}

// A file's conditionals are decided in the environment the driver
// compiles it in: after the force-included header, each #include taking
// effect where it stands.
func TestConditionsSeeIncludes(t *testing.T) {
	l := newLib(t, map[string]string{
		"include/opts.h": "#define WITH_LOG\n#define LEVEL 2\n",
		"obj/a.c": "#ifdef WITH_LOG\nvoid early() { }\n#endif\n" +
			"#include <opts.h>\n" +
			"#ifdef WITH_LOG\nvoid log() { }\n#else\nvoid nolog() { }\n#endif\n" +
			"#if TRUE && LEVEL > 1\nvoid high() { }\n#endif\n" +
			"#ifdef NOWHERE\nvoid never() { }\n#else\nvoid always() { }\n#endif\n",
		// The same conditions where a header cannot be found: a name that
		// was not seen may be defined there.
		"obj/b.c": "#include <missing.h>\n" +
			"#ifdef NOWHERE\nvoid maybe() { }\n#else\nvoid other() { }\n#endif\n",
		// A header is read on its own, whoever includes it.
		"include/cond.h": "#ifdef WITH_LOG\n#define LOGGER \"/obj/log\"\n#endif\n",
		"obj/c.c":        "#include <opts.h>\n#include <cond.h>\n#ifdef LOGGER\nvoid uses() { }\n#endif\n",
	})
	s := l.ix.Snapshot()

	a, err := s.Parse("/lib/obj/a.c")
	require.NoError(t, err)
	assert.Equal(t, []string{"always", "high", "log"}, funcNames(a.Unit))
	for _, i := range a.Tree.Sel.Sig {
		assert.False(t, a.Tree.Sel.Guessed(i), "every branch of a.c is decided")
	}
	assert.True(t, a.Env.Complete)

	b, err := s.Parse("/lib/obj/b.c")
	require.NoError(t, err)
	assert.Contains(t, funcNames(b.Unit), "maybe", "undecided: the first branch, as a guess")
	first, _ := b.Tree.Decls[0].Span()
	assert.True(t, b.Tree.Sel.Guessed(first))

	c, err := s.Parse("/lib/obj/c.c")
	require.NoError(t, err)
	assert.Contains(t, funcNames(c.Unit), "uses")
	first, _ = c.Tree.Decls[0].Span()
	assert.True(t, c.Tree.Sel.Guessed(first), "LOGGER is defined under a condition its header could not decide")
}

// A header is not compiled by itself: read for its own sake it is read
// like an object, but a name it does not see defined may be defined by
// whoever includes it. And the force-included header does not come before
// itself.
func TestHeadersReadAsUnits(t *testing.T) {
	l := newLib(t, map[string]string{
		"sys/forced.h":   "#ifndef FORCED_H\n#define FORCED_H\n#include <std.h>\n#define TRUE 1\nvoid from_forced();\n#endif\n",
		"include/std.h":  "#ifndef STD_H\n#define STD_H\n#define status int\nstatus from_std();\n#endif\n",
		"include/opt.h":  "#ifdef NDEBUG\nvoid quiet();\n#else\nvoid loud();\n#endif\n#if TRUE\nstatus sure();\n#endif\n",
		"obj/includes.c": "#include <opt.h>\n",
	})
	s := l.ix.Snapshot()

	for _, p := range []string{"/lib/sys/forced.h", "/lib/include/std.h"} {
		h, err := s.Parse(p)
		require.NoError(t, err, p)
		assert.Len(t, h.Tree.Decls, 1, "%s: its include guard is not already defined", p)
		assert.Empty(t, h.Tree.Errors, p)
	}

	opt, err := s.Parse("/lib/include/opt.h")
	require.NoError(t, err)
	assert.Equal(t, []string{"quiet", "sure"}, funcNames(opt.Unit))
	quiet, _ := opt.Tree.Decls[0].Span()
	sure, _ := opt.Tree.Decls[1].Span()
	assert.True(t, opt.Tree.Sel.Guessed(quiet), "NDEBUG may be defined by an includer")
	assert.False(t, opt.Tree.Sel.Guessed(sure), "what the header does see decides")
	assert.True(t, opt.Env.Complete, "its macros are all in view")

	// The object that includes it reads the header the same way.
	assert.Equal(t, []string{"quiet", "sure"}, funcNames(s.Unit("/obj/includes")))
}

// The tree of a file is of the text its facts were built from, or the
// file is refused: a change the index was not told about does not get
// mixed in.
func TestParseRefusesChangedText(t *testing.T) {
	l := newLib(t, map[string]string{"obj/a.c": "inherit \"/std/nope\";\n"})
	s := l.ix.Snapshot()
	p, err := s.Parse("/lib/obj/a.c")
	require.NoError(t, err)
	require.Len(t, p.Unit.Inherits, 1)

	l.ov.Set("/lib/obj/a.c", []byte("\n\nvoid f() { }\n")) // and no Invalidate
	_, err = s.Parse("/lib/obj/a.c")
	assert.ErrorIs(t, err, ErrChanged)
	assert.Same(t, p.Unit, s.Unit("/obj/a"), "the generation's facts are what they were")

	l.ix.Invalidate("/lib/obj/a.c")
	p, err = l.ix.Snapshot().Parse("/lib/obj/a.c")
	require.NoError(t, err)
	assert.Empty(t, p.Unit.Inherits)
	assert.Contains(t, funcNames(p.Unit), "f")
}

func TestParse(t *testing.T) {
	l := newLib(t, map[string]string{
		"std/base.c": "void create() { }\n",
		"obj/a.c":    "inherit \"/std/base\";\nvoid f() { }\n",
	})
	s := l.ix.Snapshot()

	p, err := s.Parse("/lib/obj/a.c")
	require.NoError(t, err)
	require.NotNil(t, p.Tree)
	assert.Equal(t, "/lib/obj/a.c", p.Tok.Path)
	assert.NotNil(t, p.Env.Lookup("TRUE"), "the force-included header is in view")
	assert.Equal(t, 1, l.fs.count("/lib/obj/a.c"), "tree and facts come from one read")
	assert.Same(t, p.Unit, s.Unit("/obj/a"))

	// A file the generation already knows is parsed for its tree only.
	base := s.Unit("/std/base")
	require.NotNil(t, s.Chain("/obj/a").Funcs["create"])
	p, err = s.Parse("/lib/std/base.c")
	require.NoError(t, err)
	assert.Same(t, base, p.Unit, "the facts are not rebuilt")
	assert.Equal(t, 2, l.fs.count("/lib/std/base.c"))

	// A file that cannot be read: the file system's word for it.
	p, err = s.Parse("/lib/obj/none.c")
	assert.Nil(t, p)
	assert.ErrorIs(t, err, fs.ErrNotExist)
	assert.Empty(t, s.Crashed())
}

// crashFS panics when one file is read: a stand-in for a bug in the
// analysis.
type crashFS struct {
	vfs.FS
	path string
}

func (c crashFS) ReadFile(p string) ([]byte, error) {
	if p == c.path {
		panic("boom")
	}
	return c.FS.ReadFile(p)
}

// A crash while a file is analyzed is that file's, is remembered for the
// generation, and makes whatever is built on the file unknown.
func TestCrashIsRecorded(t *testing.T) {
	m := vfs.Mem{
		"/lib/std/base.c": "void create() { }\n",
		"/lib/obj/a.c":    "inherit \"/std/base\";\nvoid f() { }\n",
	}
	ix := New(&config.Config{}, Layout{Tree: fileset.Tree{Root: "/lib"}}, crashFS{m, "/lib/std/base.c"})
	s := ix.Snapshot()

	p, err := s.Parse("/lib/std/base.c")
	assert.Nil(t, p)
	var crash *InternalError
	require.ErrorAs(t, err, &crash)
	assert.Equal(t, InternalError{Path: "/lib/std/base.c", Panic: "boom"}, *crash)
	assert.EqualError(t, err, "internal error analyzing /lib/std/base.c (please report): boom")

	assert.Nil(t, s.Unit("/std/base"))
	assert.True(t, s.Chain("/obj/a").Partial, "nothing is concluded from a parent that could not be analyzed")
	assert.Equal(t, LookupUnknown, s.lookupCallable(context.Background(), "/obj/a", "nowhere", false, s.usersOf, s.Chain).State)
	require.Len(t, s.Crashed(), 1)
	assert.Equal(t, "/lib/std/base.c", s.Crashed()[0].Path)

	// The next generation keeps what it knows of files that did not
	// change, and looks again at the one that did.
	ix.Invalidate("/lib/obj/a.c")
	assert.Len(t, ix.Snapshot().Crashed(), 1)
	ix.Invalidate("/lib/std/base.c")
	assert.Empty(t, ix.Snapshot().Crashed())
}

// ------------------------------------------------------------ chains

func TestChain(t *testing.T) {
	l := newLib(t, map[string]string{
		"std/top.c":  "int top_var;\nvoid shared() { }\nvoid only_top() { }\nprivate void secret() { }\nvoid later();\n",
		"std/mid.c":  "inherit \"/std/top\";\nstring mid_var;\nvoid shared() { }\nvoid later() { }\n",
		"std/side.c": "void side() { }\nvoid later();\n",
		"obj/leaf.c": "inherit \"/std/mid\";\nprivate inherit \"/std/side\";\nint leaf_var;\nvoid leaf() { }\n",
		"obj/bad.c":  "inherit UNKNOWN;\nvoid f() { }\n",
		"obj/lost.c": "inherit \"/std/missing\";\n",
		"obj/cyc1.c": "inherit \"/obj/cyc2\";\nvoid one() { }\n",
		"obj/cyc2.c": "inherit \"/obj/cyc1\";\nvoid two() { }\n",
		"std/torn.c": "inherit \"/std/top\";\nnomask void sealed() { }\nint f( { }\n",
		"obj/over.c": "inherit \"/std/torn\";\n",
		"obj/deep.c": "inherit \"/obj/lost\";\n",
	})
	s := l.ix.Snapshot()
	ch := s.Chain("/obj/leaf")
	assert.False(t, ch.Partial)
	assert.False(t, ch.Unseen)
	assert.Equal(t, []string{"/std/top", "/std/mid", "/std/side", "/obj/leaf"}, ch.Objects)

	assert.Equal(t, "/std/mid", ch.Funcs["shared"].DefinedIn, "the nearer definition wins")
	assert.Equal(t, "/lib/std/mid.c", ch.Funcs["shared"].Path)
	assert.Equal(t, 3, ch.Funcs["shared"].Line)
	assert.Equal(t, "/std/top", ch.Funcs["only_top"].DefinedIn)
	assert.True(t, ch.Funcs["secret"].Private)
	assert.True(t, ch.Funcs["side"].Private, "everything below a private inherit is private")
	assert.False(t, ch.Funcs["later"].PrototypeOnly, "a sibling's prototype does not hide a definition")
	assert.Equal(t, "/obj/leaf", ch.Funcs["leaf"].DefinedIn)

	assert.Equal(t, "/std/top", ch.Vars["top_var"].DefinedIn)
	assert.Equal(t, "int", ch.Vars["top_var"].Type)
	assert.Equal(t, "/lib/std/top.c", ch.Vars["top_var"].Path)
	assert.Equal(t, "/std/mid", ch.Vars["mid_var"].DefinedIn)
	assert.Equal(t, "/obj/leaf", ch.Vars["leaf_var"].DefinedIn)

	assert.Same(t, ch, s.Chain("/obj/leaf"), "chains are memoized")
	mid := s.Chain("/std/mid")
	assert.Same(t, mid.Funcs["shared"], ch.Funcs["shared"], "a chain shares its parents' entries")
	assert.Same(t, mid.Vars["mid_var"], ch.Vars["mid_var"])
	side := s.Chain("/std/side")
	assert.False(t, side.Funcs["side"].Private, "a private inherit makes the inheritor's entry private, not the parent's")
	assert.NotSame(t, side.Funcs["side"], ch.Funcs["side"])
	assert.True(t, s.Chain("/obj/bad").Partial, "an inherit that cannot be evaluated")
	assert.Contains(t, s.Chain("/obj/bad").Funcs, "f")
	assert.True(t, s.Chain("/obj/lost").Partial, "a parent with no file")
	assert.True(t, s.Chain("/std/missing").Partial)

	cyc := s.Chain("/obj/cyc1")
	assert.True(t, cyc.Partial, "a cycle cannot be flattened")
	assert.Contains(t, cyc.Funcs, "one")

	// An object that is not known at all is more than an entry that may
	// be missing: it may redefine anything.
	for obj, unseen := range map[string]bool{
		"/obj/bad": true, "/obj/lost": true, "/std/missing": true, "/obj/cyc1": true, "/obj/deep": true,
		"/obj/over": false, "/std/torn": false,
	} {
		assert.True(t, s.Chain(obj).Partial, obj)
		assert.Equal(t, unseen, s.Chain(obj).Unseen, obj)
	}
	assert.True(t, s.Chain("/obj/over").Funcs["sealed"].Nomask)
	assert.False(t, ch.Funcs["shared"].Nomask)

	assert.Contains(t, s.autoState().funcs, "object_name")
	assert.Equal(t, "(auto object)", s.autoState().funcs["object_name"].DefinedIn)
}

// ChainFunc and ChainUnseen say what the flattened chain says, without
// flattening it: asked of one generation, held to the chains of another.
func TestChainFuncIsChain(t *testing.T) {
	files := map[string]string{
		"std/top.c":  "int top_var;\nvoid shared() { }\nvoid only_top() { }\nprivate void secret() { }\nvoid later();\n",
		"std/mid.c":  "inherit \"/std/top\";\nstring mid_var;\nvoid shared() { }\nvoid later() { }\n",
		"std/side.c": "void side() { }\nvoid later();\nvoid shared();\n",
		"obj/leaf.c": "inherit \"/std/mid\";\nprivate inherit \"/std/side\";\nint leaf_var;\nvoid leaf() { }\nvoid only_top();\n",
		"obj/hide.c": "private inherit \"/std/mid\";\n",
		"obj/twin.c": "inherit \"/std/side\";\ninherit \"/std/mid\";\nvoid later();\n",
		"obj/bad.c":  "inherit UNKNOWN;\nvoid f() { }\n",
		"obj/lost.c": "inherit \"/std/missing\";\n",
		"obj/cyc1.c": "inherit \"/obj/cyc2\";\nvoid one() { }\n",
		"obj/cyc2.c": "inherit \"/obj/cyc1\";\nvoid two() { }\n",
		"std/torn.c": "inherit \"/std/top\";\nnomask void sealed() { }\nint f( { }\n",
		"obj/over.c": "inherit \"/std/torn\";\n",
		"obj/deep.c": "inherit \"/obj/lost\";\n",
	}
	objects := []string{"/std/top", "/std/mid", "/std/side", "/obj/leaf", "/obj/hide", "/obj/twin", "/obj/bad", "/obj/lost",
		"/obj/cyc1", "/obj/cyc2", "/std/torn", "/obj/over", "/obj/deep", "/std/missing"}
	names := []string{"top_var", "mid_var", "leaf_var", "shared", "only_top", "secret", "later", "side", "leaf", "f", "one", "two", "sealed", "none"}
	flat := newLib(t, files).ix.Snapshot()
	for _, obj := range objects {
		s := newLib(t, files).ix.Snapshot() // nothing flattened
		assert.Equal(t, flat.Chain(obj).Unseen, s.ChainUnseen(obj), obj)
		for _, name := range names {
			want, wantOK := flat.Chain(obj).Funcs[name]
			got, ok := s.ChainFunc(obj, name)
			assert.Equal(t, wantOK, ok, "%s %s", obj, name)
			if wantOK {
				assert.Equal(t, *want, got, "%s %s", obj, name)
			}
			wantVar, wantOK := flat.Chain(obj).Vars[name]
			gotVar, ok := s.ChainVar(obj, name)
			assert.Equal(t, wantOK, ok, "%s %s", obj, name)
			if wantOK {
				assert.Equal(t, *wantVar, gotVar, "%s %s", obj, name)
			}
		}
	}
}

func TestLookupCallable(t *testing.T) {
	l := newLib(t, map[string]string{
		"std/mod.c":      "void here() { }\nstatic void quiet() { }\nvoid decl();\n",
		"obj/leaf.c":     "inherit \"/std/mod\";\nvoid cb() { }\nvoid decl() { }\n",
		"obj/deep.c":     "inherit \"/obj/leaf\";\nvoid deep_cb() { }\n",
		"obj/alone.c":    "void f() { }\n",
		"obj/partial.c":  "inherit NOPE;\n",
		"sys/auto.c":     "inherit \"/sys/autobase\";\n#include \"/sys/autocode.h\"\nstring object_name() { return \"x\"; }\n",
		"sys/autobase.c": "void in_base() { }\n",
		"sys/autocode.h": "void in_code() { }\n",
		"std/code.c":     "void code_fn() { }\n",
		"obj/user.c":     "#include \"/std/code.c\"\nvoid provided() { }\n",
		"obj/heir.c":     "inherit \"/obj/user\";\nvoid inherited_cb() { }\n",
	})
	s := l.ix.Snapshot()
	state := func(target, fn string, self bool) LookupState {
		return s.lookupCallable(bg, target, fn, self, s.usersOf, s.Chain).State
	}

	lk := s.lookupCallable(bg, "/std/mod", "here", false, s.usersOf, s.Chain)
	assert.Equal(t, LookupFound, lk.State)
	assert.Equal(t, "/std/mod", lk.Fn.DefinedIn)
	assert.Equal(t, LookupFound, state("/obj/leaf", "here", false), "through the chain")
	assert.True(t, s.lookupCallable(bg, "/std/mod", "quiet", false, s.usersOf, s.Chain).Fn.Static)
	assert.Equal(t, LookupFound, state("/obj/alone", "object_name", false), "from the auto object")
	assert.Equal(t, LookupFound, state("/obj/alone", "in_base", false), "from the auto object's parent")
	assert.Equal(t, LookupFound, state("/obj/alone", "in_code", false), "from code the auto object includes")

	assert.Equal(t, LookupMissing, state("/std/mod", "cb", false), "the named object itself lacks it")
	assert.Equal(t, LookupUnknown, state("/obj/partial", "anything", false))
	assert.Equal(t, LookupUnknown, state("/obj/none", "anything", false), "no such object: its chain is unknown")
	assert.Equal(t, LookupUnknown, state("", "anything", true))

	// this_object() in a module may be any inheritor.
	assert.Equal(t, LookupFoundInUser, state("/std/mod", "cb", true))
	assert.Equal(t, LookupFoundInUser, state("/std/mod", "deep_cb", true), "an indirect inheritor")
	lk = s.lookupCallable(bg, "/std/mod", "nowhere", true, s.usersOf, s.Chain)
	assert.Equal(t, LookupMissing, lk.State)
	assert.Equal(t, SearchedUsers, lk.Searched)
	assert.Equal(t, 2, lk.Users)
	lk = s.lookupCallable(bg, "/obj/alone", "nowhere", true, s.usersOf, s.Chain)
	assert.Equal(t, Lookup{State: LookupMissing, Searched: SearchedUsers}, lk, "nobody inherits it")
	assert.Equal(t, SearchedChain, s.lookupCallable(bg, "/obj/alone", "nowhere", false, s.usersOf, s.Chain).Searched)

	// Code in the auto object's chain runs as every object.
	assert.Equal(t, LookupUnknown, state("/sys/auto", "nowhere", true))
	assert.Equal(t, LookupUnknown, state("/sys/autobase", "nowhere", true))
	assert.Equal(t, LookupUnknown, state("/sys/autocode.h", "nowhere", true))

	// A file included as code runs as its includers and their inheritors.
	assert.Equal(t, LookupFoundInUser, state("/std/code", "provided", true))
	assert.Equal(t, LookupFoundInUser, state("/std/code", "inherited_cb", true))

	assert.True(t, s.providedByUsers(bg, "/std/mod", "decl", s.usersOf, s.Chain), "leaf defines what mod declares")
	assert.False(t, s.providedByUsers(bg, "/std/mod", "nowhere", s.usersOf, s.Chain))
	assert.False(t, s.providedByUsers(bg, "/obj/alone", "f", s.usersOf, s.Chain), "nobody inherits it")

	users, ok := s.usersOf(bg, "/std/mod")
	require.True(t, ok)
	assert.Equal(t, []string{"/obj/deep", "/obj/leaf"}, users)
	users, _ = s.usersOf(bg, "/std/code")
	assert.Equal(t, []string{"/obj/heir", "/obj/user"}, users)
}

func TestNoLib(t *testing.T) {
	m := vfs.Mem{"/any/a.c": "#include \"b.h\"\ninherit \"/std/x\";\nvoid f() { }\n", "/any/b.h": "#define B 1\n"}
	ix := New(&config.Config{}, Layout{}, m)
	assert.False(t, ix.HasLib())
	s := ix.Snapshot()
	u := s.UnitAt("/any/a.c")
	require.NotNil(t, u)
	assert.Empty(t, u.LibPath)
	assert.Contains(t, funcNames(u), "f")
	assert.Equal(t, []string{"/any/a.c", "/any/b.h"}, u.Deps, "a relative include still resolves")
	assert.False(t, u.Inherits[0].Resolved)
	assert.Nil(t, s.Unit("/std/x"))
	assert.Equal(t, LookupUnknown, s.lookupCallable(bg, "/std/x", "f", false, s.usersOf, s.Chain).State)
	assert.False(t, s.Scan(bg))
	_, ok := s.usersOf(bg, "/std/x")
	assert.False(t, ok)
}

// ------------------------------------------------------- generations

// The chain a lint makes of its own file is not kept for the snapshot,
// which would keep one for every object of a run over the whole lib;
// those of what it inherits are.
func TestOwnChainIsNotKept(t *testing.T) {
	l := newLib(t, map[string]string{
		"std/base.c": "void create() { }\n",
		"obj/a.c":    "inherit \"/std/base\";\nvoid a() { }\n",
	})
	s := l.ix.Snapshot()
	q := s.Query(s.Unit("/obj/a"), false)
	ch := q.Chain("/obj/a")
	assert.Contains(t, ch.Funcs, "create")
	assert.Same(t, ch, q.Chain("/obj/a"), "made once for the query")
	assert.Equal(t, LookupFound, q.LookupCallable(bg, "/obj/a", "a", true).State)
	_, kept := s.chains.get("/obj/a")
	assert.False(t, kept)
	_, kept = s.chains.get("/std/base")
	assert.True(t, kept)
	assert.Equal(t, ch, s.Chain("/obj/a"))
}

func TestGenerationsShareWhatDidNotChange(t *testing.T) {
	l := newLib(t, map[string]string{
		"std/base.c": "void create() { }\n",
		"obj/a.c":    "inherit \"/std/base\";\nvoid a() { }\n",
		"obj/b.c":    "void b() { }\n",
	})
	s1 := l.ix.Snapshot()
	assert.Equal(t, uint64(1), s1.Generation())
	base, a, b := s1.Unit("/std/base"), s1.Unit("/obj/a"), s1.Unit("/obj/b")
	chA, chB := s1.Chain("/obj/a"), s1.Chain("/obj/b")
	require.Contains(t, chA.Funcs, "create")

	affected := l.set(t, "std/base.c", "void renamed() { }\n")
	assert.Equal(t, []string{"/lib/obj/a.c", "/lib/std/base.c"}, affected)
	s2 := l.ix.Snapshot()
	assert.Equal(t, uint64(2), s2.Generation())
	assert.Equal(t, uint64(2), l.ix.Generation())

	// The old generation answers as before.
	assert.Same(t, base, s1.Unit("/std/base"))
	assert.Same(t, chA, s1.Chain("/obj/a"))
	assert.Contains(t, s1.Chain("/obj/a").Funcs, "create")

	// The new one shares what the change did not reach.
	l.fs.reset()
	assert.Same(t, a, s2.Unit("/obj/a"), "a's own facts did not change")
	assert.Same(t, b, s2.Unit("/obj/b"))
	assert.Same(t, chB, s2.Chain("/obj/b"))
	assert.Zero(t, l.fs.count("/lib/obj/a.c"))
	assert.NotSame(t, base, s2.Unit("/std/base"))
	assert.NotSame(t, chA, s2.Chain("/obj/a"), "a's chain did")
	assert.Contains(t, s2.Chain("/obj/a").Funcs, "renamed")
	assert.NotContains(t, s2.Chain("/obj/a").Funcs, "create")
	assert.Equal(t, 1, l.fs.count("/lib/std/base.c"))
	assert.Zero(t, l.fs.count("/lib/sys/forced.h"), "the force-included header is carried over")
}

func TestInvalidateAffectedSet(t *testing.T) {
	files := map[string]string{
		"obj/defs.h":  "#define P \"/std/base\"\n",
		"std/base.c":  "void create() { }\n",
		"std/mid.c":   "inherit \"/std/base\";\n",
		"obj/leaf.c":  "#include \"defs.h\"\ninherit \"/std/mid\";\n",
		"obj/macro.c": "#include \"defs.h\"\ninherit P;\n",
		"obj/other.c": "void f() { }\n",
		"std/code.c":  "void code_fn() { }\n",
		"obj/inc.c":   "#include \"/std/code.c\"\n",
		"obj/heir.c":  "inherit \"/obj/inc\";\n",
	}
	load := func(l *lib_) *Snapshot {
		s := l.ix.Snapshot()
		for name := range files {
			if name != "obj/defs.h" {
				s.UnitAt("/lib/" + name)
			}
		}
		return s
	}

	t.Run("a header reaches its includers and their inheritors", func(t *testing.T) {
		l := newLib(t, files)
		load(l)
		assert.Equal(t, []string{"/lib/obj/defs.h", "/lib/obj/leaf.c", "/lib/obj/macro.c"},
			l.set(t, "obj/defs.h", "#define P \"/std/mid\"\n"))
		assert.Equal(t, "/std/mid", l.ix.Snapshot().Unit("/obj/macro").Inherits[0].Path)
	})

	t.Run("an object reaches everything that inherits it", func(t *testing.T) {
		l := newLib(t, files)
		load(l)
		assert.Equal(t, []string{"/lib/obj/leaf.c", "/lib/obj/macro.c", "/lib/std/base.c", "/lib/std/mid.c"},
			l.set(t, "std/base.c", "void x() { }\n"))
	})

	t.Run("code reaches its includers and their inheritors", func(t *testing.T) {
		l := newLib(t, files)
		load(l)
		assert.Equal(t, []string{"/lib/obj/heir.c", "/lib/obj/inc.c", "/lib/std/code.c"},
			l.set(t, "std/code.c", "void other() { }\n"))
		assert.Contains(t, l.ix.Snapshot().Chain("/obj/heir").Funcs, "other")
	})

	t.Run("nothing else", func(t *testing.T) {
		l := newLib(t, files)
		load(l)
		assert.Equal(t, []string{"/lib/obj/other.c"}, l.set(t, "obj/other.c", "void g() { }\n"))
		assert.Equal(t, []string{"/lib/obj/never-loaded.c"}, l.set(t, "obj/never-loaded.c", ""))
		gen := l.ix.Generation()
		assert.Equal(t, []string{"/elsewhere/x.c"}, l.ix.Invalidate("/elsewhere/x.c"))
		assert.Equal(t, gen, l.ix.Generation(), "a path that is nothing to the lib starts no generation")
	})

	t.Run("edges follow the units", func(t *testing.T) {
		l := newLib(t, files)
		load(l)
		// leaf stops including defs.h and inheriting mid.
		l.set(t, "obj/leaf.c", "void f() { }\n")
		assert.NotNil(t, l.ix.Snapshot().Unit("/obj/leaf"))
		assert.NotContains(t, l.set(t, "obj/defs.h", "#define P \"/std/base\"\n#define Q 1\n"), "/lib/obj/leaf.c")
		assert.NotContains(t, l.set(t, "std/mid.c", "inherit \"/std/base\";\nvoid m() { }\n"), "/lib/obj/leaf.c")
		// A unit that was dropped and not read again depends on nothing.
		l.set(t, "obj/macro.c", "void f() { }\n")
		assert.Equal(t, []string{"/lib/obj/defs.h"}, l.set(t, "obj/defs.h", "#define P \"/std/base\"\n"))
		for name, set := range l.ix.builtFrom {
			assert.NotEmpty(t, set, name)
		}
		for name, set := range l.ix.children {
			assert.NotEmpty(t, set, name)
		}
	})

	t.Run("a file appearing where one was looked for", func(t *testing.T) {
		l := newLib(t, map[string]string{
			"include/late.h": "#define L 1\n",
			"obj/x.c":        "#include \"late.h\"\n#include \"never.h\"\ninherit \"/std/soon\";\n",
		})
		s := l.ix.Snapshot()
		require.Len(t, s.Unit("/obj/x").BadIncludes, 1)
		require.True(t, s.Chain("/obj/x").Partial)

		// Earlier in the search order than the header in use.
		assert.Contains(t, l.set(t, "obj/late.h", "#define L 2\n"), "/lib/obj/x.c")
		assert.Contains(t, l.ix.Snapshot().Unit("/obj/x").Deps, "/lib/obj/late.h")
		assert.Contains(t, l.set(t, "include/never.h", ""), "/lib/obj/x.c")
		assert.Empty(t, l.ix.Snapshot().Unit("/obj/x").BadIncludes)
		// A parent that did not exist.
		assert.Contains(t, l.set(t, "std/soon.c", "void create() { }\n"), "/lib/obj/x.c")
		assert.False(t, l.ix.Snapshot().Chain("/obj/x").Partial)
		// And one that goes away again.
		l.ov.Delete("/lib/std/soon.c")
		assert.Contains(t, l.ix.Invalidate("/lib/std/soon.c"), "/lib/obj/x.c")
		assert.True(t, l.ix.Snapshot().Chain("/obj/x").Partial)
	})
}

func TestInvalidateForceIncludeAndAutoObject(t *testing.T) {
	l := newLib(t, map[string]string{
		"sys/forced.h": "#include \"more.h\"\n#include \"absent.h\"\n",
		"sys/more.h":   "#define PARENT \"/std/one\"\n",
		"std/one.c":    "void one() { }\n",
		"std/two.c":    "void two() { }\n",
		"obj/a.c":      "inherit PARENT;\n",
		"obj/b.c":      "void b() { }\n",
	})
	s := l.ix.Snapshot()
	a, b := s.Unit("/obj/a"), s.Unit("/obj/b")
	require.Contains(t, s.Chain("/obj/a").Funcs, "one")
	assert.Contains(t, a.Deps, "/lib/sys/more.h")

	// A header the forced header includes: every unit is rebuilt.
	affected := l.set(t, "sys/more.h", "#define PARENT \"/std/two\"\n")
	assert.Equal(t, []string{"/lib/obj/a.c", "/lib/obj/b.c", "/lib/std/one.c", "/lib/sys/more.h"}, affected)
	s = l.ix.Snapshot()
	assert.Contains(t, s.Chain("/obj/a").Funcs, "two")
	assert.NotSame(t, b, s.Unit("/obj/b"))

	// A header it looked for and did not find.
	assert.Contains(t, l.set(t, "sys/absent.h", "#define NEW 1\n"), "/lib/obj/b.c")
	// Anything else leaves the forced header's state alone.
	l.ix.Snapshot().Unit("/obj/b")
	l.fs.reset()
	l.set(t, "obj/b.c", "void b2() { }\n")
	l.ix.Snapshot().Unit("/obj/b")
	assert.Equal(t, map[string]int{"/lib/obj/b.c": 1}, l.fs.reads)

	// The auto object's functions are everybody's.
	s = l.ix.Snapshot()
	s.Unit("/obj/a")
	require.Contains(t, s.autoState().funcs, "object_name")
	affected = l.set(t, "sys/auto.c", "string other_name() { return \"y\"; }\n")
	assert.Subset(t, affected, []string{"/lib/obj/a.c", "/lib/obj/b.c", "/lib/sys/auto.c"})
	assert.Contains(t, l.ix.Snapshot().autoState().funcs, "other_name")
	assert.NotContains(t, l.ix.Snapshot().autoState().funcs, "object_name")
	assert.Contains(t, s.autoState().funcs, "object_name", "the old generation keeps its answer")
}

// A directory that is deleted, created or renamed is reported as the
// directory: it stands for the files under it.
func TestInvalidateDirectory(t *testing.T) {
	l := newLib(t, map[string]string{
		"std/base.c":  "void base() { }\n",
		"std/defs.h":  "#define N 1\n",
		"obj/leaf.c":  "inherit \"/std/base\";\n",
		"obj/user.c":  "#include \"/std/defs.h\"\nint n = N;\n",
		"obj/other.c": "void f() { }\n",
	})
	s := l.ix.Snapshot()
	require.True(t, s.Scan(bg))
	require.Contains(t, s.Chain("/obj/leaf").Funcs, "base")
	other := s.Unit("/obj/other")

	delete(l.disk, "/lib/std/base.c")
	delete(l.disk, "/lib/std/defs.h")
	affected := l.ix.Invalidate("/lib/std")
	assert.Equal(t, []string{"/lib/obj/leaf.c", "/lib/obj/user.c", "/lib/std", "/lib/std/base.c", "/lib/std/defs.h"}, affected)
	s = l.ix.Snapshot()
	assert.Nil(t, s.Unit("/std/base"))
	assert.True(t, s.Chain("/obj/leaf").Partial)
	assert.NotContains(t, s.Chain("/obj/leaf").Funcs, "base")
	assert.Len(t, s.Unit("/obj/user").BadIncludes, 1)
	assert.Same(t, other, s.Unit("/obj/other"), "what the directory did not hold is shared")
	l.fs.reset()
	require.True(t, s.Scan(bg))
	assert.Empty(t, l.fs.reads, "the scan has nothing to read for files that are gone")

	// It comes back under another name, and then under its own.
	l.disk["/lib/lib2/base.c"] = "void base() { }\n"
	assert.Equal(t, []string{"/lib/lib2", "/lib/lib2/base.c"}, l.ix.Invalidate("/lib/lib2"))
	assert.False(t, l.ix.Snapshot().Scanned(), "the new directory's files are the scan's to read")
	l.disk["/lib/std/base.c"] = "void base2() { }\n"
	l.disk["/lib/std/defs.h"] = "#define N 2\n"
	affected = l.ix.Invalidate("/lib/std")
	assert.Subset(t, affected, []string{"/lib/obj/leaf.c", "/lib/obj/user.c", "/lib/std/base.c"})
	s = l.ix.Snapshot()
	assert.Contains(t, s.Chain("/obj/leaf").Funcs, "base2")
	assert.Empty(t, s.Unit("/obj/user").BadIncludes)
	users, ok := s.usersOf(bg, "/std/base")
	require.True(t, ok)
	assert.Equal(t, []string{"/obj/leaf"}, users)
}

// Whether the lib is all there is looked at again when one of the things
// a whole lib has comes or goes; what was concluded from the old answer
// is dropped.
func TestCompletenessFollowsTheLib(t *testing.T) {
	l := newLib(t, map[string]string{"obj/a.c": "#include \"nowhere.h\"\n"})
	s := l.ix.Snapshot()
	require.True(t, s.Complete())
	require.Len(t, s.Unit("/obj/a").BadIncludes, 1)

	delete(l.disk, "/lib/sys/auto.c")
	assert.Contains(t, l.ix.Invalidate("/lib/sys/auto.c"), "/lib/obj/a.c")
	s2 := l.ix.Snapshot()
	assert.False(t, s2.Complete())
	require.Len(t, s2.Notes(), 1)
	assert.Contains(t, s2.Notes()[0].Message, "missing /sys/auto")
	assert.Empty(t, s2.Unit("/obj/a").BadIncludes, "nothing is provably missing from an incomplete lib")
	assert.Equal(t, 1, s2.Unit("/obj/a").UnknownIncludes)
	assert.True(t, s.Complete(), "the old generation keeps its answer")

	l.disk["/lib/sys/auto.c"] = "void f() { }\n"
	assert.Contains(t, l.ix.Invalidate("/lib/sys"), "/lib/obj/a.c")
	s3 := l.ix.Snapshot()
	assert.True(t, s3.Complete())
	assert.Empty(t, s3.Notes())
	assert.Len(t, s3.Unit("/obj/a").BadIncludes, 1)
}

// An include directory that is not absolute is a different one for each
// file: relative to the file, or, with ~ (the kernel library's ~/include),
// what the lib's driver object makes of it. Neither is required of a
// whole lib, and an include that may be in one is not provably missing.
func TestRelativeIncludeDirs(t *testing.T) {
	files := map[string]string{
		"usr/ann/inc/mine.h": "#define MINE 1\n",
		"usr/ann/a.c":        "#include <mine.h>\n#include <gone.h>\n",
	}
	l := newLib(t, files, func(c *config.Config) { c.IncludeDirs = []string{"/include", "inc"} })
	s := l.ix.Snapshot()
	require.True(t, s.Complete())
	a := s.Unit("/usr/ann/a")
	require.Len(t, a.BadIncludes, 1, "found beside the file; the other is provably missing")
	assert.Equal(t, "<gone.h>", a.BadIncludes[0].Raw)

	l = newLib(t, files, func(c *config.Config) { c.IncludeDirs = []string{"/include", "~/include"} })
	s = l.ix.Snapshot()
	assert.True(t, s.Complete(), "~/include is not required")
	assert.Empty(t, s.Notes())
	a = s.Unit("/usr/ann/a")
	assert.Empty(t, a.BadIncludes, "either may be in ~/include")
	assert.Equal(t, 2, a.UnknownIncludes)
}

// An object's name may have dots: its source is the name with ".c".
func TestObjectNameWithDot(t *testing.T) {
	l := newLib(t, map[string]string{
		"obj/sword.v2.c": "void swing() { }\n",
		"obj/user.c":     "inherit \"/obj/sword.v2\";\n",
		"obj/odd.h":      "void in_header();\n",
	})
	s := l.ix.Snapshot()
	assert.Equal(t, "/obj/sword.v2", s.LibPath("/lib/obj/sword.v2.c"))
	assert.Equal(t, "/lib/obj/sword.v2.c", s.ObjectFile("/obj/sword.v2"))
	assert.True(t, s.ObjectExists("/obj/sword.v2"))
	assert.False(t, s.ObjectExists("/obj/sword.v3"))
	ch := s.Chain("/obj/user")
	assert.False(t, ch.Partial)
	assert.Contains(t, ch.Funcs, "swing")
	// A header linted on its own is known by its own name.
	assert.Equal(t, "/lib/obj/odd.h", s.ObjectFile(s.LibPath("/lib/obj/odd.h")))
	assert.Contains(t, s.Chain("/obj/odd.h").Funcs, "in_header")
}

// A unit that is still being built when the files change belongs to the
// generation that started it and is not handed to the next.
func TestBuildInFlightIsNotCarried(t *testing.T) {
	l := newLib(t, map[string]string{"obj/a.c": "void old_fn() { }\n"})
	l.fs.holdReads(func(name string, _ int) bool { return name == "/lib/obj/a.c" })
	s1 := l.ix.Snapshot()
	built := make(chan *Unit)
	go func() { built <- s1.Unit("/obj/a") }()
	<-l.fs.entered // the build has started and not read the file yet

	l.set(t, "obj/a.c", "void new_fn() { }\n")
	s2 := l.ix.Snapshot()
	close(l.fs.release)
	u1 := <-built
	u2 := s2.Unit("/obj/a")
	assert.NotSame(t, u1, u2)
	assert.Contains(t, funcNames(u2), "new_fn")
	assert.Equal(t, uint64(1), u1.gen)
	assert.Equal(t, uint64(2), u2.gen)
	// The late unit did not enter the current generation's edges or scan.
	l.set(t, "obj/a.c", "void third() { }\n")
	assert.Contains(t, funcNames(l.ix.Snapshot().Unit("/obj/a")), "third")
}

// --------------------------------------------------------------- scan

func TestScan(t *testing.T) {
	files := map[string]string{
		"std/mod.c":      "void m() { }\n",
		"obj/leaf.c":     "inherit \"/std/mod\";\n",
		"attic/old.c":    "inherit \"/std/mod\";\n",
		"obj/sub/deep.c": "inherit \"/obj/leaf\";\n",
		"obj/old/gone.c": "inherit \"/std/mod\";\n",
		"obj/notes.txt":  "inherit \"/std/mod\";\n",
		"obj/x.pre.c":    "inherit \"/std/mod\";\n",
	}
	exclude := func(c *config.Config) { c.Exclude = []string{"attic/**", "**/old/**", "**/*.pre*"} }
	l := newLib(t, files, exclude)
	s := l.ix.Snapshot()
	assert.False(t, s.Scanned())
	require.True(t, s.Scan(bg))
	assert.True(t, s.Scanned())
	users, ok := s.usersOf(bg, "/std/mod")
	require.True(t, ok)
	assert.Equal(t, []string{"/obj/leaf", "/obj/sub/deep"}, users, "excluded files are not scanned")
	assert.Equal(t, 1, l.fs.walks)
	assert.Zero(t, l.fs.count("/lib/attic/old.c"))

	// A second scan reads nothing.
	l.fs.reset()
	require.True(t, s.Scan(bg))
	assert.Zero(t, l.fs.walks)
	assert.Empty(t, l.fs.reads)

	// After a change the scan reads the changed file only.
	l.set(t, "obj/leaf.c", "void f() { }\n")
	s2 := l.ix.Snapshot()
	assert.False(t, s2.Scanned())
	_, ok = s.usersOf(bg, "/std/mod")
	assert.False(t, ok, "a generation that has been replaced cannot say who inherits")
	users, ok = s2.usersOf(bg, "/std/mod")
	require.True(t, ok)
	assert.Empty(t, users)
	assert.Zero(t, l.fs.walks)
	assert.Equal(t, map[string]int{"/lib/obj/leaf.c": 1}, l.fs.reads)

	// Files that appear and disappear, in and out of the scan's scope.
	l.set(t, "obj/new.c", "inherit \"/std/mod\";\n")
	l.set(t, "attic/new.c", "inherit \"/std/mod\";\n")
	l.set(t, "obj/old/new.c", "inherit \"/std/mod\";\n")
	l.set(t, "obj/new.h", "inherit \"/std/mod\";\n")
	delete(l.disk, "/lib/obj/sub/deep.c")
	l.ix.Invalidate("/lib/obj/sub/deep.c")
	users, ok = l.ix.Snapshot().usersOf(bg, "/std/mod")
	require.True(t, ok)
	assert.Equal(t, []string{"/obj/new"}, users)
	assert.Zero(t, l.fs.walks)
}

func TestScanCancelled(t *testing.T) {
	// The scan below is cancelled while it is held at its reads: a few
	// have passed, and each worker can be inside one more. With more
	// files than that, some are left however the workers are scheduled
	// and in whatever order the scan takes the files.
	const passed = 20
	n := passed + 2*runtime.GOMAXPROCS(0) + 20
	files := map[string]string{"std/mod.c": "void m() { }\n"}
	for i := range n {
		files[fmt.Sprintf("obj/o%d.c", i)] = "inherit \"/std/mod\";\n"
	}
	l := newLib(t, files)
	s := l.ix.Snapshot()

	ctx, cancel := context.WithCancel(bg)
	cancel()
	assert.False(t, s.Scan(ctx))
	_, ok := s.usersOf(ctx, "/std/mod")
	assert.False(t, ok, "a cancelled scan is not an answer")
	assert.Empty(t, l.fs.reads)

	// Cancelled part-way: what was indexed is kept, the rest is done by
	// the next scan.
	l.fs.holdReads(func(_ string, before int) bool { return before >= passed })
	ctx, cancel = context.WithCancel(bg)
	done := make(chan bool)
	go func() { done <- s.Scan(ctx) }()
	<-l.fs.entered
	cancel()
	close(l.fs.release)
	assert.False(t, <-done)
	assert.False(t, s.Scanned())

	users, ok := s.usersOf(bg, "/std/mod")
	require.True(t, ok)
	assert.Len(t, users, n)
	for i := range n {
		assert.Equal(t, 1, l.fs.count(fmt.Sprintf("/lib/obj/o%d.c", i)), "each file is read once across both scans")
	}

	// Cancelled with the last file in hand: the scan completes, and says
	// so.
	l.set(t, "obj/o0.c", "void f() { }\n")
	s = l.ix.Snapshot()
	l.fs.holdReads(func(name string, _ int) bool { return name == "/lib/obj/o0.c" })
	ctx, cancel = context.WithCancel(bg)
	go func() { done <- s.Scan(ctx) }()
	<-l.fs.entered
	cancel()
	close(l.fs.release)
	assert.True(t, <-done)
	assert.True(t, s.Scanned())
	users, ok = s.usersOf(ctx, "/std/mod")
	require.True(t, ok)
	assert.Len(t, users, n-1)
}

// A scan the index has moved on from stops reading: it holds the one
// scan slot, and nobody will ask its generation.
func TestSupersededScanStops(t *testing.T) {
	const passed = 10
	n := passed + 2*runtime.GOMAXPROCS(0) + 200
	files := map[string]string{}
	for i := range n {
		files[fmt.Sprintf("obj/o%d.c", i)] = "void f() { }\n"
	}
	l := newLib(t, files)
	s := l.ix.Snapshot()
	l.fs.holdReads(func(_ string, before int) bool { return before >= passed })
	done := make(chan bool)
	go func() { done <- s.Scan(bg) }()
	<-l.fs.entered
	l.set(t, "obj/o0.c", "void g() { }\n")
	close(l.fs.release)
	assert.False(t, <-done)
	l.fs.mu.Lock()
	read := l.fs.total
	l.fs.mu.Unlock()
	assert.LessOrEqual(t, read, passed+2*runtime.GOMAXPROCS(0), "of %d files", n)
	assert.True(t, l.ix.Snapshot().Scan(bg))
}

// ------------------------------------------------------------- query

func TestQueryRecordsWhatItAsked(t *testing.T) {
	l := newLib(t, map[string]string{
		"std/mod.c":    "void m() { }\n",
		"obj/target.c": "inherit \"/std/mod\";\nvoid hit() { }\n",
		"obj/asker.c":  "void f() { }\n",
		"obj/idle.c":   "void f() { }\n",
	})
	s := l.ix.Snapshot()
	asker, idle := s.Unit("/obj/asker"), s.Unit("/obj/idle")
	q := s.Query(asker, false)
	assert.Equal(t, LookupFound, q.LookupCallable(bg, "/obj/target", "hit", false).State)
	assert.False(t, q.ObjectExists("/obj/soon"))
	assert.Equal(t, "/obj/rel", q.ObjectPath("obj/rel.c"))
	s.Query(idle, false).Chain("/obj/idle")

	assert.Equal(t, []string{"/lib/obj/asker.c", "/lib/obj/target.c"}, l.set(t, "obj/target.c", "void miss() { }\n"),
		"the asker depends on the object it asked about")
	assert.Equal(t, []string{"/lib/std/mod.c"}, l.set(t, "std/mod.c", "void m2() { }\n"),
		"target no longer inherits mod, so nothing reaches the asker through it")
	// A question asked generations ago still stands.
	assert.Equal(t, []string{"/lib/obj/asker.c", "/lib/obj/soon.c"}, l.set(t, "obj/soon.c", ""))

	// Once the asker itself is rebuilt, what it asked before is void.
	l.set(t, "obj/asker.c", "void g() { }\n")
	l.ix.Snapshot().Unit("/obj/asker")
	assert.Equal(t, []string{"/lib/obj/target.c"}, l.set(t, "obj/target.c", "void hit() { }\n"))
	assert.Empty(t, l.ix.refs["/obj/target"], "the void question is forgotten")
}

// A lookup made after its context was cancelled reads nothing and
// concludes nothing.
func TestQueryCancelled(t *testing.T) {
	l := newLib(t, map[string]string{
		"std/mod.c":  "void m() { }\n",
		"obj/leaf.c": "inherit \"/std/mod\";\n",
	})
	s := l.ix.Snapshot()
	q := s.Query(s.Unit("/obj/leaf"), false)
	l.fs.reset()
	ctx, cancel := context.WithCancel(bg)
	cancel()
	assert.Equal(t, LookupUnknown, q.LookupCallable(ctx, "/std/mod", "nowhere", false).State)
	assert.Equal(t, LookupUnknown, q.LookupCallable(ctx, "/obj/leaf", "nowhere", true).State)
	assert.True(t, q.ProvidedByUsers(ctx, "nowhere"), "unknown is assumed provided")
	assert.Empty(t, l.fs.reads)
	assert.Equal(t, LookupMissing, q.LookupCallable(bg, "/std/mod", "nowhere", false).State)
}

func TestQueryWithoutScan(t *testing.T) {
	l := newLib(t, map[string]string{
		"std/mod.c":  "void m() { }\n",
		"obj/leaf.c": "inherit \"/std/mod\";\nvoid cb() { }\n",
	})
	s := l.ix.Snapshot()
	mod := s.Unit("/std/mod")

	q := s.Query(mod, true)
	assert.Equal(t, LookupFound, q.LookupCallable(bg, "/std/mod", "m", true).State)
	assert.False(t, q.NeedsScan(), "nothing needed the scan yet")
	assert.Equal(t, LookupUnknown, q.LookupCallable(bg, "/std/mod", "cb", true).State)
	assert.True(t, q.ProvidedByUsers(bg, "cb"), "unknown is assumed provided")
	assert.True(t, q.NeedsScan())
	assert.Zero(t, l.fs.walks, "the query did not start the scan")

	require.True(t, s.Scan(bg))
	q = s.Query(mod, true)
	assert.Equal(t, LookupFoundInUser, q.LookupCallable(bg, "/std/mod", "cb", true).State)
	assert.Equal(t, LookupMissing, q.LookupCallable(bg, "/std/mod", "nope", true).State)
	assert.False(t, q.NeedsScan())

	// Who inherits a file can change with any object: the asker is told.
	assert.Contains(t, l.set(t, "obj/other.c", "inherit \"/std/mod\";\nvoid nope() { }\n"), "/lib/std/mod.c")
	s = l.ix.Snapshot()
	q = s.Query(s.Unit("/std/mod"), false)
	assert.Equal(t, LookupFoundInUser, q.LookupCallable(bg, "/std/mod", "nope", true).State, "without NoScan the query scans")
}

// PrivateOnly: what a call by name finds in DGD's table of functions,
// over the whole chain and every object the file's code may run as, and
// a file that asked is told when an inheritor changes the answer.
func TestPrivateOnly(t *testing.T) {
	l := newLib(t, map[string]string{
		"std/pub.c":  "void tick() { }\n",
		"std/mod.c":  "private void tick() { }\n",
		"obj/leaf.c": "inherit \"/std/mod\";\n",
		"obj/prv.c":  "private inherit \"/std/pub\";\n",
		"obj/prv2.c": "private inherit \"/std/pub\";\n",
		"obj/diam.c": "inherit \"/std/pub\";\ninherit \"/obj/prv\";\nprivate void tock() { }\n",
		"obj/hid.c":  "private void hidden_auto() { }\n",
	})
	privateOnly := func(lib, fn string) (string, bool, bool) {
		s := l.ix.Snapshot()
		f, inherited, ok := s.Query(s.Unit(lib), false).PrivateOnly(bg, fn)
		return f.DefinedIn, inherited, ok
	}
	in, inh, ok := privateOnly("/std/mod", "tick")
	assert.True(t, ok)
	assert.Equal(t, "/std/mod", in)
	assert.False(t, inh)
	_, inh, ok = privateOnly("/obj/prv2", "tick")
	assert.True(t, ok, "inherited privately")
	assert.True(t, inh)
	_, _, ok = privateOnly("/obj/prv", "tick")
	assert.False(t, ok, "an inheritor has it, inherited not privately")
	_, _, ok = privateOnly("/obj/diam", "tick")
	assert.False(t, ok, "inherited both privately and not")
	_, _, ok = privateOnly("/obj/diam", "tock")
	assert.True(t, ok)
	_, _, ok = privateOnly("/std/pub", "tick")
	assert.False(t, ok, "not private")
	_, _, ok = privateOnly("/obj/leaf", "nowhere")
	assert.False(t, ok, "no function of the name")
	_, _, ok = privateOnly("/obj/hid", "hidden_auto")
	assert.False(t, ok, "the auto object has one")

	// An inheritor that has it: this_object() may be that one.
	assert.Contains(t, l.set(t, "obj/leaf.c", "inherit \"/std/mod\";\nvoid tick() { }\n"), "/lib/std/mod.c",
		"the file that asked is told")
	_, _, ok = privateOnly("/std/mod", "tick")
	assert.False(t, ok)
}

// --------------------------------------------------------- partial libs

func TestStubsAndCheckout(t *testing.T) {
	stubs := vfs.Mem{
		"stub://lib/std/room.c":    "#include <std.h>\nvoid create() {}\nint query_light() {}\n",
		"stub://lib/include/std.h": "#define ROOM \"/std/room\"\n",
		"stub://lib/sys/auto.c":    "string object_name() {}\n",
	}
	disk := vfs.Mem{
		"/home/ann/hall.c":       "#include <std.h>\n#include \"mine.h\"\ninherit ROOM;\nvoid f() { }\nprivate void p() { }\n",
		"/home/ann/mine.h":       "#define MINE 1\n",
		"/home/ann/sub/closet.c": "inherit \"/players/ann/hall\";\n",
	}
	both := unionFS{disk, stubs}
	cfg := &config.Config{}
	cfg.IncludeDirs = []string{"/include"}
	cfg.AutoObjects = []string{"/sys/auto"}
	ix := New(cfg, Layout{
		Tree:  fileset.Tree{CheckoutDir: "/home/ann", CheckoutLib: "/players/ann"},
		Stubs: "stub://lib", Covered: []string{"/std/", "/include/"}, Holes: []string{"std/secret/**"},
	}, both)
	assert.True(t, ix.HasLib())
	assert.True(t, ix.Partial())
	assert.False(t, ix.Snapshot().Complete())

	assert.Equal(t, "/players/ann/hall", ix.LibPath("/home/ann/hall.c"))
	assert.Equal(t, "/std/room", ix.LibPath("stub://lib/std/room.c"))
	assert.Empty(t, ix.LibPath("/home/bob/x.c"))
	assert.True(t, ix.Snapshot().Covers("/players/ann/anything"))
	assert.True(t, ix.Snapshot().Covers("/std/anything"))
	assert.False(t, ix.Snapshot().Covers("/std/secret/thing"))
	assert.False(t, ix.Snapshot().Covers("/obj/anything"))

	s := ix.Snapshot()
	hall := s.Unit("/players/ann/hall")
	require.NotNil(t, hall)
	assert.Equal(t, "/home/ann/hall.c", hall.Path)
	assert.Equal(t, []string{"/home/ann/hall.c", "stub://lib/include/std.h", "/home/ann/mine.h"}, hall.Deps)
	assert.False(t, hall.Stub)
	assert.Equal(t, "/std/room", hall.Inherits[0].Path)

	room := s.Unit("/std/room")
	require.NotNil(t, room)
	assert.True(t, room.Stub)
	assert.Equal(t, "stub://lib/std/room.c", room.Path)
	assert.Equal(t, "stub://lib/std/room.c", s.Chain("/players/ann/hall").Funcs["query_light"].Path)
	assert.True(t, s.ObjectExists("/std/room"))
	assert.False(t, s.ObjectExists("/std/nothing"))
	assert.Nil(t, s.Unit("/players/ann/nothing"), "the checkout alone speaks for its directory")

	lk := s.lookupCallable(bg, "/std/room", "hidden", false, s.usersOf, s.Chain)
	assert.Equal(t, LookupMissing, lk.State)
	assert.Equal(t, SearchedStub, lk.Searched)

	// The scan covers the checkout only.
	users, ok := s.usersOf(bg, "/players/ann/hall")
	require.True(t, ok)
	assert.Equal(t, []string{"/players/ann/sub/closet"}, users)
	lk = s.lookupCallable(bg, "/players/ann/sub/closet", "nowhere", true, s.usersOf, s.Chain)
	assert.Equal(t, LookupMissing, lk.State)
	assert.Equal(t, SearchedCheckout, lk.Searched)

	// A private function of the checkout: an inheritor elsewhere may have
	// the name, and a call by name would find it.
	_, _, ok = s.Query(s.Unit("/players/ann/hall"), false).PrivateOnly(bg, "p")
	assert.False(t, ok)
}

func TestRealFileWinsOverStub(t *testing.T) {
	disk := vfs.Mem{"/lib/obj/a.c": "inherit \"/std/room\";\nprivate void mine() { }\n", "/lib/keep": ""}
	stubs := vfs.Mem{"stub://lib/std/room.c": "void from_stub() {}\n"}
	ov := vfs.NewOverlay(unionFS{disk, stubs})
	ix := New(&config.Config{}, Layout{Tree: fileset.Tree{Root: "/lib"}, Stubs: "stub://lib"}, ov)

	s := ix.Snapshot()
	assert.Contains(t, s.Chain("/obj/a").Funcs, "from_stub")
	assert.True(t, s.Unit("/std/room").Stub)
	_, _, ok := s.Query(s.Unit("/obj/a"), false).PrivateOnly(bg, "mine")
	assert.False(t, ok, "a stub lists the public functions only")

	ov.Set("/lib/std/room.c", []byte("void from_disk() { }\n"))
	assert.Equal(t, []string{"/lib/obj/a.c", "/lib/std/room.c"}, ix.Invalidate("/lib/std/room.c"))
	s = ix.Snapshot()
	assert.Contains(t, s.Chain("/obj/a").Funcs, "from_disk")
	assert.False(t, s.Unit("/std/room").Stub)
	assert.Equal(t, "/lib/std/room.c", s.Unit("/std/room").Path)
}

// unionFS answers stub locations from one FS and everything else from
// another, as profile.FS does.
type unionFS struct{ disk, stubs vfs.Mem }

func (u unionFS) pick(p string) vfs.Mem {
	if len(p) > 7 && p[:7] == "stub://" {
		return u.stubs
	}
	return u.disk
}
func (u unionFS) ReadFile(p string) ([]byte, error)            { return u.pick(p).ReadFile(p) }
func (u unionFS) Exists(p string) bool                         { return u.pick(p).Exists(p) }
func (u unionFS) IsDir(p string) bool                          { return u.pick(p).IsDir(p) }
func (u unionFS) WalkDir(root string, fn fs.WalkDirFunc) error { return u.disk.WalkDir(root, fn) }
func (u unionFS) RealPath(p string) (string, error)            { return p, nil }

// ------------------------------------------------------- concurrency

// Readers on every generation while the files change under them. Run
// under -race.
func TestConcurrentReadersAndInvalidate(t *testing.T) {
	const n = 60
	files := map[string]string{"std/mod.c": "void m() { }\n"}
	for i := range n {
		files[fmt.Sprintf("obj/o%d.c", i)] = fmt.Sprintf("inherit \"/std/mod\";\nvoid f%d() { }\n", i)
	}
	l := newLib(t, files)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := range 6 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				s := l.ix.Snapshot()
				lib := fmt.Sprintf("/obj/o%d", (g*7+i)%n)
				u := s.Unit(lib)
				ch := s.Chain(lib)
				if u == nil || !assert.Contains(t, ch.Funcs, "m") {
					t.Errorf("%s: no unit or no inherited function in generation %d", lib, s.Generation())
					return
				}
				// A generation never changes its mind.
				if s.Unit(lib) != u || s.Chain(lib) != ch {
					t.Errorf("%s: generation %d answered twice differently", lib, s.Generation())
					return
				}
				q := s.Query(u, i%2 == 0)
				q.LookupCallable(bg, lib, "nowhere", true)
				q.LookupCallable(bg, fmt.Sprintf("/obj/o%d", (i+1)%n), "m", false)
				if i%5 == 0 {
					s.Parse(u.Path)
				}
				if i%11 == 0 {
					s.Scan(bg)
				}
			}
		})
	}
	for k := range 150 {
		i := k % n
		l.set(t, fmt.Sprintf("obj/o%d.c", i), fmt.Sprintf("inherit \"/std/mod\";\nvoid f%d() { }\nvoid edit%d() { }\n", i, k))
		if k%40 == 0 {
			l.set(t, "sys/forced.h", fmt.Sprintf("#define TRUE %d\n", k))
		}
		if k%25 == 0 {
			l.set(t, "std/mod.c", fmt.Sprintf("void m() { }\nvoid mod%d() { }\n", k))
		}
	}
	close(stop)
	wg.Wait()

	// The last generation is exactly the edited lib.
	s := l.ix.Snapshot()
	assert.Equal(t, uint64(1+150+4+6), s.Generation())
	users, ok := s.usersOf(bg, "/std/mod")
	require.True(t, ok)
	assert.Len(t, users, n)
	for i := range n {
		last := i + 60 // the last edit of file i
		if last+60 < 150 {
			last += 60
		}
		ch := s.Chain(fmt.Sprintf("/obj/o%d", i))
		assert.Contains(t, ch.Funcs, fmt.Sprintf("edit%d", last))
		assert.Contains(t, ch.Funcs, "mod125")
	}
}

func TestMemo(t *testing.T) {
	m := newMemo[int]()
	var builds, commits int
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			v := m.load("k", func() int { builds++; return 42 }, func(v int, complete func()) { commits++; complete() })
			assert.Equal(t, 42, v)
		})
	}
	wg.Wait()
	assert.Equal(t, 1, builds, "one build however many ask")
	assert.Equal(t, 1, commits)
	v, ok := m.get("k")
	assert.True(t, ok)
	assert.Equal(t, 42, v)

	// A value still being computed is not carried to the next generation.
	started, finish := make(chan struct{}), make(chan struct{})
	go m.load("slow", func() int { close(started); <-finish; return 1 }, nil)
	<-started
	m.load("drop", func() int { return 2 }, nil)
	_, ok = m.get("slow")
	assert.False(t, ok)
	assert.True(t, m.has("slow"))
	next := m.carry(map[string]struct{}{"drop": {}})
	close(finish)
	assert.True(t, next.has("k"))
	assert.False(t, next.has("slow"))
	assert.False(t, next.has("drop"))
	assert.Equal(t, 7, next.load("slow", func() int { return 7 }, nil), "the next generation computes its own")
	assert.Equal(t, 1, m.load("slow", func() int { return 99 }, nil), "and the old one keeps its own")
}

// What a front end asks beside the lookups: the auto object's functions
// by name, and the text a generation's offsets count in.
func TestAutoFuncAndReadFile(t *testing.T) {
	l := newLib(t, map[string]string{"obj/a.c": "void f() { }\n"})
	s := l.ix.Snapshot()
	fn, ok := s.AutoFunc("hidden_auto")
	require.True(t, ok)
	assert.Equal(t, "/lib/sys/auto.c", fn.Path)
	assert.True(t, fn.Static)
	text, err := s.ReadFile(fn.Path)
	require.NoError(t, err)
	assert.Equal(t, "hidden_auto", string(text[fn.Off:fn.Off+11]))
	_, ok = s.AutoFunc("f")
	assert.False(t, ok)
	assert.Equal(t, "(auto object)", fn.DefinedIn)
	assert.Equal(t, "/sys/auto", s.Chain("/sys/auto").Funcs["hidden_auto"].DefinedIn, "the auto object's chain is its own")

	// A buffer is the file, as for everything else the index reads.
	l.set(t, "obj/a.c", "void g() { }\n")
	text, err = l.ix.Snapshot().ReadFile("/lib/obj/../obj/a.c")
	require.NoError(t, err)
	assert.Equal(t, "void g() { }\n", string(text))
	_, err = s.ReadFile("/lib/obj/none.c")
	assert.Error(t, err)

	// Without a lib there is no auto object.
	none := New(&config.Config{}, Layout{}, vfs.Mem{})
	_, ok = none.Snapshot().AutoFunc("anything")
	assert.False(t, ok)
	assert.False(t, none.Snapshot().AutoUnseen())
}

// What the auto object is made of, and whether all of it is known.
func TestAutoMembers(t *testing.T) {
	l := newLib(t, map[string]string{
		"sys/auto.c": "#include \"code.h\"\ninherit \"/sys/base\";\nvoid autof() { }\n",
		"sys/code.h": "void from_header() { }\n",
		"sys/base.c": "void basef() { }\n",
		"obj/a.c":    "void f() { }\n",
	})
	s := l.ix.Snapshot()
	assert.False(t, s.AutoUnseen())
	for libPath, member := range map[string]bool{"/sys/auto": true, "/sys/base": true, "/sys/code.h": true, "/obj/a": false, "/sys/none": false} {
		assert.Equal(t, member, s.AutoMember(libPath), libPath)
	}
	_, ok := s.AutoFunc("basef")
	assert.True(t, ok)

	// An object of the auto object's chain that is not there: what it
	// gives is not all known.
	l.set(t, "sys/auto.c", "inherit \"/sys/gone\";\nvoid autof() { }\n")
	assert.True(t, l.ix.Snapshot().AutoUnseen())
	// So with an auto object that is not there itself.
	missing := newLib(t, nil, func(c *config.Config) { c.AutoObjects = []string{"/sys/nowhere"} })
	assert.True(t, missing.ix.Snapshot().AutoUnseen())
}

// A file's name in the lib, and where the parameters of a declaration
// stand in it.
func TestLibFileAndParamSpans(t *testing.T) {
	l := newLib(t, map[string]string{"obj/a.c": "void f() { }\n"})
	s := l.ix.Snapshot()
	assert.Equal(t, "/obj/a.c", s.LibFile("/lib/obj/a.c"))
	assert.Equal(t, "/include/std.h", s.LibFile("/lib/include/std.h"))
	assert.Equal(t, "", s.LibFile("/elsewhere/a.c"))

	for decl, want := range map[string][]string{
		"static varargs int query(string what, mixed extra...)": {"string what", "mixed extra..."},
		"void f()":                             nil,
		"void f(void)":                         nil,
		"void f( )":                            nil,
		"f(a,b)":                               {"a", "b"},
		"int *g(  int a ,varargs string *b  )": {"int a", "varargs string *b"},
		"mixed call_other(mixed, string, mixed...)": {"mixed", "string", "mixed..."},
		"SQUARE(x)":      {"x"},
		"no parentheses": nil,
		")(":             nil,
	} {
		var got []string
		for _, span := range ParamSpans(decl) {
			got = append(got, decl[span[0]:span[1]])
		}
		assert.Equal(t, want, got, decl)
		assert.Equal(t, want, Params(decl), decl)
	}
	assert.Equal(t, [][2]int{{7, 12}, {14, 22}}, ParamSpans("void f(int a, string b)"))
}

// funcOf and varOf are the unit's function and global variable of the
// name; funcNames and varNames the names of all.
func funcOf(u *Unit, name string) FuncInfo { f, _ := u.Func(name); return f }
func varOf(u *Unit, name string) VarInfo   { v, _ := u.Var(name); return v }

func funcNames(u *Unit) []string {
	var out []string
	for _, f := range u.Funcs {
		out = append(out, f.Name)
	}
	return out
}

func varNames(u *Unit) []string {
	var out []string
	for _, v := range u.Vars {
		out = append(out, v.Name)
	}
	return out
}
