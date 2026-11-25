package rename_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/rename"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// world is a lib in memory and a linter of it.
type world struct {
	t  *testing.T
	fs vfs.Mem
	l  *lint.Linter
}

func newWorld(t *testing.T, fs vfs.Mem, where string) *world {
	t.Helper()
	prj, err := config.Discover(fs, []string{where}, config.DiscoverOptions{})
	require.NoError(t, err)
	l, err := lint.New(prj, fs)
	require.NoError(t, err)
	return &world{t: t, fs: fs, l: l}
}

// place lints the file at path and returns the offset that at marks: a
// piece of the file's text, unique in it, with a '|' where the cursor
// is.
func (w *world) place(path, at string) (*lint.Document, uint32) {
	w.t.Helper()
	doc, err := w.l.Lint(context.Background(), path, lint.Options{})
	require.NoError(w.t, err)
	cursor := strings.IndexByte(at, '|')
	require.GreaterOrEqual(w.t, cursor, 0, "no cursor in %q", at)
	needle := at[:cursor] + at[cursor+1:]
	src := string(doc.File.Src)
	require.Equal(w.t, 1, strings.Count(src, needle), "%q in %s", needle, path)
	return doc, uint32(strings.Index(src, needle) + cursor)
}

// prepare says what can be renamed at the place: "KIND NAME "text"",
// "nothing", or "refused: REASON".
func (w *world) prepare(path, at string) string {
	w.t.Helper()
	doc, off := w.place(path, at)
	t, err := doc.PrepareRename(context.Background(), off)
	var no *rename.Refusal
	switch {
	case errors.As(err, &no):
		return "refused: " + no.Reason
	case err != nil:
		w.t.Fatal(err)
	case t == nil:
		return "nothing"
	}
	return fmt.Sprintf("%s %s %q", t.Kind, t.Name, doc.File.Src[t.Start:t.End])
}

// rename renames what is at the place to name and returns the file's
// text after the edits, each warning after it on a line of its own that
// begins with "! "; or "refused: REASON".
func (w *world) rename(path, at, name string) string {
	w.t.Helper()
	doc, off := w.place(path, at)
	plan, err := doc.Rename(context.Background(), off, name, false)
	var no *rename.Refusal
	if errors.As(err, &no) {
		return "refused: " + no.Reason
	}
	require.NoError(w.t, err)
	src := string(doc.File.Src)
	for i := len(plan.Edits) - 1; i >= 0; i-- {
		e := plan.Edits[i]
		require.Equal(w.t, path, e.Path)
		require.Equal(w.t, plan.Symbol.Name, src[e.Start:e.End])
		src = src[:e.Start] + e.NewText + src[e.End:]
	}
	for _, warn := range plan.Warnings {
		src += "! " + warn + "\n"
	}
	return src
}

const cfg = `root: .
driver_config: mud.dgd
conventions:
  call_registry:
    add_action: { function: 0 }
  autosave_markers: [set_auto_save]
`

func lib() vfs.Mem {
	return vfs.Mem{
		"/lib/dgdtools.yml":   cfg,
		"/lib/mud.dgd":        "include_dirs = ({ \"/include\" });\nauto_object = \"/sys/auto\";\n",
		"/lib/include/std.h":  "#define ME this_object()\n#define STR(x) #x\n#define USES_SECRET (secret + 1)\n#define NAMES_FRESH fresh\n",
		"/lib/include/flag.h": "#ifdef MINE\n#endif\n",
		"/lib/sys/auto.c":     "static void write(string s) {}\nvoid autof() {}\nstatic void set_auto_save() {}\n",
		"/lib/std/base.c":     "int base_var;\nvoid inherited_fn() {}\nstatic void sfn() {}\n",
		"/lib/obj/a.c":        a,
	}
}

const a = `#include <std.h>
inherit base "/std/base";

#define TWICE(v) ((v) * 2)
#define LIMIT 10

private int counter;
private static int cache;
int pub;

private int helper(int n) {
    int total;
    total = n + counter;
    if (total > LIMIT) {
        int m;
        m = 3;
        total += m;
    }
    return TWICE(total);
}

private void tick();

static void sfunc() {
    int i, j;
    for (i = 0; i < LIMIT; i++) {
        j += helper(i);
    }
    call_out("helper", 1);
    tick();
    cache = j;
    try { j = 1; } catch (err) { write(err); }
}

private void tick() {}

void create() {
    base::inherited_fn();
    ME->helper(1);
    pub = base_var;
}
`

