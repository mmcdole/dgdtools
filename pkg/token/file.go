package token

import (
	"fmt"
	"iter"
	"sort"

	"github.com/mmcdole/dgdtools/pkg/diag"
)

// Dialect selects the compile-time language variants of the target driver.
// It is recorded on File so downstream tools (the formatter's relex gate in
// particular) always operate under the same dialect the file was lexed with.
type Dialect struct {
	SlashSlash bool // allow // line comments (DGD SLASHSLASH build flag)
	Closures   bool // reserve "function" as a keyword (DGD CLOSURES build flag)
}

// File is the result of lexing one source file: the raw bytes plus a flat,
// contiguous token stream covering every byte.
type File struct {
	Path    string
	Src     []byte
	Dialect Dialect
	Tokens  []Token // contiguous; last element is a zero-length EOF token
	Lines   []uint32 // byte offset of each line start; Lines[0] == 0
	Errs    []diag.Diagnostic
}

// Text returns the token's bytes as a zero-copy subslice of Src.
func (f *File) Text(t Token) []byte { return f.Src[t.Off:t.End] }

// Significant iterates over non-trivia, non-EOF tokens with their indexes
// in f.Tokens. Directive tokens are significant.
func (f *File) Significant() iter.Seq2[int, Token] {
	return func(yield func(int, Token) bool) {
		for i, t := range f.Tokens {
			if t.Kind.IsTrivia() || t.Kind == EOF {
				continue
			}
			if !yield(i, t) {
				return
			}
		}
	}
}

// HasIllegal reports whether lexing produced any Illegal token.
func (f *File) HasIllegal() bool {
	for _, t := range f.Tokens {
		if t.Kind == Illegal {
			return true
		}
	}
	return false
}

// Pos maps a byte offset to a 1-based line/column position.
func (f *File) Pos(off uint32) Pos {
	i := sort.Search(len(f.Lines), func(i int) bool { return f.Lines[i] > off }) - 1
	if i < 0 {
		i = 0
	}
	return Pos{Line: i + 1, Col: int(off-f.Lines[i]) + 1}
}

// CheckRoundTrip verifies the lossless invariant: tokens are contiguous,
// start at offset 0, and end exactly at len(Src). A violation is a lexer
// bug, never a property of the input.
func (f *File) CheckRoundTrip() error {
	if len(f.Tokens) == 0 {
		return fmt.Errorf("%s: no tokens (missing EOF)", f.Path)
	}
	last := f.Tokens[len(f.Tokens)-1]
	if last.Kind != EOF || last.Off != last.End {
		return fmt.Errorf("%s: stream does not end in zero-length EOF", f.Path)
	}
	var off uint32
	for i, t := range f.Tokens {
		if t.Off != off {
			return fmt.Errorf("%s: token %d (%s) starts at %d, want %d (gap or overlap)",
				f.Path, i, t.Kind, t.Off, off)
		}
		if t.End < t.Off {
			return fmt.Errorf("%s: token %d (%s) has negative length", f.Path, i, t.Kind)
		}
		off = t.End
	}
	if off != uint32(len(f.Src)) {
		return fmt.Errorf("%s: tokens cover %d bytes, source has %d", f.Path, off, len(f.Src))
	}
	return nil
}
