// Package format implements the dgdfmt layout engine.
//
// The engine is a trivia-only rewriter: it emits every significant token's
// bytes verbatim and synthesizes only the whitespace between them. Safety is
// enforced twice — structurally (there is no code path that alters a
// significant token) and by the gate in Format, which re-lexes the output,
// requires token-stream equality with the input (tokcmp), and requires
// idempotence, before any byte is returned.
//
// Style: KNF as practiced in DGD-era LPC. Function definitions get their
// specifiers and return type on one line, the name at column 0, and the
// opening brace on its own line at column 0; control-flow braces are
// cuddled. Case labels sit at the switch statement's level. Horizontal
// spacing between tokens is normalized gofmt-style while the author's line
// breaks are preserved. Comments are never reflowed: line comments keep
// their bytes, multi-line block comments are shifted whole, and function
// headers containing comments are left in their original layout.
package format

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/mmcdole/dgdtools/pkg/lexer"
	"github.com/mmcdole/dgdtools/pkg/structure"
	"github.com/mmcdole/dgdtools/pkg/token"
	"github.com/mmcdole/dgdtools/pkg/tokcmp"
)

// LineEndings selects the newline policy.
type LineEndings int

const (
	Preserve LineEndings = iota // keep each newline's original bytes
	LF                          // normalize to \n
	CRLF                        // normalize to \r\n
)

// HeaderStyle selects function-definition header layout.
type HeaderStyle int

const (
	// HeadersSplit is KNF: specifiers and return type on one line, the
	// function name at column 0, the brace on its own line.
	HeadersSplit HeaderStyle = iota
	// HeadersJoined keeps the whole header on one line; the brace still
	// gets its own line.
	HeadersJoined
)

// Options carries the formatter's (deliberately few) toggles. The dialect
// is not an option here: Format always relexes its output under the same
// dialect the input File was lexed with.
type Options struct {
	Indent        int // spaces per level; 0 means the default 4
	LineEndings   LineEndings
	MaxBlankLines int // max consecutive blank lines; 0 means the default 2
	FuncHeaders   HeaderStyle
}

func (o Options) indent() int {
	if o.Indent <= 0 {
		return 4
	}
	return o.Indent
}

func (o Options) maxBlank() int {
	if o.MaxBlankLines <= 0 {
		return 2
	}
	return o.MaxBlankLines
}

// ErrIllegal is returned for files that did not fully tokenize; dgdfmt
// refuses to format anything it could not prove lossless.
var ErrIllegal = errors.New("file contains unlexable content; refusing to format")

// ErrGateFailure means the engine produced output whose significant token
// stream differs from the input — an engine bug caught before writing.
var ErrGateFailure = errors.New("formatter gate failure (output tokens diverge from input)")

// ErrNotIdempotent means formatting its own output changed it again.
var ErrNotIdempotent = errors.New("formatter is not idempotent on this input")

// Format returns the formatted source, or an error if the gate trips.
// The input File is not modified.
func Format(f *token.File, o Options) ([]byte, error) {
	if f.HasIllegal() {
		return nil, fmt.Errorf("%s: %w", f.Path, ErrIllegal)
	}

	out := engine(f, o)

	relexed := lexer.Lex(f.Path, out, f.Dialect)
	if err := relexed.CheckRoundTrip(); err != nil {
		return nil, fmt.Errorf("%s: %w: %v", f.Path, ErrGateFailure, err)
	}
	if eq, div := tokcmp.Compare(f, relexed); !eq {
		return nil, fmt.Errorf("%s: %w: input %s vs output %s", f.Path, ErrGateFailure, div.APos, div.BPos)
	}
	if again := engine(relexed, o); !bytes.Equal(again, out) {
		return nil, fmt.Errorf("%s: %w", f.Path, ErrNotIdempotent)
	}
	return out, nil
}

// engine performs the actual layout. It must be deterministic.
func engine(f *token.File, o Options) []byte {
	p := &printer{
		f:          f,
		o:          o,
		nl:         dominantNewline(f),
		plan:       buildPlan(f, o),
		prevSigIdx: -1,
	}
	p.run()
	return p.buf.Bytes()
}

