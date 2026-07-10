// Package token defines the lossless token model for DGD LPC source.
//
// A lexed File holds one flat, contiguous token stream in which whitespace,
// newlines, and comments are tokens ("trivia") like any other. Every byte of
// the source belongs to exactly one token, so re-concatenating all token
// texts reproduces the input byte-for-byte. Tokens store offsets into the
// original source, never copies.
package token

import "fmt"

// Kind identifies a token's lexical class.
type Kind uint8

const (
	Illegal Kind = iota // byte sequence that is not valid DGD LPC
	EOF                 // zero-length sentinel at end of file

	// Directive is one whole preprocessor directive: '#' at the start of a
	// line through the end of the (backslash-continued) logical line, not
	// including the terminating newline. Opaque to formatting.
	Directive

	// Trivia.
	Space        // run of spaces, tabs, \v, \f
	Newline      // exactly one line terminator: \n, \r\n, or bare \r
	LineComment  // // to end of line (dialect-gated)
	BlockComment // /* ... */, possibly spanning lines

	// Names and literals.
	Ident
	IntLit
	FloatLit
	StringLit
	CharLit

	// Keywords (DGD 1.7).
	KwAtomic
	KwBreak
	KwCase
	KwCatch
	KwContinue
	KwDefault
	KwDo
	KwElse
	KwFloat
	KwFor
	KwFunction // reserved only when Dialect.Closures is set
	KwGoto
	KwIf
	KwInherit
	KwInt
	KwMapping
	KwMixed
	KwNew
	KwNil
	KwNomask
	KwObject
	KwOperator
	KwPrivate
	KwReturn
	KwRlimits
	KwStatic
	KwString
	KwSwitch
	KwTry
	KwVarargs
	KwVoid
	KwWhile

	// Operators and punctuation.
	LParen     // (
	RParen     // )
	LBrace     // {
	RBrace     // }
	LBracket   // [
	RBracket   // ]
	Semicolon  // ;
	Comma      // ,
	Question   // ?
	Colon      // :
	Dot        // .
	Not        // !
	Tilde      // ~
	Assign     // =
	Lt         // <
	Gt         // >
	Plus       // +
	Minus      // -
	Star       // *
	Slash      // /
	Percent    // %
	Amp        // &
	Pipe       // |
	Caret      // ^
	Hash       // # (outside a directive; valid only in macro bodies)
	HashHash   // ##
	NotEq      // !=
	EqEq       // ==
	LtEq       // <=
	GtEq       // >=
	LAnd       // &&
	LOr        // ||
	Inc        // ++
	Dec        // --
	Arrow      // ->
	LArrow     // <- (instanceof)
	ColonColon // ::
	DotDot     // .. (range)
	Ellipsis   // ...
	Shl        // <<
	Shr        // >>
	PlusEq     // +=
	MinusEq    // -=
	StarEq     // *=
	SlashEq    // /=
	PercentEq  // %=
	AmpEq      // &=
	PipeEq     // |=
	CaretEq    // ^=
	ShlEq      // <<=
	ShrEq      // >>=

	kindCount
)

var kindNames = [...]string{
	Illegal: "Illegal", EOF: "EOF", Directive: "Directive",
	Space: "Space", Newline: "Newline",
	LineComment: "LineComment", BlockComment: "BlockComment",
	Ident: "Ident", IntLit: "IntLit", FloatLit: "FloatLit",
	StringLit: "StringLit", CharLit: "CharLit",
	KwAtomic: "atomic", KwBreak: "break", KwCase: "case", KwCatch: "catch",
	KwContinue: "continue", KwDefault: "default", KwDo: "do", KwElse: "else",
	KwFloat: "float", KwFor: "for", KwFunction: "function", KwGoto: "goto",
	KwIf: "if", KwInherit: "inherit", KwInt: "int", KwMapping: "mapping",
	KwMixed: "mixed", KwNew: "new", KwNil: "nil", KwNomask: "nomask",
	KwObject: "object", KwOperator: "operator", KwPrivate: "private",
	KwReturn: "return", KwRlimits: "rlimits", KwStatic: "static",
	KwString: "string", KwSwitch: "switch", KwTry: "try",
	KwVarargs: "varargs", KwVoid: "void", KwWhile: "while",
	LParen: "(", RParen: ")", LBrace: "{", RBrace: "}",
	LBracket: "[", RBracket: "]", Semicolon: ";", Comma: ",",
	Question: "?", Colon: ":", Dot: ".", Not: "!", Tilde: "~",
	Assign: "=", Lt: "<", Gt: ">", Plus: "+", Minus: "-", Star: "*",
	Slash: "/", Percent: "%", Amp: "&", Pipe: "|", Caret: "^",
	Hash: "#", HashHash: "##", NotEq: "!=", EqEq: "==", LtEq: "<=",
	GtEq: ">=", LAnd: "&&", LOr: "||", Inc: "++", Dec: "--",
	Arrow: "->", LArrow: "<-", ColonColon: "::", DotDot: "..",
	Ellipsis: "...", Shl: "<<", Shr: ">>", PlusEq: "+=", MinusEq: "-=",
	StarEq: "*=", SlashEq: "/=", PercentEq: "%=", AmpEq: "&=",
	PipeEq: "|=", CaretEq: "^=", ShlEq: "<<=", ShrEq: ">>=",
}

func (k Kind) String() string {
	if int(k) < len(kindNames) && kindNames[k] != "" {
		return kindNames[k]
	}
	return fmt.Sprintf("Kind(%d)", k)
}

// IsTrivia reports whether the kind carries no semantic weight.
func (k Kind) IsTrivia() bool { return k >= Space && k <= BlockComment }

// IsKeyword reports whether the kind is a reserved word.
func (k Kind) IsKeyword() bool { return k >= KwAtomic && k <= KwWhile }

// Keywords maps the DGD 1.7 reserved words to their kinds. "function" is
// included here; the lexer excludes it unless the dialect enables closures.
var Keywords = map[string]Kind{
	"atomic": KwAtomic, "break": KwBreak, "case": KwCase, "catch": KwCatch,
	"continue": KwContinue, "default": KwDefault, "do": KwDo, "else": KwElse,
	"float": KwFloat, "for": KwFor, "function": KwFunction, "goto": KwGoto,
	"if": KwIf, "inherit": KwInherit, "int": KwInt, "mapping": KwMapping,
	"mixed": KwMixed, "new": KwNew, "nil": KwNil, "nomask": KwNomask,
	"object": KwObject, "operator": KwOperator, "private": KwPrivate,
	"return": KwReturn, "rlimits": KwRlimits, "static": KwStatic,
	"string": KwString, "switch": KwSwitch, "try": KwTry,
	"varargs": KwVarargs, "void": KwVoid, "while": KwWhile,
}

// Token is one lexical unit: a half-open byte range [Off, End) in File.Src.
type Token struct {
	Kind     Kind
	Off, End uint32
}

// Len returns the token's length in bytes.
func (t Token) Len() int { return int(t.End - t.Off) }

// Pos is a 1-based line/column position. Col counts bytes, not runes.
type Pos struct {
	Line, Col int
}

func (p Pos) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Col) }