// What is renamed, by kind: every occurrence of the symbol, and nothing
// else of the name.
func TestRenameKinds(t *testing.T) {
	w := newWorld(t, lib(), "/lib")
	require.True(t, w.l.Scan(context.Background()))
	const p = "/lib/obj/a.c"

	// A local: its declaration and its uses in its scope.
	got := w.rename(p, "int to|tal;", "sum")
	assert.Contains(t, got, "    int sum;\n    sum = n + counter;\n    if (sum > LIMIT) {")
	assert.Contains(t, got, "        sum += m;\n    }\n    return TWICE(sum);")
	assert.NotContains(t, got, "total")

	// A parameter, from a use.
	got = w.rename(p, "total = |n + counter", "count")
	assert.Contains(t, got, "private int helper(int count) {\n    int total;\n    total = count + counter;")

	// A local of an inner block, and a catch variable.
	got = w.rename(p, "        |m = 3;", "k")
	assert.Contains(t, got, "        int k;\n        k = 3;\n        total += k;")
	got = w.rename(p, "catch (e|rr)", "e")
	assert.Contains(t, got, "catch (e) { write(e); }")

	// A private function: definition, prototypes, calls; the call_out
	// string and ->helper are no calls of it in DGD, and are left, with
	// a warning each.
	got = w.rename(p, "private int hel|per(", "assist")
	assert.Contains(t, got, "private int assist(int n) {")
	assert.Contains(t, got, "j += assist(i);")
	assert.Contains(t, got, `call_out("helper", 1);`)
	assert.Contains(t, got, "ME->helper(1);")
	assert.Contains(t, got, `! line 29: call_out("helper") cannot reach the private function 'helper'`)
	assert.Contains(t, got, "! line 39: ->helper cannot reach the private function 'helper'")
	got = w.rename(p, "    ti|ck();", "tock")
	assert.Contains(t, got, "private void tock();")
	assert.Contains(t, got, "    tock();\n")
	assert.Contains(t, got, "private void tock() {}")

	// A private global variable, static or not saved.
	got = w.rename(p, "private int coun|ter;", "hits")
	assert.Contains(t, got, "private int hits;")
	assert.Contains(t, got, "total = n + hits;")
	got = w.rename(p, "    cac|he = j;", "memo")
	assert.Contains(t, got, "private static int memo;")
	assert.Contains(t, got, "    memo = j;")

	// A macro the file defines: the #define, and every use.
	got = w.rename(p, "#define LIM|IT 10", "MAX")
	assert.Contains(t, got, "#define MAX 10")
	assert.Contains(t, got, "if (total > MAX)")
	assert.Contains(t, got, "i < MAX;")
	got = w.rename(p, "return TWI|CE(", "DOUBLE")
	assert.Contains(t, got, "#define DOUBLE(v) ((v) * 2)")
	assert.Contains(t, got, "return DOUBLE(total);")

	// An inherit label.
	got = w.rename(p, "inherit ba|se", "parent")
	assert.Contains(t, got, "inherit parent \"/std/base\";")
	assert.Contains(t, got, "    parent::inherited_fn();")

	// The same name is a plan without edits.
	assert.Equal(t, a, w.rename(p, "int to|tal;", "total"))
}

