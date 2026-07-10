package format

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/lexer"
)

func fmtSrc(t *testing.T, src string, o Options) string {
	t.Helper()
	f := lexer.Lex("t.c", []byte(src), lexer.Default)
	require.NoError(t, f.CheckRoundTrip())
	out, err := Format(f, o)
	require.NoError(t, err)
	return string(out)
}

func TestKNFHeaders(t *testing.T) {
	// One-line definition explodes to KNF: specs+type line, name at col 0,
	// brace on its own line.
	in := "static void create() {\n    ::create();\n}\n"
	want := "static void\ncreate()\n{\n    ::create();\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))

	// Already-KNF input is a fixed point.
	assert.Equal(t, want, fmtSrc(t, want, Options{}))

	// public macro is part of the header.
	in = "public nomask int query_count() {\n    return 1;\n}\n"
	want = "public nomask int\nquery_count()\n{\n    return 1;\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))

	// K&R typeless: no header line, brace still gets its own line.
	in = "add_penalty(int pen) {\n    return pen;\n}\n"
	want = "add_penalty(int pen)\n{\n    return pen;\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))

	// Prototypes collapse to one line.
	in = "static void\nset_count(int n);\n"
	want = "static void set_count(int n);\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))

	// Headers containing comments are left in their original layout.
	in = "static void /* ctor */ create() {\n    ::create();\n}\n"
	out := fmtSrc(t, in, Options{})
	assert.Contains(t, out, "static void /* ctor */ create() {")
}

func TestControlFlowCuddling(t *testing.T) {
	in := "void\nf()\n{\n    if (x)\n    {\n        y = 1;\n    }\n    else\n    {\n        y = 2;\n    }\n}\n"
	want := "void\nf()\n{\n    if (x) {\n        y = 1;\n    } else {\n        y = 2;\n    }\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestSwitchFrames(t *testing.T) {
	in := "void\nf()\n{\nswitch (x) {\ncase 0:  return;\ncase 1..5:\nbreak;\ndefault:\nbreak;\n}\n}\n"
	want := "void\nf()\n{\n    switch (x) {\n    case 0: return;\n    case 1..5:\n        break;\n    default:\n        break;\n    }\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestSpacingNormalization(t *testing.T) {
	cases := map[string]string{
		"int x;x=a+b*c;":              "int x; x = a + b * c;",
		"f( a,b );":                   "f(a, b);",
		"if(x&&!y){z( );}":            "if (x && !y) { z(); }",
		"x=({\"a\",\"b\"});":          "x = ({ \"a\", \"b\" });",
		"m=([\"k\":1,\"l\":2]);":      "m = ([ \"k\": 1, \"l\": 2 ]);",
		"y=a?b:c;":                    "y = a ? b : c;",
		"i++;--j;x=-1;":               "i++; --j; x = -1;",
		"obj->fn(::query(),args...);": "obj->fn(::query(), args...);",
		"if(o<-\"/std/room\")r=1;":    "if (o <- \"/std/room\") r = 1;",
		"for(i=0;i<n;i++)go();":       "for (i = 0; i < n; i++) go();",
		"a=b[1..2];":                  "a = b[1..2];",
	}
	for in, want := range cases {
		assert.Equal(t, want+"\n", fmtSrc(t, in, Options{}), "input: %s", in)
	}
}

func TestUnaryVsBinary(t *testing.T) {
	cases := map[string]string{
		"x=a- -b;":     "x = a - -b;",
		"int *arr;":    "int *arr;",
		"x=a*b;":       "x = a * b;",
		"f(&callback);": "f(&callback);",
		"y=!x;":        "y = !x;",
		"z=~mask;":     "z = ~mask;",
	}
	for in, want := range cases {
		assert.Equal(t, want+"\n", fmtSrc(t, in, Options{}), "input: %s", in)
	}
}

