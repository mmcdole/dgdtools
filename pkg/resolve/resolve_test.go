package resolve_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/calls"
	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/resolve"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// world is a lib in memory and a linter of it.
type world struct {
	t  *testing.T
	fs vfs.Mem
	l  *lint.Linter
}

// newWorld resolves the project of the file or directory at where.
func newWorld(t *testing.T, fs vfs.Mem, where string) *world {
	t.Helper()
	prj, err := config.Discover(fs, []string{where}, config.DiscoverOptions{})
	require.NoError(t, err)
	l, err := lint.New(prj, fs)
	require.NoError(t, err)
	return &world{t: t, fs: fs, l: l}
}

// ref resolves the place in the file at path that at marks: a piece of
// the file's text, unique in it, with a '|' where the cursor is.
func (w *world) ref(path, at string) (*resolve.Ref, *lint.Document) {
	w.t.Helper()
	doc, err := w.l.Lint(context.Background(), path, lint.Options{NoScan: true})
	require.NoError(w.t, err)
	cursor := strings.IndexByte(at, '|')
	require.GreaterOrEqual(w.t, cursor, 0, "no cursor in %q", at)
	needle := at[:cursor] + at[cursor+1:]
	src := string(doc.File.Src)
	require.Equal(w.t, 1, strings.Count(src, needle), "%q in %s", needle, path)
	return doc.Resolve(uint32(strings.Index(src, needle) + cursor)), doc
}

// at says what the place refers to, in a line:
//
//	KIND NAME "the text that names it" [decl] -> FILE:LINE:COL "the name there" ...
//
// with the file alone for a declaration that is a file as a whole.
func (w *world) at(path, at string) string {
	w.t.Helper()
	ref, doc := w.ref(path, at)
	if ref == nil {
		return "nothing"
	}
	out := fmt.Sprintf("%s %s %q", ref.Kind, ref.Name, doc.File.Src[ref.Start:ref.End])
	if ref.Decl {
		out += " decl"
	}
	for _, d := range ref.Defs {
		assert.Equal(w.t, ref.Kind, d.Kind)
		out += " -> " + d.Path
		if d.Start == d.End {
			continue
		}
		text, err := doc.Snapshot.ReadFile(d.Path)
		require.NoError(w.t, err, d.Path)
		line := 1 + bytes.Count(text[:d.Start], []byte("\n"))
		col := int(d.Start) - bytes.LastIndexByte(text[:d.Start], '\n')
		out += fmt.Sprintf(":%d:%d %q", line, col, text[d.Start:d.End])
	}
	return out
}

// driverConfig is the driver's config file of the libs of these tests.
const driverConfig = "include_file = \"/include/auto.h\";\ninclude_dirs = ({ \"/include\" });\nauto_object = \"/sys/auto\";\n"

const config1 = `root: .
driver_config: ../mud.dgd
conventions:
  call_registry:
    add_action: { function: 0 }
    store_fp: [{ object: 0, function: 1 }, { function: 0 }]
  object_registry:
    load: 0
`

func wholeLib() vfs.Mem {
	return vfs.Mem{
		"/lib/dgdtools.yml":   config1,
		"/mud.dgd":            driverConfig,
		"/lib/include/auto.h": "#define AUTO 1\n",
		"/lib/include/std.h": "#define STD \"/std/base\"\n#define ROOM_D \"/sys/roomd\"\n" +
			"#  define /* squares */ SQUARE(x) ((x)*(x))\n#define ME this_object()\n#define status int\n",
		"/lib/sys/auto.c": "void autof() {}\nstatic void write(string s) {}\nvoid create() {}\nnomask int sealed() { return 1; }\n",
		"/lib/std/base.c": "int base_var;\nprivate int secret;\nvoid create() {}\nint foo(int a) { return a; }\n" +
			"static void sfn() {}\nprivate void pfn() {}\nstring query_name();\n",
		"/lib/std/mid.c":   "inherit \"/std/base\";\nvoid create() { ::create(); }\nvoid bar() {}\n",
		"/lib/std/side.c":  "void setup() {}\nvoid create() {}\n",
		"/lib/sys/roomd.c": "void register(object o) {}\n",
		"/lib/obj/local.h": "#define LOCAL 1\nint from_header() { return LOCAL; }\nint header_var;\n",
		"/lib/obj/b.c":     "inherit \"/std/mid\";\nprivate void mine() {}\nvoid create() { }\n",
		"/lib/obj/a.c": `#include <std.h>
#include "local.h"
inherit STD;
inherit side "/std/side";

int count;
void later();

void create() {
    int n;
    ::create();
    side::setup();
    foo(1);
    autof();
    sizeof(({ }));
    count = base_var + n + SQUARE(2) + header_var;
    this_object()->foo(2);
    ME->later();
    "/obj/b"->bar();
    ROOM_D->register(this_object());
    call_other("/obj/b", "create");
    call_other(this_player(), "bar");
    call_out("later", 1);
    add_action("sfn", "x");
    store_fp("/sys/roomd", "register");
    clone_object("/obj/b");
    load("/sys/roomd");
    n->unknown();
    from_header(LOCAL, AUTO);
    "/obj/b"->mine();
    "/obj/b"->autof();
    "/obj/none"->autof();
    "/obj/b"->pfn();
    write("foo");
    query_name();
}
void later() { secret; pfn(); /* foo( */ }
status typed(int count, int other) { return count + base_var; }
void broken() { int count = ; foo(0); count = 2; base_var = 4; }
#if 0
void dead() { foo(3); }
#ifdef AUTO
#endif
#elif AUTO
#endif
#ifdef AUTO
#define TWICE(v) (SQUARE(v) + v + count)
#undef TWICE
#endif
`,
	}
}

