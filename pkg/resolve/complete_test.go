package resolve_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/resolve"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// completeIn writes text, which has a '|' where the cursor is, as the
// file at path, and asks what can be written at the cursor.
func (w *world) completeIn(path, text string) (*resolve.Completion, *lint.Document) {
	w.t.Helper()
	off := strings.IndexByte(text, '|')
	require.GreaterOrEqual(w.t, off, 0, "no cursor in %q", text)
	w.fs[path] = text[:off] + text[off+1:]
	w.l.Invalidate(path)
	doc, err := w.l.Lint(context.Background(), path, lint.Options{NoScan: true})
	require.NoError(w.t, err)
	c := doc.Complete(uint32(off))
	if c != nil {
		// The items are in order, each name once for a kind, and what
		// they replace is around the cursor.
		assert.True(w.t, c.Start <= uint32(off) && uint32(off) <= c.End && int(c.End) <= len(doc.File.Src), "%q", text)
		assert.True(w.t, slices.IsSortedFunc(c.Items, func(a, b resolve.Item) int {
			if a.Rank != b.Rank {
				return a.Rank - b.Rank
			}
			return strings.Compare(a.Name, b.Name)
		}), "%q", text)
		seen := map[string]bool{}
		for _, it := range c.Items {
			key := fmt.Sprint(it.Kind == resolve.Function, it.Name)
			assert.False(w.t, seen[key], "%s twice in %q", it.Name, text)
			seen[key] = true
		}
	}
	return c, doc
}

// offered says what a completion offers, in its order: "KIND NAME" for
// each item, of the kinds given or of all.
func offered(c *resolve.Completion, kinds ...resolve.Kind) []string {
	out := []string{}
	if c == nil {
		return out
	}
	for _, it := range c.Items {
		if len(kinds) == 0 || slices.Contains(kinds, it.Kind) {
			out = append(out, it.Kind.String()+" "+it.Name)
		}
	}
	return out
}

// names are the names a completion offers, sorted.
func names(c *resolve.Completion) []string {
	out := []string{}
	if c != nil {
		for _, it := range c.Items {
			out = append(out, it.Name)
		}
	}
	slices.Sort(out)
	return out
}

// item returns the item of a completion that has the name, the function
// of that name when fn says so.
func item(t *testing.T, c *resolve.Completion, name string, fn bool) resolve.Item {
	t.Helper()
	require.NotNil(t, c)
	for _, it := range c.Items {
		if it.Name == name && (it.Kind == resolve.Function) == fn {
			return it
		}
	}
	require.Fail(t, "no item "+name)
	return resolve.Item{}
}

const completeConfig = `root: .
driver_config: ../mud.dgd
dialect:
  kfuns: ["int *rusage()"]
conventions:
  macros: { public: specifier }
  call_registry:
    add_action: { function: 0 }
    store_fp: [{ object: 0, function: 1 }, { function: 0 }]
`

func completeLib() vfs.Mem {
	return vfs.Mem{
		"/lib/dgdtools.yml":        completeConfig,
		"/mud.dgd":                 driverConfig,
		"/lib/include/auto.h":      "#define AUTO 1\n",
		"/lib/include/std.h":       "#define STD \"/std/base\"\n#define ROOM_D \"/sys/roomd\"\n// The square of x.\n#define SQUARE(x) ((x)*(x))\n#define ME this_object()\n#define status int\n#define FOREVER for (;;)\n",
		"/lib/include/sys/types.h": "",
		"/lib/include/notes.txt":   "",
		"/lib/sys/auto.c":          "static void write(string s) {}\nvoid autof() {}\nprivate void auto_private() {}\nobject clone_object(string path) { return ::clone_object(path); }\n",
		"/lib/std/base.c":          "int base_var;\nprivate int secret;\n/* Called when the object is made. */\nvoid create() {}\nint foo(int a) { return a; }\nstatic void sfn() {}\nprivate void pfn() {}\nstring query_name();\n",
		"/lib/std/side.c":          "void setup() {}\nvoid create() {}\nprivate void hidden() {}\n",
		"/lib/std/room/exit.c":     "void leave() {}\n",
		"/lib/sys/roomd.c":         "void register(object o) {}\nstatic void internal() {}\nprivate void own() {}\n",
		"/lib/obj/local.h":         "#define LOCAL 1\nint from_header() { return LOCAL; }\nint header_var;\n",
		"/lib/obj/b.c":             "inherit \"/std/base\";\nvoid bar(int n) {}\nstatic void quiet() {}\n",
		"/lib/obj/sub/c.c":         "",
	}
}