// Prepare answers the name's place, or why not.
func TestPrepare(t *testing.T) {
	w := newWorld(t, lib(), "/lib")
	require.True(t, w.l.Scan(context.Background()))
	const p = "/lib/obj/a.c"
	for at, want := range map[string]string{
		"int to|tal;":           `local total "total"`,
		"int total|;":           `local total "total"`,
		"helper(int |n)":        `parameter n "n"`,
		"catch (e|rr)":          `local err "err"`,
		"j += hel|per(i)":       `function helper "helper"`,
		"private int coun|ter;": `variable counter "counter"`,
		"#define LIM|IT":        `macro LIMIT "LIMIT"`,
		"base::inh|erited_fn":   `function inherited_fn "inherited_fn"`,
		"inherit ba|se":         `label base "base"`,
		"|base::":               `label base "base"`,

		"int pu|b;":                `variable pub "pub"`,
		"static void sfu|nc":       `function sfunc "sfunc"`,
		"void cre|ate":             "refused: 'create' is the function the driver calls by name to initialize every object (create in the configuration)",
		"wri|te(err)":              `function write "write"`,
		"pub = base_v|ar":          `variable base_var "base_var"`,
		"|ME->":                    `macro ME "ME"`,
		"#include <st|d.h>":        "refused: 'std.h' names a file: rename the file itself (in the editor, or with dgdtools rename FILE NEW), and the paths that name it are changed with it",
		"inherit base \"/std/b|as": "refused: '/std/base' names a file: rename the file itself (in the editor, or with dgdtools rename FILE NEW), and the paths that name it are changed with it",
		"call_out(\"hel|per":       "refused: DGD does not call a private function by name (call_out()): this is not a call of 'helper'; rename it where it is declared or called",
		"ME->hel|per":              "refused: DGD does not call a private function by name (->): this is not a call of 'helper'; rename it where it is declared or called",
		"call_o|ut(":               "refused: 'call_out' is a kfun, a function of the driver",
		"in|t total;":              "nothing",
		"for (|i = 0":              `local i "i"`,
		"i < LIMIT; i|++":          `local i "i"`,
		"1|0\n":                    "nothing",
	} {
		assert.Equal(t, want, w.prepare(p, at), at)
	}
}

// moreLib is lib with files for the refusals.
func moreLib() vfs.Mem {
	fs := lib()
	for p, text := range map[string]string{
		"/lib/obj/saver.c":   "private int gold;\nvoid save() { save_object(\"/data/x\"); }\n",
		"/lib/obj/parent.c":  "private int tally;\nvoid f() { tally++; }\n",
		"/lib/obj/heir.c":    "inherit \"/obj/parent\";\nvoid create() { set_auto_save(); }\n",
		"/lib/obj/inc.c":     "private void incfn() {}\nvoid g() { incfn(); }\n",
		"/lib/obj/user.c":    "#include \"inc.c\"\nvoid h() {}\n",
		"/lib/obj/dead.c":    "private void dfn() {}\nvoid f() {\n    int x;\n#ifdef NOPE\n    x = 1;\n    dfn();\n#endif\n    x = 2;\n    dfn();\n}\nvoid g() {\n    int fresh;\n#ifdef NOPE\n    fresh = 1;\n#endif\n}\n",
		"/lib/obj/bad.c":     "void f() {\n    int y, q;\n    y = = 2;\n    y = 3;\n    q = 1;\n}\nvoid g() {\n    int q;\n    q = 2;\n}\n",
		"/lib/obj/calls.c":   "private void p() {}\nvoid f() {\n    object ob;\n    p();\n    later();\n    zz = 1;\n    ob->foo();\n}\n",
		"/lib/obj/mac.c":     "#define MINE 1\n#include <flag.h>\nvoid f() { int y; y = MINE; }\n",
		"/lib/obj/through.c": "#include <std.h>\nprivate int secret;\nvoid f() { int z; z = USES_SECRET; }\nvoid h() { int z; z = NAMES_FRESH; }\n",
		"/lib/obj/str.c":     "#include <std.h>\nvoid f() { int w; write(STR(w)); }\n",
	} {
		fs[p] = text
	}
	return fs
}