func TestWholeLib(t *testing.T) {
	w := newWorld(t, wholeLib(), "/lib")
	const a = "/lib/obj/a.c"
	for at, want := range map[string]string{
		// #include: the header, from the include directories or beside
		// the file; anywhere on the line.
		"#include <st|d.h>":   `include std.h "<std.h>" -> /lib/include/std.h`,
		"#incl|ude <std.h>":   `include std.h "<std.h>" -> /lib/include/std.h`,
		"#include <std.h>|":   `include std.h "<std.h>" -> /lib/include/std.h`,
		"#include \"loc|al.h": `include local.h "\"local.h\"" -> /lib/obj/local.h`,

		// inherit: the object, by its path however it is written; the
		// label is declared where it stands.
		"inherit ST|D;":       `object /std/base "STD" -> /lib/std/base.c`,
		"inherit side \"/st|": `object /std/side "\"/std/side\"" -> /lib/std/side.c`,
		"inherit si|de":       `label side "side" decl -> /lib/obj/a.c:4:9 "side"`,
		"inh|erit STD;":       `object /std/base "STD" -> /lib/std/base.c`,
		"inherit STD|;":       `object /std/base "STD" -> /lib/std/base.c`,
		"|inherit side":       `object /std/side "\"/std/side\"" -> /lib/std/side.c`,
		"si|de::setup":        `label side "side" -> /lib/obj/a.c:4:9 "side"`,

		// Calls the compiler binds: the file's own function, the
		// inherited one, the auto object's; nothing for the driver's.
		"::cre|ate();":     `function create "create" -> /lib/std/base.c:3:6 "create" -> /lib/std/side.c:2:6 "create"`,
		"side::se|tup":     `function setup "setup" -> /lib/std/side.c:1:6 "setup"`,
		"|foo(1)":          `function foo "foo" -> /lib/std/base.c:4:5 "foo"`,
		"foo|(1)":          `function foo "foo" -> /lib/std/base.c:4:5 "foo"`,
		"auto|f();\n    s": `function autof "autof" -> /lib/sys/auto.c:1:6 "autof"`,
		"size|of":          `function sizeof "sizeof"`,
		"wri|te(\"foo\")":  `function write "write" -> /lib/sys/auto.c:2:13 "write"`,
		"from_he|ader(":    `function from_header "from_header" -> /lib/obj/local.h:2:5 "from_header"`,
		// A prototype is all the chain has of it.
		"query_na|me();": `function query_name "query_name" -> /lib/std/base.c:7:8 "query_name"`,
		// A private function of an inherited object is not this one's.
		"pf|n(); /*": `function pfn "pfn"`,

		// A declaration is its own, a prototype goes to the definition.
		"void lat|er();":   `function later "later" decl -> /lib/obj/a.c:37:6 "later"`,
		"void lat|er() {":  `function later "later" decl -> /lib/obj/a.c:37:6 "later"`,
		"void cre|ate() {": `function create "create" decl -> /lib/obj/a.c:9:6 "create"`,
		"int cou|nt;":      `variable count "count" decl -> /lib/obj/a.c:6:5 "count"`,

		// Global variables: the file's, an inherited object's, one a
		// header declares; never a private one of another object. A
		// local or a parameter where one is in scope.
		"cou|nt = base_var":     `variable count "count" -> /lib/obj/a.c:6:5 "count"`,
		"count = base_v|ar":     `variable base_var "base_var" -> /lib/std/base.c:1:5 "base_var"`,
		"+ header_v|ar":         `variable header_var "header_var" -> /lib/obj/local.h:3:5 "header_var"`,
		"base_var + |n":         `local n "n" -> /lib/obj/a.c:10:9 "n"`,
		"int |n;":               `local n "n" decl -> /lib/obj/a.c:10:9 "n"`,
		"sec|ret;":              `nothing`,
		"return cou|nt":         `parameter count "count" -> /lib/obj/a.c:38:18 "count"`,
		"int cou|nt, int other": `parameter count "count" decl -> /lib/obj/a.c:38:18 "count"`,
		"count + base_va|r; }":  `variable base_var "base_var" -> /lib/std/base.c:1:5 "base_var"`,
		// A statement that could not be read may declare what it names.
		"base_v|ar = 4":          `variable base_var "base_var" -> /lib/std/base.c:1:5 "base_var"`,
		"cou|nt = 2":             `nothing`,
		"|status typed":          `macro status "status" -> /lib/include/std.h:5:9 "status"`,
		"int count, int oth|er)": `parameter other "other" decl -> /lib/obj/a.c:38:29 "other"`,

		// Macros: the #define, in the header, the force-included one,
		// or this file; in a directive too.
		"SQU|ARE(2)":         `macro SQUARE "SQUARE" -> /lib/include/std.h:3:25 "SQUARE"`,
		"header(LOC|AL":      `macro LOCAL "LOCAL" -> /lib/obj/local.h:1:9 "LOCAL"`,
		"LOCAL, AU|TO":       `macro AUTO "AUTO" -> /lib/include/auto.h:1:9 "AUTO"`,
		"RO|OM_D->":          `macro ROOM_D "ROOM_D" -> /lib/include/std.h:2:9 "ROOM_D"`,
		"|ME->":              `macro ME "ME" -> /lib/include/std.h:4:9 "ME"`,
		"#ifdef AU|TO\n#def": `macro AUTO "AUTO" -> /lib/include/auto.h:1:9 "AUTO"`,
		"#define TWI|CE":     `macro TWICE "TWICE" decl -> /lib/obj/a.c:47:9 "TWICE"`,
		"(SQUA|RE(v)":        `macro SQUARE "SQUARE" -> /lib/include/std.h:3:25 "SQUARE"`,
		"#undef TWI|CE":      `macro TWICE "TWICE" -> /lib/obj/a.c:47:9 "TWICE"`,
		"SQUARE(|v) + v":     `nothing`, // a parameter of the macro
		"+ v + cou|nt)":      `nothing`, // what a body means is decided where it is used
		"#def|ine TWICE":     `nothing`,
		"#if|def AUTO\n#def": `nothing`,
		"#elif AU|TO":        `macro AUTO "AUTO" -> /lib/include/auto.h:1:9 "AUTO"`,
		"#ifdef AU|TO\n#end": `nothing`, // in a branch that is skipped

		// Calls bound when they run, with a receiver that is known.
		"this_object()->fo|o(2)":  `function foo "foo" -> /lib/std/base.c:4:5 "foo"`,
		"ME->lat|er":              `function later "later" -> /lib/obj/a.c:37:6 "later"`,
		"\"/obj/b\"->ba|r":        `function bar "bar" -> /lib/std/mid.c:3:6 "bar"`,
		"ROOM_D->regi|ster":       `function register "register" -> /lib/sys/roomd.c:1:6 "register"`,
		"\"/obj/b\", \"cre|ate":   `function create "create" -> /lib/obj/b.c:3:6 "create"`,
		"\"/obj/b\", \"create\"|": `function create "create" -> /lib/obj/b.c:3:6 "create"`,
		"call_out(\"lat|er":       `function later "later" -> /lib/obj/a.c:37:6 "later"`,
		"add_action(\"sf|n":       `function sfn "sfn" -> /lib/std/base.c:5:13 "sfn"`,
		"roomd\", \"regi|ster":    `function register "register" -> /lib/sys/roomd.c:1:6 "register"`,
		"\"/obj/b\"->mi|ne":       `function mine "mine" -> /lib/obj/b.c:2:14 "mine"`,
		"\"/obj/b\"->auto|f":      `function autof "autof" -> /lib/sys/auto.c:1:6 "autof"`,
		// A receiver nobody knows, an object that is not there, a
		// function private to what the receiver inherits: the call is
		// one, and nothing is said of where it goes.
		"this_player(), \"ba|r": `function bar "bar"`,
		"n->unkn|own":           `function unknown "unknown"`,
		"\"/obj/none\"->auto|f": `function autof "autof"`,
		"\"/obj/b\"->pf|n":      `function pfn "pfn"`,
		// The registrar itself is a function like another.
		"call_o|ut(": `function call_out "call_out"`,

		// Object paths handed to the functions that take one.
		"clone_object(\"/ob|j/b": `object /obj/b "/obj/b" -> /lib/obj/b.c`,
		"load(\"/sys|/roomd":     `object /sys/roomd "/sys/roomd" -> /lib/sys/roomd.c`,

		// Nothing: a plain string, a comment, code that is not compiled,
		// a keyword, an operator.
		"write(\"fo|o\")":  `nothing`,
		"/* fo|o( */":      `nothing`,
		"dead() { fo|o(3)": `nothing`,
		"vo|id later();":   `nothing`,
		"count |= base":    `nothing`,
	} {
		assert.Equal(t, want, w.at(a, at), "at %q", at)
	}

	// What a reference carries beside the places: the facts of the
	// declaration, and of a late-bound call how it is made.
	ref, _ := w.ref(a, "|foo(1)")
	require.Len(t, ref.Defs, 1)
	assert.Equal(t, "/std/base", ref.Defs[0].Object)
	assert.Equal(t, "int foo(int a)", ref.Defs[0].Func.Decl)
	assert.Nil(t, ref.Call)
	ref, _ = w.ref(a, "auto|f();\n    s")
	assert.True(t, ref.Defs[0].Auto)
	assert.Equal(t, "/sys/auto", ref.Defs[0].Object)
	ref, _ = w.ref(a, "count = base_v|ar")
	assert.Equal(t, "/std/base", ref.Defs[0].Object)
	assert.Equal(t, "int", ref.Defs[0].Var.Type)
	ref, _ = w.ref(a, "SQU|ARE(2)")
	assert.Equal(t, "((x)*(x))", ref.Defs[0].Macro.Body)
	ref, _ = w.ref(a, "n->unkn|own")
	require.NotNil(t, ref.Call)
	assert.Equal(t, calls.TargetUnknown, ref.Call.Target)
	ref, _ = w.ref(a, "\"/obj/b\"->ba|r")
	assert.Equal(t, calls.TargetPath, ref.Call.Target)
	assert.Equal(t, "/obj/b", ref.Call.TargetPath)
	assert.Equal(t, "/std/mid", ref.Defs[0].Object)
	ref, _ = w.ref(a, "call_out(\"lat|er")
	assert.Equal(t, "call_out", ref.Call.Via)

	for k, name := range map[resolve.Kind]string{
		resolve.Include: "include", resolve.Object: "object", resolve.Label: "label",
		resolve.Function: "function", resolve.Variable: "variable", resolve.Macro: "macro", 0: "unknown",
		resolve.Local: "local", resolve.Parameter: "parameter", resolve.Keyword: "keyword", resolve.Directory: "directory",
	} {
		assert.Equal(t, name, k.String())
	}
}

