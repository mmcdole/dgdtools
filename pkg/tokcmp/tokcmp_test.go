package tokcmp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/lexer"
	"github.com/mmcdole/dgdtools/pkg/token"
)

func lex(src string) *token.File {
	return lexer.Lex("t.c", []byte(src), lexer.Default)
}

const sample = `#include "/d/Area/area.h"

inherit AREA_I_ROOM;

static void
create()
{
    ::create();
    set_short("a dusty road");
    if (random(2) == 1) {
        set_long("The road stretches east. " +
            "Dust hangs in the air.");
    }
}
`

func TestEqualUnderReformat(t *testing.T) {
	// Same tokens, radically different layout: must compare equal.
	mangled := strings.ReplaceAll(sample, "\n", " ")
	mangled = strings.ReplaceAll(mangled, "    ", "\t")
	// Keep the directive on its own line or it swallows the file.
	mangled = strings.Replace(mangled, `#include "/d/Area/area.h" `, "#include \"/d/Area/area.h\"\n", 1)
	eq, div := Compare(lex(sample), lex(mangled))
	require.True(t, eq, "divergence: %+v", div)

	// Comment changes are formatting-level too.
	commented := strings.Replace(sample, "::create();", "::create(); /* chain */", 1)
	eq, _ = Compare(lex(sample), lex(commented))
	assert.True(t, eq)
}

func TestSelfCompare(t *testing.T) {
	eq, _ := Compare(lex(sample), lex(sample))
	assert.True(t, eq)
}

func TestDirectiveTrailingBlanks(t *testing.T) {
	a := lex("#define X 1 \t\nint y;\n")
	b := lex("#define X 1\nint y;\n")
	eq, _ := Compare(a, b)
	assert.True(t, eq, "trailing blanks on a directive are formatting")

	// Interior directive changes are NOT formatting.
	c := lex("#define X  1\nint y;\n")
	eq, _ = Compare(b, c)
	assert.False(t, eq, "interior directive whitespace is significant (strings!)")
}

// TestMutations seeds single-token damage and asserts detection + location.
func TestMutations(t *testing.T) {
	base := lex(sample)

	mutations := []struct {
		name string
		src  string
	}{
		{"deleted token", strings.Replace(sample, "::create();", "::create()", 1)},
		{"duplicated token", strings.Replace(sample, "::create();", "::create();;", 1)},
		{"altered ident", strings.Replace(sample, "set_short", "set_shorts", 1)},
		{"altered int", strings.Replace(sample, "random(2)", "random(20)", 1)},
		{"altered string", strings.Replace(sample, "a dusty road", "a dusty r0ad", 1)},
		{"swapped operator", strings.Replace(sample, "== 1", "!= 1", 1)},
		{"string merge", strings.Replace(sample, `east. " +`, `east. "`, 1)},
		{"directive body", strings.Replace(sample, "/d/Area/area.h", "/d/Area/area2.h", 1)},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			require.NotEqual(t, sample, m.src, "mutation must change the source")
			eq, div := Compare(base, lex(m.src))
			require.False(t, eq, "mutation must be detected")
			require.NotNil(t, div)
			assert.True(t, div.APos.Line > 0 && div.BPos.Line > 0, "divergence must be located")
		})
	}
}

func TestStreamEndsEarly(t *testing.T) {
	a := lex("int x; int y;")
	b := lex("int x;")
	eq, div := Compare(a, b)
	require.False(t, eq)
	assert.True(t, div.BMissing)

	eq, div = Compare(b, a)
	require.False(t, eq)
	assert.True(t, div.AMissing)
}

func TestContext(t *testing.T) {
	f := lex("int x; string y; mapping z;")
	// find index of "y"
	for i, tok := range f.Significant() {
		if string(f.Text(tok)) == "y" {
			before, after := Context(f, i, 2)
			assert.Equal(t, []string{"x", ";", "string"}[1:], before[len(before)-2:])
			assert.Equal(t, []string{";", "mapping"}, after)
		}
	}
}
