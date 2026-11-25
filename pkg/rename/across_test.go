package rename_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/rename"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// wideLib is a lib where symbols are named across files: a function
// redefined down a hierarchy and called every way there is, a function
// of the same name that is another, global variables, a header's macros,
// a private function of a file another includes.
func wideLib() vfs.Mem {
	return vfs.Mem{
		"/lib/dgdtools.yml":    cfg,
		"/lib/mud.dgd":         "include_dirs = ({ \"/include\" });\nauto_object = \"/sys/auto\";\n",
		"/lib/include/std.h":   "#define ME this_object()\n#define LIVING \"/std/living\"\n#define QN query_name()\n",
		"/lib/include/hp.h":    "#define MAX_HP 100\n",
		"/lib/sys/auto.c":      "static void write(string s) {}\nvoid autof() {}\n",
		"/lib/std/base.c":      "string name;\n\nstring query_name() { return name; }\nvoid set_name(string n) { name = n; }\n",
		"/lib/std/living.c":    "#include <std.h>\n#include <hp.h>\ninherit \"/std/base\";\n\nint hp;\n\nstring query_name() { return \"living \" + ::query_name(); }\nvoid heal() { hp = MAX_HP; }\n",
		"/lib/obj/monster.c":   monster,
		"/lib/obj/other.c":     "string query_name() { return \"other\"; }\nvoid g() { query_name(); \"/obj/other\"->query_name(); }\n",
		"/lib/obj/caller.c":    caller,
		"/lib/obj/dead.c":      "inherit \"/std/living\";\nvoid f() {\n#ifdef NOPE\n    query_name();\n#endif\n}\n",
		"/lib/obj/saver.c":     "inherit \"/std/living\";\nvoid save() { save_object(\"/data/x\"); }\n",
		"/lib/std/room.c":      "int light;\nvoid set_light(int l) { light = l; }\n",
		"/lib/obj/room1.c":     "inherit \"/std/room\";\nvoid create() { light = 2; set_light(1); }\n",
		"/lib/obj/room2.c":     "inherit \"/std/room\";\nvoid f() { int lamp; lamp = light; }\nvoid g() { brightness(); }\n",
		"/lib/obj/inc.c":       "private void incfn() {}\nvoid g() { incfn(); }\n",
		"/lib/obj/user.c":      "#include \"inc.c\"\nvoid h() { incfn(); call_out(\"incfn\", 1); }\n",
		"/lib/obj/cmds.c":      "void do_look() {}\nvoid f(string v) { call_out(\"do_\" + v, 1); call_other(this_object(), v); }\n",
		"/lib/obj/clash.c":     "inherit \"/std/living\";\nstring query_short() { return \"x\"; }\n",
		"/lib/obj/hidden.c":    "inherit \"/std/room\";\nvoid f() { int dim; dim = light; }\n",
		"/lib/sys/parts.h":     "void helpme();\n",
		"/lib/sys/impl.c":      "void helpme() {}\n",
		"/lib/sys/use.c":       "#include \"parts.h\"\nvoid g() { helpme(); }\n",
		"/lib/sys/whole.c":     "#include \"parts.h\"\n#include \"impl.c\"\n#include \"use.c\"\n",
		"/lib/obj/stringize.c": "#define STR(x) #x\ninherit \"/std/room\";\nvoid f() { write(STR(light)); }\n",
	}
}

const monster = `#include <std.h>
inherit LIVING;

string query_name() { return "monster"; }

void f(object ob) {
    write(query_name());
    ob->query_name();
    LIVING->query_name();
    call_other(this_object(), "query_name");
    call_out("query_name", 1);
    hp = 3;
}
`

const caller = `#include <std.h>
void h(object ob, string verb) {
    "/obj/monster"->query_name();
    "/obj/other"->query_name();
    ob->query_name();
    call_other(ob, "do_" + verb);
    call_other(ob, verb);
    write(QN);
}
`