func dominantNewline(f *token.File) []byte {
	crlf, lf := 0, 0
	for _, t := range f.Tokens {
		if t.Kind == token.Newline {
			if t.Len() >= 2 { // \r\n
				crlf++
			} else if f.Src[t.Off] == '\n' {
				lf++
			}
		}
	}
	if crlf > lf {
		return []byte("\r\n")
	}
	return []byte("\n")
}

// plan carries the KNF header layout decisions derived from pkg/structure.
// Keys are token indexes. Only headers free of comments get plan entries.
type plan struct {
	breakBefore map[int]bool // exactly one newline before this token
	breakAfter  map[int]bool // exactly one newline after this token
	joinBefore  map[int]bool // exactly one space before this token
	bodyBrace   map[int]bool // function-body '{' (exempt from cuddling)
}

func buildPlan(f *token.File, o Options) plan {
	pl := plan{
		breakBefore: map[int]bool{},
		breakAfter:  map[int]bool{},
		joinBefore:  map[int]bool{},
		bodyBrace:   map[int]bool{},
	}
	fs := structure.Analyze(f, structure.DefaultConfig())
	for i := range fs.Items {
		it := &fs.Items[i]
		if it.Kind != structure.FuncDef && it.Kind != structure.Prototype {
			continue
		}
		if it.Kind == structure.FuncDef {
			pl.bodyBrace[it.BodyL] = true
		}
		if it.HeaderComments {
			continue
		}
		// A definition written entirely on one line is a deliberate
		// vertical decision (the accessor idiom); preserve it, like gofmt
		// preserves single-line functions. KNF explosion applies only to
		// definitions that are already multi-line.
		if it.Kind == structure.FuncDef && !spanHasNewline(f, it.First, it.BodyR) {
			continue
		}
		hdr := append(append([]int{}, it.SpecIdxs...), it.TypeIdxs...)
		for _, idx := range hdr[min(1, len(hdr)):] {
			pl.joinBefore[idx] = true
		}
		if len(hdr) > 0 {
			if it.Kind == structure.FuncDef && o.FuncHeaders == HeadersSplit {
				pl.breakBefore[it.NameIdx] = true // name at column 0
			} else {
				pl.joinBefore[it.NameIdx] = true // one-line header
			}
		}
		if it.Kind == structure.FuncDef {
			pl.breakBefore[it.BodyL] = true // brace on its own line
			pl.breakAfter[it.BodyL] = true  // body starts on the next line
			pl.breakBefore[it.BodyR] = true // closing brace on its own line
		}
	}
	return pl
}

// spanHasNewline reports whether any newline trivia lies between token
// indexes a and b.
func spanHasNewline(f *token.File, a, b int) bool {
	for i := a; i <= b && i < len(f.Tokens); i++ {
		if f.Tokens[i].Kind == token.Newline {
			return true
		}
	}
	return false
}

// frame is one open bracket on the printer's stack.
type frame struct {
	kind     token.Kind // LParen, LBracket, LBrace
	literal  bool       // "({" array or "([" mapping literal
	isSwitch bool       // brace of a switch body
}

type printer struct {
	f    *token.File
	o    Options
	buf  bytes.Buffer
	nl   []byte
	plan plan

	stack      []frame
	prevSig    token.Kind
	prevSigIdx int  // token index of last significant token (-1 = none)
	prevLast   byte // last byte of last significant token
	lastUnary  bool // last emitted +,-,*,& was unary
	lastPrefix bool // last emitted ++/-- was prefix
	pendSwitch bool // saw 'switch', its brace not yet opened
	ternary    []int // stack depths of pending '?'s
}

func (p *printer) run() {
	var gap []token.Token
	for _, t := range p.f.Tokens {
		switch {
		case t.Kind.IsTrivia():
			gap = append(gap, t)
		case t.Kind == token.EOF:
			p.finish(gap)
			return
		default:
			p.renderGap(gap, t)
			gap = gap[:0]
			p.emit(t)
		}
	}
}

// gapComments reports whether the gap contains any comment.
func gapComments(gap []token.Token) bool {
	for _, g := range gap {
		if g.Kind == token.LineComment || g.Kind == token.BlockComment {
			return true
		}
	}
	return false
}

func gapNewlines(gap []token.Token) int {
	n := 0
	for _, g := range gap {
		if g.Kind == token.Newline {
			n++
		}
	}
	return n
}

