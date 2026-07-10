package lexer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/token"
)

// kinds lexes src with the default dialect and returns the significant
// token kinds (skipping trivia and EOF).
func kinds(t *testing.T, src string) []token.Kind {
	t.Helper()
	f := Lex("test.c", []byte(src), Default)
	require.NoError(t, f.CheckRoundTrip())
	var ks []token.Kind
	for _, tok := range f.Significant() {
		ks = append(ks, tok.Kind)
	}
	return ks
}

func texts(t *testing.T, src string) []string {
	t.Helper()
	f := Lex("test.c", []byte(src), Default)
	require.NoError(t, f.CheckRoundTrip())
	var out []string
	for _, tok := range f.Significant() {
		out = append(out, string(f.Text(tok)))
	}
	return out
}

func TestRoundTripByteExact(t *testing.T) {
	srcs := []string{
		"",
		"int x;\n",
		"/* unterminated",
		"\"unterminated\nint y;\n",
		"a\r\nb\rc\nd",
		"weird \x01 bytes \xff here",
		"#define FOO \\\n    bar\nint x;\n",
	}
	for _, src := range srcs {
		f := Lex("t.c", []byte(src), Default)
		require.NoError(t, f.CheckRoundTrip(), "src=%q", src)
		var buf []byte
		for _, tok := range f.Tokens {
			buf = append(buf, f.Text(tok)...)
		}
		assert.Equal(t, src, string(buf), "reassembly must be byte-exact")
	}
}

func TestRangeVsFloat(t *testing.T) {
	// "1..2" is IntLit DotDot IntLit, never two floats.
	assert.Equal(t,
		[]token.Kind{token.IntLit, token.DotDot, token.IntLit},
		kinds(t, "1..2"))
	assert.Equal(t,
		[]token.Kind{token.FloatLit},
		kinds(t, "1.5"))
	assert.Equal(t,
		[]token.Kind{token.FloatLit},
		kinds(t, ".5"))
	assert.Equal(t,
		[]token.Kind{token.FloatLit},
		kinds(t, "1.5e10"))
	assert.Equal(t,
		[]token.Kind{token.IntLit, token.Ident},
		kinds(t, "1e"), "no-digit exponent must not be consumed")
	assert.Equal(t,
		[]token.Kind{token.IntLit, token.Ellipsis},
		kinds(t, "1..."))
	assert.Equal(t, []token.Kind{token.IntLit}, kinds(t, "0x1F"))
	assert.Equal(t, []token.Kind{token.IntLit}, kinds(t, "0777"))
}

func TestOperators(t *testing.T) {
	assert.Equal(t,
		[]token.Kind{token.Ident, token.Arrow, token.Ident, token.LParen, token.RParen},
		kinds(t, "obj->fn()"))
	assert.Equal(t,
		[]token.Kind{token.Ident, token.LArrow, token.StringLit},
		kinds(t, `obj <- "/std/room"`))
	assert.Equal(t,
		[]token.Kind{token.ColonColon, token.Ident, token.LParen, token.RParen},
		kinds(t, "::create()"))
	assert.Equal(t,
		[]token.Kind{token.Ident, token.ShlEq, token.IntLit, token.Semicolon},
		kinds(t, "x <<= 2;"))
	assert.Equal(t,
		[]token.Kind{token.Ident, token.ShrEq, token.IntLit},
		kinds(t, "x >>= 2"))
	// ({ and ([ are two tokens each.
	assert.Equal(t,
		[]token.Kind{token.LParen, token.LBrace, token.StringLit, token.RBrace, token.RParen},
		kinds(t, `({ "a" })`))
	assert.Equal(t,
		[]token.Kind{token.LParen, token.LBracket, token.StringLit, token.Colon, token.IntLit, token.RBracket, token.RParen},
		kinds(t, `([ "k": 1 ])`))
	// Spread / varargs ellipsis.
	assert.Equal(t,
		[]token.Kind{token.Ident, token.Ellipsis},
		kinds(t, "args..."))
}

func TestKeywordsAndDialect(t *testing.T) {
	assert.Equal(t,
		[]token.Kind{token.KwPrivate, token.KwStatic, token.KwNomask, token.KwAtomic, token.KwVarargs},
		kinds(t, "private static nomask atomic varargs"))
	// "function" is an identifier unless closures are enabled.
	assert.Equal(t, []token.Kind{token.Ident}, kinds(t, "function"))
	fc := Lex("t.c", []byte("function"), Dialect{SlashSlash: true, Closures: true})
	require.NoError(t, fc.CheckRoundTrip())
	assert.Equal(t, token.KwFunction, fc.Tokens[0].Kind)
	// "status" is NOT a keyword (pre-DGD leftover in old corpora).
	assert.Equal(t, []token.Kind{token.Ident, token.Ident, token.Semicolon}, kinds(t, "status flag;"))
}

