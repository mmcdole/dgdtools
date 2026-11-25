package resolve_test

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/index"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/resolve"
	"github.com/mmcdole/dgdtools/pkg/token"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// refs finds the references of what the place in the file at file that
// at marks names (see world.ref), and writes them a line each:
//
//	FILE:LINE:COL "the text" [decl]
//
// after a first line that says what was searched and what was found
// beside: "KIND NAME [complete] [partial] [possible N in M]".
func (w *world) refs(file, at string, decl bool) string {
	w.t.Helper()
	doc, off := w.doc(file, at)
	refs := doc.References(context.Background(), off, decl)
	if refs == nil {
		return "nothing"
	}
	out := fmt.Sprintf("%s %s", refs.Ref.Kind, refs.Ref.Name)
	if refs.Complete {
		out += " complete"
	}
	if refs.Partial {
		out += " partial"
	}
	if refs.Possible > 0 {
		out += fmt.Sprintf(" possible %d in %d", refs.Possible, refs.PossibleFiles)
	}
	for _, s := range refs.Sites {
		out += "\n" + w.site(doc, s)
	}
	return out
}

// highlights is refs for the places of the file itself, each with how it
// uses a variable: "LINE:COL "the text" read|write".
func (w *world) highlights(file, at string) string {
	w.t.Helper()
	doc, off := w.doc(file, at)
	sites := doc.Highlights(off)
	if sites == nil {
		return "nothing"
	}
	var lines []string
	for _, s := range sites {
		line := strings.TrimPrefix(w.site(doc, s), file+":")
		switch s.Access {
		case resolve.Read:
			line += " read"
		case resolve.Write:
			line += " write"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// doc lints the file and returns the offset at marks in it.
func (w *world) doc(file, at string) (*lint.Document, uint32) {
	w.t.Helper()
	doc, err := w.l.Lint(context.Background(), file, lint.Options{})
	require.NoError(w.t, err)
	cursor := strings.IndexByte(at, '|')
	require.GreaterOrEqual(w.t, cursor, 0, "no cursor in %q", at)
	needle := at[:cursor] + at[cursor+1:]
	src := string(doc.File.Src)
	require.Equal(w.t, 1, strings.Count(src, needle), "%q in %s", needle, file)
	return doc, uint32(strings.Index(src, needle) + cursor)
}

// site writes a place: its file, line and column, its text; whether it
// is a declaration. The line and the column the index has are held to
// the text.
func (w *world) site(doc *lint.Document, s resolve.Site) string {
	w.t.Helper()
	text, err := doc.Snapshot.ReadFile(s.Path)
	require.NoError(w.t, err, s.Path)
	line := 1 + bytes.Count(text[:s.Start], []byte("\n"))
	col := int(s.Start) - bytes.LastIndexByte(text[:s.Start], '\n')
	if s.Line != 0 {
		assert.Equal(w.t, [2]int{line, col}, [2]int{int(s.Line), int(s.Col)}, "%s:%d", s.Path, s.Start)
	}
	out := fmt.Sprintf("%s:%d:%d %q", s.Path, line, col, text[s.Start:s.End])
	if s.Decl {
		out += " decl"
	}
	return out
}

func TestReferences(t *testing.T) {
	w := newWorld(t, wholeLib(), "/lib")
	require.True(t, w.l.Scan(context.Background()))
	const a, mid = "/lib/obj/a.c", "/lib/std/mid.c"
	for _, c := range []struct {
		file, at string
		decl     bool
		want     string
	}{
		// A function: its calls of every form that reaches it, in every
		// file; by ::, from an inheritor; by -> on an object that
		// inherits it. Calls on objects nobody can tell are not among
		// them, and are counted.
		{a, "|foo(1)", false, `function foo complete
/lib/obj/a.c:13:5 "foo"
/lib/obj/a.c:17:20 "foo"`},
		{"/lib/std/base.c", "int fo|o(", true, `function foo complete
/lib/obj/a.c:13:5 "foo"
/lib/obj/a.c:17:20 "foo"
/lib/std/base.c:4:5 "foo" decl`},
		{"/lib/std/base.c", "int fo|o(", false, `function foo complete
/lib/obj/a.c:13:5 "foo"
/lib/obj/a.c:17:20 "foo"`},
		// One that the auto object gives, and one an inheritor
		// redefines: the redefinition and its callers are another's.
		{a, "\"/obj/b\"->ba|r", true, `function bar complete possible 1 in 1
/lib/obj/a.c:19:15 "bar"
/lib/std/mid.c:3:6 "bar" decl`},
		{a, "call_other(this_player(), \"ba|r", true, "nothing"},
		{mid, "void cre|ate() {", true, `function create complete
/lib/std/mid.c:2:6 "create" decl`},
		{"/lib/std/base.c", "void cre|ate() {}", true, `function create complete
/lib/obj/a.c:11:7 "create"
/lib/std/base.c:3:6 "create" decl
/lib/std/mid.c:2:19 "create"`},
		{a, "\"/obj/b\", \"cre|ate", true, `function create complete
/lib/obj/a.c:21:27 "create"
/lib/obj/b.c:3:6 "create" decl`},
		{a, "call_out(\"lat|er", true, `function later complete
/lib/obj/a.c:7:6 "later" decl
/lib/obj/a.c:18:9 "later"
/lib/obj/a.c:23:15 "later"
/lib/obj/a.c:37:6 "later" decl`},
		{a, "auto|f();\n    s", false, `function autof complete
/lib/obj/a.c:14:5 "autof"
/lib/obj/a.c:31:15 "autof"`},
		// Unknown receivers: the name is the same, and nothing proves
		// the call is this function's.
		{"/lib/std/mid.c", "void ba|r", true, `function bar complete possible 1 in 1
/lib/obj/a.c:19:15 "bar"
/lib/std/mid.c:3:6 "bar" decl`},

		// A global variable, in its object and those that inherit it; a
		// local of the same name hides it.
		{"/lib/std/base.c", "int base_v|ar", true, `variable base_var complete
/lib/obj/a.c:16:13 "base_var"
/lib/obj/a.c:38:53 "base_var"
/lib/obj/a.c:39:50 "base_var"
/lib/std/base.c:1:5 "base_var" decl`},
		{a, "cou|nt = base_var", true, `variable count complete
/lib/obj/a.c:6:5 "count" decl
/lib/obj/a.c:16:5 "count"`},

		// Locals and parameters: in their scope only.
		{a, "base_var + |n", true, `local n complete
/lib/obj/a.c:10:9 "n" decl
/lib/obj/a.c:16:24 "n"
/lib/obj/a.c:28:5 "n"`},
		{a, "return cou|nt", true, `parameter count complete
/lib/obj/a.c:38:18 "count" decl
/lib/obj/a.c:38:45 "count"`},
		{a, "return cou|nt", false, `parameter count complete
/lib/obj/a.c:38:45 "count"`},

		// A macro: in code, in other directives and in the bodies of
		// other macros, in the headers too; and in an inherit
		// statement, which is about the object and names the macro.
		{a, "SQU|ARE(2)", true, `macro SQUARE complete
/lib/include/std.h:3:25 "SQUARE" decl
/lib/obj/a.c:16:28 "SQUARE"
/lib/obj/a.c:47:19 "SQUARE"`},
		{a, "LOCAL, AU|TO", true, `macro AUTO complete
/lib/include/auto.h:1:9 "AUTO" decl
/lib/obj/a.c:29:24 "AUTO"
/lib/obj/a.c:44:7 "AUTO"
/lib/obj/a.c:46:8 "AUTO"`},
		{"/lib/include/std.h", "#define ST|D", true, `macro STD complete
/lib/include/std.h:1:9 "STD" decl
/lib/obj/a.c:3:9 "STD"`},
		{"/lib/obj/local.h", "#define LO|CAL", true, `macro LOCAL complete
/lib/obj/a.c:29:17 "LOCAL"
/lib/obj/local.h:1:9 "LOCAL" decl
/lib/obj/local.h:2:28 "LOCAL"`},

		// An object: the inherit statements and the paths handed to the
		// functions that take one. A file: the #includes of it.
		{a, "inherit ST|D;", true, `object /std/base complete
/lib/obj/a.c:3:9 "STD"
/lib/std/mid.c:1:9 "\"/std/base\""`},
		{a, "clone_object(\"/ob|j/b", true, `object /obj/b complete
/lib/obj/a.c:26:19 "/obj/b"`},
		{a, "#include <st|d.h>", true, `include std.h complete
/lib/obj/a.c:1:10 "<std.h>"`},

		// Nothing declared: a function of the driver's, a call on an
		// unknown receiver, a keyword.
		{a, "size|of", true, "nothing"},
		{a, "n->unkn|own", true, "nothing"},
		{a, "vo|id later() {", true, "nothing"},
	} {
		assert.Equal(t, c.want, w.refs(c.file, c.at, c.decl), "%s at %q", c.file, c.at)
	}
}

func TestHighlights(t *testing.T) {
	w := newWorld(t, wholeLib(), "/lib")
	const a = "/lib/obj/a.c"
	for at, want := range map[string]string{
		// Variables are read and written; the declaration is a write.
		"cou|nt = base_var": `6:5 "count" decl write
16:5 "count" write`,
		"base_var + |n": `10:9 "n" decl write
16:24 "n" read
28:5 "n" read`,
		"int cou|nt, int other": `38:18 "count" decl write
38:45 "count" read`,
		"count = base_v|ar": `16:13 "base_var" read
38:53 "base_var" read
39:50 "base_var" write`,
		// Anything else is text, the declarations among the places.
		"call_out(\"lat|er": `7:6 "later" decl
18:9 "later"
23:15 "later"
37:6 "later" decl`,
		"si|de::setup": `4:9 "side" decl
12:5 "side"`,
		"SQU|ARE(2)": `16:28 "SQUARE"
47:19 "SQUARE"`,
		"size|of":         "nothing",
		"cou|nt = 2":      "nothing",
		"vo|id later() {": "nothing",
	} {
		assert.Equal(t, want, w.highlights(a, at), "at %q", at)
	}
}

// What every file's At says, at every offset, is what References finds:
// for every place At names something declared at, the references of it
// are the places of every file of the lib where At names one of the same
// declarations (and the name of a macro in an inherit statement), each
// once, the declarations themselves added.
func TestReferencesAreAt(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, wholeLib(), "/lib")
	require.True(t, w.l.Scan(ctx))
	var files []string
	for p := range w.fs {
		if ext := path.Ext(p); ext == ".c" || ext == ".h" {
			files = append(files, p)
		}
	}
	slices.Sort(files)
	all := bruteForce(t, w.l, files)
	asked := 0
	for _, file := range files {
		doc, err := w.l.Lint(ctx, file, lint.Options{})
		require.NoError(t, err)
		for off := 0; off <= len(doc.File.Src); off++ {
			ref := doc.Resolve(uint32(off))
			if ref == nil || len(ref.Defs) == 0 {
				assert.Nil(t, doc.References(ctx, uint32(off), true), "%s:%d", file, off)
				continue
			}
			got := doc.References(ctx, uint32(off), true)
			require.NotNil(t, got, "%s:%d", file, off)
			want := all.of(ref, file)
			assert.Equal(t, want, placesOf(got.Sites), "%s:%d %s %s", file, off, ref.Kind, ref.Name)
			// Highlights are the places of the file.
			var here []string
			for _, p := range want {
				if strings.HasPrefix(p, file+":") {
					here = append(here, p)
				}
			}
			assert.Equal(t, here, placesOf(doc.Highlights(uint32(off))), "%s:%d", file, off)
			asked++
		}
	}
	assert.Greater(t, asked, 500)
	t.Logf("%d offsets that name something declared, %d places in %d files", asked, len(all.at), len(files))
}

// places are what At says at every offset of every file of a lib.
type places struct {
	at []place
}

type place struct {
	path       string
	start, end uint32
	decl       bool
	kind       resolve.Kind
	defs       []resolve.Def
}

// bruteForce asks At of every offset of every one of files, and of the
// name of a macro in an inherit statement what macro it is.
func bruteForce(t *testing.T, l *lint.Linter, files []string) *places {
	ctx := context.Background()
	all := &places{}
	for _, file := range files {
		doc, err := l.Lint(ctx, file, lint.Options{})
		require.NoError(t, err)
		seen := map[[3]uint32]bool{}
		add := func(ref *resolve.Ref) {
			if len(ref.Defs) == 0 {
				return
			}
			k := [3]uint32{ref.Start, ref.End, uint32(ref.Kind)}
			if seen[k] {
				return
			}
			seen[k] = true
			all.at = append(all.at, place{file, ref.Start, ref.End, ref.Decl, ref.Kind, ref.Defs})
		}
		for off := 0; off <= len(doc.File.Src); off++ {
			ref := doc.Resolve(uint32(off))
			if ref == nil {
				continue
			}
			add(ref)
			if tok := doc.File.Tokens[doc.File.TokenAt(uint32(off))]; ref.Kind == resolve.Object && tok.Kind == token.Ident {
				if m := doc.Env.Definition(string(doc.File.Text(tok))); m != nil {
					add(&resolve.Ref{Kind: resolve.Macro, Name: m.Name, Start: tok.Off, End: tok.End,
						Defs: []resolve.Def{{Kind: resolve.Macro, Path: m.Path, Start: m.NameOff}}})
				}
			}
		}
	}
	return all
}

// of returns the places that name one of ref's declarations: of the file
// only for a local, a parameter and a label; and the declarations.
func (all *places) of(ref *resolve.Ref, file string) []string {
	type key struct {
		kind  resolve.Kind
		path  string
		start uint32
	}
	want := map[key]bool{}
	for _, d := range ref.Defs {
		want[key{d.Kind, d.Path, d.Start}] = true
	}
	set := map[string]bool{}
	for _, p := range all.at {
		if p.kind != ref.Kind {
			continue
		}
		if (ref.Kind == resolve.Local || ref.Kind == resolve.Parameter || ref.Kind == resolve.Label) && p.path != file {
			continue
		}
		for _, d := range p.defs {
			if want[key{d.Kind, d.Path, d.Start}] {
				set[fmt.Sprintf("%s:%d-%d", p.path, p.start, p.end)] = true
			}
		}
	}
	for _, d := range ref.Defs {
		if d.Kind != resolve.Object && d.Kind != resolve.Include {
			set[fmt.Sprintf("%s:%d-%d", d.Path, d.Start, d.End)] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func placesOf(sites []resolve.Site) []string {
	out := make([]string, 0, len(sites))
	for _, s := range sites {
		out = append(out, fmt.Sprintf("%s:%d-%d", s.Path, s.Start, s.End))
	}
	sort.Strings(out)
	return out
}

// A wizard's checkout with a profile: what the checkout names is found,
// a declaration in a stub among the places, and the answer says it is
// the checkout's only.
func TestReferencesPartialCheckout(t *testing.T) {
	p := &profile.Profile{
		Manifest: profile.Manifest{
			Name: "mini", Dialect: profile.Dialect{Driver: "dgd"},
			IncludeDirs: []string{"/include"}, AutoObjects: []string{"/sys/auto"},
			Covered: []string{"/std/", "/include/", "/sys/"},
		},
		Files: map[string]string{
			"/include/std.h": "#define ROOM \"/std/room\"\n",
			"/sys/auto.c":    "void autof();\n",
			"/std/room.c":    "inherit \"/std/object\";\nint room_var;\nvoid set_short(string s);\n",
			"/std/object.c":  "void create();\n",
		},
	}
	data, err := p.Marshal()
	require.NoError(t, err)
	fs := vfs.Mem{
		"/home/ann/mini.profile": string(data),
		"/home/ann/dgdtools.yml": "profile: mini.profile\nlib_path: /players/ann\n",
		"/home/ann/hall.c":       "#include <std.h>\ninherit ROOM;\nvoid create() { set_short(\"x\"); room_var = 1; ob->set_short(\"z\"); }\n",
		"/home/ann/yard.c":       "inherit \"/std/room\";\nvoid create() { set_short(\"y\"); \"/std/room\"->set_short(\"w\"); }\n",
	}
	w := newWorld(t, fs, "/home/ann")
	require.True(t, w.l.Scan(context.Background()))
	const stubs = "dgdtools-profile://mini"
	assert.Equal(t, `function set_short complete partial possible 1 in 1
/home/ann/hall.c:3:17 "set_short"
/home/ann/yard.c:2:17 "set_short"
/home/ann/yard.c:2:46 "set_short"
`+stubs+`/std/room.c:3:6 "set_short" decl`, w.refs("/home/ann/hall.c", "set_sh|ort(\"x", true))
	assert.Equal(t, `variable room_var complete partial
/home/ann/hall.c:3:33 "room_var"`, w.refs("/home/ann/hall.c", "room_v|ar", false))
	assert.Equal(t, `object /std/room complete partial
/home/ann/hall.c:2:9 "ROOM"
/home/ann/yard.c:1:9 "\"/std/room\""`, w.refs("/home/ann/yard.c", "inherit \"/st|d", true))
}

// References follow the files as they change: a unit built again has
// the places of its new text, and the others keep theirs.
func TestReferencesAfterChange(t *testing.T) {
	ctx := context.Background()
	fs := wholeLib()
	w := newWorld(t, fs, "/lib")
	require.True(t, w.l.Scan(ctx))
	const base = "/lib/std/base.c"
	before := w.refs(base, "int fo|o(", false)
	fs["/lib/obj/b.c"] = "inherit \"/std/mid\";\nprivate void mine() {}\nvoid create() { foo(7); }\n"
	w.l.Invalidate("/lib/obj/b.c")
	require.True(t, w.l.Scan(ctx))
	assert.Equal(t, before+"\n/lib/obj/b.c:3:17 \"foo\"", w.refs(base, "int fo|o(", false))
	// A header that changes changes what the files that include it
	// name: SQUARE is no longer a macro of std.h.
	fs["/lib/include/std.h"] = strings.Replace(fs["/lib/include/std.h"], "SQUARE", "CUBE", 1)
	w.l.Invalidate("/lib/include/std.h")
	require.True(t, w.l.Scan(ctx))
	assert.Equal(t, "nothing", w.refs("/lib/obj/a.c", "SQU|ARE(2)", true))
}

// A call whose function's name is made when it runs is a Use of its own
// kind, under a name no search for a name finds, with the constant parts
// of the name as its pattern.
func TestDynamicUses(t *testing.T) {
	fs := vfs.Mem{
		"/lib/dgdtools.yml": "root: .\ndriver_config: mud.dgd\n",
		"/lib/mud.dgd":      "include_dirs = ({ \"/include\" });\n",
		"/lib/obj/a.c":      "void do_look() {}\nvoid f(object ob, string v) {\n    call_out(\"do_\" + v, 1);\n    call_other(ob, v);\n    call_other(ob, \"do_look\");\n}\n",
	}
	w := newWorld(t, fs, "/lib")
	require.True(t, w.l.Scan(context.Background()))
	doc, err := w.l.Lint(context.Background(), "/lib/obj/a.c", lint.Options{})
	require.NoError(t, err)
	var got []string
	for _, u := range doc.Unit.Uses(index.DynamicName) {
		require.Equal(t, index.UseDynamic, u.Kind)
		got = append(got, fmt.Sprintf("%d:%d %s %s", u.Line, u.Col, doc.File.Src[u.Start:u.End], u.Aux))
	}
	assert.Equal(t, []string{`3:14 "do_" + v do_*`, `4:20 v *`}, got)
	assert.Empty(t, doc.Unit.Uses("v"), "a parameter is no Use")
	assert.Len(t, doc.Unit.Uses("do_look"), 2)
}
