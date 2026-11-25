package format

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/lexer"
	"github.com/mmcdole/dgdtools/pkg/pp"
	"github.com/mmcdole/dgdtools/pkg/token"
)

func fmtSrc(t *testing.T, src string, o Options) string {
	t.Helper()
	f := lexer.Lex("t.c", []byte(src), lexer.Default)
	require.NoError(t, f.CheckRoundTrip())
	out, err := Format(f, o)
	require.NoError(t, err)
	return string(out)
}

func TestHeadersAsWritten(t *testing.T) {
	// A function's header keeps its lines: the formatter changes the
	// spacing in it, as anywhere, and never where it breaks.
	for _, in := range []string{
		"static void create() {\n    ::create();\n}\n",
		"static void\ncreate()\n{\n    ::create();\n}\n",
		"static void create()\n{\n    ::create();\n}\n",
		"add_penalty(int pen) {\n    return pen;\n}\n",
		"static void\nset_count(int n);\nstring *\nquery_names();\n",
		"SETUP_COMMAND\n\nstatic int main(string str) {\n    return 1;\n}\n",
		"static void /* ctor */ create() {\n    ::create();\n}\n",
	} {
		assert.Equal(t, in, fmtSrc(t, in, Options{}))
	}
	// Spacing is normalized inside it.
	in := "string * f( int x ) {\n    return ({ });\n}\nstatic   int\n  g (  ) ;\n"
	want := "string *f(int x) {\n    return ({ });\n}\nstatic int\ng();\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
	in = "public nomask int   query_count( ){\n    return 1;\n}\n"
	want = "public nomask int query_count() {\n    return 1;\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{Macros: map[string]pp.Class{"public": pp.ClassEmpty}}))
	in = "status *  query_flags(status all)\n{\nreturn flags;\n}\n"
	want = "status *query_flags(status all)\n{\n    return flags;\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestOneLinersPreserved(t *testing.T) {
	// A definition written on one line stays on one line (the accessor
	// idiom); only its spacing is normalized. Like gofmt.
	in := "string query_key_code() { return key_code; }\nvoid   set_key_code(string c) { key_code=c; }\nint open() { return 1; }\n"
	want := "string query_key_code() { return key_code; }\nvoid set_key_code(string c) { key_code = c; }\nint open() { return 1; }\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
	assert.Equal(t, want, fmtSrc(t, want, Options{}))
	// A body that ends on a line of its own keeps that line too.
	in = "string query_key_code() { return key_code;\n}\n"
	assert.Equal(t, in, fmtSrc(t, in, Options{}))
}