func TestComments(t *testing.T) {
	f := Lex("t.c", []byte("a /* x */ b // y\nc"), Default)
	require.NoError(t, f.CheckRoundTrip())
	assert.Equal(t,
		[]token.Kind{token.Ident, token.Ident, token.Ident},
		kinds(t, "a /* x */ b // y\nc"))

	// Comment markers inside strings must not open/close comments.
	assert.Equal(t,
		[]token.Kind{token.StringLit, token.Semicolon},
		kinds(t, `"/* not a comment */";`))
	assert.Equal(t,
		[]token.Kind{token.Ident, token.Assign, token.StringLit, token.Semicolon},
		kinds(t, `x = "start /*";`))

	// String-looking content inside comments must not open strings.
	assert.Equal(t,
		[]token.Kind{token.Ident},
		kinds(t, "/* \"quote */ x"))

	// Non-nesting block comments.
	assert.Equal(t,
		[]token.Kind{token.Ident, token.Slash},
		kinds(t, "/* /* */ x /"), "inner /* does not nest")

	// // disabled without SlashSlash: two Slash tokens.
	f2 := Lex("t.c", []byte("a // b"), Dialect{SlashSlash: false})
	require.NoError(t, f2.CheckRoundTrip())
	var ks []token.Kind
	for _, tok := range f2.Significant() {
		ks = append(ks, tok.Kind)
	}
	assert.Equal(t, []token.Kind{token.Ident, token.Slash, token.Slash, token.Ident}, ks)
}

func TestStrings(t *testing.T) {
	assert.Equal(t, []string{`"a\"b"`}, texts(t, `"a\"b"`))
	assert.Equal(t, []string{`"tab\there"`}, texts(t, `"tab\there"`))
	// Backslash-newline continuation inside a string.
	src := "\"line one\\\nline two\""
	assert.Equal(t, []token.Kind{token.StringLit}, kinds(t, src))
	// High bytes and ANSI escapes pass through inside strings.
	src = "\"latin1: \xe9 esc: \x1b[1m\""
	assert.Equal(t, []token.Kind{token.StringLit}, kinds(t, src))
	// Unterminated string is Illegal but round-trips.
	f := Lex("t.c", []byte("\"oops\nnext"), Default)
	require.NoError(t, f.CheckRoundTrip())
	assert.True(t, f.HasIllegal())
	assert.NotEmpty(t, f.Errs)
}

func TestCharConstants(t *testing.T) {
	assert.Equal(t, []token.Kind{token.CharLit}, kinds(t, "'x'"))
	assert.Equal(t, []token.Kind{token.CharLit}, kinds(t, `'\n'`))
	assert.Equal(t, []token.Kind{token.CharLit}, kinds(t, `'\''`))
	assert.Equal(t, []token.Kind{token.CharLit}, kinds(t, `'\\'`))
	assert.Equal(t, []token.Kind{token.CharLit}, kinds(t, `'\x1b'`))
	assert.Equal(t, []token.Kind{token.CharLit}, kinds(t, `'\007'`))
	// Empty and multi-char constants are Illegal.
	for _, src := range []string{"''", "'ab'"} {
		f := Lex("t.c", []byte(src), Default)
		require.NoError(t, f.CheckRoundTrip())
		assert.True(t, f.HasIllegal(), "src=%q", src)
	}
}

func TestDirectives(t *testing.T) {
	// Whole directive is one token; newline is separate.
	f := Lex("t.c", []byte("#include <std.h>\nint x;\n"), Default)
	require.NoError(t, f.CheckRoundTrip())
	assert.Equal(t, token.Directive, f.Tokens[0].Kind)
	assert.Equal(t, "#include <std.h>", string(f.Text(f.Tokens[0])))

	// Backslash continuation keeps the directive going.
	f = Lex("t.c", []byte("#define M(a) \\\n    ((a) + 1)\nint x;\n"), Default)
	require.NoError(t, f.CheckRoundTrip())
	assert.Equal(t, token.Directive, f.Tokens[0].Kind)
	assert.Contains(t, string(f.Text(f.Tokens[0])), "((a) + 1)")

	// Block comment inside a directive may span lines.
	f = Lex("t.c", []byte("#define A /* one\ntwo */ 1\nint x;\n"), Default)
	require.NoError(t, f.CheckRoundTrip())
	assert.Equal(t, token.Directive, f.Tokens[0].Kind)
	assert.Contains(t, string(f.Text(f.Tokens[0])), "two */ 1")

	// String contents inside a directive can't end it early.
	f = Lex("t.c", []byte("#define S \"a\\\"b\"\nint x;\n"), Default)
	require.NoError(t, f.CheckRoundTrip())
	assert.Equal(t, `#define S "a\"b"`, string(f.Text(f.Tokens[0])))

	// '#' only starts a directive at line start (whitespace/comments ok).
	assert.Equal(t,
		[]token.Kind{token.Ident, token.Hash, token.Ident},
		kinds(t, "a # b"))
	f = Lex("t.c", []byte("  \t#pragma foo\n"), Default)
	require.NoError(t, f.CheckRoundTrip())
	assert.Equal(t, token.Directive, f.Tokens[1].Kind)
}

func TestLineEndings(t *testing.T) {
	f := Lex("t.c", []byte("a\r\nb\rc\n"), Default)
	require.NoError(t, f.CheckRoundTrip())
	var nl []string
	for _, tok := range f.Tokens {
		if tok.Kind == token.Newline {
			nl = append(nl, string(f.Text(tok)))
		}
	}
	assert.Equal(t, []string{"\r\n", "\r", "\n"}, nl)
	// Positions: 'c' is on line 3.
	for i, tok := range f.Tokens {
		if tok.Kind == token.Ident && string(f.Text(tok)) == "c" {
			assert.Equal(t, token.Pos{Line: 3, Col: 1}, f.Pos(f.Tokens[i].Off))
		}
	}
}

func TestInheritForms(t *testing.T) {
	assert.Equal(t,
		[]token.Kind{token.KwInherit, token.StringLit, token.Semicolon},
		kinds(t, `inherit "/std/room";`))
	assert.Equal(t,
		[]token.Kind{token.KwPrivate, token.KwInherit, token.Ident, token.StringLit, token.Semicolon},
		kinds(t, `private inherit base "/std/object";`))
}
