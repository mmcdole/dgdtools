package resolve_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/lexer"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/resolve"
	"github.com/mmcdole/dgdtools/pkg/token"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// offset returns the place in the linted file at path that at marks, as
// world.ref reads it.
func (w *world) offset(path, at string) (uint32, *lint.Document) {
	w.t.Helper()
	doc, err := w.l.Lint(context.Background(), path, lint.Options{NoScan: true})
	require.NoError(w.t, err)
	cursor := strings.IndexByte(at, '|')
	require.GreaterOrEqual(w.t, cursor, 0, "no cursor in %q", at)
	needle := at[:cursor] + at[cursor+1:]
	src := string(doc.File.Src)
	require.Equal(w.t, 1, strings.Count(src, needle), "%q in %s", needle, path)
	return uint32(strings.Index(src, needle) + cursor), doc
}

// hover says what a reader is told at the place, a line for each thing
// the name may be:
//
//	KIND NAME `CODE` (CLASS) @ WHERE // DOC ! NOTE
//
// with the parts that are there.
func (w *world) hover(path, at string) string {
	w.t.Helper()
	off, doc := w.offset(path, at)
	h := doc.Hover(off)
	if h == nil {
		return "nothing"
	}
	require.NotEmpty(w.t, h.Infos)
	assert.Equal(w.t, doc.Resolve(off), h.Ref)
	var lines []string
	for _, info := range h.Infos {
		assert.Equal(w.t, h.Ref.Kind, info.Kind)
		line := fmt.Sprintf("%s %s", info.Kind, info.Name)
		if info.Code != "" {
			line += " `" + info.Code + "`"
		}
		if info.Class != "" {
			line += " (" + info.Class + ")"
		}
		if where := info.Origin.String(); where != "" {
			line += " @ " + where
		}
		if info.Doc != "" {
			line += " // " + strings.ReplaceAll(info.Doc, "\n", "\\n")
		}
		if info.Note != "" {
			line += " ! " + info.Note
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

const hoverConfig = `root: .
driver_config: ../mud.dgd
dialect:
  kfuns:
    - "int *rusage()"
    - "string argon2_hash(string password, int ops_limit, int mem_limit)"
conventions:
  defines: [SITE]
  macros: { public: specifier }
  call_registry:
    add_action: { function: 0 }
`

func hoverLib() vfs.Mem {
	return vfs.Mem{
		"/lib/dgdtools.yml":   hoverConfig,
		"/mud.dgd":            driverConfig,
		"/lib/include/auto.h": "/* Set for everybody. */\n#define AUTO 1\n",
		"/lib/include/std.h": "#define STD \"/std/base\"\n\n// The square\n// of x.\n#define SQUARE(x) ((x)*(x))\n" +
			"#define ME this_object()\n#define status int\n#define LOG(s) { write(s); \\\n  write(\"\\n\"); }\n#define LONG " + strings.Repeat("1 + ", 150) + "1\n",
		"/lib/sys/auto.c": "/*\n * Tell the user.\n *   Indented.\n */\nstatic void write(string s) {}\n" +
			"object clone_object(string path) { return ::clone_object(::find_object(path)); }\n",
		"/lib/std/base.c": "/* The weight, in grams. */\nprivate static int weight;\nstring *names, title;\n\n" +
			"/**\n * Called when the object is made.\n *\n * Twice, at times.\n */\nvoid create() {}\n\n" +
			"// not about foo\n\nstatic varargs int foo(int a, string b...) { return a; }\n" +
			"int bar(); /* trailing */\nmixed baz() {}\n/* gr\xf8t */\nvoid old() {}\n",
		"/lib/std/side.c": "void create() {}\n",
		"/lib/obj/a.c": `#include <std.h>
inherit STD;
/* The other side. */
inherit side "/std/side";

/* How many. */
int count;
void later(int when);

void create() {
    ::create();
    foo(1);
    count = strlen("x") + sizeof(({ })) + SQUARE(2) + AUTO + LONG;
    names = explode(title, " ");
    write("x");
    clone_object("/obj/b");
    rusage();
    ::sscanf("x", "%s", title);
    side::time();
    this_object()->later(1);
    "/obj/b"->later(2);
    "/obj/b"->hidden();
    "/obj/nowhere"->later(3);
    this_player()->later(4);
    call_other(this_player(), "later", 5);
    call_out("later", 1, 2);
    nope();
    baz(); old();
#ifdef SITE
    LOG("x");
#endif
}
/* Later:
   not now. */
void later(int when) { bar(); }
`,
		"/lib/obj/b.c": "inherit \"/std/base\";\n// Some time.\nvoid later(int when) {}\nprivate void hidden() {}\npublic void shown() {}\n",
	}
}

func TestHover(t *testing.T) {
	w := newWorld(t, hoverLib(), "/lib")
	const a = "/lib/obj/a.c"
	for at, want := range map[string]string{
		// Functions: the declaration with its specifiers and types, the
		// file that has it, the comment directly above it.
		"::cre|ate();": "function create `void create()` @ /std/base.c:10 // Called when the object is made.\\n\\nTwice, at times.\n" +
			"function create `void create()` @ /std/side.c:1",
		"fo|o(1);":               "function foo `static varargs int foo(int a, string b...)` @ /std/base.c:14",
		"void cre|ate() {":       "function create `void create()` @ /obj/a.c:10",
		"void lat|er(int when);": "function later `void later(int when)` @ /obj/a.c:35 // Later:\\nnot now.",
		"this_object()->lat|er":  "function later `void later(int when)` @ /obj/a.c:35 // Later:\\nnot now.",
		"\"/obj/b\"->lat|er":     "function later `void later(int when)` @ /obj/b.c:3 // Some time.",
		"call_out(\"lat|er\"":    "function later `void later(int when)` @ /obj/a.c:35 // Later:\\nnot now.",
		// A comment at the end of another line is about that line; one
		// in Latin-1 is read as that.
		"{ ba|r(); }": "function bar `int bar()` @ /std/base.c:15",
		"ba|z(); old": "function baz `mixed baz()` @ /std/base.c:16",
		"baz(); ol|d": "function old `void old()` @ /std/base.c:18 // grøt",
		// The auto object's.
		"wri|te(\"x\");":      "function write `static void write(string s)` @ auto object, /sys/auto.c:5 // Tell the user.\\n  Indented.",
		"clone_ob|ject(\"/ob": "function clone_object `object clone_object(string path)` @ auto object, /sys/auto.c:6",
		// The driver's, from the table, and the project's own beside
		// them; ::name and label::name reach them too.
		"str|len(":    "function strlen `int strlen(string)` @ kfun, DGD 1.7.9 // return the length of a string",
		"size|of(":    "function sizeof `int sizeof(mixed *)` @ kfun, DGD 1.7.9 // return the size of an array",
		"expl|ode(":   "function explode `string *explode(string, string)` @ kfun, DGD 1.7.9 // explode a string",
		"rus|age()":   "function rusage `int *rusage()` @ kfun, declared by the project (dialect.kfuns)",
		"::ssc|anf(":  "function sscanf `int sscanf(string, string, lvalue...)` @ kfun, DGD 1.7.9 // scan a string",
		"side::ti|me": "function time `int time()` @ kfun, DGD 1.7.9 // return the current time",
		"call_ou|t(":  "function call_out `int call_out(string, mixed, mixed...)` @ kfun, DGD 1.7.9 // start a call_out",
		// A call whose receiver is not known is said to be that; where
		// the receiver is known and has no such function, and for a
		// function nobody declares, there is nothing to say.
		"this_player()->lat|er":    "function later ! " + resolve.UnknownReceiver,
		"this_player(), \"lat|er":  "function later ! " + resolve.UnknownReceiver,
		"\"/obj/b\"->hid|den":      "function hidden `private void hidden()` @ /obj/b.c:4",
		"\"/obj/nowhere\"->lat|er": "nothing",
		"no|pe();":                 "nothing",

		// Variables: the type, the file, the comment.
		"cou|nt = strlen": "variable count `int count` @ /obj/a.c:7 // How many.",
		"int cou|nt;":     "variable count `int count` @ /obj/a.c:7 // How many.",
		"nam|es = expl":   "variable names `string *names` @ /std/base.c:3",
		"explode(tit|le":  "variable title `string title` @ /std/base.c:3",

		// Macros: the #define, what the parser takes it for, the comment.
		"SQU|ARE(2)":   "macro SQUARE `#define SQUARE(x) ((x)*(x))` (expression) @ /include/std.h:5 // The square\\nof x.",
		"+ AU|TO +":    "macro AUTO `#define AUTO 1` (expression) @ /include/auto.h:2 // Set for everybody.",
		"inherit ST|D": "object /std/base @ /std/base.c",
		"LO|G(\"x\")":  "macro LOG `#define LOG(s) { write(s); write(\"\\n\"); }` (statement) @ /include/std.h:8",
		// A name the configuration defines is none a file could show.
		"#ifdef SI|TE": "nothing",
		"+ LO|NG;":     "macro LONG `#define LONG " + strings.Repeat("1 + ", 100) + " ...` (expression) @ /include/std.h:10",

		// Files, objects, labels.
		"#include <st|d.h>":      "include std.h `#include <std.h>` @ /include/std.h",
		"inherit side \"/st|d":   "object /std/side @ /std/side.c",
		"clone_object(\"/ob|j/b": "object /obj/b @ /obj/b.c",
		"inherit si|de":          "label side `inherit side \"/std/side\";` @ /std/side.c",
		"si|de::time":            "label side `inherit side \"/std/side\";` @ /std/side.c",

		// No name, no hover.
		"vo|id later(int when);": "nothing",
		"count |= strlen":        "nothing",
		"/* How ma|ny. */":       "nothing",
	} {
		assert.Equal(t, want, w.hover(a, at), "at %q", at)
	}
	// A private variable is told as that, in the file that has it.
	assert.Equal(t, "variable weight `private static int weight` @ /std/base.c:2 // The weight, in grams.",
		w.hover("/lib/std/base.c", "int wei|ght;"))
	// A macro the configuration says what to take for, and no file defines.
	assert.Equal(t, "macro public `#define public` (specifier) @ the project's configuration (conventions)",
		w.hover("/lib/obj/b.c", "pub|lic void"))
	// In the auto object itself a kfun's name is the kfun.
	assert.Equal(t, "function find_object `object find_object(string)` @ kfun, DGD 1.7.9 // find the loaded object for a given object name",
		w.hover("/lib/sys/auto.c", "::find_ob|ject"))
	assert.Equal(t, "function clone_object `object clone_object(object)` @ kfun, DGD 1.7.9 // clone a new object",
		w.hover("/lib/sys/auto.c", "::clone_ob|ject"))
}

// A kfun is the meaning of a name only where every object of the
// program is known: one the index cannot see may have a function of the
// name, as the auto object has clone_object in the lib above.
func TestHoverKfunProved(t *testing.T) {
	fs := hoverLib()
	fs["/lib/obj/c.c"] = "inherit \"/std/gone\";\nvoid f() { strlen(\"x\"); write(\"x\"); }\n"
	w := newWorld(t, fs, "/lib")
	assert.Equal(t, "nothing", w.hover("/lib/obj/c.c", "str|len"))
	assert.Equal(t, "nothing", w.hover("/lib/obj/c.c", "wri|te"))

	// The same when the auto object is what cannot be seen.
	fs = hoverLib()
	delete(fs, "/lib/sys/auto.c")
	w = newWorld(t, fs, "/lib")
	assert.Equal(t, "nothing", w.hover("/lib/obj/a.c", "str|len("))
	assert.Equal(t, "nothing", w.hover("/lib/obj/a.c", "wri|te(\"x\");"))

	// And a file with no lib around it has no program to know.
	w = newWorld(t, vfs.Mem{"/tmp/x/a.c": "void f() { strlen(\"x\"); }\n"}, "/tmp/x/a.c")
	assert.Equal(t, "nothing", w.hover("/tmp/x/a.c", "str|len"))
}

// A wizard's checkout with a profile: a declaration in a stub is the
// profile's, and has no line, but the comment the stub kept; the kfuns
// the profile says the driver has are there.
func TestHoverPartialCheckout(t *testing.T) {
	p := &profile.Profile{
		Manifest: profile.Manifest{
			Name: "mini", Dialect: profile.Dialect{Driver: "dgd", SlashSlash: true, Kfuns: []string{"int *rusage()"}},
			IncludeDirs: []string{"/include"}, AutoObjects: []string{"/sys/auto"},
			Covered: []string{"/std/", "/include/", "/sys/"},
		},
		Files: map[string]string{
			"/include/std.h": "#define ROOM \"/std/room\"\n",
			"/sys/auto.c":    "static void write(string s);\n",
			"/std/room.c":    "inherit \"/std/object\";\n// Set by the room.\nint room_var;\n/*\n * Sets the short\n * description.\n */\nvoid set_short(string s);\n",
			"/std/object.c":  "void create();\n",
		},
	}
	data, err := p.Marshal()
	require.NoError(t, err)
	fs := vfs.Mem{
		"/home/ann/mini.profile": string(data),
		"/home/ann/dgdtools.yml": "profile: mini.profile\nlib_path: /players/ann\n",
		"/home/ann/other.c":      "/* Where. */\nvoid there() {}\n",
		"/home/ann/hall.c": "#include <std.h>\ninherit ROOM;\nvoid create() {\n    ::create(); set_short(\"x\"); room_var = 1; write(\"x\"); rusage(); strlen(\"x\");\n" +
			"    \"/players/ann/other\"->there(); \"/players/bob/x\"->enter(); \"/d/town/square\"->enter(); ob->enter();\n}\n",
	}
	w := newWorld(t, fs, "/home/ann")
	for at, want := range map[string]string{
		"#include <st|d.h>": "include std.h `#include <std.h>` @ profile mini, /include/std.h",
		"inherit RO|OM":     "object /std/room @ profile mini, /std/room.c",
		"::cre|ate()":       "function create `void create()` @ profile mini, /std/object.c",
		"set_sh|ort":        "function set_short `void set_short(string s)` @ profile mini, /std/room.c // Sets the short\\ndescription.",
		"room_v|ar":         "variable room_var `int room_var` @ profile mini, /std/room.c // Set by the room.",
		"wri|te(":           "function write `static void write(string s)` @ auto object, profile mini, /sys/auto.c",
		"rus|age":           "function rusage `int *rusage()` @ kfun, declared by the project (dialect.kfuns)",
		"str|len":           "function strlen `int strlen(string)` @ kfun, DGD 1.7.9 // return the length of a string",
		"other\"->the|re":   "function there `void there()` @ /players/ann/other.c:2 // Where.",
		// Code in neither the checkout nor the profile: nothing is known,
		// and nothing is said.
		"bob/x\"->ent|er":  "nothing",
		"square\"->ent|er": "nothing",
		"ob->ent|er":       "function enter ! " + resolve.UnknownReceiver,
	} {
		assert.Equal(t, want, w.hover("/home/ann/hall.c", at), "at %q", at)
	}
	// And what the calls take: a stub's declaration is one like another.
	for at, want := range map[string]string{
		"::create(|)":        "create #0 `void create()` @ profile mini, /std/object.c",
		"set_short(|\"x\")":  "set_short #0 `void set_short(string s)` [string s] @ profile mini, /std/room.c // Sets the short\\ndescription.",
		"write(|\"x\")":      "write #0 `static void write(string s)` [string s] @ auto object, profile mini, /sys/auto.c",
		"rusage(|)":          "rusage #0 `int *rusage()` @ kfun, declared by the project (dialect.kfuns)",
		"strlen(|\"x\")":     "strlen #0 `int strlen(string)` [string] @ kfun, DGD 1.7.9 // return the length of a string",
		"other\"->there(|)":  "there #0 `void there()` @ /players/ann/other.c:2 // Where.",
		"bob/x\"->enter(|)":  "nothing",
		"square\"->enter(|)": "nothing",
		"ob->enter(|)":       "nothing",
	} {
		assert.Equal(t, want, w.sig("/home/ann/hall.c", at), "at %q", at)
	}
}

func TestComment(t *testing.T) {
	for src, want := range map[string]string{
		"/* one */\nint ^f() {}":                                        "one",
		"/* one */ int ^f() {}":                                         "one",
		"/* one */\n\nint ^f() {}":                                      "",
		"/* one */\n/* two */\nint ^f() {}":                             "two",
		"int x; /* x */\nint ^f() {}":                                   "",
		"int x; // x\nint ^f() {}":                                      "",
		"// one\n//   two\n\n//three\n// four\nint ^f() {}":             "three\n four",
		"   // one\n   // two\n   int ^f() {}":                          "one\ntwo",
		"/*\n * one\n *\n *   two\n */\nstatic nomask\nstring *^f() {}": "one\n\n  two",
		"/**\n\t* one\n\t* two\n\t**/\nint ^f() {}":                     "one\ntwo",
		"/* one\n   two */\nint ^f() {}":                                "one\ntwo",
		"/******\n * one\n ******/\nint ^f() {}":                        "one",
		"int a;\n/* b */\nint b, ^c;":                                   "b",
		"void g() {}\n/* f */\nint ^f() {}":                             "f",
		"#include <x.h>\n/* f */\nint ^f() {}":                          "f",
		"/* h */\n#include <x.h>\nint ^f() {}":                          "",
		"/* M */\n^#define M 1":                                         "M",
		"// M\n  ^#  define M 1":                                        "M",
		"int x;\n^#define M 1":                                          "",
		"^int f() {}":                                                   "",
		"/* */\nint ^f() {}":                                            "",
		"/* bl\xe5b\xe6r */\nint ^f() {}":                               "blåbær",
		"/* one */\r\nint ^f() {}":                                      "one",
	} {
		off := strings.IndexByte(src, '^')
		text := src[:off] + src[off+1:]
		f := lexer.Lex("x.c", []byte(text), token.Dialect{SlashSlash: true})
		assert.Equal(t, want, resolve.Comment(f, uint32(off)), "%q", src)
	}
}
