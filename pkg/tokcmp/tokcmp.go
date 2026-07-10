// Package tokcmp compares the significant token streams of two lexed files.
//
// Two files compare equal when their non-trivia tokens match pairwise in
// kind and exact bytes. Whitespace, newlines, and comments are ignored —
// so equality proves a diff is formatting-only and cannot have changed
// program behavior. This is the safety gate for dgdfmt: the formatter
// refuses to write output whose token stream diverges from its input.
//
// The only permitted normalization is trailing space/tab trimming at the
// end of a preprocessor Directive token; everything else, including string
// escapes and numeric literal spellings, is compared byte-exact.
package tokcmp

import (
	"bytes"

	"github.com/mmcdole/dgdtools/pkg/token"
)

// Divergence describes the first point where two token streams differ.
// A zero-kind (Illegal) token with Missing set means one stream ended early.
type Divergence struct {
	AIndex, BIndex int // indexes into the respective File.Tokens
	A, B           token.Token
	APos, BPos     token.Pos
	AMissing       bool // stream A ended before B
	BMissing       bool // stream B ended before A
}

// Compare walks both significant streams and reports the first divergence,
// or (true, nil) if the streams are token-identical.
func Compare(a, b *token.File) (bool, *Divergence) {
	next := func(f *token.File, i int) (int, token.Token) {
		for ; i < len(f.Tokens); i++ {
			t := f.Tokens[i]
			if !t.Kind.IsTrivia() && t.Kind != token.EOF {
				return i, t
			}
		}
		return -1, token.Token{}
	}

	ai, bi := 0, 0
	for {
		aIdx, at := next(a, ai)
		bIdx, bt := next(b, bi)

		switch {
		case aIdx < 0 && bIdx < 0:
			return true, nil
		case aIdx < 0:
			return false, &Divergence{
				AIndex: -1, BIndex: bIdx, B: bt,
				APos: a.Pos(uint32(len(a.Src))), BPos: b.Pos(bt.Off),
				AMissing: true,
			}
		case bIdx < 0:
			return false, &Divergence{
				AIndex: aIdx, BIndex: -1, A: at,
				APos: a.Pos(at.Off), BPos: b.Pos(uint32(len(b.Src))),
				BMissing: true,
			}
		}

		if !tokensEqual(a, at, b, bt) {
			return false, &Divergence{
				AIndex: aIdx, BIndex: bIdx, A: at, B: bt,
				APos: a.Pos(at.Off), BPos: b.Pos(bt.Off),
			}
		}
		ai, bi = aIdx+1, bIdx+1
	}
}

func tokensEqual(a *token.File, at token.Token, b *token.File, bt token.Token) bool {
	if at.Kind != bt.Kind {
		return false
	}
	ta, tb := a.Text(at), b.Text(bt)
	if at.Kind == token.Directive {
		ta = trimTrailingBlanks(ta)
		tb = trimTrailingBlanks(tb)
	}
	return bytes.Equal(ta, tb)
}

func trimTrailingBlanks(s []byte) []byte {
	return bytes.TrimRight(s, " \t")
}

// Context returns up to n significant tokens on each side of token index i,
// rendered as strings — for divergence display.
func Context(f *token.File, i, n int) (before, after []string) {
	if i < 0 {
		return nil, nil
	}
	for j := i - 1; j >= 0 && len(before) < n; j-- {
		t := f.Tokens[j]
		if !t.Kind.IsTrivia() && t.Kind != token.EOF {
			before = append([]string{string(f.Text(t))}, before...)
		}
	}
	for j := i + 1; j < len(f.Tokens) && len(after) < n; j++ {
		t := f.Tokens[j]
		if !t.Kind.IsTrivia() && t.Kind != token.EOF {
			after = append(after, string(f.Text(t)))
		}
	}
	return before, after
}
