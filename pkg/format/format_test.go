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

func TestIndentation(t *testing.T) {
	in := "static void\ncreate() {\n::create();\n  set_short(\"road\");\n\tif (x) {\n\ty = 1 +\n\t2;\n}\n}\n"
	want := "static void\ncreate() {\n    ::create();\n    set_short(\"road\");\n    if (x) {\n        y = 1 +\n            2;\n    }\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestArrayLiteralIndent(t *testing.T) {
	in := "void f() {\nx = ({\n\"a\",\n\"b\",\n});\n}\n"
	want := "void f() {\n    x = ({\n        \"a\",\n        \"b\",\n    });\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestContinuationInsideParens(t *testing.T) {
	in := "void f() {\nset_long(\"aaa\" +\n\"bbb\" +\n\"ccc\");\n}\n"
	want := "void f() {\n    set_long(\"aaa\" +\n        \"bbb\" +\n        \"ccc\");\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestClosingParenLine(t *testing.T) {
	in := "void f() {\ncall(\na,\nb\n);\n}\n"
	want := "void f() {\n    call(\n        a,\n        b\n    );\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestTrailingWhitespaceAndFinalNewline(t *testing.T) {
	in := "int x;   \nint y;\t"
	want := "int x;\nint y;\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestDirectivesAtColumnZero(t *testing.T) {
	in := "  #include <std.h>\n   #define X 1\nint y;\n"
	want := "#include <std.h>\n#define X 1\nint y;\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestMidLineSpacingPreserved(t *testing.T) {
	// v1 does not normalize horizontal spacing between tokens mid-line.
	in := "int x;  /* note */\n"
	assert.Equal(t, in, fmtSrc(t, in, Options{}))
}

func TestBlankLinesKept(t *testing.T) {
	in := "int x;\n\n\nint y;\n"
	assert.Equal(t, in, fmtSrc(t, in, Options{}))
	// Whitespace-only lines become empty lines.
	in = "int x;\n   \nint y;\n"
	want := "int x;\n\nint y;\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestBlockCommentShift(t *testing.T) {
	in := "void f() {\n        /* one\n           two */\nx = 1;\n}\n"
	// Comment moves from col 8 to col 4; interior shifts by the same delta.
	want := "void f() {\n    /* one\n       two */\n    x = 1;\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestLineEndingPolicies(t *testing.T) {
	in := "int x;\r\nint y;\r\n"
	assert.Equal(t, in, fmtSrc(t, in, Options{}), "preserve keeps CRLF")
	assert.Equal(t, "int x;\nint y;\n", fmtSrc(t, in, Options{LineEndings: LF}))
	assert.Equal(t, "int x;\r\nint y;\r\n",
		fmtSrc(t, "int x;\nint y;\n", Options{LineEndings: CRLF}))
	// Preserve on a mixed file keeps each line's own ending.
	mixed := "int a;\r\nint b;\nint c;\r\n"
	assert.Equal(t, mixed, fmtSrc(t, mixed, Options{}))
}

func TestIndentWidthToggle(t *testing.T) {
	in := "void f() {\nx = 1;\n}\n"
	assert.Equal(t, "void f() {\n  x = 1;\n}\n", fmtSrc(t, in, Options{Indent: 2}))
}

func TestRefusesIllegal(t *testing.T) {
	f := lexer.Lex("t.c", []byte("\"unterminated\nint x;\n"), lexer.Default)
	_, err := Format(f, Options{})
	assert.ErrorIs(t, err, ErrIllegal)
}

func TestIdempotence(t *testing.T) {
	in := "static void\ncreate() {\n::create();\nif (a &&\nb) {\nreturn;\n}\n}\n"
	once := fmtSrc(t, in, Options{})
	twice := fmtSrc(t, once, Options{})
	assert.Equal(t, once, twice)
}

func TestMultilineStringUntouched(t *testing.T) {
	// Backslash-continued strings span lines inside one token; their bytes
	// (including interior newlines) must never change.
	in := "void f() {\nx = \"line one\\\nline two\";\n}\n"
	out := fmtSrc(t, in, Options{})
	assert.Contains(t, out, "\"line one\\\nline two\"")
}