func TestIndentation(t *testing.T) {
	in := "static void\ncreate()\n{\n::create();\n  set_short(\"road\");\n\tif (x) {\n\ty = 1 +\n\t2;\n}\n}\n"
	want := "static void\ncreate()\n{\n    ::create();\n    set_short(\"road\");\n    if (x) {\n        y = 1 +\n            2;\n    }\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestArrayLiteralIndent(t *testing.T) {
	in := "void\nf()\n{\nx = ({\n\"a\",\n\"b\",\n});\n}\n"
	want := "void\nf()\n{\n    x = ({\n        \"a\",\n        \"b\",\n    });\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestContinuationInsideParens(t *testing.T) {
	in := "void\nf()\n{\nset_long(\"aaa\" +\n\"bbb\" +\n\"ccc\");\n}\n"
	want := "void\nf()\n{\n    set_long(\"aaa\" +\n        \"bbb\" +\n        \"ccc\");\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestTrailingWhitespaceAndFinalNewline(t *testing.T) {
	in := "int x;   \nint y;\t"
	want := "int x;\nint y;\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
	// Trailing blank lines are dropped.
	assert.Equal(t, "int x;\n", fmtSrc(t, "int x;\n\n\n\n", Options{}))
}

func TestDirectivesAtColumnZero(t *testing.T) {
	in := "  #include <std.h>\n   #define X 1\nint y;\n"
	want := "#include <std.h>\n#define X 1\nint y;\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestTrailingCommentAlignmentPreserved(t *testing.T) {
	in := "int x;      /* aligned */\n"
	assert.Equal(t, in, fmtSrc(t, in, Options{}))
}

func TestBlankLineCap(t *testing.T) {
	in := "int x;\n\n\n\n\nint y;\n"
	want := "int x;\n\n\nint y;\n" // capped at 2 blank lines
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
	want = "int x;\n\nint y;\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{MaxBlankLines: 1}))
}

func TestBlockCommentShift(t *testing.T) {
	in := "void\nf()\n{\n        /* one\n           two */\nx = 1;\n}\n"
	want := "void\nf()\n{\n    /* one\n       two */\n    x = 1;\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestLineEndingPolicies(t *testing.T) {
	in := "int x;\r\nint y;\r\n"
	assert.Equal(t, in, fmtSrc(t, in, Options{}), "preserve keeps CRLF")
	assert.Equal(t, "int x;\nint y;\n", fmtSrc(t, in, Options{LineEndings: LF}))
	assert.Equal(t, "int x;\r\nint y;\r\n",
		fmtSrc(t, "int x;\nint y;\n", Options{LineEndings: CRLF}))
}

func TestIndentWidthToggle(t *testing.T) {
	in := "void\nf()\n{\nx = 1;\n}\n"
	assert.Equal(t, "void\nf()\n{\n  x = 1;\n}\n", fmtSrc(t, in, Options{Indent: 2}))
}

func TestRefusesIllegal(t *testing.T) {
	f := lexer.Lex("t.c", []byte("\"unterminated\nint x;\n"), lexer.Default)
	_, err := Format(f, Options{})
	assert.ErrorIs(t, err, ErrIllegal)
}

func TestIdempotence(t *testing.T) {
	srcs := []string{
		"static void\ncreate() {\n::create();\nif (a &&\nb) {\nreturn;\n}\n}\n",
		"void f()\n{\nswitch (x) {\ncase 0: break;\n}\n}\n",
		"x=({1,2});m=([1:2]);\n",
	}
	for _, in := range srcs {
		once := fmtSrc(t, in, Options{})
		assert.Equal(t, once, fmtSrc(t, once, Options{}), "input: %q", in)
	}
}

func TestMultilineStringUntouched(t *testing.T) {
	in := "void\nf()\n{\nx = \"line one\\\nline two\";\n}\n"
	out := fmtSrc(t, in, Options{})
	assert.Contains(t, out, "\"line one\\\nline two\"")
}

func TestCommentOnlyGapsPreserved(t *testing.T) {
	in := "int x; /* trailing */\n/* own line */\nint y;\n"
	out := fmtSrc(t, in, Options{})
	assert.Contains(t, out, "int x; /* trailing */")
	assert.Contains(t, out, "\n/* own line */\nint y;")
}