// renderGap writes the trivia between the previous significant token and
// next, applying header plans, cuddling, blank-line capping, spacing
// normalization, and comment preservation.
func (p *printer) renderGap(gap []token.Token, next token.Token) {
	nextIdx := p.tokenIndex(next)
	hasComments := gapComments(gap)

	// Planned KNF header layout (headers with comments get no plan).
	if !hasComments && p.prevSigIdx >= 0 {
		if p.plan.joinBefore[nextIdx] {
			p.buf.WriteByte(' ')
			return
		}
		if p.plan.breakBefore[nextIdx] || p.plan.breakAfter[p.prevSigIdx] {
			p.writeNewline(nil)
			p.writeIndent(next)
			return
		}
	}

	// Cuddle control-flow braces and else.
	if !hasComments && gapNewlines(gap) > 0 && p.shouldCuddle(next, nextIdx) {
		p.buf.WriteByte(' ')
		return
	}

	// General path.
	pending := 0            // newlines seen but not yet written
	var pendingNl []token.Token // their tokens, for byte preservation
	var lastSpace []byte    // space run immediately before a comment
	wroteComment := false

	flushNewlines := func() {
		if pending == 0 {
			return
		}
		maxN := p.o.maxBlank() + 1
		if p.buf.Len() == 0 {
			maxN = pending // leading region: keep (still capped below)
			if maxN > p.o.maxBlank() {
				maxN = p.o.maxBlank()
			}
			if pending > 0 && maxN == 0 {
				maxN = 0
			}
		}
		n := pending
		if n > maxN {
			n = maxN
		}
		for i := 0; i < n; i++ {
			var orig []byte
			if i < len(pendingNl) {
				orig = p.f.Text(pendingNl[i])
			}
			p.writeNewline(orig)
		}
		pending = 0
		pendingNl = pendingNl[:0]
	}

	for _, g := range gap {
		switch g.Kind {
		case token.Newline:
			pending++
			pendingNl = append(pendingNl, g)
			lastSpace = nil
		case token.Space:
			lastSpace = p.f.Text(g)
		case token.LineComment, token.BlockComment:
			if pending > 0 {
				flushNewlines()
				p.writeCommentIndent()
			} else if p.buf.Len() > 0 {
				// Trailing comment: preserve the author's alignment run.
				if len(lastSpace) > 0 {
					p.buf.Write(lastSpace)
				} else {
					p.buf.WriteByte(' ')
				}
			}
			p.emitComment(g)
			wroteComment = true
			lastSpace = nil
		}
	}

	switch {
	case pending > 0:
		flushNewlines()
		p.writeIndent(next)
	case p.buf.Len() == 0:
		// Start of file: next begins at column 0.
	case wroteComment:
		p.buf.WriteByte(' ')
	case p.prevSigIdx >= 0:
		p.buf.WriteString(p.sep(next))
	}
}

// finish handles the trivia after the last significant token: trailing
// comments are kept, trailing blank lines dropped, and the file ends with
// exactly one newline.
func (p *printer) finish(gap []token.Token) {
	sawNewline := false
	var lastSpace []byte
	for _, g := range gap {
		switch g.Kind {
		case token.Newline:
			sawNewline = true
			lastSpace = nil
		case token.Space:
			lastSpace = p.f.Text(g)
		case token.LineComment, token.BlockComment:
			if p.buf.Len() > 0 {
				if sawNewline {
					p.writeNewline(nil)
				} else if len(lastSpace) > 0 {
					p.buf.Write(lastSpace)
				} else {
					p.buf.WriteByte(' ')
				}
			}
			p.emitComment(g)
			sawNewline = false
			lastSpace = nil
		}
	}
	if p.buf.Len() > 0 {
		p.writeNewline(nil)
	}
}

// tokenIndex locates t in f.Tokens by offset (tokens are contiguous).
func (p *printer) tokenIndex(t token.Token) int {
	// Binary search over offsets.
	lo, hi := 0, len(p.f.Tokens)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		switch {
		case p.f.Tokens[mid].Off < t.Off:
			lo = mid + 1
		case p.f.Tokens[mid].Off > t.Off:
			hi = mid - 1
		default:
			return mid
		}
	}
	return -1
}