// head is what the file the completions are asked in begins with.
const head = `#include <std.h>
#include "local.h"
inherit STD;
inherit side "/std/side";
private static int count;
string *names;
/* Does it later. */
void later(int when);
`

func TestCompleteInCode(t *testing.T) {
	w := newWorld(t, completeLib(), "/lib")
	const a = "/lib/obj/a.c"
	c, _ := w.completeIn(a, head+"void create(string arg) {\n    int n, count;\n    { object ob; | }\n}\nvoid later(int when) { int z; }\n")
	require.NotNil(t, c)
	assert.Equal(t, resolve.InCode, c.Context)
	assert.Equal(t, c.Start, c.End)
	assert.False(t, c.Incomplete)
	assert.False(t, c.Called)
	// The locals in scope, the nearest first by rank, and what hides a
	// global; the file's own, what it inherits (not what is private
	// there), macros, the auto object's, kfuns, keywords.
	assert.Equal(t, []string{"parameter arg", "local count", "local n", "local ob"}, offered(c, resolve.Local, resolve.Parameter))
	assert.Equal(t, []string{"variable names", "variable base_var", "variable header_var"}, offered(c, resolve.Variable))
	assert.Equal(t, []string{"macro AUTO", "macro FOREVER", "macro LOCAL", "macro ME", "macro ROOM_D", "macro SQUARE", "macro STD", "macro public", "macro status"}, offered(c, resolve.Macro))
	fns := offered(c, resolve.Function)
	assert.Equal(t, []string{"function create", "function later", "function foo", "function from_header", "function query_name", "function setup", "function sfn",
		"function autof", "function clone_object", "function write"}, fns[:10])
	assert.Contains(t, fns, "function strlen")
	assert.Contains(t, fns, "function rusage")
	for _, not := range []string{"function pfn", "function hidden", "function auto_private", "function z", "function bar"} {
		assert.NotContains(t, fns, not)
	}
	// A statement begins here: every keyword that can.
	assert.Equal(t, []string{"keyword break", "keyword case", "keyword catch", "keyword continue", "keyword default", "keyword do", "keyword else",
		"keyword float", "keyword for", "keyword goto", "keyword if", "keyword int", "keyword mapping", "keyword mixed", "keyword new", "keyword nil",
		"keyword object", "keyword return", "keyword rlimits", "keyword string", "keyword switch", "keyword try", "keyword void", "keyword while"}, offered(c, resolve.Keyword))

	// What each item says: the declaration, where it is, whether it is
	// called and with arguments, where its comment is to be read.
	for name, want := range map[string]string{
		"arg":      "parameter `string arg` @ /obj/a.c:9 rank 0",
		"count":    "local `int count` @ /obj/a.c:10 rank 0",
		"names":    "variable `string *names` @ /obj/a.c:6 rank 1 doc /lib/obj/a.c",
		"base_var": "variable `int base_var` @ /std/base.c:1 rank 2 doc /lib/std/base.c",
		"SQUARE":   "macro `#define SQUARE(x) ((x)*(x))` @ /include/std.h rank 3 call(args) doc /lib/include/std.h",
		"ME":       "macro `#define ME this_object()` @ /include/std.h rank 3 doc /lib/include/std.h",
		"public":   "macro `#define public` @ the project's configuration (conventions) rank 3",
		"if":       "keyword rank 6",
		"int":      "keyword rank 6",
	} {
		assert.Equal(t, want, describe(item(t, c, name, false)), name)
	}
	for name, want := range map[string]string{
		"later":        "function `void later(int when)` @ /obj/a.c:13 rank 1 call(args) doc /lib/obj/a.c",
		"create":       "function `void create(string arg)` @ /obj/a.c:9 rank 1 call(args) doc /lib/obj/a.c",
		"foo":          "function `int foo(int a)` @ /std/base.c:5 rank 2 call(args) doc /lib/std/base.c",
		"setup":        "function `void setup()` @ /std/side.c:1 rank 2 call doc /lib/std/side.c",
		"from_header":  "function `int from_header()` @ /obj/local.h:2 rank 2 call doc /lib/obj/local.h",
		"write":        "function `static void write(string s)` @ auto object, /sys/auto.c:1 rank 4 call(args) doc /lib/sys/auto.c",
		"clone_object": "function `object clone_object(string path)` @ auto object, /sys/auto.c:4 rank 4 call(args) doc /lib/sys/auto.c",
		"strlen":       "function `int strlen(string)` @ kfun, DGD 1.7.9 rank 5 call(args) // return the length of a string",
		"rusage":       "function `int *rusage()` @ kfun, declared by the project (dialect.kfuns) rank 5 call",
	} {
		assert.Equal(t, want, describe(item(t, c, name, true)), name)
	}

	// The comment of an item, read when it is asked for.
	_, doc := w.completeIn(a, head+"void create() { | }\nvoid later(int when) { }\n")
	assert.Equal(t, "Does it later.", doc.Comment(a, uint32(strings.Index(head, "later(int"))))
	assert.Equal(t, "Called when the object is made.", doc.Comment("/lib/std/base.c", uint32(strings.Index(w.fs["/lib/std/base.c"], "create"))))
	assert.Equal(t, "The square of x.", doc.Comment("/lib/include/std.h", uint32(strings.Index(w.fs["/lib/include/std.h"], "#define SQUARE"))))
	assert.Empty(t, doc.Comment("/lib/std/base.c", 0))
	assert.Empty(t, doc.Comment("/lib/gone.c", 0))
	assert.Empty(t, doc.Comment("", 0))
}