// Which of several inherited functions a name means.
func TestInherited(t *testing.T) {
	fs := wholeLib()
	// mid redefines what base has: through both, the name is mid's.
	fs["/lib/obj/d.c"] = "inherit \"/std/base\";\ninherit m \"/std/mid\";\nvoid f() { create(); ::create(); m::create(); foo(1); ::foo(1); m::bar(); m::nope(); x::bar(); }\n"
	// A prototype of the file's own beside an inherited definition, and
	// beside nothing; a function the file has, whatever is inherited.
	fs["/lib/obj/e.c"] = "inherit \"/std/base\";\nint foo(int a);\nvoid elsewhere();\nvoid create() { foo(1); elsewhere(); create(); ::create(); ::autof(); ::write(\"x\"); }\n"
	// Everything below a private inherit is private to the inheritor:
	// its own code reaches it, an inheritor's does not.
	fs["/lib/std/priv.c"] = "private inherit \"/std/base\";\nvoid f() { foo(1); base_var; }\n"
	fs["/lib/obj/f.c"] = "inherit \"/std/priv\";\nvoid g() { foo(1); base_var; f(); \"/std/priv\"->foo(1); }\n"
	// An inherit that does not resolve: what is known stands, the rest
	// is silent. What the object nobody can see has may be a function of
	// the auto object's name, unless that one is nomask.
	fs["/lib/obj/g.c"] = "inherit NOWHERE;\ninherit \"/std/gone\";\ninherit \"/std/side\";\nvoid g() { setup(); autof(); nope(); sealed(); ::autof(); ::sealed(); \"/obj/g\"->autof(); \"/obj/g\"->sealed(); }\n"
	w := newWorld(t, fs, "/lib")

	const mid, base = `-> /lib/std/mid.c:2:6 "create"`, `-> /lib/std/base.c:3:6 "create"`
	for at, want := range map[string]string{
		"{ cre|ate()":   `function create "create" ` + mid,
		"; ::cre|ate()": `function create "create" ` + mid,
		"m::cre|ate()":  `function create "create" ` + mid,
		"; fo|o(1)":     `function foo "foo" -> /lib/std/base.c:4:5 "foo"`,
		"::fo|o(1)":     `function foo "foo" -> /lib/std/base.c:4:5 "foo"`,
		"m::ba|r()":     `function bar "bar" -> /lib/std/mid.c:3:6 "bar"`,
		"m::no|pe()":    `function nope "nope"`,
		"x::ba|r()":     `function bar "bar"`,
		"|x::bar()":     `label x "x"`,
	} {
		assert.Equal(t, want, w.at("/lib/obj/d.c", at), "at %q", at)
	}
	for at, want := range map[string]string{
		"{ fo|o(1)":       `function foo "foo" -> /lib/std/base.c:4:5 "foo"`,
		"int fo|o(int a)": `function foo "foo" decl -> /lib/std/base.c:4:5 "foo"`,
		"; elsew|here()":  `function elsewhere "elsewhere" -> /lib/obj/e.c:3:6 "elsewhere"`,
		"; cre|ate();":    `function create "create" -> /lib/obj/e.c:4:6 "create"`,
		"; ::cre|ate()":   `function create "create" ` + base,
		"::auto|f()":      `function autof "autof" -> /lib/sys/auto.c:1:6 "autof"`,
		"::wri|te(":       `function write "write" -> /lib/sys/auto.c:2:13 "write"`,
	} {
		assert.Equal(t, want, w.at("/lib/obj/e.c", at), "at %q", at)
	}
	assert.Equal(t, `function foo "foo" -> /lib/std/base.c:4:5 "foo"`, w.at("/lib/std/priv.c", "{ fo|o(1)"))
	assert.Equal(t, `variable base_var "base_var" -> /lib/std/base.c:1:5 "base_var"`, w.at("/lib/std/priv.c", "base_v|ar;"))
	for at, want := range map[string]string{
		"{ fo|o(1)":   `function foo "foo"`,
		"base_v|ar;":  `nothing`,
		"; |f();":     `function f "f" -> /lib/std/priv.c:2:6 "f"`,
		"\"->fo|o(1)": `function foo "foo"`,
	} {
		assert.Equal(t, want, w.at("/lib/obj/f.c", at), "at %q", at)
	}
	for at, want := range map[string]string{
		"inherit NOW|HERE":    `nothing`,
		"inherit \"/std/go|n": `object /std/gone "\"/std/gone\""`,
		"set|up()":            `function setup "setup" -> /lib/std/side.c:1:6 "setup"`,
		" auto|f()":           `function autof "autof"`,
		"no|pe()":             `function nope "nope"`,
		" seal|ed()":          `function sealed "sealed" -> /lib/sys/auto.c:4:12 "sealed"`,
		"::auto|f()":          `function autof "autof"`,
		"::seal|ed()":         `function sealed "sealed" -> /lib/sys/auto.c:4:12 "sealed"`,
		"\"->auto|f()":        `function autof "autof"`,
		"\"->seal|ed()":       `function sealed "sealed" -> /lib/sys/auto.c:4:12 "sealed"`,
	} {
		assert.Equal(t, want, w.at("/lib/obj/g.c", at), "at %q", at)
	}
}

