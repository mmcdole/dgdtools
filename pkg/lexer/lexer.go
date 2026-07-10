// Package lexer implements a byte-lossless tokenizer for DGD LPC.
//
// The scanner is byte-oriented and never transcodes: bytes >= 0x80 pass
// through untouched inside strings, char constants, and comments, and become
// Illegal tokens elsewhere. Lexing never fails and never panics; malformed
// input produces Illegal tokens plus diagnostics, and the round-trip
// invariant (every byte belongs to exactly one token) holds regardless.
//
// Dialect notes (DGD 1.7): "//" comments exist only when Dialect.SlashSlash
// is set (the driver's SLASHSLASH build flag); "function" is a keyword only
// when Dialect.Closures is set (the CLOSURES build flag). "({" and "(["
// lex as two tokens; pairing is the parser's business. A preprocessor
// directive is one opaque Directive token spanning the logical line.
package lexer

import (
	"fmt"

	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/token"
)

// Dialect selects the compile-time language variants of the target driver.
type Dialect = token.Dialect

// Default is the pragmatic dialect: // comments on ("//" is never a legal
// DGD token sequence outside strings, so accepting it is harmless),
// closures off (stock DGD ships without CLOSURES).
var Default = Dialect{SlashSlash: true, Closures: false}

// Lex tokenizes src. The returned File always satisfies CheckRoundTrip.
func Lex(path string, src []byte, d Dialect) *token.File {
	lx := &lexer{
		f:           &token.File{Path: path, Src: src, Dialect: d},
		src:         src,
		dialect:     d,
		atLineStart: true,
	}
	lx.f.Lines = append(lx.f.Lines, 0)
	lx.run()
	return lx.f
}

type lexer struct {
	f       *token.File
	src     []byte
	pos     int
	dialect Dialect

	// atLineStart is true when only whitespace and comments have appeared
	// since the last newline; a '#' here starts a directive (matching cpp).
	atLineStart bool
}

func (lx *lexer) run() {
	for lx.pos < len(lx.src) {
		lx.scanToken()
	}
	lx.emit(token.EOF, lx.pos)
}

// emit appends a token spanning [start, lx.pos).
func (lx *lexer) emit(k token.Kind, start int) {
	lx.f.Tokens = append(lx.f.Tokens, token.Token{Kind: k, Off: uint32(start), End: uint32(lx.pos)})
	switch k {
	case token.Space, token.LineComment, token.BlockComment, token.EOF:
		// no effect on line-start state
	case token.Newline:
		lx.atLineStart = true
	default:
		lx.atLineStart = false
	}
}

func (lx *lexer) errorf(off int, format string, args ...any) {
	p := lx.posOf(off)
	lx.f.Errs = append(lx.f.Errs, diag.Diagnostic{
		Path: lx.f.Path, Line: p.Line, Col: p.Col,
		Severity: diag.Error, Message: fmt.Sprintf(format, args...),
	})
}

// posOf computes a position from the line starts recorded so far.
func (lx *lexer) posOf(off int) token.Pos {
	line := len(lx.f.Lines)
	for line > 1 && lx.f.Lines[line-1] > uint32(off) {
		line--
	}
	return token.Pos{Line: line, Col: off - int(lx.f.Lines[line-1]) + 1}
}

func (lx *lexer) peek(ahead int) byte {
	if lx.pos+ahead < len(lx.src) {
		return lx.src[lx.pos+ahead]
	}
	return 0
}