// describe writes an item as a line:
//
//	KIND `DETAIL` @ WHERE rank N [call[(args)]] [doc PATH] [// DOC]
func describe(it resolve.Item) string {
	s := it.Kind.String()
	if it.Detail != "" {
		s += " `" + it.Detail + "`"
	}
	if where := it.Origin.String(); where != "" {
		s += " @ " + where
	}
	s += fmt.Sprintf(" rank %d", it.Rank)
	if it.Call {
		s += " call"
		if it.Args {
			s += "(args)"
		}
	}
	if it.Path != "" {
		s += " doc " + it.Path
	}
	if it.Doc != "" {
		s += " // " + it.Doc
	}
	return s
}

// Where the cursor is decides what is offered: the word it is in or
// behind is what an item replaces.
func TestCompletePlaces(t *testing.T) {
	w := newWorld(t, completeLib(), "/lib")
	const a = "/lib/obj/a.c"
	body := func(s string) string {
		return head + "void create(string arg) {\n    int n;\n" + s + "\n}\nvoid later(int when) { }\n"
	}
	type want struct {
		ctx      resolve.Context
		replaced string // the text an item replaces, with the cursor in it
		has, not []string
	}
	for text, x := range map[string]want{
		// A word being written is replaced whole; what follows it says
		// whether it is called already.
		body("    la|"):          {resolve.InCode, "la|", []string{"later", "n", "arg", "strlen", "if", "SQUARE"}, nil},
		body("    la|ter(1);"):   {resolve.InCode, "la|ter", []string{"later"}, nil},
		body("    n = |;"):       {resolve.InCode, "|", []string{"later", "n", "nil", "new", "catch"}, []string{"if", "return", "int"}},
		body("    n = (|"):       {resolve.InCode, "|", []string{"int", "string", "n"}, []string{"if"}},
		body("    if (n) |"):     {resolve.InCode, "|", []string{"return", "n"}, nil},
		body("    n = new |"):    {resolve.InCode, "|", []string{"object"}, []string{"int"}},
		body("    foo(n, |"):     {resolve.InCode, "|", []string{"arg", "foo"}, []string{"while"}},
		body("    return n + |"): {resolve.InCode, "|", []string{"count", "SQUARE"}, []string{"while"}},
		// Before the declaration of a local, and in another function.
		head + "void create() { | int late; }\n":             {resolve.InCode, "|", []string{"count"}, []string{"late"}},
		head + "void create() { int x; }\nvoid g() { x| }\n": {resolve.InCode, "x|", []string{"count"}, []string{"x"}},

		// Where declarations stand: types, specifiers, the macros that
		// are one; nothing that is code.
		head + "|":                               {resolve.InCode, "|", []string{"int", "void", "private", "static", "varargs", "inherit", "status", "public"}, []string{"if", "later", "count", "strlen", "SQUARE", "nil"}},
		head + "st|":                             {resolve.InCode, "st|", []string{"static", "string", "status"}, []string{"strlen"}},
		head + "void f(int a, st|":               {resolve.InCode, "st|", []string{"string", "status"}, []string{"a", "strlen"}},
		head + "void f() { }\n|\nvoid g() { }\n": {resolve.InCode, "|", []string{"int"}, []string{"if"}},
		// The value of a global variable: an expression, of a
		// declarator, not after one.
		head + "int x = |":                       {resolve.InCode, "|", []string{"SQUARE", "count", "strlen", "later", "nil"}, []string{"int", "if", "static"}},
		head + "mapping m = ([ \"a\": ({ 1, ST|": {resolve.InCode, "ST|", []string{"STD", "SQUARE"}, []string{"int"}},
		head + "int x = f(1, 2), |":              {resolve.InCode, "|", []string{"int"}, []string{"SQUARE", "nil"}},
		head + "int x = 1;\n|":                   {resolve.InCode, "|", []string{"int"}, []string{"SQUARE", "nil"}},
		head + "void f() { }\nint *a = ({ 1, |":  {resolve.InCode, "|", []string{"SQUARE"}, []string{"int"}},
		// After inherit: the macros that are a path.
		"#include <std.h>\ninherit |":        {resolve.InCode, "|", []string{"STD", "ROOM_D"}, []string{"int", "status", "FOREVER"}},
		"#include <std.h>\ninherit side S|;": {resolve.InCode, "S|", []string{"STD"}, []string{"int"}},
	} {
		c, doc := w.completeIn(a, text)
		require.NotNil(t, c, text)
		assert.Equal(t, x.ctx, c.Context, text)
		off := uint32(strings.IndexByte(text, '|'))
		src := string(doc.File.Src)
		assert.Equal(t, x.replaced, src[c.Start:off]+"|"+src[off:c.End], text)
		got := names(c)
		for _, name := range x.has {
			assert.Contains(t, got, name, text)
		}
		for _, name := range x.not {
			assert.NotContains(t, got, name, text)
		}
	}
	c, _ := w.completeIn(a, body("    la|ter(1);"))
	assert.True(t, c.Called)
	c, _ = w.completeIn(a, body("    la|  (1);"))
	assert.True(t, c.Called)

	// Where there is nothing to say: comments, numbers, strings that
	// name nothing, code that is not compiled.
	for _, text := range []string{
		body("    /* la| */"), body("    // la|"), body("    n = 12|;"), body("    n = 1.|;"), body("    write(\"la|\");"),
		body("    write(\"la|"), body("    n = 'a|';"), body("#if 0\n    la|\n#endif"), body("#if 0\n|\n#endif"),
		body("    call_out(\"later\", |"[:0] + "    arg = \"x\" + \"la|\";"),
		"#define X la|\n", "#pragma la|\n", "#incl|ude <std.h>\n", "#include <std.h> |\n", "#include|\n",
	} {
		c, _ := w.completeIn(a, text)
		assert.Nil(t, c, text)
	}
	// Behind a comment and a string that are closed, code goes on.
	for _, text := range []string{body("    /* x */|"), body("    n = \"x\"|"), body("    n = 'a'|")} {
		c, _ := w.completeIn(a, text)
		require.NotNil(t, c, text)
		assert.Contains(t, names(c), "later", text)
	}
	assert.Nil(t, resolve.New(resolve.File{}).Complete(0))
}

