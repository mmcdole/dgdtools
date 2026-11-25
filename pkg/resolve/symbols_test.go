package resolve_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/resolve"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// symbols lists the outline of the file at path, a line for each
// declaration:
//
//	KIND NAME (DETAIL) LINE:COL "the name there" [the declaration, its space as one]
func (w *world) symbols(path string) []string {
	w.t.Helper()
	doc, err := w.l.Lint(context.Background(), path, lint.Options{NoScan: true})
	require.NoError(w.t, err)
	src := doc.File.Src
	var out []string
	last := uint32(0)
	for _, s := range doc.Symbols() {
		// In the order of the file, the name inside the declaration.
		assert.GreaterOrEqual(w.t, s.Start, last, s.Name)
		last = s.Start
		assert.True(w.t, s.Start <= s.NameStart && s.NameStart < s.NameEnd && s.NameEnd <= s.End && int(s.End) <= len(src), s.Name)
		pos := doc.File.Pos(s.NameStart)
		out = append(out, fmt.Sprintf("%s %s (%s) %d:%d %q [%s]", s.Kind, s.Name, s.Detail, pos.Line, pos.Col,
			src[s.NameStart:s.NameEnd], strings.Join(strings.Fields(string(src[s.Start:s.End])), " ")))
	}
	return out
}

func TestSymbols(t *testing.T) {
	fs := wholeLib()
	fs["/lib/dgdtools.yml"] += "  macros: { public: specifier }\n"
	fs["/lib/obj/s.c"] = `#include <std.h>
# define LIMIT 3
#define TWICE(a, b) \
    ((a) + (b))
private inherit STD;
inherit side "/std/side";
inherit "/std/" + "mid";
inherit NOWHERE;

private static int count, *list;
public string name;
/* A prototype, and what it is of. */
static varargs int later(int when, string why...);
mapping m; void proto(object o);

void create() {
    int local;
    ::create();
}
static varargs int later(int when, string why...) { return 1; }
status ok() { return 1; }
untyped(a, b) { }
int broken( { }
#if 0
void dead() {}
#define DEAD 1
#endif
#undef LIMIT
#define LIMIT 4
`
	w := newWorld(t, fs, "/lib")
	assert.Equal(t, []string{
		`macro LIMIT (#define) 2:10 "LIMIT" [# define LIMIT 3]`,
		`macro TWICE (#define (a, b)) 3:9 "TWICE" [#define TWICE(a, b) \ ((a) + (b))]`,
		`object /std/base (private inherit) 5:17 "STD" [private inherit STD;]`,
		`object /std/side (inherit side) 6:14 "\"/std/side\"" [inherit side "/std/side";]`,
		`object /std/mid (inherit) 7:9 "\"/std/\" + \"mid\"" [inherit "/std/" + "mid";]`,
		`object NOWHERE (inherit) 8:9 "NOWHERE" [inherit NOWHERE;]`,
		`variable count (private static int) 10:20 "count" [count]`,
		`variable list (private static int *) 10:28 "list" [list]`,
		`variable name (public string) 11:15 "name" [public string name;]`,
		`function later (static varargs int (int when, string why...)) 13:20 "later" [static varargs int later(int when, string why...);]`,
		`variable m (mapping) 14:9 "m" [mapping m;]`,
		`function proto (void (object o)) 14:17 "proto" [void proto(object o);]`,
		`function create (void ()) 16:6 "create" [void create() { int local; ::create(); }]`,
		`function later (static varargs int (int when, string why...)) 20:20 "later" [static varargs int later(int when, string why...) { return 1; }]`,
		`function ok (status ()) 21:8 "ok" [status ok() { return 1; }]`,
		`function untyped ((a, b)) 22:1 "untyped" [untyped(a, b) { }]`,
		`macro LIMIT (#define) 29:9 "LIMIT" [#define LIMIT 4]`,
	}, w.symbols("/lib/obj/s.c"))

	// A header's, and a file with nothing.
	assert.Equal(t, []string{
		`macro LOCAL (#define) 1:9 "LOCAL" [#define LOCAL 1]`,
		`function from_header (int ()) 2:5 "from_header" [int from_header() { return LOCAL; }]`,
		`variable header_var (int) 3:5 "header_var" [int header_var;]`,
	}, w.symbols("/lib/obj/local.h"))
	fs["/lib/obj/empty.c"] = "/* nothing */\n"
	assert.Empty(t, newWorld(t, fs, "/lib").symbols("/lib/obj/empty.c"))
	assert.Nil(t, resolve.New(resolve.File{}).Symbols())
}

// An operator function is named by its operator.
func TestSymbolsOperator(t *testing.T) {
	fs := vfs.Mem{"/lib/dgdtools.yml": "root: .\n", "/lib/obj/v.c": "static object operator+ (object other) { return other; }\nint operator [] (int i) { return i; }\n"}
	assert.Equal(t, []string{
		`function operator+ (static object (object other)) 1:15 "operator+" [static object operator+ (object other) { return other; }]`,
		`function operator[] (int (int i)) 2:5 "operator []" [int operator [] (int i) { return i; }]`,
	}, newWorld(t, fs, "/lib").symbols("/lib/obj/v.c"))
}