func TestBracesStayOnTheirLines(t *testing.T) {
	// A brace or an else keeps the line it is on, Allman, GNU or K&R;
	// only its indentation changes: a block's braces stand at the level
	// of the statement that owns it.
	for _, in := range []string{
		"void\nf()\n{\n    if (x)\n    {\n        y = 1;\n    }\n    else\n    {\n        y = 2;\n    }\n}\n",
		"void\nf()\n{\n    if (x) {\n        y = 1;\n    } else {\n        y = 2;\n    }\n}\n",
		"void\nf()\n{\n    if (x) {\n        y = 1;\n    }\n    else {\n        y = 2;\n    }\n}\n",
		"void\nf()\n{\n    while (x)\n    {\n        x--;\n    }\n    switch (x)\n    {\n    case 1:\n        break;\n    }\n}\n",
		// One-line brace blocks on their own lines.
		"void\nf()\n{\n    if (a)\n    { m1(); }\n    else if (b)\n    { m2(); }\n    else\n    { m3(); }\n}\n",
		"void\nf()\n{\n    if (a) {\n        m1();\n    }\n    else if (b)\n    { m2(); }\n}\n",
	} {
		assert.Equal(t, in, fmtSrc(t, in, Options{}))
	}
	// GNU's half-indented braces come to the statement's level.
	in := "void\nf()\n{\n  if (x)\n    {\n      y = 1;\n    }\n  else\n    {\n      y = 2;\n    }\n}\n"
	want := "void\nf()\n{\n    if (x)\n    {\n        y = 1;\n    }\n    else\n    {\n        y = 2;\n    }\n}\n"
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
		"x=a- -b;":      "x = a - -b;",
		"int *arr;":     "int *arr;",
		"x=a*b;":        "x = a * b;",
		"f(&callback);": "f(&callback);",
		"y=!x;":         "y = !x;",
		"z=~mask;":      "z = ~mask;",
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
	// The same at the start of a file; none at its end.
	assert.Equal(t, "\n\nint x;\n", fmtSrc(t, "\n\n\nint x;\n\n\n", Options{}))
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

func TestIndentStyles(t *testing.T) {
	in := "void\nf()\n{\nif (x)\ny = a +\nb;\n}\n"
	assert.Equal(t, "void\nf()\n{\n  if (x)\n    y = a +\n      b;\n}\n", fmtSrc(t, in, Options{IndentWidth: 2}))
	tabs := Options{IndentStyle: Tabs, IndentWidth: 8}
	want := "void\nf()\n{\n\tif (x)\n\t\ty = a +\n\t\t\tb;\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, tabs))
	assert.Equal(t, want, fmtSrc(t, want, tabs))
	// Either style reads the other.
	assert.Equal(t, "void\nf()\n{\n    if (x)\n        y = a +\n            b;\n}\n", fmtSrc(t, want, Options{}))
}

func TestBlockCommentIndentation(t *testing.T) {
	// The lines of a comment move with its first line, in the
	// indentation that line gets: tabs become spaces and spaces tabs.
	tabbed := "void\nf()\n{\n\tif (x) {\n\t\t/*\n\t\t * one\n\t\t *   two\n\t\t */\n\t\tx = 1;\n\t}\n}\n"
	spaced := "void\nf()\n{\n    if (x) {\n        /*\n         * one\n         *   two\n         */\n        x = 1;\n    }\n}\n"
	assert.Equal(t, spaced, fmtSrc(t, tabbed, Options{}))
	assert.Equal(t, tabbed, fmtSrc(t, spaced, Options{IndentStyle: Tabs}))
	assert.Equal(t, tabbed, fmtSrc(t, tabbed, Options{IndentStyle: Tabs}))
	// A line that is not indented like the first keeps its distance.
	in := "void\nf()\n{\n/* one\ntwo */\nx = 1;\n}\n"
	assert.Equal(t, "void\nf()\n{\n    /* one\n    two */\n    x = 1;\n}\n", fmtSrc(t, in, Options{}))
}

// What is inside a bracket is indented from the line the bracket is
// opened on, and the closer stands at that line: a literal that opens on
// a line continuing a call, and a call inside a call's arguments.
func TestBracketsIndentFromTheirLine(t *testing.T) {
	in := "void f() {\nfoo(name,\n({\n1,\n2,\n}),\nn);\nfoo(a,\nbar(b,\nc),\nd);\n}\n"
	want := "void f() {\n    foo(name,\n        ({\n            1,\n            2,\n        }),\n        n);\n" +
		"    foo(a,\n        bar(b,\n            c),\n        d);\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
	assert.Equal(t, want, fmtSrc(t, want, Options{}))
}

// The ':' of catch's handler is spaced like a conditional's.
func TestCatchHandlerColon(t *testing.T) {
	in := "void f() {\ncatch { i++; }: { j--; }\ncatch {\ni++;\n}:{\nj--;\n}\n}\n"
	want := "void f() {\n    catch { i++; } : { j--; }\n    catch {\n        i++;\n    } : {\n        j--;\n    }\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestRefusesIllegal(t *testing.T) {
	f := lexer.Lex("t.c", []byte("int x;\n  \"unterminated\nint y;\n"), lexer.Default)
	_, err := Format(f, Options{})
	assert.ErrorIs(t, err, ErrIllegal)
	assert.ErrorContains(t, err, "t.c:2:3: ", "where the first illegal token is")
	assert.ErrorContains(t, err, "(unterminated string literal)")
}

// A line splice outside a directive is DGD's too: the two lines are left
// as they are written, and the rest is formatted.
func TestSpliceIsKept(t *testing.T) {
	in := "void f() {\n\tx = 1 +\\\n2;\n  y = a  \\\n    +   b;\n}\n"
	want := "void f() {\n    x = 1 +\\\n2;\n    y = a  \\\n    + b;\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
	assert.Equal(t, want, fmtSrc(t, want, Options{}))
	crlf := strings.ReplaceAll(want, "\n", "\r\n")
	assert.Equal(t, crlf, fmtSrc(t, want, Options{LineEndings: CRLF}))
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

// body wraps statements in a function, so that they stand at level 1.
func body(stmts string) string { return "void\nf()\n{\n" + stmts + "}\n" }

func TestBracelessBodies(t *testing.T) {
	// A statement governed by if/else/for/while/do without braces is one
	// level in, however deep the nesting; else-if is a chain.
	in := body("if (a)\nb();\nelse if (c)\n  d();\nelse\n        e();\n" +
		"for (i = 0; i < n; i++)\nif (x[i])\nwhile (y)\ng(i,\nj);\n" +
		"do\nz();\nwhile (q);\n")
	want := body("    if (a)\n        b();\n    else if (c)\n        d();\n    else\n        e();\n" +
		"    for (i = 0; i < n; i++)\n        if (x[i])\n            while (y)\n                g(i,\n                    j);\n" +
		"    do\n        z();\n    while (q);\n")
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
	assert.Equal(t, want, fmtSrc(t, want, Options{}))

	// A body on its statement's line belongs to that line; an `if` on the
	// line after its `else` is nested, as written.
	in = body("if (a) b(1,\n2);\nelse\nif (k)\nl();\n")
	want = body("    if (a) b(1,\n        2);\n    else\n        if (k)\n            l();\n")
	assert.Equal(t, want, fmtSrc(t, in, Options{}))

	// `else` joins a '}' only when that brace is its own if's.
	in = body("if (a)\nfor (;;) {\nb();\n}\nelse\nc();\n")
	want = body("    if (a)\n        for (;;) {\n            b();\n        }\n    else\n        c();\n")
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestContinuationLines(t *testing.T) {
	// A line that does not begin a statement continues one, whatever it
	// starts with.
	in := body("x = foo\n+ bar\n- baz;\nreturn a ? b\n: c;\ns = \"abc\"\n\"def\";\nstring u,\n*v;\n")
	want := body("    x = foo\n        + bar\n        - baz;\n    return a ? b\n        : c;\n" +
		"    s = \"abc\"\n        \"def\";\n    string u,\n        *v;\n")
	assert.Equal(t, want, fmtSrc(t, in, Options{}))

	// Declarations: specifiers and type stand at the left, the names on
	// further lines continue.
	in = "int a,\nb,\n  *c;\nprivate\n  static string\n*names;\n"
	want = "int a,\n    b,\n    *c;\nprivate\nstatic string\n    *names;\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestCommentsFollowTheCode(t *testing.T) {
	// A comment on its own line is indented like the line after it;
	// before a '}' or a case label it belongs to the statements inside.
	in := body("switch (x) {\n/* first */\ncase 1:\nif (y)\n/* why */\nz();\n/* fall */\ncase 2:\nfoo(a,\n/* about b */\nb);\n/* end */\n}\n")
	want := body("    switch (x) {\n        /* first */\n    case 1:\n        if (y)\n            /* why */\n            z();\n" +
		"        /* fall */\n    case 2:\n        foo(a,\n            /* about b */\n            b);\n        /* end */\n    }\n")
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
}

func TestRolesFromTheTree(t *testing.T) {
	// Inside a function the parser says what each token is; the same
	// tokens at top level are not statements, and only their neighbours
	// speak for them.
	cases := map[string]string{
		"x=(int)-y;":                  "x = (int) -y;",
		"if(a){b();}++c;":             "if (a) { b(); } ++c;",
		"m=([\"a\":x?1:2,\"b\":-3]);": "m = ([ \"a\": x ? 1 : 2, \"b\": -3 ]);",
		"x=a?-b:*c;":                  "x = a ? -b : *c;",
		"string**p,*q;":               "string **p, *q;",
		"x=a&b;f(&g);":                "x = a & b; f(&g);",
		"i=j++ +k;":                   "i = j++ + k;",
		"x=catch(y)?0:1;":             "x = catch (y) ? 0 : 1;",
		"switch(x){case 1:y=a?b:c;}":  "switch (x) { case 1: y = a ? b : c; }",
		"x=c['a'..'b'];l:goto l;":     "x = c['a'..'b']; l: goto l;",
		"x=({a,({b}),([c:d])});":      "x = ({ a, ({ b }), ([ c: d ]) });",
	}
	for in, want := range cases {
		assert.Equal(t, "void f() { "+want+" }\n", fmtSrc(t, "void f(){"+in+"}", Options{}), "input: %s", in)
	}
	assert.Equal(t, "static int *f(int *a, mixed b...);\n", fmtSrc(t, "static int*f(int*a,mixed b...);", Options{}))
	assert.Equal(t, "x = (int) - y;\n", fmtSrc(t, "x=(int)-y;", Options{}))
	// After a directive the token before says nothing; the tree does.
	assert.Equal(t, body("    x = a\n#if 1\n        - 1\n#endif\n        ;\n"), fmtSrc(t, body("x=a\n#if 1\n-1\n#endif\n;\n"), Options{}))
}

func TestConditionalBranches(t *testing.T) {
	// Every branch is laid out from the state the conditional began
	// with, so braces opened in each of them count once; the code after
	// continues from the branch the preprocessor model selects.
	in := "void f() {\n#ifdef A\nif (a) {\n#else\nif (b) {\n#endif\nc();\n}\n" +
		"#if 0\nwhile (x)\ny();\nif (q) {\n#endif\nz();\n}\n"
	want := "void f() {\n#ifdef A\n    if (a) {\n#else\n    if (b) {\n#endif\n        c();\n    }\n" +
		"#if 0\n    while (x)\n        y();\n    if (q) {\n#endif\n    z();\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
	assert.Equal(t, want, fmtSrc(t, want, Options{}))

	// Each branch is read by the parser in turn: the #else of a function
	// is a function too.
	in = "#ifdef B\nint g() {\nif (a)\nreturn 1;\n#else\nint g(int x) {\nif (b)\nreturn 2\n+ x;\n#endif\nreturn 0;\n}\n"
	want = "#ifdef B\nint g() {\n    if (a)\n        return 1;\n#else\nint g(int x) {\n    if (b)\n        return 2\n            + x;\n#endif\n    return 0;\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))

	// A conditional inside a statement, and nested ones.
	in = body("foo(a,\n#ifdef X\nb,\n#else\n#if Y\nc + d,\n#else\n-e,\n#endif\n#endif\nf);\n")
	want = body("    foo(a,\n#ifdef X\n        b,\n#else\n#if Y\n        c + d,\n#else\n        -e,\n#endif\n#endif\n        f);\n")
	assert.Equal(t, want, fmtSrc(t, in, Options{}))

	// The branch selected is the project's: a name it predefines decides.
	in = "void f() {\n#ifdef DEBUG\n{\n#endif\nx();\n#ifdef DEBUG\n}\n#endif\n}\n"
	assert.Equal(t, "void f() {\n#ifdef DEBUG\n    {\n#endif\n        x();\n#ifdef DEBUG\n    }\n#endif\n}\n",
		fmtSrc(t, in, Options{Defined: map[string]bool{"DEBUG": true}}))
}

func TestErrorRegions(t *testing.T) {
	// What the parser cannot read keeps its lines (no brace is moved) and
	// is indented from the last level that was understood; the code
	// around it is laid out as usual.
	in := body("if (a)\nb();\nforeach (x in y)\n{\nif (x)\n{\nc( x;\n}\n}\nif (d)\n{\ne();\n}\nelse\nf();\n")
	want := body("    if (a)\n        b();\n    foreach(x in y)\n    {\n        if (x)\n        {\n            c(x;\n        }\n    }\n" +
		"    if (d)\n    {\n        e();\n    }\n    else\n        f();\n")
	assert.Equal(t, want, fmtSrc(t, in, Options{}))
	assert.Equal(t, want, fmtSrc(t, want, Options{}))

	// A declaration the parser gives up on: the brace-counting fallback,
	// and the next declaration is unaffected.
	in = "bad(a, new) {\nif (a) {\nb();\n}\n}\nint ok() {\nreturn 1;\n}\n"
	want = "bad(a, new) {\n    if (a) {\n        b();\n    }\n}\nint ok() {\n    return 1;\n}\n"
	assert.Equal(t, want, fmtSrc(t, in, Options{}))

	// A macro whose arguments are not expressions.
	in = body("x = NEW(int   *, 3)+1;\n")
	assert.Equal(t, body("    x = NEW(int *, 3) + 1;\n"), fmtSrc(t, in, Options{}))
}

// A literal inside a literal is one level further in, whichever of the
// two kinds each is, and closes where it began.
func TestNestedLiterals(t *testing.T) {
	want := body(`    m = ([
        "a": ({
            1,
            2,
        }),
        "b": 2,
        "c": ([
            "x": ({ 1,
                2 }),
            /* about d */
        ]),
        "d": foo(1,
            2) +
            3,
    ]);
    x = ({ ([ "a": 1,
            "b": 2 ]),
        ({
            3,
        }),
    });
    y = a[
        b];
`)
	flat := strings.ReplaceAll(want, "\n        ", "\n")
	flat = strings.ReplaceAll(strings.ReplaceAll(flat, "\n    ", "\n"), "\n    ", "\n")
	require.NotContains(t, flat, "\n ")
	assert.Equal(t, want, fmtSrc(t, flat, Options{}))
	assert.Equal(t, want, fmtSrc(t, want, Options{}))
	// At the top level too.
	decl := "mapping m = ([\n    \"a\": ({\n        1,\n    }),\n    \"b\": ([\n        \"c\": 2,\n    ]),\n]);\n"
	assert.Equal(t, decl, fmtSrc(t, strings.ReplaceAll(decl, "\n    ", "\n"), Options{}))
}

// A control statement whose body is missing before the brace that closes
// the block: the brace is the block's, and the result is a fixed point.
func TestMissingBody(t *testing.T) {
	for in, want := range map[string]string{
		"f(){\nif(1)}":                                          "f() {\n    if (1) }\n",
		"void f()\n{\n    if (x) }\n":                           "void f()\n{\n    if (x) }\n",
		"void f()\n{\n    while (x) }\n":                        "void f()\n{\n    while (x) }\n",
		"void f()\n{\n    for (;;) }\n":                         "void f()\n{\n    for (;;) }\n",
		"void f()\n{\n    if (x) { if (y) } }\n":                "void f()\n{\n    if (x) { if (y) } }\n",
		"void f()\n{\n    x = \"a\" \"b\";\n    if (x = 1) }\n": "void f()\n{\n    x = \"a\" \"b\";\n    if (x = 1) }\n",
	} {
		assert.Equal(t, want, fmtSrc(t, in, Options{}), "%q", in)
		assert.Equal(t, want, fmtSrc(t, want, Options{}), "%q", in)
	}
}

// Texts no compiler takes, found by FuzzFormat: the formatter still gives
// back the tokens it was given.
func TestOddTokensStayWhatTheyAre(t *testing.T) {
	for in, want := range map[string]string{
		// A '#' in a line is no directive, and is not made one.
		"A(){#0\n00": "A() { # 0\n    00\n",
		// A number does not take the '.' after it.
		"int x = 0 .a;\n": "int x = 0 .a;\n",
		// A directive that ends the file without ending.
		"#define A \\":   "#define A \\",
		"#define A /* b": "#define A /* b",
		"#/*\n":          "#/*\n",
		"#endif":         "#endif\n",
		// The end of the file ends a line, before it is written to.
		"A(){for(;;)\n{": "A() { for (;;)\n    {\n",
		// A line break keeps its bytes, the last one too, unless it is a
		// carriage return alone.
		"\r\n\r\n0\n\n":      "\r\n\r\n0\n",
		"int x;\r\nint y;\n": "int x;\r\nint y;\n",
		"int x;\nint y;\r":   "int x;\nint y;\n",
	} {
		assert.Equal(t, want, fmtSrc(t, in, Options{}), "%q", in)
	}
}

// A block that is never closed does not take the brace of the block
// inside it for its own.
func TestUnclosedBlock(t *testing.T) {
	src := "void\nf()\n{\n    if (x) {\n        y();\n    }\n\n    retrieve()\n    {\n        move();\n        return 1;\n    }\n"
	assert.Equal(t, src, fmtSrc(t, src, Options{}))
	assert.Equal(t, "A() { rlimits (0; 0)\n    { }\n", fmtSrc(t, "A() {rlimits(0;0)\n{}", Options{}))
}

// The gate holds the comments too: all of them, in order, with nothing
// changed but where a block comment's further lines begin.
func TestGateComparesComments(t *testing.T) {
	lex := func(src string) *token.File { return lexer.Lex("t.c", []byte(src), lexer.Default) }
	same := func(a, b string) bool { return sameComments(lex(a), lex(b)) }
	assert.True(t, same("int x; // one\n/* two\n   more */\n", "int x;   // one\n    /* two\n\t\tmore */\n"))
	assert.False(t, same("int x; // one\n", "int x;\n"), "a comment is gone")
	assert.False(t, same("int x;\n", "int x; /* new */\n"))
	assert.False(t, same("/* a */ /* b */\n", "/* b */ /* a */\n"), "order")
	assert.False(t, same("// one \n", "// one\n"), "a line comment keeps its bytes")
	assert.False(t, same("/* two\n   more  */\n", "/* two\n   more */\n"), "only the indentation of a line may differ")
}

func TestOptionsFor(t *testing.T) {
	c := &config.Config{}
	c.Conventions.Defines = []string{"DEBUG"}
	c.Conventions.Macros = map[string]string{"STATUS": "type", "visible": "specifier"}
	o := OptionsFor(c, config.Format{IndentStyle: config.IndentTab, IndentWidth: 8, LineEndings: config.LineEndingsLF})
	assert.Equal(t, Tabs, o.IndentStyle)
	assert.Equal(t, 8, o.IndentWidth)
	assert.Equal(t, LF, o.LineEndings)
	assert.True(t, o.Defined["DEBUG"])
	assert.True(t, o.Defined["__DGD__"])
	assert.Equal(t, map[string]pp.Class{"visible": pp.ClassSpecifier, "STATUS": pp.ClassType}, o.Macros)

	// Unset settings are the defaults.
	o = OptionsFor(c, config.Format{})
	assert.Equal(t, Options{Defined: o.Defined, Macros: o.Macros}, o)
	assert.Equal(t, "    ", o.unit())
}