// newline consumes one line terminator at lx.pos (\r\n, \n, or bare \r)
// and records the new line start. Reports false if none is present.
func (lx *lexer) newline() bool {
	switch lx.peek(0) {
	case '\n':
		lx.pos++
	case '\r':
		lx.pos++
		if lx.peek(0) == '\n' {
			lx.pos++
		}
	default:
		return false
	}
	lx.f.Lines = append(lx.f.Lines, uint32(lx.pos))
	return true
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHexDigit(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func (lx *lexer) scanToken() {
	start := lx.pos
	c := lx.src[lx.pos]

	switch {
	case c == ' ' || c == '\t' || c == '\v' || c == '\f':
		for lx.pos < len(lx.src) {
			c := lx.src[lx.pos]
			if c != ' ' && c != '\t' && c != '\v' && c != '\f' {
				break
			}
			lx.pos++
		}
		lx.emit(token.Space, start)

	case c == '\n' || c == '\r':
		lx.newline()
		lx.emit(token.Newline, start)

	case c == '/':
		lx.scanSlash(start)

	case c == '#' && lx.atLineStart:
		lx.scanDirective(start)

	case c == '"':
		lx.scanString(start)

	case c == '\'':
		lx.scanChar(start)

	case isDigit(c):
		lx.scanNumber(start)

	case isIdentStart(c):
		for lx.pos < len(lx.src) && isIdentPart(lx.src[lx.pos]) {
			lx.pos++
		}
		word := string(lx.src[start:lx.pos])
		kind := token.Ident
		if kw, ok := token.Keywords[word]; ok && (kw != token.KwFunction || lx.dialect.Closures) {
			kind = kw
		}
		lx.emit(kind, start)

	default:
		lx.scanOperator(start, c)
	}
}

// scanSlash handles '/', '/=', block comments, and (dialect) line comments.
func (lx *lexer) scanSlash(start int) {
	switch lx.peek(1) {
	case '*':
		lx.pos += 2
		for lx.pos < len(lx.src) {
			if lx.src[lx.pos] == '*' && lx.peek(1) == '/' {
				lx.pos += 2
				lx.emit(token.BlockComment, start)
				return
			}
			if !lx.newline() {
				lx.pos++
			}
		}
		lx.errorf(start, "unterminated block comment")
		lx.emit(token.Illegal, start)
	case '/':
		if lx.dialect.SlashSlash {
			lx.pos += 2
			for lx.pos < len(lx.src) && lx.src[lx.pos] != '\n' && lx.src[lx.pos] != '\r' {
				lx.pos++
			}
			lx.emit(token.LineComment, start)
			return
		}
		// Without SLASHSLASH this is division by something unary-less —
		// lex as two Slash tokens; the first is emitted here.
		lx.pos++
		lx.emit(token.Slash, start)
	case '=':
		lx.pos += 2
		lx.emit(token.SlashEq, start)
	default:
		lx.pos++
		lx.emit(token.Slash, start)
	}
}

// scanDirective consumes '#' through the end of the logical line: backslash-
// newline continues it, newlines inside block comments and after a backslash
// do not end it, and string/char literals are skipped so their contents
// cannot desync the scan. The terminating newline is NOT part of the token.
func (lx *lexer) scanDirective(start int) {
	lx.pos++ // '#'
	for lx.pos < len(lx.src) {
		switch lx.src[lx.pos] {
		case '\n', '\r':
			lx.emit(token.Directive, start)
			return
		case '\\':
			lx.pos++
			if !lx.newline() && lx.pos < len(lx.src) {
				lx.pos++ // escaped non-newline char
			}
		case '/':
			if lx.peek(1) == '*' {
				lx.pos += 2
				for lx.pos < len(lx.src) {
					if lx.src[lx.pos] == '*' && lx.peek(1) == '/' {
						lx.pos += 2
						break
					}
					if !lx.newline() {
						lx.pos++
					}
				}
			} else if lx.peek(1) == '/' && lx.dialect.SlashSlash {
				for lx.pos < len(lx.src) && lx.src[lx.pos] != '\n' && lx.src[lx.pos] != '\r' {
					lx.pos++
				}
			} else {
				lx.pos++
			}
		case '"', '\'':
			quote := lx.src[lx.pos]
			lx.pos++
			for lx.pos < len(lx.src) {
				c := lx.src[lx.pos]
				if c == '\\' {
					lx.pos++
					if !lx.newline() && lx.pos < len(lx.src) {
						lx.pos++
					}
					continue
				}
				if c == '\n' || c == '\r' || c == quote {
					if c == quote {
						lx.pos++
					}
					break
				}
				lx.pos++
			}
		default:
			lx.pos++
		}
	}
	lx.emit(token.Directive, start) // directive runs to EOF
}

// scanString consumes a double-quoted string. Backslash-newline is a line
// continuation inside the literal; a raw newline means unterminated.
func (lx *lexer) scanString(start int) {
	lx.pos++ // opening quote
	for lx.pos < len(lx.src) {
		switch lx.src[lx.pos] {
		case '"':
			lx.pos++
			lx.emit(token.StringLit, start)
			return
		case '\\':
			lx.pos++
			if !lx.newline() && lx.pos < len(lx.src) {
				lx.pos++
			}
		case '\n', '\r':
			lx.errorf(start, "unterminated string literal")
			lx.emit(token.Illegal, start)
			return
		default:
			lx.pos++
		}
	}
	lx.errorf(start, "unterminated string literal")
	lx.emit(token.Illegal, start)
}

// scanChar consumes a character constant: exactly one character or escape
// between single quotes. Anything else is Illegal (DGD errors on it too).
func (lx *lexer) scanChar(start int) {
	lx.pos++ // opening quote
	chars := 0
	for lx.pos < len(lx.src) {
		switch lx.src[lx.pos] {
		case '\'':
			lx.pos++
			if chars == 1 {
				lx.emit(token.CharLit, start)
			} else {
				lx.errorf(start, "illegal character constant")
				lx.emit(token.Illegal, start)
			}
			return
		case '\\':
			lx.pos++
			lx.scanEscapeTail()
			chars++
		case '\n', '\r':
			lx.errorf(start, "unterminated character constant")
			lx.emit(token.Illegal, start)
			return
		default:
			lx.pos++
			chars++
		}
	}
	lx.errorf(start, "unterminated character constant")
	lx.emit(token.Illegal, start)
}

// scanEscapeTail consumes the body of an escape sequence after the
// backslash: up to 3 octal digits, \x plus up to 2 hex digits, or one
// arbitrary character (DGD: unknown escape means the literal character).
func (lx *lexer) scanEscapeTail() {
	if lx.pos >= len(lx.src) {
		return
	}
	c := lx.src[lx.pos]
	switch {
	case c >= '0' && c <= '7':
		for n := 0; n < 3 && lx.pos < len(lx.src) && lx.src[lx.pos] >= '0' && lx.src[lx.pos] <= '7'; n++ {
			lx.pos++
		}
	case c == 'x' || c == 'X':
		lx.pos++
		for n := 0; n < 2 && lx.pos < len(lx.src) && isHexDigit(lx.src[lx.pos]); n++ {
			lx.pos++
		}
	default:
		lx.pos++
	}
}

// scanNumber consumes an integer or float starting with a digit.
// Disambiguation per DGD: "1..2" is IntLit DotDot IntLit, never floats.
func (lx *lexer) scanNumber(start int) {
	if lx.src[lx.pos] == '0' && (lx.peek(1) == 'x' || lx.peek(1) == 'X') {
		lx.pos += 2
		for lx.pos < len(lx.src) && isHexDigit(lx.src[lx.pos]) {
			lx.pos++
		}
		lx.emit(token.IntLit, start)
		return
	}
	for lx.pos < len(lx.src) && isDigit(lx.src[lx.pos]) {
		lx.pos++
	}
	isFloat := false
	if lx.peek(0) == '.' && lx.peek(1) != '.' {
		// Fraction — but not a range operator.
		lx.pos++
		for lx.pos < len(lx.src) && isDigit(lx.src[lx.pos]) {
			lx.pos++
		}
		isFloat = true
	}
	if lx.scanExponent() {
		isFloat = true
	}
	if isFloat {
		lx.emit(token.FloatLit, start)
	} else {
		lx.emit(token.IntLit, start)
	}
}

// scanExponent consumes [eE][+-]?digits if and only if the digits are
// present; otherwise it consumes nothing (so "1e" lexes as IntLit, Ident).
func (lx *lexer) scanExponent() bool {
	if c := lx.peek(0); c != 'e' && c != 'E' {
		return false
	}
	n := 1
	if c := lx.peek(n); c == '+' || c == '-' {
		n++
	}
	if !isDigit(lx.peek(n)) {
		return false
	}
	lx.pos += n
	for lx.pos < len(lx.src) && isDigit(lx.src[lx.pos]) {
		lx.pos++
	}
	return true
}

// scanOperator handles punctuation, multi-char operators, leading-dot
// floats, stray '#', and illegal bytes.
func (lx *lexer) scanOperator(start int, c byte) {
	two := func(k token.Kind) {
		lx.pos += 2
		lx.emit(k, start)
	}
	one := func(k token.Kind) {
		lx.pos++
		lx.emit(k, start)
	}

	switch c {
	case '(':
		one(token.LParen)
	case ')':
		one(token.RParen)
	case '{':
		one(token.LBrace)
	case '}':
		one(token.RBrace)
	case '[':
		one(token.LBracket)
	case ']':
		one(token.RBracket)
	case ';':
		one(token.Semicolon)
	case ',':
		one(token.Comma)
	case '?':
		one(token.Question)
	case '~':
		one(token.Tilde)

	case '.':
		if isDigit(lx.peek(1)) {
			lx.pos++ // '.'
			for lx.pos < len(lx.src) && isDigit(lx.src[lx.pos]) {
				lx.pos++
			}
			lx.scanExponent()
			lx.emit(token.FloatLit, start)
		} else if lx.peek(1) == '.' {
			if lx.peek(2) == '.' {
				lx.pos += 3
				lx.emit(token.Ellipsis, start)
			} else {
				two(token.DotDot)
			}
		} else {
			one(token.Dot)
		}

	case ':':
		if lx.peek(1) == ':' {
			two(token.ColonColon)
		} else {
			one(token.Colon)
		}
	case '!':
		if lx.peek(1) == '=' {
			two(token.NotEq)
		} else {
			one(token.Not)
		}
	case '=':
		if lx.peek(1) == '=' {
			two(token.EqEq)
		} else {
			one(token.Assign)
		}
	case '+':
		switch lx.peek(1) {
		case '+':
			two(token.Inc)
		case '=':
			two(token.PlusEq)
		default:
			one(token.Plus)
		}
	case '-':
		switch lx.peek(1) {
		case '-':
			two(token.Dec)
		case '=':
			two(token.MinusEq)
		case '>':
			two(token.Arrow)
		default:
			one(token.Minus)
		}
	case '*':
		if lx.peek(1) == '=' {
			two(token.StarEq)
		} else {
			one(token.Star)
		}
	case '%':
		if lx.peek(1) == '=' {
			two(token.PercentEq)
		} else {
			one(token.Percent)
		}
	case '&':
		switch lx.peek(1) {
		case '&':
			two(token.LAnd)
		case '=':
			two(token.AmpEq)
		default:
			one(token.Amp)
		}
	case '|':
		switch lx.peek(1) {
		case '|':
			two(token.LOr)
		case '=':
			two(token.PipeEq)
		default:
			one(token.Pipe)
		}
	case '^':
		if lx.peek(1) == '=' {
			two(token.CaretEq)
		} else {
			one(token.Caret)
		}
	case '<':
		switch lx.peek(1) {
		case '=':
			two(token.LtEq)
		case '-':
			two(token.LArrow)
		case '<':
			if lx.peek(2) == '=' {
				lx.pos += 3
				lx.emit(token.ShlEq, start)
			} else {
				two(token.Shl)
			}
		default:
			one(token.Lt)
		}
	case '>':
		switch lx.peek(1) {
		case '=':
			two(token.GtEq)
		case '>':
			if lx.peek(2) == '=' {
				lx.pos += 3
				lx.emit(token.ShrEq, start)
			} else {
				two(token.Shr)
			}
		default:
			one(token.Gt)
		}
	case '#':
		// Not at line start: legal only inside macro bodies, which live in
		// Directive tokens — but pass it through rather than reject.
		if lx.peek(1) == '#' {
			two(token.HashHash)
		} else {
			one(token.Hash)
		}

	default:
		// Illegal byte(s): high bytes outside strings/comments, stray
		// control characters, backslashes outside literals. Group a run of
		// illegal bytes into one token to keep diagnostics readable.
		lx.errorf(start, "illegal character %q", c)
		for lx.pos < len(lx.src) && illegalByte(lx.src[lx.pos]) {
			lx.pos++
		}
		if lx.pos == start {
			lx.pos++
		}
		lx.emit(token.Illegal, start)
	}
}

func illegalByte(c byte) bool {
	if c >= 0x80 {
		return true
	}
	switch c {
	case '\\', '@', '$', '`':
		return true
	}
	return c < 0x20 && c != '\t' && c != '\n' && c != '\r' && c != '\v' && c != '\f'
}