// A wizard's checkout and a profile: what is not in the checkout is in
// the profile's stubs, and a declaration there is at the stub's location.
func TestPartialCheckout(t *testing.T) {
	p := &profile.Profile{
		Manifest: profile.Manifest{
			Name: "mini", Dialect: profile.Dialect{Driver: "dgd"},
			IncludeDirs: []string{"/include"}, IncludeFile: "/include/auto.h", AutoObjects: []string{"/sys/auto"},
			Covered: []string{"/std/", "/include/", "/sys/"},
			Conventions: profile.Conventions{CallRegistry: map[string]profile.CallForms{
				"add_action": {{Function: 0}},
			}},
		},
		Files: map[string]string{
			"/include/auto.h": "#define AUTO 1\n",
			"/include/std.h":  "#define ROOM \"/std/room\"\n",
			"/sys/auto.c":     "void autof();\n",
			"/std/room.c":     "inherit \"/std/object\";\nint room_var;\nvoid set_short(string s);\nstatic void reset();\n",
			"/std/object.c":   "void create();\n",
		},
	}
	data, err := p.Marshal()
	require.NoError(t, err)
	fs := vfs.Mem{
		"/home/ann/mini.profile": string(data),
		"/home/ann/dgdtools.yml": "profile: mini.profile\nlib_path: /players/ann\n",
		"/home/ann/mine.h":       "#define MINE \"/players/ann/other\"\n",
		"/home/ann/other.c":      "void there() {}\n",
		"/home/ann/hall.c": `#include <std.h>
#include "mine.h"
inherit ROOM;
void create() {
    ::create();
    set_short("x");
    room_var = AUTO;
    autof();
    add_action("reset", "x");
    MINE->there();
    "/players/ann/other"->there();
    "/std/room"->set_short("y");
    "/d/town/square"->enter();
    "/players/bob/x"->enter();
    clone_object("/std/object");
    clone_object("/players/bob/x");
}
`,
	}
	w := newWorld(t, fs, "/home/ann")
	const stubs = "dgdtools-profile://mini"
	for at, want := range map[string]string{
		"#include <st|d.h>":    `include std.h "<std.h>" -> ` + stubs + `/include/std.h`,
		"#include \"mi|ne.h":   `include mine.h "\"mine.h\"" -> /home/ann/mine.h`,
		"inherit RO|OM":        `object /std/room "ROOM" -> ` + stubs + `/std/room.c`,
		"::cre|ate()":          `function create "create" -> ` + stubs + `/std/object.c:1:6 "create"`,
		"set_sh|ort(\"x\")":    `function set_short "set_short" -> ` + stubs + `/std/room.c:3:6 "set_short"`,
		"room_v|ar":            `variable room_var "room_var" -> ` + stubs + `/std/room.c:2:5 "room_var"`,
		"= AU|TO":              `macro AUTO "AUTO" -> ` + stubs + `/include/auto.h:1:9 "AUTO"`,
		"auto|f()":             `function autof "autof" -> ` + stubs + `/sys/auto.c:1:6 "autof"`,
		"add_action(\"res|et":  `function reset "reset" -> ` + stubs + `/std/room.c:4:13 "reset"`,
		"MI|NE->":              `macro MINE "MINE" -> /home/ann/mine.h:1:9 "MINE"`,
		"MINE->the|re":         `function there "there" -> /home/ann/other.c:1:6 "there"`,
		"other\"->the|re":      `function there "there" -> /home/ann/other.c:1:6 "there"`,
		"\"->set_sh|ort(\"y":   `function set_short "set_short" -> ` + stubs + `/std/room.c:3:6 "set_short"`,
		"square\"->ent|er":     `function enter "enter"`,
		"bob/x\"->ent|er":      `function enter "enter"`,
		"clone_object(\"/st|d": `object /std/object "/std/object" -> ` + stubs + `/std/object.c`,
		"clone_object(\"/pl|a": `object /players/bob/x "/players/bob/x"`,
	} {
		assert.Equal(t, want, w.at("/home/ann/hall.c", at), "at %q", at)
	}
}