// renamed is what a rename did: the files after its certain edits (and,
// with possible, its possible ones), each as "== FILE" and its text,
// in path order; then the counts, the warnings and the dynamic calls.
// Or "refused: REASON".
func (w *world) renamed(path, at, name string, force, possible bool) string {
	w.t.Helper()
	doc, off := w.place(path, at)
	plan, err := rename.Rename(context.Background(), lintFile(doc, force), off, name)
	var no *rename.Refusal
	if errors.As(err, &no) {
		return "refused: " + no.Reason
	}
	require.NoError(w.t, err)
	edits := slices.Clone(plan.Edits)
	if possible {
		edits = append(edits, plan.PossibleEdits...)
	}
	byPath := map[string][]rename.Edit{}
	for _, e := range edits {
		byPath[e.Path] = append(byPath[e.Path], e)
	}
	var b strings.Builder
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		src := w.fs[p]
		es := byPath[p]
		slices.SortFunc(es, func(a, b rename.Edit) int { return int(a.Start) - int(b.Start) })
		for i := len(es) - 1; i >= 0; i-- {
			e := es[i]
			require.Equal(w.t, plan.Symbol.Name, src[e.Start:e.End], "%s at %d", p, e.Start)
			src = src[:e.Start] + e.NewText + src[e.End:]
		}
		fmt.Fprintf(&b, "== %s\n%s", p, src)
	}
	fmt.Fprintf(&b, "-- certain %d, possible %d, excluded %d\n", plan.Count(rename.Certain), plan.Count(rename.Possible), plan.Count(rename.Excluded))
	for _, o := range plan.Occurrences {
		if o.Class != rename.Certain {
			fmt.Fprintf(&b, "%s %s:%d:%d %s\n", o.Class, o.Path, o.Line, o.Col, o.Reason)
		}
	}
	for _, warn := range plan.Warnings {
		fmt.Fprintf(&b, "! %s\n", warn)
	}
	for _, d := range plan.Dynamic {
		fmt.Fprintf(&b, "? %s:%d %s %v\n", d.Path, d.Line, d.Pattern, d.Matches)
	}
	return b.String()
}

// lintFile is the rename.File of a document, as lint.Document makes it.
func lintFile(doc *lint.Document, force bool) rename.File {
	return doc.RenameFile(force)
}

// A function that is not private: its definition, the redefinitions up
// and down the hierarchy (one function by the driver's rules), and every
// call that reaches one of them, in every file; the same name in an
// object outside the hierarchy is another function; calls on objects
// nobody can tell are possible, and made only when asked for.
func TestRenameFunctionAcross(t *testing.T) {
	w := newWorld(t, wideLib(), "/lib")
	require.True(t, w.l.Scan(context.Background()))
	got := w.renamed("/lib/obj/monster.c", "string query_na|me()", "query_short_name", false, false)

	assert.Contains(t, got, "== /lib/std/base.c\nstring name;\n\nstring query_short_name() { return name; }\n")
	assert.Contains(t, got, "string query_short_name() { return \"living \" + ::query_short_name(); }")
	assert.Contains(t, got, "string query_short_name() { return \"monster\"; }")
	assert.Contains(t, got, "    write(query_short_name());\n    ob->query_name();\n    LIVING->query_short_name();\n    call_other(this_object(), \"query_short_name\");\n    call_out(\"query_short_name\", 1);")
	assert.Contains(t, got, "    \"/obj/monster\"->query_short_name();\n    \"/obj/other\"->query_name();\n    ob->query_name();")
	assert.NotContains(t, got, "== /lib/obj/other.c", "another function of the name")
	assert.Contains(t, got, "-- certain 9, possible 4, excluded 4\n")
	assert.Contains(t, got, "possible /lib/obj/monster.c:8:9 a call on an object nobody can tell, which may be one that has it")
	assert.Contains(t, got, "possible /lib/obj/caller.c:5:9 a call on an object nobody can tell")
	assert.Contains(t, got, "possible /lib/include/std.h:3:12 in the body of the macro QN, where what it names depends on where the macro is used")
	assert.Contains(t, got, "possible /lib/obj/dead.c:4:5 in code that is not compiled here (a branch of #if that is not taken)")
	assert.Contains(t, got, "excluded /lib/obj/other.c:1:8 names the function 'query_name' (/obj/other.c)")
	assert.Contains(t, got, "excluded /lib/obj/caller.c:4:19 names the function 'query_name' (/obj/other.c)")
	assert.Contains(t, got, "! edits in base files: /std/base.c (5 objects inherit it), /std/living.c (4 objects inherit it): an object keeps")
	assert.Contains(t, got, "must be recompiled for the rename to take effect")
	assert.Contains(t, got, "? /lib/obj/caller.c:6 do_* false\n")
	assert.Contains(t, got, "? /lib/obj/caller.c:7 * false\n")

	// The same from base, with the possible edits.
	got = w.renamed("/lib/std/base.c", "string query_na|me()", "query_short_name", false, true)
	assert.Contains(t, got, "    ob->query_short_name();")
	assert.Contains(t, got, "#define QN query_short_name()")
	assert.Contains(t, got, "    query_short_name();\n#endif")
	assert.NotContains(t, got, "== /lib/obj/other.c")

	// A function whose name a call made at run time begins with.
	got = w.renamed("/lib/obj/cmds.c", "void do_lo|ok", "do_see", false, false)
	assert.Contains(t, got, "void do_see() {}")
	assert.True(t, strings.Index(got, "? /lib/obj/cmds.c:2 do_* true") < strings.Index(got, "? /lib/obj/caller.c:7 * false"), got)
}