// Each reason a symbol is not renamed.
func TestRefusals(t *testing.T) {
	w := newWorld(t, moreLib(), "/lib")
	require.True(t, w.l.Scan(context.Background()))
	for _, c := range []struct{ path, at, want string }{
		{"/lib/obj/saver.c", "private int go|ld", "refused: 'gold' is written by save_object to the save files under its name (this file names save_object at line 2): renamed, restore_object no longer finds the saved value"},
		{"/lib/obj/parent.c", "private int tal|ly", "refused: 'tally' is written by save_object to the save files under its name (/obj/heir names set_auto_save): renamed, restore_object no longer finds the saved value"},
		{"/lib/obj/inc.c", "private void inc|fn", `function incfn "incfn"`},
		{"/lib/obj/dead.c", "    x = |2;", "nothing"},
		{"/lib/obj/dead.c", "    |x = 1;", "refused: 'x' is in code that is not compiled here (a branch of #if that is not taken): what it names cannot be told"},
		{"/lib/obj/bad.c", "    |y = = 2;", "refused: 'y' is in code that does not parse: what it names cannot be told"},
		{"/lib/obj/calls.c", "    z|z = 1;", "refused: 'zz' names nothing the tools know of here"},
		{"/lib/obj/calls.c", "ob->fo|o()", "refused: 'foo' is called in an object that cannot be told: what it names is not known"},
		{"/lib/obj/calls.c", "    la|ter();", "refused: no file the tools see declares a function 'later'"},
		{"/lib/obj/mac.c", "#define MI|NE", "refused: 'MINE' is used in /include/flag.h, which this file includes, where it is no macro of this file's"},
	} {
		assert.Equal(t, c.want, w.prepare(c.path, c.at), c.path+": "+c.at)
	}
	for _, c := range []struct{ path, at, name, want string }{
		// Code that cannot be read, where the old name or the new one
		// stands.
		{"/lib/obj/dead.c", "    |x = 2;", "y", "refused: 'x' at line 5 is in code that is not compiled here (a branch of #if that is not taken): it may be the local variable being renamed"},
		{"/lib/obj/dead.c", "void d|fn", "efn", "refused: 'dfn' at line 6 is in code that is not compiled here (a branch of #if that is not taken): it may be the function being renamed"},
		{"/lib/obj/dead.c", "    int fre|sh;", "stale", "refused: 'fresh' at line 14 is in code that is not compiled here (a branch of #if that is not taken): it may be the local variable being renamed"},
		{"/lib/obj/bad.c", "    int |y, q;", "z", "refused: 'y' at line 3 is in code that does not parse: it may be the local variable being renamed"},
		{"/lib/obj/bad.c", "    |q = 1;", "y", "refused: 'y' is at line 3, in code that does not parse: what it names there cannot be told"},
		{"/lib/obj/through.c", "private int sec|ret", "hidden", "refused: 'secret' is named in the body of the macro USES_SECRET, used at line 3: the macro would have to change with it"},
		{"/lib/obj/through.c", "void h() { int |z", "fresh", "refused: the macro NAMES_FRESH, used at line 4, names 'fresh' in its body: it would name the renamed local variable there"},
		{"/lib/obj/str.c", "int |w;", "v", "refused: 'w' at line 2 is an argument of the macro STR, which makes a string of its arguments or pastes them (# or ##): it may be the local variable being renamed"},
	} {
		assert.Equal(t, c.want, w.rename(c.path, c.at, c.name), c.path+": "+c.at+" to "+c.name)
	}
	// A local in a function with code that does not parse is renamed
	// where neither name is in it.
	assert.Contains(t, w.rename("/lib/obj/bad.c", "    |q = 2;", "r"), "    int r;\n    r = 2;\n")
}

