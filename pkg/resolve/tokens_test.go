package resolve_test

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/resolve"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// tokens writes the semantic tokens of the file a line each:
// "LINE:COL text type [modifiers]".
func (w *world) tokens(file string) string {
	w.t.Helper()
	doc, err := w.l.Lint(context.Background(), file, lint.Options{})
	require.NoError(w.t, err)
	var lines []string
	for _, tk := range doc.Tokens() {
		src := doc.File.Src
		line := 1 + bytes.Count(src[:tk.Start], []byte("\n"))
		col := int(tk.Start) - bytes.LastIndexByte(src[:tk.Start], '\n')
		s := fmt.Sprintf("%d:%d %s %s", line, col, src[tk.Start:tk.End], tk.Type)
		if m := tk.Mods.Names(); len(m) > 0 {
			s += " " + strings.Join(m, ",")
		}
		lines = append(lines, s)
	}
	return strings.Join(lines, "\n")
}

func TestTokens(t *testing.T) {
	w := newWorld(t, wholeLib(), "/lib")
	want := `3:9 STD macro
4:9 side namespace declaration
6:5 count variable declaration
7:6 later function declaration
9:6 create function definition
10:9 n variable declaration,local
11:7 create function inherited
12:5 side namespace
12:11 setup function inherited
13:5 foo function inherited
14:5 autof function defaultLibrary
15:5 sizeof function defaultLibrary
16:5 count variable
16:13 base_var variable inherited
16:24 n variable local
16:28 SQUARE macro
16:40 header_var variable
17:5 this_object function defaultLibrary
17:20 foo method inherited
18:5 ME macro
18:9 later method
19:15 bar method
20:5 ROOM_D macro
20:13 register method
20:22 this_object function defaultLibrary
21:5 call_other function defaultLibrary
22:5 call_other function defaultLibrary
23:5 call_out function defaultLibrary
26:5 clone_object function defaultLibrary
28:5 n variable local
28:8 unknown method
29:5 from_header function
29:17 LOCAL macro
29:24 AUTO macro
30:15 mine method
31:15 autof method defaultLibrary
32:18 autof method
33:15 pfn method
34:5 write function static,defaultLibrary
35:5 query_name function inherited
37:6 later function definition
38:1 status type macro
38:8 typed function definition
38:18 count parameter declaration
38:29 other parameter declaration
38:45 count parameter
38:53 base_var variable inherited
39:6 broken function definition
39:50 base_var variable inherited
44:7 AUTO macro
46:8 AUTO macro
47:9 TWICE macro declaration
47:19 SQUARE macro
48:8 TWICE macro`
	// Not there: the #includes and the path of an inherit statement
	// (strings), the registrars and load (no function the tools know of),
	// secret and pfn() from later() (private where they are), what is in
	// #if 0, the local count after a statement that does not parse, and
	// the words of a #define that are no macros.
	assert.Equal(t, want, w.tokens("/lib/obj/a.c"))
}

func TestTokensOfEachKind(t *testing.T) {
	fs := vfs.Mem{
		"/lib/dgdtools.yml": "root: .\ndriver_config: mud.dgd\ndialect: {closures: true}\nconventions: {macros: {public: specifier}}\n",
		"/lib/mud.dgd":      "auto_object = \"/sys/auto\";\n",
		"/lib/sys/auto.c":   "void autof() {}\n",
		"/lib/std/base.c":   "static int hidden;\nint shown;\nstatic void sfn() {}\n",
		"/lib/obj/a.c": `inherit "/std/base";
#define FOREVER for (;;)
#define NUMBER int
#define PUBLIC static
public void f() {
    function g;
    NUMBER x;
    g = &f();
    hidden = shown;
    sfn();
    FOREVER break;
}
PUBLIC int h(int a) {
    try { a = 1; } catch (err) { a = strlen(err); }
    return a;
}
private int p();
`,
	}
	w := newWorld(t, fs, "/lib")
	want := `2:9 FOREVER macro declaration
3:9 NUMBER macro declaration
4:9 PUBLIC macro declaration
5:1 public modifier macro
5:13 f function definition
6:5 function keyword
6:14 g variable declaration,local
7:5 NUMBER type macro
7:12 x variable declaration,local
8:5 g variable local
8:10 f function
9:5 hidden variable static,inherited
9:14 shown variable inherited
10:5 sfn function static,inherited
11:5 FOREVER macro
13:1 PUBLIC modifier macro
13:12 h function definition
13:18 a parameter declaration
14:11 a parameter
14:27 err variable declaration,local
14:34 a parameter
14:38 strlen function defaultLibrary
14:45 err variable local
15:12 a parameter
17:13 p function declaration`
	assert.Equal(t, want, w.tokens("/lib/obj/a.c"))
	// Without closures `function` is a name like any other.
	fs["/lib/dgdtools.yml"] = "driver_config: mud.dgd\n"
	fs["/lib/obj/a.c"] = "void f() { int function; function = 1; }\n"
	w = newWorld(t, fs, "/lib")
	assert.Equal(t, "1:6 f function definition\n1:16 function variable declaration,local\n1:26 function variable local", w.tokens("/lib/obj/a.c"))
}