// A global variable that is not private: its object and the objects
// that inherit it; and the variables save_object writes, refused unless
// forced.
func TestRenameVariableAcross(t *testing.T) {
	w := newWorld(t, wideLib(), "/lib")
	require.True(t, w.l.Scan(context.Background()))
	got := w.renamed("/lib/obj/room1.c", "|light = 2", "lux", false, false)
	assert.Contains(t, got, "== /lib/std/room.c\nint lux;\nvoid set_light(int l) { lux = l; }\n")
	assert.Contains(t, got, "== /lib/obj/room1.c\ninherit \"/std/room\";\nvoid create() { lux = 2; set_light(1); }\n")
	assert.Contains(t, got, "void f() { int lamp; lamp = lux; }")
	assert.Contains(t, got, "-- certain 5, possible 1, excluded 0\n")

	// A local where the variable is used would hide it; a name that
	// stands for nothing would be captured; an inheritor's variable of
	// the name; a stringizing macro.
	assert.Equal(t, "refused: at /obj/room2.c:2 the renamed variable would be hidden by the local variable 'lamp' declared at line 2",
		w.renamed("/lib/std/room.c", "int li|ght", "lamp", false, false))
	assert.Equal(t, "refused: at /obj/hidden.c:2 the renamed variable would be hidden by the local variable 'dim' declared at line 2",
		w.renamed("/lib/std/room.c", "int li|ght", "dim", false, false))
	assert.Contains(t, w.renamed("/lib/std/room.c", "int li|ght", "glow", false, false),
		"possible /lib/obj/stringize.c:3:22 an argument of the macro STR, which makes a string of its arguments or pastes them (# or ##)")

	// hp: /obj/saver inherits living and saves itself.
	want := "refused: 'hp' is written by save_object to the save files under its name (/obj/saver names save_object): renamed, restore_object no longer finds the saved value"
	assert.Equal(t, want, w.prepare("/lib/std/living.c", "int h|p;"))
	assert.Equal(t, want, w.renamed("/lib/obj/monster.c", "    h|p = 3;", "health", false, false))
	got = w.renamed("/lib/obj/monster.c", "    h|p = 3;", "health", true, false)
	assert.Contains(t, got, "int health;")
	assert.Contains(t, got, "void heal() { health = MAX_HP; }")
	assert.Contains(t, got, "    health = 3;")
	assert.Contains(t, got, "! renamed although 'hp' is written by save_object")
}

