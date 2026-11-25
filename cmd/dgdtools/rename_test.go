package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// renameLib is a lib with a function redefined down a hierarchy and
// called from a wizard's and a domain's trees, a call on an object
// nobody can tell, a variable save_object writes, a header's macro.
func renameLib() map[string]string {
	return map[string]string{
		"lib/dgdtools.yml":               "root: .\ndriver_config: mud.dgd\nprofile_build: {checkouts: [\"players/*\", \"d/*\"]}\n",
		"lib/mud.dgd":                    "include_dirs = ({ \"/include\" });\n",
		"lib/include/std.h":              "#define LIVING \"/std/living\"\n",
		"lib/std/living.c":               "#include <std.h>\nint hp;\nstring query_name() { return \"x\"; }\n",
		"lib/obj/monster.c":              "#include <std.h>\ninherit LIVING;\nstring query_name() { return \"m\"; }\n",
		"lib/players/ann/pet.c":          "inherit \"/std/living\";\nvoid f(object ob) { write(query_name()); ob->query_name(); }\n",
		"lib/d/Ruins/guard.c":            "void f() { \"/obj/monster\"->query_name(); }\n",
		"lib/d/Ruins/saver.c":            "inherit \"/std/living\";\nvoid save() { save_object(\"/data/x\"); }\n",
		"lib/players/ann/workroom.c":     "void f() { int count; count = 1; }\n",
		"lib/players/ann/notes/README.c": "void g() {}\n",
	}
}

// The default prints the certain edits as diffs, by owner, and a
// summary; exit 1 for the possible place left. Nothing is written.
func TestRenameDiff(t *testing.T) {
	dir := tree(t, renameLib())
	t.Chdir(filepath.Join(dir, "lib"))
	code, out, errs := dgdtools(t, "", "rename", "/std/living::query_name", "query_short")
	assert.Equal(t, 1, code, errs)
	assert.Equal(t, `# d/Ruins: 1 file
--- d/Ruins/guard.c
+++ d/Ruins/guard.c
@@ -1 +1 @@
-void f() { "/obj/monster"->query_name(); }
+void f() { "/obj/monster"->query_short(); }
# players/ann: 1 file
--- players/ann/pet.c
+++ players/ann/pet.c
@@ -1,2 +1,2 @@
 inherit "/std/living";
-void f(object ob) { write(query_name()); ob->query_name(); }
+void f(object ob) { write(query_short()); ob->query_name(); }
# other: 2 files
--- obj/monster.c
+++ obj/monster.c
@@ -1,3 +1,3 @@
 #include <std.h>
 inherit LIVING;
-string query_name() { return "m"; }
+string query_short() { return "m"; }
--- std/living.c
+++ std/living.c
@@ -1,3 +1,3 @@
 #include <std.h>
 int hp;
-string query_name() { return "x"; }
+string query_short() { return "x"; }
`, out)
	assert.Contains(t, errs, "rename function query_name to query_short (/std/living.c:3)\n  4 certain, 1 possible, 0 excluded;")
	assert.Contains(t, errs, "  d/Ruins              1 file, 1 edit\n  players/ann          1 file, 1 edit\n  other                2 files, 2 edits\n")
	assert.Contains(t, errs, "1 place may be the renamed function and is not renamed (--include-possible edits it too):\n    players/ann/pet.c:2:46: a call on an object nobody can tell")
	assert.Contains(t, errs, "warning: edits in base files: /std/living.c (3 objects inherit it)")
	assert.Contains(t, errs, "4 files would change (--write writes them)")
	assert.Equal(t, renameLib()["lib/std/living.c"], read(t, filepath.Join(dir, "lib/std/living.c")))

	// With the possible place too: complete, exit 0.
	code, out, _ = dgdtools(t, "", "rename", "--include-possible", "-q", "/std/living.c::query_name", "query_short")
	assert.Equal(t, 0, code)
	assert.Contains(t, out, "+void f(object ob) { write(query_short()); ob->query_short(); }")
}

