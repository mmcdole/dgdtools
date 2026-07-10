# dgdtools

Source tooling for the [DGD](https://github.com/dworkin/dgd) dialect of LPC:

- **dgdfmt** — a formatter in the gofmt tradition: one style, a handful of
  toggles (`-indent`, `-line-endings`), mode flags `-l` / `-d` / `-w`.
- **dgdcmp** — proves a diff is formatting-only by comparing significant
  token streams (whitespace and comments ignored, everything else
  byte-exact). Also a lexer debugger (`-dump`).
- **dgdlint** — a linter (under construction).

Existing LPC tooling targets LDMud/FluffOS and mishandles DGD syntax —
`({ })` / `([ ])` literals, `<-` instanceof, `..` ranges, labeled inherits,
K&R-style typeless functions. These tools implement the DGD 1.7 dialect,
with driver build variants (`SLASHSLASH` line comments, `CLOSURES`) exposed
as flags.

## Safety model

dgdfmt must be provably behavior-preserving, because LPC codebases are live
game worlds with decades of accumulated code:

1. The lexer is **byte-lossless**: every byte of a file belongs to exactly
   one token; whitespace and comments are tokens too. Reassembling the
   stream reproduces the file byte-for-byte. Bytes are never transcoded —
   Latin-1, UTF-8, ANSI escapes in string literals, and CRLF endings pass
   through untouched.
2. The formatter is a **trivia-only rewriter**: it emits significant tokens
   verbatim and synthesizes only the whitespace between them.
3. Every output is **gated** before it is returned: re-lexed, required to be
   token-stream identical to the input (dgdcmp's comparison), and required
   to be idempotent. A file that trips the gate is left untouched.
4. Files that do not fully tokenize (dead files, prose saved as `.c`) are
   refused, never guessed at.

## Usage

```
dgdfmt file.c              # print formatted source
dgdfmt -l lib/             # list files that would change
dgdfmt -d file.c           # show a diff
dgdfmt -w lib/             # rewrite in place (gated)
dgdcmp old.c new.c         # exit 0 iff the diff is formatting-only
dgdcmp -dump file.c        # token stream dump
```

## Testing

`make test` runs the unit suite. `make corpus LPC_CORPUS=/path/to/lib`
runs the full-tree invariants against a real mudlib: lex round-trip
byte-equality, comparator self-compare, and the formatter gate on every
file.

## Clean-room note

The DGD dialect facts implemented here (keyword list, operator inventory,
literal and escape forms, preprocessor behavior) were taken as facts from
DGD's grammar and documentation. No DGD source code (AGPLv3) has been
copied or translated into this repository.