// The clashes of a new name across files.
func TestClashesAcross(t *testing.T) {
	w := newWorld(t, wideLib(), "/lib")
	require.True(t, w.l.Scan(context.Background()))
	for _, c := range []struct{ path, at, name, want string }{
		{"/lib/std/base.c", "string query_na|me()", "set_name", "refused: /std/base has a function 'set_name' already (/std/base.c:4)"},
		{"/lib/std/base.c", "string query_na|me()", "query_short", "refused: /obj/clash, which has the renamed function, has a function 'query_short' already (/obj/clash.c:2)"},
		{"/lib/std/base.c", "string query_na|me()", "heal", "refused: /std/living has a function 'heal' already (/std/living.c:8)"},
		{"/lib/std/base.c", "string query_na|me()", "strlen", "refused: 'strlen' is a kfun: calls of it in the objects that have the renamed function would reach the renamed function instead"},
		{"/lib/std/base.c", "string query_na|me()", "autof", "refused: 'autof' is a function of the auto object, which every object has"},
		{"/lib/std/base.c", "string query_na|me()", "ME", "refused: 'ME' is a macro in /obj/caller.c (#define in /include/std.h)"},
		{"/lib/std/room.c", "void set_li|ght", "brightness", "refused: 'brightness' is called at /obj/room2.c:3: the call would reach the renamed function"},
		{"/lib/std/room.c", "int li|ght", "name", "refused: no clash expected"},
		{"/lib/include/hp.h", "#define MAX_|HP", "hp", "refused: 'hp' is used at /std/living.c:5: the renamed macro would replace it"},
		{"/lib/include/hp.h", "#define MAX_|HP", "int", "refused: 'int' is a keyword"},
	} {
		got := w.renamed(c.path, c.at, c.name, false, false)
		if c.want == "refused: no clash expected" {
			assert.NotContains(t, got, "refused", c.at+" to "+c.name)
			continue
		}
		assert.Equal(t, c.want, got, c.path+": "+c.at+" to "+c.name)
	}
}

// A header's macro: every file that includes it.
func TestRenameMacroAcross(t *testing.T) {
	w := newWorld(t, wideLib(), "/lib")
	require.True(t, w.l.Scan(context.Background()))
	got := w.renamed("/lib/std/living.c", "hp = MAX|_HP", "HP_MAX", false, false)
	assert.Equal(t, "== /lib/include/hp.h\n#define HP_MAX 100\n== /lib/std/living.c\n"+
		strings.Replace(wideLib()["/lib/std/living.c"], "MAX_HP", "HP_MAX", 1)+
		"-- certain 2, possible 0, excluded 0\n! edits in base files: /std/living.c (4 objects inherit it), /include/hp.h (1 file includes it): an object keeps the program it was compiled with, so the objects that inherit or include them must be recompiled for the rename to take effect\n", got)

	got = w.renamed("/lib/include/std.h", "#define LIV|ING", "LIVING_OB", false, false)
	assert.Contains(t, got, "#define LIVING_OB \"/std/living\"")
	assert.Contains(t, got, "inherit LIVING_OB;")
	assert.Contains(t, got, "    LIVING_OB->query_name();")
}

// A private function of a file another includes: its calls in every
// file that has it; a call by name reaches no private function.
func TestRenamePrivateIncluded(t *testing.T) {
	w := newWorld(t, wideLib(), "/lib")
	require.True(t, w.l.Scan(context.Background()))
	got := w.renamed("/lib/obj/inc.c", "private void inc|fn", "helper", false, false)
	assert.Contains(t, got, "== /lib/obj/inc.c\nprivate void helper() {}\nvoid g() { helper(); }\n")
	assert.Contains(t, got, "== /lib/obj/user.c\n#include \"inc.c\"\nvoid h() { helper(); call_out(\"incfn\", 1); }\n")
	assert.Contains(t, got, "excluded /lib/obj/user.c:2:31 DGD does not call a private function by name")
	assert.Contains(t, got, "! /obj/user.c:2: the string or ->incfn cannot reach the private function 'incfn'")
}