// --write writes the files; a symbol by its place; a local.
func TestRenameWrite(t *testing.T) {
	dir := tree(t, renameLib())
	t.Chdir(dir)
	code, out, errs := dgdtools(t, "", "rename", "--write", "lib/obj/monster.c:3:10", "query_short")
	assert.Equal(t, 1, code, errs)
	assert.Empty(t, out)
	assert.Contains(t, errs, "4 files written\n")
	assert.Equal(t, "#include <std.h>\nint hp;\nstring query_short() { return \"x\"; }\n", read(t, filepath.Join(dir, "lib/std/living.c")))
	assert.Equal(t, "void f() { \"/obj/monster\"->query_short(); }\n", read(t, filepath.Join(dir, "lib/d/Ruins/guard.c")))
	assert.Equal(t, "inherit \"/std/living\";\nvoid f(object ob) { write(query_short()); ob->query_name(); }\n", read(t, filepath.Join(dir, "lib/players/ann/pet.c")))

	code, _, errs = dgdtools(t, "", "rename", "--write", "lib/players/ann/workroom.c:1:20", "n")
	assert.Equal(t, 0, code, errs)
	assert.Equal(t, "void f() { int n; n = 1; }\n", read(t, filepath.Join(dir, "lib/players/ann/workroom.c")))

	// A header's macro, from the lib.
	t.Chdir(filepath.Join(dir, "lib"))
	code, _, errs = dgdtools(t, "", "rename", "--write", "/include/std.h::LIVING", "LIVING_OB")
	assert.Equal(t, 0, code, errs)
	assert.Equal(t, "#include <std.h>\ninherit LIVING_OB;\nstring query_short() { return \"m\"; }\n", read(t, filepath.Join(dir, "lib/obj/monster.c")))
}

// A variable save_object writes is refused, exit 2, unless forced.
func TestRenameForce(t *testing.T) {
	dir := tree(t, renameLib())
	t.Chdir(filepath.Join(dir, "lib"))
	code, out, errs := dgdtools(t, "", "rename", "/std/living::hp", "health")
	assert.Equal(t, 2, code)
	assert.Empty(t, out)
	assert.Equal(t, "dgdtools rename: refused: 'hp' is written by save_object to the save files under its name (/d/Ruins/saver names save_object): renamed, restore_object no longer finds the saved value (--force renames it all the same)\n", errs)

	code, out, errs = dgdtools(t, "", "rename", "--force", "/std/living::hp", "health")
	assert.Equal(t, 0, code, errs)
	assert.Contains(t, out, "+int health;")
	assert.Contains(t, errs, "warning: renamed although 'hp' is written by save_object")
}

// The JSON of a plan.
func TestRenameJSON(t *testing.T) {
	dir := tree(t, renameLib())
	t.Chdir(filepath.Join(dir, "lib"))
	code, out, errs := dgdtools(t, "", "rename", "--output-format", "json", "/obj/monster::query_name", "query_short")
	assert.Equal(t, 1, code)
	assert.Empty(t, errs)
	var got struct {
		Symbol struct {
			Kind, Name, Path string
			Line             int
		}
		New      string
		Complete bool
		Written  bool
		Counts   struct{ Certain, Possible, Excluded int }
		Files    []struct {
			Path, Owner string
			Edits       int
		}
		Occurrences []struct {
			Path      string
			Line, Col int
			Class     string
			Decl      bool
			Reason    string
		}
		Warnings []string
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.Equal(t, "function", got.Symbol.Kind)
	assert.Equal(t, "/obj/monster.c", got.Symbol.Path)
	assert.Equal(t, 3, got.Symbol.Line)
	assert.False(t, got.Complete)
	assert.False(t, got.Written)
	assert.Equal(t, 4, got.Counts.Certain)
	assert.Equal(t, 1, got.Counts.Possible)
	require.Len(t, got.Files, 4)
	assert.Equal(t, "d/Ruins/guard.c", got.Files[0].Path)
	assert.Equal(t, "d/Ruins", got.Files[0].Owner)
	assert.Len(t, got.Occurrences, 5)
	assert.Len(t, got.Warnings, 1)
}

// What is wrong with the command line, and refusals: exit 2.
func TestRenameErrors(t *testing.T) {
	dir := tree(t, renameLib())
	t.Chdir(filepath.Join(dir, "lib"))
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"x"}, "want SYMBOL (or FILE) and NEW"},
		{[]string{"--write", "--diff", "/std/living::hp", "y"}, "--write and --diff cannot be combined"},
		{[]string{"--output-format", "xml", "/std/living::hp", "y"}, `unknown format "xml"`},
		{[]string{"query_name", "y"}, `"query_name" is no symbol`},
		{[]string{"/std/nothing::f", "y"}, "/std/nothing: no such object in the mudlib"},
		{[]string{"/std/living::nothing", "y"}, "/std/living has no function, global variable or macro nothing"},
		{[]string{"/include/std.h::NOPE", "y"}, "/include/std.h has no #define of NOPE"},
		{[]string{"std/living.c:99:1", "y"}, "has no line 99, column 1"},
		{[]string{"/std/living::query_name", "hp y"}, "refused: 'hp y' is not a name"},
		{[]string{"std/living.c:2:1", "y"}, "refused: nothing here can be renamed"},
	} {
		code, out, errs := dgdtools(t, "", append([]string{"rename"}, c.args...)...)
		assert.Equal(t, 2, code, "%v: %s", c.args, errs)
		assert.Empty(t, out, "%v", c.args)
		assert.Contains(t, errs, c.want, "%v", c.args)
	}
	// Without a lib.
	t.Chdir(t.TempDir())
	code, _, errs := dgdtools(t, "", "rename", "/std/living::hp", "y")
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, "a rename needs a mudlib: no dgdtools.yml, and no --root")
}