// shouldCuddle reports whether next, currently on its own line, should be
// joined to the previous line: control-flow '{' after ')', else/do/try
// braces, and 'else' after '}'.
func (p *printer) shouldCuddle(next token.Token, nextIdx int) bool {
	switch next.Kind {
	case token.LBrace:
		if p.plan.bodyBrace[nextIdx] || p.prevSig == token.LParen {
			return false
		}
		switch p.prevSig {
		case token.RParen, token.KwElse, token.KwDo, token.KwTry:
			return true
		}
	case token.KwElse:
		return p.prevSig == token.RBrace
	}
	return false
}

func (p *printer) writeNewline(orig []byte) {
	switch p.o.LineEndings {
	case LF:
		p.buf.WriteByte('\n')
	case CRLF:
		p.buf.WriteString("\r\n")
	default:
		if orig == nil {
			p.buf.Write(p.nl)
		} else {
			p.buf.Write(orig)
		}
	}
}

// braceLevel counts open braces.
func (p *printer) braceLevel() int {
	n := 0
	for _, fr := range p.stack {
		if fr.kind == token.LBrace {
			n++
		}
	}
	return n
}

// innermostBrace returns the top-most brace frame, or nil.
func (p *printer) innermostBrace() *frame {
	for i := len(p.stack) - 1; i >= 0; i-- {
		if p.stack[i].kind == token.LBrace {
			return &p.stack[i]
		}
	}
	return nil
}

// writeIndent writes the indentation for a line whose first token is t.
func (p *printer) writeIndent(t token.Token) {
	if t.Kind == token.Directive {
		return // directives start at column 0
	}
	level := p.braceLevel()
	switch t.Kind {
	case token.RBrace:
		if level > 0 {
			level--
		}
	case token.RParen, token.RBracket:
		// Closing a continuation bracket sits at the base level.
	case token.KwCase, token.KwDefault:
		if br := p.innermostBrace(); br != nil && br.isSwitch && level > 0 {
			level--
		}
	default:
		if p.continuation() {
			level++
		}
	}
	p.pad(level)
}

// writeCommentIndent indents a line that starts with a comment.
func (p *printer) writeCommentIndent() {
	p.pad(p.braceLevel())
}

func (p *printer) pad(level int) {
	n := level * p.o.indent()
	for i := 0; i < n; i++ {
		p.buf.WriteByte(' ')
	}
}

// continuation reports whether the upcoming line continues an unfinished
// construct: an unclosed paren/bracket, or a previous line ending in a
// binary/assignment operator.
func (p *printer) continuation() bool {
	if len(p.stack) > 0 {
		if k := p.stack[len(p.stack)-1].kind; k == token.LParen || k == token.LBracket {
			return true
		}
	}
	return trailingOpContinues(p.prevSig)
}

// trailingOpContinues lists the operators that, at end of line, mark the
// next line as a continuation. Comma and colon are excluded deliberately:
// comma separates elements at the current level, colon ends case labels.
func trailingOpContinues(k token.Kind) bool {
	switch k {
	case token.Plus, token.Minus, token.Star, token.Slash, token.Percent,
		token.Lt, token.Gt, token.LtEq, token.GtEq, token.EqEq, token.NotEq,
		token.LAnd, token.LOr, token.Amp, token.Pipe, token.Caret,
		token.Shl, token.Shr, token.Assign,
		token.PlusEq, token.MinusEq, token.StarEq, token.SlashEq,
		token.PercentEq, token.AmpEq, token.PipeEq, token.CaretEq,
		token.ShlEq, token.ShrEq,
		token.Question, token.Arrow, token.ColonColon, token.LArrow:
		return true
	}
	return false
}