// A wizard's checkout: a function that is not private is renamed in the
// checkout, with a warning; one that redefines a function of the
// profile's is refused.
func TestRenamePartialAcross(t *testing.T) {
	p := &profile.Profile{
		Manifest: profile.Manifest{
			Name: "mini", Dialect: profile.Dialect{Driver: "dgd"},
			IncludeDirs: []string{"/include"}, AutoObjects: []string{"/sys/auto"},
			Covered: []string{"/std/", "/include/", "/sys/"},
		},
		Files: map[string]string{
			"/include/std.h": "#define ROOM \"/std/room\"\n",
			"/sys/auto.c":    "void autof();\n",
			"/std/room.c":    "int room_var;\nvoid set_short(string s);\nvoid init();\n",
		},
	}
	data, err := p.Marshal()
	require.NoError(t, err)
	fs := vfs.Mem{
		"/home/ann/mini.profile": string(data),
		"/home/ann/dgdtools.yml": "profile: mini.profile\nlib_path: /players/ann\n",
		"/home/ann/hall.c":       "#include <std.h>\ninherit ROOM;\nvoid open_door() {}\nvoid init() { open_door(); }\n",
		"/home/ann/key.c":        "void f() { \"/players/ann/hall\"->open_door(); }\n",
	}
	w := newWorld(t, fs, "/home/ann")
	require.True(t, w.l.Scan(context.Background()))
	got := w.renamed("/home/ann/hall.c", "void open_d|oor", "unlock", false, false)
	assert.Contains(t, got, "== /home/ann/hall.c\n#include <std.h>\ninherit ROOM;\nvoid unlock() {}\nvoid init() { unlock(); }\n")
	assert.Contains(t, got, "== /home/ann/key.c\nvoid f() { \"/players/ann/hall\"->unlock(); }\n")
	assert.Contains(t, got, "! this checkout is part of the mudlib: the places outside it that name 'open_door' are not renamed, and cannot be seen from here")
	assert.Equal(t, "refused: 'init' redefines, or is redefined by, the function of /std/room.c, which is defined in the mudlib profile, not in your checkout: the two are one function and are renamed together",
		w.prepare("/home/ann/hall.c", "void in|it()"))
}

// A program made of files included as code: the prototype a header has,
// the definition in one file and the calls in another, each file read on
// its own, are one function.
func TestRenameProgramOfFiles(t *testing.T) {
	w := newWorld(t, wideLib(), "/lib")
	require.True(t, w.l.Scan(context.Background()))
	got := w.renamed("/lib/sys/impl.c", "void help|me", "assist", false, false)
	assert.Contains(t, got, "== /lib/sys/impl.c\nvoid assist() {}\n")
	assert.Contains(t, got, "== /lib/sys/parts.h\nvoid assist();\n")
	assert.Contains(t, got, "== /lib/sys/use.c\n#include \"parts.h\"\nvoid g() { assist(); }\n")
}