// After ::, label:: and ->, and in the string that names a callback:
// functions, of the objects that are known.
func TestCompleteMembers(t *testing.T) {
	w := newWorld(t, completeLib(), "/lib")
	const a = "/lib/obj/a.c"
	body := func(s string) string {
		return head + "void create(string arg) {\n    object ob;\n" + s + "\n}\nvoid later(int when) { }\nstatic void mine() { }\n"
	}
	self := []string{"function create", "function later", "function mine", "function foo", "function from_header", "function query_name", "function setup", "function sfn",
		"function autof", "function clone_object", "function write"}
	b := []string{"function bar", "function create", "function foo", "function query_name", "function autof", "function clone_object"}
	roomd := []string{"function register", "function autof", "function clone_object"}
	for text, want := range map[string]struct {
		ctx      resolve.Context
		replaced string
		items    []string
	}{
		// The inherited functions, without those private there; the
		// auto object's behind them. Through a label, that object's.
		body("    ::|"):          {resolve.AfterScope, "|", []string{"function create", "function foo", "function query_name", "function setup", "function sfn", "function autof", "function clone_object", "function write"}},
		body("    ::cr|eate();"): {resolve.AfterScope, "cr|eate", []string{"function create", "function foo", "function query_name", "function setup", "function sfn", "function autof", "function clone_object", "function write"}},
		body("    side::|"):      {resolve.AfterScope, "|", []string{"function create", "function setup"}},
		body("    side::se|"):    {resolve.AfterScope, "se|", []string{"function create", "function setup"}},
		body("    nolabel::|"):   {resolve.AfterScope, "|", []string{}},

		// this_object(), written so or as a macro: what the object's own
		// code calls, static and all, and no kfun.
		body("    this_object()->|"):   {resolve.AfterArrow, "|", self},
		body("    ME->la|"):            {resolve.AfterArrow, "la|", self},
		body("    (this_object())->|"): {resolve.AfterArrow, "|", self},
		// A constant path: what a call from outside reaches.
		body("    \"/obj/b\"->|"):          {resolve.AfterArrow, "|", b},
		body("    \"/obj/b\"->ba|r(1);"):   {resolve.AfterArrow, "ba|r", b},
		body("    (\"/obj/\" + \"b\")->|"): {resolve.AfterArrow, "|", b},
		body("    ROOM_D->|"):              {resolve.AfterArrow, "|", roomd},
		// Nothing is guessed: a variable, a call, an object that is not
		// there, a call on what a call returned.
		body("    ob->|"):                {resolve.AfterArrow, "|", []string{}},
		body("    ob->se|"):              {resolve.AfterArrow, "se|", []string{}},
		body("    this_player()->|"):     {resolve.AfterArrow, "|", []string{}},
		body("    \"/obj/none\"->|"):     {resolve.AfterArrow, "|", []string{}},
		body("    \"/obj/b\"->bar()->|"): {resolve.AfterArrow, "|", []string{}},
		body("    arg->|"):               {resolve.AfterArrow, "|", []string{}},

		// The string that names a function: the object's own for
		// call_out and a registrar, the receiver's for call_other.
		body("    call_out(\"|\", 1);"):               {resolve.InCallback, "|", self},
		body("    call_out(\"la|\", 1);"):             {resolve.InCallback, "la|", self},
		body("    call_out(\"la|ter\", 1);"):          {resolve.InCallback, "la|", self},
		body("    call_out(\"la|"):                    {resolve.InCallback, "la|", self},
		body("    add_action(\"|\", \"verb\");"):      {resolve.InCallback, "|", self},
		body("    store_fp(\"|\");"):                  {resolve.InCallback, "|", self},
		body("    call_other(this_object(), \"|\");"): {resolve.InCallback, "|", self},
		body("    call_other(\"/obj/b\", \"|\");"):    {resolve.InCallback, "|", b},
		body("    call_other(ROOM_D, \"re|\", ob);"):  {resolve.InCallback, "re|", roomd},
		body("    store_fp(\"/sys/roomd\", \"|\");"):  {resolve.InCallback, "|", roomd},
		body("    call_other(ob, \"|\");"):            {resolve.InCallback, "|", []string{}},
		body("    foo(call_out(\"|\", 1), 2);"):       {resolve.InCallback, "|", self},
	} {
		c, doc := w.completeIn(a, text)
		require.NotNil(t, c, text)
		assert.Equal(t, want.ctx, c.Context, text)
		off := uint32(strings.IndexByte(text, '|'))
		src := string(doc.File.Src)
		assert.Equal(t, want.replaced, src[c.Start:off]+"|"+src[off:c.End], text)
		assert.Equal(t, want.items, offered(c), text)
	}
	// A name in a string is not called; one after -> is.
	c, _ := w.completeIn(a, body("    call_out(\"|\", 1);"))
	assert.False(t, item(t, c, "later", true).Call)
	c, _ = w.completeIn(a, body("    this_object()->|"))
	assert.True(t, item(t, c, "later", true).Call)
	// Strings that name no function: another argument, another call, a
	// call in another object, more than a name.
	for _, text := range []string{
		body("    call_out(\"later\", \"|\");"), body("    foo(\"|\");"), body("    ob->call_out(\"|\");"), body("    call_out(\"la ter|\");"),
		body("    call_other(\"|\", \"later\");"), "string s = \"|\";\n",
	} {
		c, _ := w.completeIn(a, text)
		assert.Nil(t, c, text)
	}

	// In the auto object and what it is made of, the kfuns are what is
	// above it.
	c, _ = w.completeIn("/lib/sys/auto.c", "object clone_object(string path) { return ::cl|(path); }\n")
	require.NotNil(t, c)
	assert.Contains(t, names(c), "clone_object")
	assert.Contains(t, names(c), "find_object")
	assert.Equal(t, "function `object clone_object(object)` @ kfun, DGD 1.7.9 rank 5 call(args) // clone a new object", describe(item(t, c, "clone_object", true)))
}