// emit writes a significant token and updates layout state.
func (p *printer) emit(t token.Token) {
	text := p.f.Text(t)

	switch t.Kind {
	case token.KwSwitch:
		p.pendSwitch = true
	case token.LParen:
		p.stack = append(p.stack, frame{kind: t.Kind})
	case token.LBracket:
		p.stack = append(p.stack, frame{kind: t.Kind, literal: p.prevSig == token.LParen})
	case token.LBrace:
		lit := p.prevSig == token.LParen
		p.stack = append(p.stack, frame{
			kind: token.LBrace, literal: lit,
			isSwitch: p.pendSwitch && !lit,
		})
		if !lit {
			p.pendSwitch = false
		}
	case token.RParen, token.RBracket, token.RBrace:
		if n := len(p.stack); n > 0 && p.stack[n-1].kind == opener(t.Kind) {
			p.stack = p.stack[:n-1]
		}
	case token.Question:
		p.ternary = append(p.ternary, len(p.stack))
	case token.Colon:
		if n := len(p.ternary); n > 0 && p.ternary[n-1] == len(p.stack) {
			p.ternary = p.ternary[:n-1]
		}
	case token.Plus, token.Minus, token.Star, token.Amp:
		p.lastUnary = !isOperand(p.prevSig)
	case token.Inc, token.Dec:
		p.lastPrefix = !isOperand(p.prevSig)
	}

	if t.Kind == token.BlockComment {
		// unreachable: comments come through emitComment
		p.emitComment(t)
		return
	}
	p.buf.Write(text)
	p.prevSig = t.Kind
	p.prevSigIdx = p.tokenIndex(t)
	if len(text) > 0 {
		p.prevLast = text[len(text)-1]
	}
}

func opener(closer token.Kind) token.Kind {
	switch closer {
	case token.RParen:
		return token.LParen
	case token.RBracket:
		return token.LBracket
	default:
		return token.LBrace
	}
}

// isOperand reports whether k can end an expression operand — used to
// disambiguate unary from binary +,-,*,& and pre from postfix ++/--.
func isOperand(k token.Kind) bool {
	switch k {
	case token.Ident, token.IntLit, token.FloatLit, token.StringLit,
		token.CharLit, token.RParen, token.RBracket, token.RBrace,
		token.Inc, token.Dec, token.KwNil:
		return true
	}
	return false
}

// colonIsTernary reports whether the NEXT colon closes a pending '?'.
func (p *printer) colonIsTernary() bool {
	n := len(p.ternary)
	return n > 0 && p.ternary[n-1] == len(p.stack)
}

// topLiteral reports whether the innermost open frame is a literal of the
// given bracket kind.
func (p *printer) topLiteral(kind token.Kind) bool {
	if n := len(p.stack); n > 0 {
		fr := p.stack[n-1]
		return fr.kind == kind && fr.literal
	}
	return false
}

// sep decides the canonical same-line spacing between the previously
// emitted significant token and next: "" or " ".
func (p *printer) sep(next token.Token) string {
	s := p.sepRaw(next)
	if s == "" && p.mergeRisk(next) {
		return " "
	}
	return s
}

func (p *printer) sepRaw(next token.Token) string {
	prev := p.prevSig
	nk := next.Kind

	// Separators and closers bind tight.
	switch nk {
	case token.Comma, token.Semicolon:
		return ""
	case token.RParen:
		return ""
	case token.RBracket:
		if p.topLiteral(token.LBracket) {
			return " " // ([ ... ])
		}
		return ""
	case token.RBrace:
		return " " // ({ ... }) padding and one-line blocks alike
	}

	// After openers.
	switch prev {
	case token.LParen:
		return "" // (x, ((, ({, ([
	case token.LBracket:
		if p.topLiteral(token.LBracket) {
			return " " // ([ "k" ...
		}
		return ""
	case token.LBrace:
		return " " // ({ "a" — and one-line blocks
	}

	// Tight glue operators.
	if nk == token.Arrow || prev == token.Arrow ||
		nk == token.Dot || prev == token.Dot ||
		nk == token.DotDot || prev == token.DotDot ||
		nk == token.Ellipsis {
		return ""
	}
	if nk == token.ColonColon {
		if prev == token.Ident { // label::func
			return ""
		}
		return " "
	}
	if prev == token.ColonColon {
		return ""
	}

	// Increment/decrement.
	if nk == token.Inc || nk == token.Dec {
		if isOperand(prev) {
			return "" // postfix
		}
		return " "
	}
	if (prev == token.Inc || prev == token.Dec) && p.lastPrefix {
		return "" // ++i
	}

	// Unary operators bind tight to their operand.
	if prev == token.Not || prev == token.Tilde {
		return ""
	}
	if (prev == token.Plus || prev == token.Minus || prev == token.Star || prev == token.Amp) && p.lastUnary {
		return ""
	}

	// Colons: ternary spaced; case/label/mapping colons tight on the left.
	if nk == token.Colon {
		if p.colonIsTernary() {
			return " "
		}
		return ""
	}
	if prev == token.Colon || nk == token.Question || prev == token.Question {
		return " "
	}

	// Keywords space out from their neighbors.
	if prev.IsKeyword() || nk.IsKeyword() {
		return " "
	}

	// Calls and indexing.
	if nk == token.LParen || nk == token.LBracket {
		if isOperand(prev) {
			return ""
		}
		return " "
	}
	if nk == token.LBrace {
		return " "
	}

	// Binary operators.
	if isBinaryOp(nk) || isBinaryOp(prev) {
		return " "
	}

	return " "
}

