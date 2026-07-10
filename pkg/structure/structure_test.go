package structure

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/lexer"
	"github.com/mmcdole/dgdtools/pkg/token"
)

func analyze(t *testing.T, src string) *FileStructure {
	t.Helper()
	f := lexer.Lex("t.c", []byte(src), lexer.Default)
	require.NoError(t, f.CheckRoundTrip())
	return Analyze(f, DefaultConfig())
}

func name(fs *FileStructure, it *Item) string {
	if it.NameIdx < 0 {
		return ""
	}
	return string(fs.File.Text(fs.File.Tokens[it.NameIdx]))
}

func TestBasicFile(t *testing.T) {
	fs := analyze(t, `#include "/d/Area/area.h"

inherit AREA_I_ROOM;

private int _count;
private object *things, backup;

static void
create()
{
    ::create();
}

public nomask int
query_count()
{
    return _count;
}

void set_count(int n);
`)
	kinds := []ItemKind{}
	for _, it := range fs.Items {
		kinds = append(kinds, it.Kind)
	}
	assert.Equal(t, []ItemKind{Directive, Inherit, VarDecl, VarDecl, FuncDef, FuncDef, Prototype}, kinds)

	// VarDecl with two names.
	assert.Len(t, fs.Items[3].VarIdxs, 2)
	// public counts as a specifier macro.
	f2 := fs.Items[5]
	assert.Equal(t, "query_count", name(fs, &f2))
	assert.Contains(t, f2.SpecMacros(fs.File), "public")
	assert.True(t, f2.Has(fs.File, token.KwNomask))
	// Prototype recognized.
	assert.Equal(t, "set_count", name(fs, &fs.Items[6]))
}

func TestKnRTypeless(t *testing.T) {
	fs := analyze(t, "add_tmp_penalty(int pen) {\n    return pen;\n}\n")
	require.Len(t, fs.Items, 1)
	it := fs.Items[0]
	assert.Equal(t, FuncDef, it.Kind)
	assert.Empty(t, it.TypeIdxs)
	assert.Equal(t, "add_tmp_penalty", name(fs, &it))
}

func TestInheritForms(t *testing.T) {
	fs := analyze(t, "inherit \"/std/room\";\ninherit base \"/std/object\";\nprivate inherit EMP_I_DAEMON;\ninherit EMP_DIR + \"std/monster\";\n")
	require.Len(t, fs.Items, 4)
	assert.Equal(t, -1, fs.Items[0].LabelIdx)
	assert.NotEqual(t, -1, fs.Items[1].LabelIdx)
	assert.Equal(t, -1, fs.Items[2].LabelIdx, "macro path is not a label")
	assert.True(t, fs.Items[2].Has(fs.File, token.KwPrivate))
	assert.Equal(t, -1, fs.Items[3].LabelIdx, "macro+concat path is not a label")
}

func TestTypedObjectAndArrays(t *testing.T) {
	fs := analyze(t, "static object \"/std/room\" *\nfind_rooms(string area)\n{\n    return 0;\n}\n")
	require.Len(t, fs.Items, 1)
	it := fs.Items[0]
	assert.Equal(t, FuncDef, it.Kind)
	assert.Len(t, it.TypeIdxs, 3) // object, "/std/room", *
}

func TestOperatorOverload(t *testing.T) {
	fs := analyze(t, "static int\noperator+ (int x)\n{\n    return x;\n}\n")
	require.Len(t, fs.Items, 1)
	assert.Equal(t, FuncDef, fs.Items[0].Kind)
	assert.Equal(t, token.KwOperator, fs.File.Tokens[fs.Items[0].NameIdx].Kind)
}

func TestUnrecognizedRecovery(t *testing.T) {
	// A pre-DGD 'status' declaration is not recognized, but the scan
	// recovers and still sees the following function.
	fs := analyze(t, "status flag;\n\nvoid f()\n{\n}\n")
	require.Len(t, fs.Items, 2)
	assert.Equal(t, Unrecognized, fs.Items[0].Kind)
	assert.Equal(t, FuncDef, fs.Items[1].Kind)
}

func TestHeaderComments(t *testing.T) {
	fs := analyze(t, "static void /* ctor */ create()\n{\n}\n")
	require.Len(t, fs.Items, 1)
	assert.True(t, fs.Items[0].HeaderComments)

	fs = analyze(t, "static void\ncreate()\n{\n    /* body comments do not count */\n}\n")
	require.Len(t, fs.Items, 1)
	assert.False(t, fs.Items[0].HeaderComments)
}

func TestConditionalBranchBrackets(t *testing.T) {
	// Brackets split across #ifdef/#else branches are balanced within one
	// branch only; the scanner takes the first branch, so both this
	// definition and the one after it must be recognized.
	fs := analyze(t, `void later();

void
cross()
{
    int i;
#ifdef __DGD__
    if (sizeof(files[0]) &&
#else
    if (sizeof(files) &&
#endif
        i > 0) {
        i = 1;
    }
}

void
later()
{
}
`)
	var defs []string
	fs.Funcs(func(it *Item) bool {
		if it.Kind == FuncDef {
			defs = append(defs, name(fs, it))
		}
		return true
	})
	assert.Equal(t, []string{"cross", "later"}, defs)
}

func TestNoPanicOnGarbage(t *testing.T) {
	for _, src := range []string{
		"", ";;;", "int", "void f(", "}}}}", "int x = ({ ([",
		"inherit", "static private", "operator",
	} {
		fs := analyze(t, src)
		_ = fs
	}
}