// Paths: of an #include, beside the file and in the include
// directories, and of an inherit statement, in the lib.
func TestCompletePaths(t *testing.T) {
	w := newWorld(t, completeLib(), "/lib")
	const a = "/lib/obj/a.c"
	for text, want := range map[string]struct {
		ctx      resolve.Context
		replaced string
		items    []string
	}{
		"#include <|":            {resolve.InInclude, "|", []string{"include auto.h", "include std.h", "directory sys", "include notes.txt"}},
		"#include <|>\n":         {resolve.InInclude, "|", []string{"include auto.h", "include std.h", "directory sys", "include notes.txt"}},
		"#include <st|":          {resolve.InInclude, "st|", []string{"include auto.h", "include std.h", "directory sys", "include notes.txt"}},
		"#include <st|d.h>\n":    {resolve.InInclude, "st|", []string{"include auto.h", "include std.h", "directory sys", "include notes.txt"}},
		"#include <sys/|":        {resolve.InInclude, "|", []string{"include types.h"}},
		"#include <sys/ty|":      {resolve.InInclude, "ty|", []string{"include types.h"}},
		"#include <nowhere/|":    {resolve.InInclude, "|", []string{}},
		"#  include  \"|":        {resolve.InInclude, "|", []string{"include auto.h", "include local.h", "include std.h", "directory sub", "directory sys", "include a.c", "include b.c", "include notes.txt"}},
		"#include \"sub/|\"\n":   {resolve.InInclude, "|", []string{"include c.c"}},
		"#include \"../std/|":    {resolve.InInclude, "|", []string{"directory room", "include base.c", "include side.c"}},
		"#include \"/include/s|": {resolve.InInclude, "s|", []string{"include auto.h", "include std.h", "directory sys", "include notes.txt"}},
		"#include \"/|":          {resolve.InInclude, "|", []string{"directory include", "directory obj", "directory std", "directory sys", "include dgdtools.yml"}},

		"inherit \"|":                   {resolve.InInherit, "|", []string{"directory include", "directory obj", "directory std", "directory sys"}},
		"inherit \"/|\";\n":             {resolve.InInherit, "|", []string{"directory include", "directory obj", "directory std", "directory sys"}},
		"inherit \"/std/|":              {resolve.InInherit, "|", []string{"object base", "object side", "directory room"}},
		"inherit \"/std/ba|se\";":       {resolve.InInherit, "ba|", []string{"object base", "object side", "directory room"}},
		"inherit side \"/std/ro|":       {resolve.InInherit, "ro|", []string{"object base", "object side", "directory room"}},
		"private inherit \"/std/room/|": {resolve.InInherit, "|", []string{"object exit"}},
		"inherit object \"/sys/|":       {resolve.InInherit, "|", []string{"object auto", "object roomd"}},
		"inherit \"/nowhere/|":          {resolve.InInherit, "|", []string{}},
	} {
		c, doc := w.completeIn(a, text)
		require.NotNil(t, c, text)
		assert.Equal(t, want.ctx, c.Context, text)
		off := uint32(strings.IndexByte(text, '|'))
		src := string(doc.File.Src)
		assert.Equal(t, want.replaced, src[c.Start:off]+"|"+src[off:c.End], text)
		assert.Equal(t, want.items, offered(c), text)
	}

	// The macros in a condition.
	c, doc := w.completeIn(a, "#include <std.h>\n#ifdef RO|\n#endif\n")
	require.NotNil(t, c)
	assert.Equal(t, resolve.InDirective, c.Context)
	assert.Equal(t, "RO", string(doc.File.Src[c.Start:c.End]))
	assert.Equal(t, []string{"macro AUTO", "macro FOREVER", "macro ME", "macro ROOM_D", "macro SQUARE", "macro STD", "macro public", "macro status"}, offered(c))
	assert.False(t, item(t, c, "SQUARE", false).Call)
	for _, text := range []string{"#if defined(|)\n#endif\n", "#if 1\n#elif |\n#endif\n", "#undef |\n", "#ifndef |\n#endif\n"} {
		c, _ := w.completeIn(a, text)
		require.NotNil(t, c, text)
		assert.Equal(t, resolve.InDirective, c.Context, text)
		assert.Contains(t, names(c), "AUTO", text)
	}
}