// A file with no lib around it: what it says of itself, and the headers
// beside it.
func TestNoLib(t *testing.T) {
	fs := vfs.Mem{
		"/tmp/x/defs.h": "#define LIMIT 3\n",
		"/tmp/x/a.c": "#include \"defs.h\"\n#include <gone.h>\ninherit \"/std/room\";\nint n;\nvoid g();\n" +
			"void f() { g(); n = LIMIT; write(\"x\"); ::create(); this_object()->g(); \"/obj/b\"->g(); }\nvoid g() {}\n",
	}
	w := newWorld(t, fs, "/tmp/x/a.c")
	for at, want := range map[string]string{
		"#include \"de|fs.h": `include defs.h "\"defs.h\"" -> /tmp/x/defs.h`,
		"<go|ne.h>":          `include gone.h "<gone.h>"`,
		"inherit \"/st|d":    `nothing`,
		"{ |g();":            `function g "g" -> /tmp/x/a.c:7:6 "g"`,
		"|n = LIMIT":         `variable n "n" -> /tmp/x/a.c:4:5 "n"`,
		"n = LIM|IT":         `macro LIMIT "LIMIT" -> /tmp/x/defs.h:1:9 "LIMIT"`,
		"wri|te(":            `function write "write"`,
		"::cre|ate()":        `function create "create"`,
		"this_object()->|g":  `function g "g" -> /tmp/x/a.c:7:6 "g"`,
		"\"/obj/b\"->|g":     `function g "g"`,
	} {
		assert.Equal(t, want, w.at("/tmp/x/a.c", at), "at %q", at)
	}
}