// TestTokensAreAt holds the tokens of every file of the libs of the
// tests to At: at every offset where At names something, the token there
// is what At says it is, or there is none where At knows nothing of a
// declaration; and every token is At's.
func TestTokensAreAt(t *testing.T) {
	ctx := context.Background()
	libs := []vfs.Mem{wholeLib(), completeLib()}
	n := 0
	for _, fs := range libs {
		w := newWorld(t, fs, "/lib")
		require.True(t, w.l.Scan(ctx))
		var files []string
		for p := range fs {
			if ext := path.Ext(p); ext == ".c" || ext == ".h" {
				files = append(files, p)
			}
		}
		slices.Sort(files)
		for _, file := range files {
			doc, err := w.l.Lint(ctx, file, lint.Options{})
			require.NoError(t, err)
			n += tokensAreAt(t, doc)
		}
	}
	assert.Greater(t, n, 100)
}

// tokensAreAt holds the tokens of a document to At at every offset
// (TestTokensAreAt) and returns how many tokens it held so.
func tokensAreAt(t assert.TestingT, doc *lint.Document) int {
	file, src := doc.Path, doc.File.Src
	n := 0
	tokens := map[uint32]resolve.SemanticToken{}
	end := uint32(0)
	for _, tk := range doc.Tokens() {
		assert.GreaterOrEqual(t, tk.Start, end, "%s:%d: tokens in order, none overlapping", file, tk.Start)
		assert.NotContains(t, string(src[tk.Start:tk.End]), "\n")
		tokens[tk.Start], end = tk, tk.End
	}
	seen := map[uint32]bool{}
	for off := uint32(0); off < uint32(len(src)); off++ {
		ref := doc.Resolve(off)
		if ref == nil || ref.Start != off || seen[off] {
			continue
		}
		seen[off] = true
		tk, has := tokens[off]
		where := fmt.Sprintf("%s:%d %s %s", file, off, ref.Kind, ref.Name)
		typ, ok := wantToken(doc, ref)
		switch {
		case ref.Kind == resolve.Object || ref.Kind == resolve.Include:
			// Only the macro of an inherit statement.
			if has {
				assert.Equal(t, resolve.TokenMacro, tk.Type, where)
				assert.NotNil(t, doc.Env.Lookup(string(src[tk.Start:tk.End])), where)
				delete(tokens, off)
			}
			continue
		case !ok:
			assert.False(t, has, "%s: no token, got %v", where, tk)
			continue
		}
		if assert.True(t, has, "%s: a token", where) {
			assert.Equal(t, ref.End, tk.End, where)
			if typ == resolve.TokenMacro {
				assert.Contains(t, []resolve.TokenType{resolve.TokenMacro, resolve.TokenTypeName, resolve.TokenModifier}, tk.Type, where)
			} else {
				assert.Equal(t, typ, tk.Type, where)
			}
			assert.Equal(t, ref.Decl, tk.Mods&(resolve.ModDeclaration|resolve.ModDefinition) != 0, where)
			delete(tokens, off)
			n++
		}
	}
	for _, tk := range tokens {
		text := string(src[tk.Start:tk.End])
		if ref := doc.Resolve(tk.Start); ref != nil && ref.Kind == resolve.Object {
			// A macro inside the path of an inherit statement, which is
			// At's from where the path begins: inherit(ROOM).
			assert.Equal(t, resolve.TokenMacro, tk.Type, "%s:%d", file, tk.Start)
			assert.NotNil(t, doc.Env.Lookup(text), "%s:%d", file, tk.Start)
			continue
		}
		assert.Equal(t, "function", text, "%s:%d: a token where At says nothing", file, tk.Start)
	}
	return n
}

// wantToken is the type of the token of a place At names, and false
// where there is none.
func wantToken(doc *lint.Document, ref *resolve.Ref) (resolve.TokenType, bool) {
	switch ref.Kind {
	case resolve.Macro:
		return resolve.TokenMacro, true
	case resolve.Label:
		return resolve.TokenNamespace, true
	case resolve.Local:
		return resolve.TokenVariable, true
	case resolve.Parameter:
		return resolve.TokenParameter, true
	case resolve.Variable:
		return resolve.TokenVariable, len(ref.Defs) > 0
	case resolve.Function:
		switch {
		case ref.Call != nil:
			return resolve.TokenMethod, ref.Start < ref.End && doc.File.Src[ref.Start-1] != '"'
		case ref.Decl || len(ref.Defs) > 0:
			return resolve.TokenFunction, true
		}
		h := doc.Hover(ref.Start)
		return resolve.TokenFunction, h != nil && len(h.Infos) == 1 && h.Infos[0].Origin.Kfun
	}
	return 0, false
}