// A wizard's checkout and a profile: what the stubs declare is offered
// as the checkout's own is, and a path leads through both.
func TestCompletePartialCheckout(t *testing.T) {
	p := &profile.Profile{
		Manifest: profile.Manifest{
			Name: "mini", Dialect: profile.Dialect{Driver: "dgd", Kfuns: []string{"int *rusage()"}},
			IncludeDirs: []string{"/include"}, AutoObjects: []string{"/sys/auto"},
			Covered:     []string{"/std/", "/include/", "/sys/"},
			Conventions: profile.Conventions{CallRegistry: map[string]profile.CallForms{"add_action": {{Function: 0}}}},
		},
		Files: map[string]string{
			"/include/std.h": "#define ROOM \"/std/room\"\n",
			"/sys/auto.c":    "void autof();\n",
			"/std/room.c":    "inherit \"/std/object\";\nint room_var;\nvoid set_short(string s);\nstatic void reset();\n",
			"/std/object.c":  "void create();\n",
		},
	}
	data, err := p.Marshal()
	require.NoError(t, err)
	fs := vfs.Mem{
		"/home/ann/mini.profile": string(data),
		"/home/ann/dgdtools.yml": "profile: mini.profile\nlib_path: /players/ann\n",
		"/home/ann/mine.h":       "#define MINE \"/players/ann/other\"\n",
		"/home/ann/other.c":      "void there() {}\nstatic void quiet() {}\n",
		"/home/ann/sub/deep.c":   "",
	}
	w := newWorld(t, fs, "/home/ann")
	const hall = "/home/ann/hall.c"
	head := "#include <std.h>\n#include \"mine.h\"\ninherit ROOM;\n"
	c, _ := w.completeIn(hall, head+"void create() { | }\n")
	require.NotNil(t, c)
	assert.Equal(t, []string{"function create", "function reset", "function set_short", "function autof"}, offered(c, resolve.Function)[:4])
	assert.Contains(t, names(c), "rusage")
	assert.Contains(t, names(c), "strlen")
	assert.Equal(t, []string{"variable room_var"}, offered(c, resolve.Variable))
	assert.Equal(t, []string{"macro MINE", "macro ROOM"}, offered(c, resolve.Macro))
	// A stub has the declaration and neither a line nor a comment.
	assert.Equal(t, "function `void set_short(string s)` @ profile mini, /std/room.c rank 2 call(args)", describe(item(t, c, "set_short", true)))
	assert.Equal(t, "function `void autof()` @ auto object, profile mini, /sys/auto.c rank 4 call", describe(item(t, c, "autof", true)))
	assert.Equal(t, "variable `int room_var` @ profile mini, /std/room.c rank 2", describe(item(t, c, "room_var", false)))
	assert.Equal(t, "macro `#define ROOM \"/std/room\"` @ profile mini, /include/std.h rank 3", describe(item(t, c, "ROOM", false)))

	for text, want := range map[string][]string{
		head + "void create() { ::| }\n":                       {"function create", "function reset", "function set_short", "function autof"},
		head + "void create() { MINE->| }\n":                   {"function there", "function autof"},
		head + "void create() { \"/std/room\"->| }\n":          {"function set_short", "function create", "function autof"},
		head + "void create() { \"/d/town/square\"->| }\n":     {},
		head + "void create() { add_action(\"|\", \"x\"); }\n": {"function create", "function reset", "function set_short", "function autof"},
		"#include <|":              {"include std.h"},
		"#include \"|":             {"include mine.h", "include std.h", "directory sub", "include dgdtools.yml", "include hall.c", "include mini.profile", "include other.c"},
		"inherit \"/|":             {"directory include", "directory players", "directory std", "directory sys"},
		"inherit \"/std/|":         {"object object", "object room"},
		"inherit \"/players/|":     {"directory ann"},
		"inherit \"/players/ann/|": {"object hall", "object other", "directory sub"},
	} {
		c, _ := w.completeIn(hall, text)
		require.NotNil(t, c, text)
		assert.Equal(t, want, offered(c), text)
	}

	// An inherit that does not resolve: what the object nobody can see
	// has is not known, and no name is known to be a kfun; the auto
	// object's functions are functions all the same.
	c, _ = w.completeIn(hall, "inherit \"/d/town/base\";\nvoid create() { | }\n")
	require.NotNil(t, c)
	assert.Equal(t, []string{"function create", "function autof"}, offered(c, resolve.Function))
}