// A document that is not there to ask.
func TestNothingToAsk(t *testing.T) {
	assert.Nil(t, resolve.New(resolve.File{}).At(0))
	w := newWorld(t, wholeLib(), "/lib")
	ref, doc := w.ref("/lib/obj/b.c", "inherit \"/std/mi|d\"")
	require.NotNil(t, ref)
	// Offsets at and past the end of the text.
	for off := uint32(len(doc.File.Src)) - 1; off < uint32(len(doc.File.Src))+3; off++ {
		assert.Nil(t, doc.Resolve(off))
	}
}

// A lib whose driver has function pointers: &fn(...) names a function as
// a call does.
func TestFunctionPointer(t *testing.T) {
	fs := vfs.Mem{
		"/lib/dgdtools.yml": "root: .\ndriver_config: mud.dgd\ndialect: {closures: true}\n",
		"/lib/mud.dgd":      "auto_object = \"/sys/auto\";\n",
		"/lib/sys/auto.c":   "void autof() {}\n",
		"/lib/obj/a.c":      "int twice(int a) { return a * 2; }\nvoid f() { function g; g = &twice(); g = &autof(); g = &nope(); (*g)(1); }\n",
	}
	w := newWorld(t, fs, "/lib")
	for at, want := range map[string]string{
		"&twi|ce()": `function twice "twice" -> /lib/obj/a.c:1:5 "twice"`,
		"&aut|of()": `function autof "autof" -> /lib/sys/auto.c:1:6 "autof"`,
		"&no|pe()":  `function nope "nope"`,
		"(*|g)(1)":  `local g "g" -> /lib/obj/a.c:2:21 "g"`,
	} {
		assert.Equal(t, want, w.at("/lib/obj/a.c", at), "at %q", at)
	}
}

