// Package format implements the dgdfmt layout engine.
//
// The engine is a trivia-only rewriter: it emits every significant token's
// bytes verbatim and synthesizes only the whitespace between them. Safety is
// enforced twice — structurally (there is no code path that alters a
// significant token) and by the gate in Format, which re-lexes the output,
// requires token-stream equality with the input (tokcmp), and requires
// idempotence, before any byte is returned.
//
// This is the milestone-3 engine: indentation, tabs-to-spaces, trailing
// whitespace removal, final newline, and line-ending policy. It preserves
// the author's line breaks and mid-line spacing; KNF function headers,
// switch/case frames, blank-line policy, and horizontal spacing
// normalization are later milestones.
package format

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/mmcdole/dgdtools/pkg/lexer"
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

// Options carries the formatter's (deliberately few) toggles. The dialect
// is not an option here: Format always relexes its output under the same
// dialect the input File was lexed with.
type Options struct {
	Indent      int // spaces per level; 0 means the default 4
	LineEndings LineEndings
}

func (o Options) indent() int {
	if o.Indent <= 0 {
		return 4
	}
	return o.Indent
}

// ErrIllegal is returned for files that did not fully tokenize; dgdfmt
// refuses to format anything it could not prove lossless.
var ErrIllegal = errors.New("file contains unlexable content; refusing to format")

// ErrGateFailure means the engine produced output whose significant token
// stream differs from the input — an engine bug caught before writing.
var ErrGateFailure = errors.New("formatter gate failure (output tokens diverge from input)")

// ErrNotIdempotent means formatting its own output changed it again.
var ErrNotIdempotent = errors.New("formatter is not idempotent on this input")

// Format returns the formatted source, or the original error if the gate
// trips. The input File is not modified.
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
		f:        f,
		o:        o,
		nl:       dominantNewline(f),
		atStart:  true,
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

type printer struct {
	f   *token.File
	o   Options
	buf bytes.Buffer
	nl  []byte // dominant line ending, for synthesized newlines

	stack   []token.Kind // open brackets: LParen, LBracket, LBrace
	prevSig token.Kind   // last significant token emitted (0 = none)
	atStart bool         // next emit is at the start of a line
}

func (p *printer) run() {
	toks := p.f.Tokens
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch t.Kind {
		case token.EOF:
			// Ensure exactly one final newline on non-empty output.
			if p.buf.Len() > 0 && !p.atStart {
				p.writeNewline(nil)
			}

		case token.Space:
			if p.atStart {
				continue // replaced by computed indentation
			}
			if isTrailing(toks, i) {
				continue // trailing whitespace: dropped
			}
			p.buf.Write(p.f.Text(t)) // mid-line spacing preserved (v1)

		case token.Newline:
			p.writeNewline(p.f.Text(t))

		default:
			if p.atStart {
				p.writeIndent(t)
			}
			p.emitToken(t)
		}
	}
}

// isTrailing reports whether the Space token at index i is followed only by
// a Newline (or EOF).
func isTrailing(toks []token.Token, i int) bool {
	if i+1 < len(toks) {
		k := toks[i+1].Kind
		return k == token.Newline || k == token.EOF
	}
	return true
}

func (p *printer) writeNewline(orig []byte) {
	switch p.o.LineEndings {
	case LF:
		p.buf.WriteByte('\n')
	case CRLF:
		p.buf.Write([]byte("\r\n"))
	default:
		if orig == nil {
			p.buf.Write(p.nl)
		} else {
			p.buf.Write(orig)
		}
	}
	p.atStart = true
}

// writeIndent computes and writes the indentation for a line whose first
// token is t.
func (p *printer) writeIndent(t token.Token) {
	p.atStart = false
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
	default:
		if p.continuation() {
			level++
		}
	}
	if level > 0 {
		p.buf.Write(bytes.Repeat([]byte{' '}, level*p.o.indent()))
	}
}

func (p *printer) braceLevel() int {
	n := 0
	for _, k := range p.stack {
		if k == token.LBrace {
			n++
		}
	}
	return n
}

// continuation reports whether the upcoming line continues an unfinished
// construct: an unclosed paren/bracket, or a previous line ending in a
// binary/assignment operator.
func (p *printer) continuation() bool {
	if len(p.stack) > 0 {
		if k := p.stack[len(p.stack)-1]; k == token.LParen || k == token.LBracket {
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

// emitToken writes a significant token or comment, tracking bracket depth.
// Multi-line block comments are shifted whole: every interior line moves by
// the same column delta as the "/*" line, preserving internal alignment.
func (p *printer) emitToken(t token.Token) {
	text := p.f.Text(t)

	switch t.Kind {
	case token.LParen, token.LBracket, token.LBrace:
		p.stack = append(p.stack, t.Kind)
	case token.RParen, token.RBracket, token.RBrace:
		if n := len(p.stack); n > 0 && p.stack[n-1] == opener(t.Kind) {
			p.stack = p.stack[:n-1]
		}
	}

	if t.Kind == token.BlockComment && bytes.ContainsAny(text, "\r\n") {
		p.emitShiftedComment(t, text)
	} else {
		p.buf.Write(text)
	}

	if !t.Kind.IsTrivia() {
		p.prevSig = t.Kind
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

// emitShiftedComment writes a multi-line block comment, adjusting each
// interior line's leading whitespace by the column delta between the
// comment's original position and where it is being emitted now.
func (p *printer) emitShiftedComment(t token.Token, text []byte) {
	oldCol := int(t.Off - p.f.Lines[p.f.Pos(t.Off).Line-1])
	newCol := lineLen(p.buf.Bytes())
	delta := newCol - oldCol

	lines := splitKeepEndings(text)
	p.buf.Write(lines[0])
	for _, line := range lines[1:] {
		switch {
		case delta > 0:
			p.buf.Write(bytes.Repeat([]byte{' '}, delta))
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