// A file with no lib around it: what it says of itself.
func TestCompleteNoLib(t *testing.T) {
	fs := vfs.Mem{"/tmp/x/defs.h": "#define LIMIT 3\n", "/tmp/x/sub/more.h": ""}
	w := newWorld(t, fs, "/tmp/x/a.c")
	const a = "/tmp/x/a.c"
	c, _ := w.completeIn(a, "#include \"defs.h\"\nint n;\nvoid g();\nvoid f(int a) { | }\nvoid g() {}\n")
	require.NotNil(t, c)
	assert.Equal(t, []string{"parameter a", "function f", "function g", "variable n", "macro LIMIT"}, offered(c, resolve.Parameter, resolve.Variable, resolve.Function, resolve.Macro))
	for text, want := range map[string][]string{
		"void f() { this_object()->| }\nvoid g() {}\n": {"function f", "function g"},
		"void f() { \"/obj/b\"->| }\n":                 {},
		"void f() { ::| }\n":                           {},
		"#include \"|":                                 {"include defs.h", "directory sub", "include a.c"},
		"#include <|":                                  {},
		"inherit \"/|":                                 {},
	} {
		c, _ := w.completeIn(a, text)
		require.NotNil(t, c, text)
		assert.Equal(t, want, offered(c), text)
	}
}