// A FILE moves: the diffs of the paths that name it, the macro's place
// possible (exit 1); --write writes them and moves the file.
func TestRenameMove(t *testing.T) {
	dir := tree(t, renameLib())
	t.Chdir(filepath.Join(dir, "lib"))
	code, out, errs := dgdtools(t, "", "rename", "/std/living.c", "being.c")
	assert.Equal(t, 1, code, errs)
	assert.Equal(t, `# d/Ruins: 1 file
--- d/Ruins/saver.c
+++ d/Ruins/saver.c
@@ -1,2 +1,2 @@
-inherit "/std/living";
+inherit "/std/being";
 void save() { save_object("/data/x"); }
# players/ann: 1 file
--- players/ann/pet.c
+++ players/ann/pet.c
@@ -1,2 +1,2 @@
-inherit "/std/living";
+inherit "/std/being";
 void f(object ob) { write(query_name()); ob->query_name(); }
`, out)
	assert.Contains(t, errs, "move /std/living.c to /std/being.c\n  2 certain, 2 possible, 0 excluded;")
	assert.Contains(t, errs, "2 places may name a moved file and are not changed (--include-possible edits those it can):\n"+
		"    include/std.h:1:17: a string in a directive equal to the old path, not known to be an object path\n"+
		"    obj/monster.c:2:9: names /std/living.c by the macro LIVING")
	assert.Contains(t, errs, "2 files would change (--write writes them and moves /std/living.c)")
	assert.FileExists(t, filepath.Join(dir, "lib/std/living.c"))

	code, _, errs = dgdtools(t, "", "rename", "--write", "/std/living.c", "/std/being.c")
	assert.Equal(t, 1, code, errs)
	assert.Contains(t, errs, "2 files written, /std/living.c moved to /std/being.c")
	assert.NoFileExists(t, filepath.Join(dir, "lib/std/living.c"))
	assert.Equal(t, renameLib()["lib/std/living.c"], read(t, filepath.Join(dir, "lib/std/being.c")))
	assert.Equal(t, "inherit \"/std/being\";\nvoid save() { save_object(\"/data/x\"); }\n", read(t, filepath.Join(dir, "lib/d/Ruins/saver.c")))

	// Errors: no such file, a place taken.
	code, _, errs = dgdtools(t, "", "rename", "/std/living.c", "x.c")
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, "/std/living.c: no such file or directory, on disk or in the mudlib")
	code, _, errs = dgdtools(t, "", "rename", "/std/being.c", "/obj/monster.c")
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, "/obj/monster.c exists already")
}

// FILE is a path on disk when there is one, from the working directory,
// and NEW is then one too; a directory moves with its files.
func TestRenameMoveOnDisk(t *testing.T) {
	files := renameLib()
	files["dgdtools.yml"] = "root: lib\ndriver_config: lib/mud.dgd\n"
	delete(files, "lib/dgdtools.yml")
	dir := tree(t, files)
	t.Chdir(dir)
	code, out, errs := dgdtools(t, "", "rename", "lib/std/living.c", "lib/std/being.c")
	assert.Equal(t, 1, code, errs)
	assert.NotContains(t, out, "# ", "no checkout patterns: the files are not grouped")
	assert.Contains(t, out, "+inherit \"/std/being\";")
	assert.Contains(t, errs, "move /std/living.c to /std/being.c\n")

	t.Chdir(filepath.Join(dir, "lib/std"))
	code, _, errs = dgdtools(t, "", "rename", "--write", "living.c", "being.c")
	assert.Equal(t, 1, code, errs)
	assert.FileExists(t, filepath.Join(dir, "lib/std/being.c"))

	t.Chdir(dir)
	code, _, errs = dgdtools(t, "", "rename", "--write", "/d/Ruins", "/d/Old")
	assert.Equal(t, 0, code, errs)
	assert.Contains(t, errs, "move /d/Ruins to /d/Old\n")
	assert.FileExists(t, filepath.Join(dir, "lib/d/Old/guard.c"))
	assert.NoDirExists(t, filepath.Join(dir, "lib/d/Ruins"))
}