// Parameters and local variables: the one in scope where the name
// stands, a global where none is.
func TestLocals(t *testing.T) {
	fs := wholeLib()
	fs["/lib/obj/l.c"] = `inherit "/std/base";
int count;
string name;
void proto(int count, string unused);
int f(int count, string *list) {
    int i, name;
    name = count + i;
    {
        string count;
        count = "x" + base_var;
        { float count; count = 1.0; }
        count = "y";
    }
    count++;
    if (i) { object name; name->foo(); }
    try { i = 1; } catch (err) { name = err; }
    err;
    return list[i];
}
void g() {
    count = 1;
    name = "n";
    { int count; }
    count = 2;
    later = 3;
    int later;
    later = 4;
}
void h(int a) {
    int x = ;
    k();
    x = a;
    count = 1;
    { name = "z"; }
}
void k() { x; h(1); }
`
	w := newWorld(t, fs, "/lib")
	const l = "/lib/obj/l.c"
	for at, want := range map[string]string{
		// Where they are declared.
		"int f(int cou|nt":       `parameter count "count" decl -> /lib/obj/l.c:5:11 "count"`,
		"string *li|st":          `parameter list "list" decl -> /lib/obj/l.c:5:26 "list"`,
		"int |i, name;":          `local i "i" decl -> /lib/obj/l.c:6:9 "i"`,
		"int i, na|me;":          `local name "name" decl -> /lib/obj/l.c:6:12 "name"`,
		"string cou|nt;":         `local count "count" decl -> /lib/obj/l.c:9:16 "count"`,
		"catch (er|r)":           `local err "err" decl -> /lib/obj/l.c:16:27 "err"`,
		"void proto(int cou|nt,": `parameter count "count" decl -> /lib/obj/l.c:4:16 "count"`,
		"string unu|sed)":        `parameter unused "unused" decl -> /lib/obj/l.c:4:30 "unused"`,
		// The parameter before the global, the local before both; a
		// block's own inside it and no longer after it.
		"na|me = count + i":   `local name "name" -> /lib/obj/l.c:6:12 "name"`,
		"name = cou|nt + i":   `parameter count "count" -> /lib/obj/l.c:5:11 "count"`,
		"count + |i;":         `local i "i" -> /lib/obj/l.c:6:9 "i"`,
		"cou|nt = \"x\"":      `local count "count" -> /lib/obj/l.c:9:16 "count"`,
		"\"x\" + base_v|ar":   `variable base_var "base_var" -> /lib/std/base.c:1:5 "base_var"`,
		"float count; cou|nt": `local count "count" -> /lib/obj/l.c:11:17 "count"`,
		"cou|nt = \"y\"":      `local count "count" -> /lib/obj/l.c:9:16 "count"`,
		"cou|nt++":            `parameter count "count" -> /lib/obj/l.c:5:11 "count"`,
		"object name; na|me":  `local name "name" -> /lib/obj/l.c:15:21 "name"`,
		"name->fo|o()":        `function foo "foo"`,
		"{ na|me = err":       `local name "name" -> /lib/obj/l.c:6:12 "name"`,
		"name = er|r":         `local err "err" -> /lib/obj/l.c:16:27 "err"`,
		"    er|r;":           `nothing`,
		"return li|st[i]":     `parameter list "list" -> /lib/obj/l.c:5:26 "list"`,
		"return list[|i]":     `local i "i" -> /lib/obj/l.c:6:9 "i"`,
		// Another function: the globals, around a block that has its
		// own; a local from its declaration on, and nothing before it.
		"cou|nt = 1;\n    name": `variable count "count" -> /lib/obj/l.c:2:5 "count"`,
		"na|me = \"n\"":         `variable name "name" -> /lib/obj/l.c:3:8 "name"`,
		"cou|nt = 2":            `variable count "count" -> /lib/obj/l.c:2:5 "count"`,
		"lat|er = 3":            `nothing`,
		"lat|er = 4":            `local later "later" -> /lib/obj/l.c:26:9 "later"`,
		// A declaration that could not be read: what it names is not
		// known after it, in its block and in those inside; the rest is.
		"|x = a":             `nothing`,
		"x = |a":             `parameter a "a" -> /lib/obj/l.c:29:12 "a"`,
		"cou|nt = 1;\n    {": `variable count "count" -> /lib/obj/l.c:2:5 "count"`,
		"{ na|me = \"z\"":    `variable name "name" -> /lib/obj/l.c:3:8 "name"`,
		"void k() { |x;":     `nothing`,
	} {
		assert.Equal(t, want, w.at(l, at), "at %q", at)
	}
	// What a reference to one carries: its type, and that the place of a
	// use and of the declaration are one variable.
	at, doc := w.offset(l, "name = cou|nt + i")
	use := doc.Resolve(at)
	at, _ = w.offset(l, "int f(int cou|nt")
	decl := doc.Resolve(at)
	require.NotNil(t, use.Defs[0].Local)
	assert.Same(t, use.Defs[0].Local, decl.Defs[0].Local)
	assert.Equal(t, "int", use.Defs[0].Local.Type)
	assert.True(t, use.Defs[0].Local.Param)
	ref, _ := w.ref(l, "return li|st[i]")
	assert.Equal(t, "string *", ref.Defs[0].Local.Type)
	ref, _ = w.ref(l, "name = er|r")
	assert.True(t, ref.Defs[0].Local.Catch)
	ref, _ = w.ref(l, "string unu|sed)")
	assert.Equal(t, "string", ref.Defs[0].Local.Type)

	// What a hover says of one: its type, and the line it is declared
	// on.
	for at, want := range map[string]string{
		"name = cou|nt + i": "parameter count `int count` @ /obj/l.c:5",
		"return li|st[i]":   "parameter list `string *list` @ /obj/l.c:5",
		"count + |i;":       "local i `int i` @ /obj/l.c:6",
		"name = er|r":       "local err `string err` @ /obj/l.c:16",
		"string unu|sed)":   "parameter unused `string unused` @ /obj/l.c:4",
		"float cou|nt;":     "local count `float count` @ /obj/l.c:11",
		"lat|er = 3":        "nothing",
	} {
		assert.Equal(t, want, w.hover(l, at), "at %q", at)
	}
}