// More than MaxItems: the best matches of what is written, and the
// answer says it is not all.
func TestCompleteCapped(t *testing.T) {
	fs := vfs.Mem{"/lib/dgdtools.yml": "root: .\n"}
	var b strings.Builder
	for i := range resolve.MaxItems + 200 {
		fmt.Fprintf(&b, "void fn_%04d() {}\n", i)
	}
	w := newWorld(t, fs, "/lib")
	c, _ := w.completeIn("/lib/a.c", b.String()+"void target_one() {}\nvoid f() { | }\n")
	require.NotNil(t, c)
	assert.True(t, c.Incomplete)
	assert.Len(t, c.Items, resolve.MaxItems)
	c, _ = w.completeIn("/lib/a.c", b.String()+"void target_one() {}\nvoid f() { tar| }\n")
	require.NotNil(t, c)
	assert.True(t, c.Incomplete)
	assert.Equal(t, []string{"function target_one", "function connect_datagram", "function datagram_challenge", "function send_datagram"}, offered(c))
	c, _ = w.completeIn("/lib/a.c", b.String()+"void target_one() {}\nvoid f() { fn_00| }\n")
	require.NotNil(t, c)
	assert.True(t, c.Incomplete)
	// fn_00NN first, then what has an f, an n, a _ and two 0s in it in
	// that order (fuzzy's subsequence), and nothing else.
	require.Greater(t, len(c.Items), 100)
	assert.Less(t, len(c.Items), resolve.MaxItems)
	for i := range 100 {
		assert.Equal(t, fmt.Sprintf("fn_%04d", i), c.Items[i].Name)
	}
	assert.NotContains(t, names(c), "target_one")
	// Under the cap everything is there, whatever is written.
	c, _ = w.completeIn("/lib/a.c", "void target_one() {}\nvoid f() { zzz| }\n")
	require.NotNil(t, c)
	assert.False(t, c.Incomplete)
	assert.Contains(t, names(c), "target_one")
}

// A file as it is typed, a byte at a time: nothing panics, and what is
// offered is in order.
func TestCompleteEveryPrefix(t *testing.T) {
	w := newWorld(t, completeLib(), "/lib")
	const a = "/lib/obj/a.c"
	text := head + "void create(string arg) {\n    int n;\n    if (arg) { ::create(); side::setup(); this_object()->later(n); \"/obj/b\"->bar(1); call_out(\"later\", 1); }\n" +
		"    n = SQUARE(n) + count;\n}\n#ifdef AUTO\nvoid later(int when) { /* x */ return; }\n#endif\n"
	contexts := map[resolve.Context]int{}
	for i := 0; i <= len(text); i++ {
		c, _ := w.completeIn(a, text[:i]+"|")
		if c != nil {
			contexts[c.Context]++
		}
	}
	for _, ctx := range []resolve.Context{resolve.InCode, resolve.AfterArrow, resolve.AfterScope, resolve.InInclude, resolve.InInherit, resolve.InCallback, resolve.InDirective} {
		assert.NotZero(t, contexts[ctx], "context %d", ctx)
	}
	// And at every place of the file as it is.
	w.fs[a] = text
	w.l.Invalidate(a)
	doc, err := w.l.Lint(context.Background(), a, lint.Options{NoScan: true})
	require.NoError(t, err)
	for off := 0; off <= len(text)+1; off++ {
		doc.Complete(uint32(off))
	}
}