// The driver object's functions that the driver calls by name are
// refused, and an inheritor's redefinition with them; a function the
// driver calls in a connection's object is renamed with a warning.
func TestRenameDriverCalls(t *testing.T) {
	w := newWorld(t, vfs.Mem{
		"/lib/dgdtools.yml": "root: .\ndriver_config: mud.dgd\n",
		"/lib/mud.dgd":      "driver_object = \"/sys/driver\";\n",
		"/lib/sys/base.c":   "string path_read(string p) { return p; }\n",
		"/lib/sys/driver.c": "inherit \"/sys/base\";\nstring path_write(string p) { return p; }\nvoid helper() {}\n",
		"/lib/obj/user.c":   "void receive_message(string s) {}\nstring path_write(string p) { return p; }\n",
		"/lib/obj/other.c":  "string path_read(string p) { return p; }\n",
	}, "/lib")
	require.True(t, w.l.Scan(context.Background()))
	assert.Equal(t, "refused: 'path_write' is a function of the driver object (/sys/driver), which the driver calls by name",
		w.prepare("/lib/sys/driver.c", "string path_w|rite"))
	assert.Equal(t, "refused: 'path_read' is a function of the driver object (/sys/driver), which the driver calls by name",
		w.prepare("/lib/sys/base.c", "path_r|ead"))
	// Another object's function of a name the driver object has.
	assert.Equal(t, "function path_read \"path_read\"", w.prepare("/lib/obj/other.c", "path_r|ead"))
	got := w.renamed("/lib/obj/user.c", "receive_m|essage", "receive_text", false, false)
	assert.Contains(t, got, "! the driver calls 'receive_message' by name in an object that is a connection")

	// Nor is a function renamed to what the driver calls by name: the
	// create function anywhere, the driver object's in the driver object.
	assert.Equal(t, "refused: 'create' is the function the driver calls by name to initialize every object (create in the configuration): renamed so, the function is called when an object is made",
		w.renamed("/lib/obj/other.c", "path_r|ead", "create", false, false))
	assert.Equal(t, "refused: 'compile_error' is a function the driver calls by name in the driver object (/sys/driver): renamed so, the function is called by the driver",
		w.renamed("/lib/sys/driver.c", "void hel|per", "compile_error", false, false))
	assert.NotContains(t, w.renamed("/lib/obj/other.c", "path_r|ead", "compile_error", false, false), "refused", "not the driver object's")
}

// A macro used in a file an edit lands in, whose body names the new
// name, would name the renamed symbol there.
func TestClashThroughMacroAcross(t *testing.T) {
	w := newWorld(t, vfs.Mem{
		"/lib/dgdtools.yml": "root: .\n",
		"/lib/std/room.c":   "int light;\n",
		"/lib/obj/r.c":      "#define DIM dimness\ninherit \"/std/room\";\nvoid f() { light = DIM; }\n",
	}, "/lib")
	require.True(t, w.l.Scan(context.Background()))
	assert.Equal(t, "refused: the macro DIM, used at /obj/r.c:3, names 'dimness' in its body: it would name the renamed global variable there",
		w.renamed("/lib/std/room.c", "int li|ght", "dimness", false, false))
	assert.NotContains(t, w.renamed("/lib/std/room.c", "int li|ght", "glow", false, false), "refused")
}

// A callback named by a macro whose body is the name as a string is a
// possible place, at the string in the macro's body; a call whose name is
// made when it runs is listed where it is.
func TestRenameCallbackByMacro(t *testing.T) {
	fs := vfs.Mem{
		"/lib/dgdtools.yml": "root: .\ndriver_config: mud.dgd\n",
		"/lib/mud.dgd":      "include_dirs = ({ \"/include\" });\n",
		"/lib/include/m.h":  "#define NAME_FN \"query_name\"\n#define OTHER_FN \"other\"\n",
		"/lib/std/living.c": "#include <m.h>\nstring query_name() { return \"x\"; }\n",
		"/lib/obj/caller.c": "#include <m.h>\ninherit \"/std/living\";\nvoid f(string s) {\n    call_other(this_object(), NAME_FN);\n    call_other(this_object(), OTHER_FN);\n    call_out(s, 1);\n}\n",
	}
	w := newWorld(t, fs, "/lib")
	require.True(t, w.l.Scan(context.Background()))
	got := w.renamed("/lib/std/living.c", "string query_na|me", "qn", false, true)
	assert.Contains(t, got, "== /lib/include/m.h\n#define NAME_FN \"qn\"\n#define OTHER_FN \"other\"\n")
	assert.Contains(t, got, "possible /lib/include/m.h:1:18 the body of the macro NAME_FN, by which /obj/caller.c:4 names it; other places may use the macro too")
	assert.Contains(t, got, "? /lib/obj/caller.c:6 * false")
	assert.NotContains(t, got, "macro OTHER_FN")
}