// A new name that would change what a name means.
func TestClashes(t *testing.T) {
	w := newWorld(t, moreLib(), "/lib")
	require.True(t, w.l.Scan(context.Background()))
	const p = "/lib/obj/a.c"
	for _, c := range []struct{ at, name, want string }{
		{"int to|tal;", "n", "refused: 'n' is declared in the same scope already, at line 11"},
		{"int to|tal;", "m", "refused: at line 17 the renamed local variable would be hidden by the 'm' declared at line 15"},
		{"int to|tal;", "counter", "refused: 'counter' at line 13 names the global variable 'counter' (line 7): the renamed local variable would take its place there"},
		{"        |m = 3;", "total", "refused: 'total' at line 17 names the local variable 'total' (line 12): the renamed local variable would take its place there"},
		{"helper(int |n)", "LIMIT", "refused: 'LIMIT' is a macro here (#define in /obj/a.c)"},
		{"helper(int |n)", "ME", "refused: 'ME' is a macro here (#define in /include/std.h)"},
		{"for (|i = 0", "int", "refused: 'int' is a keyword"},
		{"for (|i = 0", "2x", "refused: '2x' is not a name: a name is letters, digits and '_', and does not begin with a digit"},
		{"for (|i = 0", "j", "refused: 'j' is declared in the same scope already, at line 25"},
		{"private int coun|ter;", "total", "refused: at line 13 the renamed variable would be hidden by the local variable 'total' declared at line 12"},
		{"private int coun|ter;", "pub", "refused: the object has a global variable 'pub' already (line 9)"},
		{"private int coun|ter;", "base_var", "refused: 'base_var' is a global variable inherited from /std/base: the renamed variable would hide it here"},
		{"private int hel|per(", "tick", "refused: the object has a function 'tick' already (line 35)"},
		{"private int hel|per(", "inherited_fn", "refused: 'inherited_fn' is a function the object inherits from /std/base: the renamed function would take its place in this file's calls"},
		{"private int hel|per(", "autof", "refused: 'autof' is a function the object has from the auto object: the renamed function would take its place in this file's calls"},
		{"private int hel|per(", "strlen", "refused: 'strlen' is a kfun: this file's calls of it would call the renamed function instead"},
		{"#define LIM|IT 10", "total", "refused: 'total' is used in this file already, at line 12"},
		{"#define LIM|IT 10", "ME", "refused: 'ME' is a macro here (#define in /include/std.h)"},
		{"#define LIM|IT 10", "autof", "refused: 'autof' is a function of the auto object: the macro would replace it in this file"},
		{"#define LIM|IT 10", "secret", "refused: 'secret' is used in /include/std.h, which this file includes"},
		{"inherit ba|se", "LIMIT", "refused: 'LIMIT' is a macro here (#define in /obj/a.c)"},
	} {
		assert.Equal(t, c.want, w.rename(p, c.at, c.name), c.at+" to "+c.name)
	}
	assert.Equal(t, "refused: 'later' is called at line 5: the call would reach the renamed function",
		w.rename("/lib/obj/calls.c", "private void |p", "later"))
	// A local may take the name of one it is in the scope of, where that
	// one is not used: it hides it.
	assert.Contains(t, w.rename(p, "        |m = 3;", "n"), "        int n;\n        n = 3;\n        total += n;")
	// Or a global's name, in a function that does not use the global.
	assert.Contains(t, w.rename(p, "    int i|, j;", "pub"), "for (pub = 0; pub < LIMIT; pub++)")
}

// A wizard's checkout with a profile: what a stub declares is not to be
// renamed from the checkout. Before the scan is complete, a symbol other
// files may name is not renamed either.
func TestRefusalsPartialAndUnscanned(t *testing.T) {
	p := &profile.Profile{
		Manifest: profile.Manifest{
			Name: "mini", Dialect: profile.Dialect{Driver: "dgd"},
			IncludeDirs: []string{"/include"}, AutoObjects: []string{"/sys/auto"},
			Covered: []string{"/std/", "/include/", "/sys/"},
		},
		Files: map[string]string{
			"/include/std.h": "#define ROOM \"/std/room\"\n#define SHORT \"a hall\"\n",
			"/sys/auto.c":    "void autof();\n",
			"/std/room.c":    "int room_var;\nvoid set_short(string s);\n",
		},
	}
	data, err := p.Marshal()
	require.NoError(t, err)
	fs := vfs.Mem{
		"/home/ann/mini.profile": string(data),
		"/home/ann/dgdtools.yml": "profile: mini.profile\nlib_path: /players/ann\n",
		"/home/ann/hall.c":       "#include <std.h>\ninherit ROOM;\nprivate int seen;\nvoid create() { set_short(SHORT); seen = 1; }\n",
	}
	w := newWorld(t, fs, "/home/ann")
	const hall = "/home/ann/hall.c"

	// Not scanned, and no time to: a local is renamed, a private
	// variable is not.
	doc, err := w.l.Lint(context.Background(), hall, lint.Options{NoScan: true})
	require.NoError(t, err)
	off := uint32(strings.Index(string(doc.File.Src), "seen = 1") + 2)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = doc.PrepareRename(cancelled, off)
	assert.EqualError(t, err, "the mudlib is not indexed in full yet, so it cannot be told whether another file includes this one: try again in a moment")

	require.True(t, w.l.Scan(context.Background()))
	assert.Equal(t, "refused: 'set_short' is defined in the mudlib profile, not in your checkout", w.prepare(hall, "set_sh|ort(SHORT"))
	assert.Equal(t, "refused: 'SHORT' is defined in the mudlib profile, not in your checkout", w.prepare(hall, "set_short(SHO|RT)"))
	assert.Equal(t, "#include <std.h>\ninherit ROOM;\nprivate int saw;\nvoid create() { set_short(SHORT); saw = 1; }\n",
		w.rename(hall, "se|en = 1", "saw"))
}