func isBinaryOp(k token.Kind) bool {
	switch k {
	case token.Plus, token.Minus, token.Star, token.Slash, token.Percent,
		token.Lt, token.Gt, token.LtEq, token.GtEq, token.EqEq, token.NotEq,
		token.LAnd, token.LOr, token.Amp, token.Pipe, token.Caret,
		token.Shl, token.Shr, token.LArrow, token.Assign,
		token.PlusEq, token.MinusEq, token.StarEq, token.SlashEq,
		token.PercentEq, token.AmpEq, token.PipeEq, token.CaretEq,
		token.ShlEq, token.ShrEq:
		return true
	}
	return false
}

// mergeRisk reports whether writing next directly after the previous token
// could fuse them into a different token. The gate would catch any miss;
// this keeps it from ever tripping.
func (p *printer) mergeRisk(next token.Token) bool {
	if next.Len() == 0 {
		return false
	}
	a, b := p.prevLast, p.f.Text(next)[0]
	if isWordByte(a) && isWordByte(b) {
		return true
	}
	switch a {
	case '+':
		return b == '+' || b == '='
	case '-':
		return b == '-' || b == '=' || b == '>'
	case '<':
		return b == '<' || b == '=' || b == '-'
	case '>':
		return b == '>' || b == '='
	case '=', '!', '^', '*', '%':
		return b == '='
	case '&':
		return b == '&' || b == '='
	case '|':
		return b == '|' || b == '='
	case '/':
		return b == '=' || b == '*' || b == '/'
	case '.':
		if b == '.' {
			return true
		}
		// Only a lone '.' fuses with a digit (into a float); ".." + digit
		// relexes to DotDot IntLit unchanged.
		return p.prevSig == token.Dot && b >= '0' && b <= '9'
	case ':':
		return b == ':'
	case '#':
		return b == '#'
	}
	return false
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// emitComment writes a comment; multi-line block comments are shifted
// whole: every interior line moves by the same column delta as the "/*"
// line, preserving internal alignment.
func (p *printer) emitComment(t token.Token) {
	text := p.f.Text(t)
	if t.Kind == token.BlockComment && bytes.ContainsAny(text, "\r\n") {
		p.emitShiftedComment(t, text)
		return
	}
	p.buf.Write(text)
}

func (p *printer) emitShiftedComment(t token.Token, text []byte) {
	oldCol := int(t.Off - p.f.Lines[p.f.Pos(t.Off).Line-1])
	newCol := lineLen(p.buf.Bytes())
	delta := newCol - oldCol

	lines := splitKeepEndings(text)
	p.buf.Write(lines[0])
	for _, line := range lines[1:] {
		switch {
		case delta > 0:
			for i := 0; i < delta; i++ {
				p.buf.WriteByte(' ')
			}
			p.buf.Write(line)
		case delta < 0:
			p.buf.Write(trimLeadingWS(line, -delta))
		default:
			p.buf.Write(line)
		}
	}
}

// lineLen returns the length in bytes of the last (current) line of buf.
func lineLen(buf []byte) int {
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		return len(buf) - i - 1
	}
	return len(buf)
}

// splitKeepEndings splits text after each newline, keeping the terminators
// attached to the preceding piece.
func splitKeepEndings(text []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			out = append(out, text[start:i+1])
			start = i + 1
		}
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}

// trimLeadingWS removes up to n leading space/tab bytes from line.
func trimLeadingWS(line []byte, n int) []byte {
	i := 0
	for i < len(line) && i < n && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return line[i:]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
