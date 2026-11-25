# The formatter: design

`dgdtools format` and `pkg/format`. How the engine lays a file out (the
plan from the syntax tree, the printer, every `#if` arm, the fallback
where the parser gives up) is in [architecture.md](architecture.md),
section 8. This document is about what the tool is: its model, its
options, and how it is driven.

## 1. What the others do

| Tool | Model | Options | Line breaks | Skip | Partial |
|---|---|---|---|---|---|
| gofmt | prints the syntax tree, keeps comments | none | the author's; nothing is wrapped; blank-line runs collapse to one | none | none |
| black | reflows to a line length | line length, a few toggles; "uncompromising" by charter | its own | `# fmt: off` / `on`, `# fmt: skip` | `--line-ranges` (24.1) |
| ruff format | black's style, reimplemented | line length, indent style and width, quote style, line ending; nothing else by policy | its own | black's comments | `--range` (one file) |
| Prettier | reflows to a print width | a deliberately frozen handful; the "option philosophy" page explains that options were a mistake of adoption and no more are taken | its own | `// prettier-ignore` (next node), range pragmas in some languages | `--range-start/--range-end` |
| rustfmt | reflows | a small stable set; dozens of options that have been unstable for years because each must work with every other | its own | `#[rustfmt::skip]` | `--file-lines` (unstable) |
| clang-format | reflows by penalties | 150+ options and `BasedOnStyle` presets; the counterexample: two teams' "clang-format" outputs have nothing in common, and bugs live in option interactions | its own | `// clang-format off` / `on` | `--lines`, `git clang-format`; indentation adapts to the unformatted lines around |
| Biome | Prettier's style | few; `formatter` and `linter` are sections of one `biome.json` | its own | `// biome-ignore format: reason` | no |

Three things recur in the tools people do not fight with:

- **The value is that there is one style.** Every option divides the
  code a tool has been tested on and the people who can read each
  other's diffs. Prettier and black say it outright; rustfmt and
  clang-format show the cost.
- **The options that survive everywhere are the ones a formatter cannot
  decide for you because an editor must agree with it:** indentation
  (tabs or spaces, how wide) and line endings. Those are also exactly
  what `.editorconfig` states.
- **Old code needs a way in:** a skip marker for the table somebody
  aligned by hand, a check mode for CI, and a way to format only what
  was touched.

## 2. Decisions

### 2.1 The model: trivia only, behind a gate

The formatter writes every significant token's bytes unchanged and
chooses only the whitespace between them. The result is returned only if
it re-lexes to the same token stream (`pkg/tokcmp`), has the same
comments in the same order (the further lines of a block comment may
begin at another column, and nothing else may differ), and formatting it
again changes nothing.

This is gofmt's contract (a formatter cannot change what the program
means) made checkable per file, and it is the right one here for reasons
the reflowing formatters do not have:

- The corpus is a hundred thousand files written by hand over thirty
  years, many of which do not parse (macros from headers that are not
  there, dead dialects). A formatter that must print from a tree cannot
  touch them; one that rearranges tokens cannot be trusted on them. A
  trivia-only formatter with a token-equality gate can do both.
- The gate turns "we believe the formatter is safe" into something a
  whole-lib run proves: 0 gate failures over 180,850 files.

### 2.2 No line wrapping

Lines are never broken for length and never joined: the author's line
breaks are kept, as in gofmt, function headers included (2.3), and so
are braces and `else`s on lines of their own (`if (x)` with its `{` on
the next line stays that way, and so does `}` with `else` under it;
K&R, Allman and GNU layouts all come out as written, indented by the
one rule). The only line breaks the formatter removes are those of
blank lines beyond two in a row and at the end of a file.

The one rule for a line inside brackets: it is indented from the line
the bracket is opened on, one level for a literal's elements or a
block's statements, one level as a continuation inside a parenthesis or
an index, and the closer on a line of its own stands at the level of
the opener's line. A literal that opens on a line continuing a call is
indented from that line, and the arguments of a call inside a call's
arguments one level further than that call. A bracket opened on the same
line as one it is inside counts from the lines inside that one, so
`({ ([` on one line puts the mapping's lines two levels in.

Joining a lone `{` or `else` onto the line above was the last place the
formatter moved a line, and it was taken out for the same reason as the
header layout: it is a style of new code, not a layout everyone agrees
on, and on this corpus it was most of what was left of the edits that
change lines. Over a 5,205-file sample lib, 607 files had a non-blank
line joined to another; over the 180,850-file VikingMUD lib, 21,200
did. Now none does in either: every line edit left removes blank lines
(in 1,507 files of the sample, 58,025 of the lib).

Why not wrap:

- Wrapping needs a style for every construct (where to break a call, a
  chain of `+` over string literals, a mapping literal) and then
  everybody's hand-laid descriptions and tables are reflowed to it. On
  this corpus that is most of the text in the lib, changed for no gain.
- A wrapper has to understand everything it wraps. Where the parser
  gives up, the formatter moves no line break at all, which is
  what keeps it a fixed point; a wrapper would need a second, weaker
  layout for those regions.
- Width-driven output is unstable under small edits (one more argument
  reflows the statement), which is what makes blame noisy.

A future opt-in (`format.line_width`, off by default) is possible
without changing the model: breaking and joining lines is still trivia.
It would be a second engine pass over statements the tree understands,
and it is not planned.

### 2.3 The option set

| Key (`format:`) | Values | Default | Also from |
|---|---|---|---|
| `indent_style` | `space`, `tab` | `space` | `.editorconfig` `indent_style` |
| `indent_width` | 1 to 16 | 4 | `.editorconfig` `indent_size` (`tab_width` when it says `tab`) |
| `line_endings` | `preserve`, `lf`, `crlf` | `preserve` | `.editorconfig` `end_of_line` |

That is all. `indent_width` is the number of spaces per level with
`indent_style: space` and is not used with `tab` (one tab per level;
nothing in the engine measures columns). A multi-line comment moves with
its first line: the indentation that line had is replaced, on every line
that begins with it, by the indentation it gets, so a comment indented
with tabs comes out indented with spaces and the reverse.

`line_endings: preserve` keeps each line break's bytes and gives a new
one the file's dominant form. It is the default because a lib that came
through thirty years of FTP has both, and a formatter should not make
every file differ in every line the first time it runs.

Two things that could be options are the house style instead:

- **Function headers: as written.** The formatter normalizes the
  spacing in a header (`string * f( int x )` becomes `string *f(int x)`)
  and never where it breaks: a return type on its own line stays there,
  `static void create() {` stays on one line, and a definition written
  on one line (`int query_x() { return x; }`) stays one line. This is
  gofmt's camp: a formatter that only chooses spacing and indentation is
  one nobody has to argue with, and on this corpus forcing one header
  layout was most of the line breaks the formatter moved (28,505 of the
  edits over a 5,205-file sample lib changed lines; without it 4,535
  do). An earlier version forced KNF (the name at column 0, so that
  `grep '^name('` finds a definition); that is a reason for a style of
  new code, which, if it is wanted, belongs in an opt-in lint rule, not
  in the formatter. It is not built.
- **Blank lines: at most two in a row, none at the end of a file,
  which ends with exactly one newline.** gofmt, Prettier and clang-format
  keep one; black keeps two between top-level definitions. Two is kept
  because the corpus separates functions and sections of declarations
  with two, deliberately, and one would rewrite that in most files for
  nothing. A knob for it has no user: nobody's style depends on three.

The dialect (`//` comments, closures) and the macro conventions
(`conventions.macros`, `conventions.defines`) are not format
options. They are facts about the project, they come from the project's
configuration or its profile, and the formatter reads them like every
other tool.

### 2.4 Skip markers

```c
/* dgdtools-ignore-start format */
mapping exits = ([ "north" : "/room/hall",
                   "south" : "/room/yard",   ]);
/* dgdtools-ignore-end format */
```

A comment that begins with `dgdtools-ignore-start` and names `format`
turns the formatter off from that comment on; `dgdtools-ignore-end
format` (or an end that names nothing) turns it back on after it. Either
comment form works (`//` needs a driver with `SLASHSLASH`, so the block
form is the one to document). Between the two, every byte is kept as it
is. Without an end the rest of the file is kept: that is the whole-file
form, for generated files that are not excluded by path.

- A range, not a next-statement marker: the regions people protect are
  hand-aligned tables and ASCII art, which are ranges of lines, and one
  mechanism is enough. clang-format and black have the same pair.
- The comment is one of the tool's directives (`pkg/directive`; the
  grammar and why there is one prefix for linter and formatter are in
  [architecture.md](architecture.md), section 4). The formatter
  reads the `format` target and nothing else; the form for a single
  line, `dgdtools-ignore format`, is not defined for it, and the linter
  says so (`invalid-suppression`).
- A directive is the first thing in its comment, so prose that mentions
  one is not one.
- The tokens in a skipped region still go through the engine (its
  bracket and conditional state must stay right for the code after), and
  the gate still covers the whole file.

### 2.5 Check, diff, standard input

```
dgdtools format PATH...            rewrite the files in place
dgdtools format --check PATH...    write nothing; list the files that would change
dgdtools format --diff PATH...     write nothing; print unified diffs
dgdtools format -                  standard input to standard output
dgdtools format --stdin-filename usr/ann/room.c     (implies -)
```

- **In place by default**, as ruff format, black, rustfmt and `gofmt -w`
  users expect of a command named `format`. A path is required: a bare
  `dgdtools format` in a lib of 100,000 files should not be one keypress
  from rewriting all of them (black's rule; `.` is not much to type).
- **Exit codes:** 0 nothing to do or done; 1 `--check` or `--diff` found
  files that would change; 2 usage, configuration or I/O error, or a
  file that could not be formatted (it does not tokenize, or the gate
  tripped). An error wins over 1. Same numbers as ruff and Prettier, and
  as `dgdtools lint`.
- `--check` prints one path per line on standard output (what
  `gofmt -l` prints, so scripts can read it); `--diff` prints unified
  diffs there. The summary (`3 files reformatted, 120 files unchanged`)
  goes to standard error and `--quiet` drops it. With `-`, the default
  mode prints the formatted text and nothing else.
- `--stdin-filename` names the buffer: settings are discovered from that
  path (project, nested scopes, `.editorconfig`) and messages use it.
  The file need not exist. This is what an editor that pipes a buffer
  needs, and what ruff, Prettier and Biome call it.
- A file named explicitly is formatted even if `exclude` matches it;
  excludes prune directory walks.

### 2.6 Formatting part of a file

Two users: an editor (`textDocument/rangeFormatting`, format-modified-
lines-on-save) and somebody who changed one function in a legacy file
and does not want the other two thousand lines in the diff.

**Library.** `format.Lines(file, options, []LineRange)` formats only
the given lines (1-based, inclusive) and returns the whole text.
`format.Edits(file, out)` turns any result into the minimal list of
replacements, one per whitespace gap that changed, which is what an LSP
server returns for formatting and range formatting alike.

The unit is the *gap*: the whitespace and comments between two
significant tokens. Formatting a file is replacing every gap; formatting
lines is laying out the parts of gaps that lie on them and copying
everything else byte for byte. A gap is made of *rows*, the stretches
between its line breaks (a block comment can make a row of several
lines):

- the rows of a gap whose lines are all selected are laid out, when
  they are one run; the rows at either end that are not selected are
  copied, line breaks and all. So a selected line's indentation, the
  spacing inside it and the comments and blank lines in the selection
  are formatted, and the comments above it that are not selected stay
  as they are;
- trailing spaces on a selected line are removed;
- the end of the file is formatted when all of it is selected.

Nothing on a line outside the selection changes, so the edits of a range
lie inside it, as LSP asks.
`TestLinesEveryRange`, `FuzzFormat` and `TestCorpusLines` hold the edits
of ranges to their lines.

**Indentation blends in.** The unselected lines keep their indentation,
which in a legacy file is not the canonical one (tabs, two spaces). A
selected line is therefore indented from what is around it, the way
clang-format does it and one step further: an unselected line that
begins a statement teaches the indentation of its nesting level; a
selected line asks the lines above it in its block for its level, then
the lines below it (clang-format only looks up, which leaves the first
statement of a block out of line); with nobody to ask it adds the
configured unit to the nearest level that was taught. Case labels and
braces teach nothing, and the top level is always the margin. In a file
that is already formatted this is the canonical indentation, so range
formatting and whole-file formatting agree there; all lines as a range
are exactly `Format`.

**The gate still holds.** The token streams and the comments must be
equal, and formatting the same gaps of the result again must change nothing (a gap
is identified by the number of tokens before it, which formatting does
not change). `TestCorpusLines` checks this on every file of the lib.

**Command line.**

```
dgdtools format --lines 120-180 [--lines 300-310] FILE
dgdtools format --changed PATH...             lines that differ from HEAD
dgdtools format --changed --base origin/main PATH...
```

- `--lines` takes one file (or standard input) and line numbers of the
  input.
- `--changed` asks git: `git diff -U0 BASE -- PATH` gives the changed
  line ranges of each file (working tree against `BASE`, default
  `HEAD`, so staged and unstaged both count); a file git does not track
  yet is formatted whole; a file without changes is not read. It needs a
  git work tree and says so otherwise. It composes with `--check` and
  `--diff`, which is the CI form: "the lines this branch touched are
  formatted".
- git is a front-end concern. The library takes line ranges and never
  runs a program.
- A range or a changed-lines run does not normalize the rest of the
  file: line endings, blank-line runs and indentation outside the
  selection stay. A file converges as it is edited.

Why lines and not syntax nodes: lines are what an editor selection, a
diff hunk and `git blame` are made of, and every tool with this feature
(clang-format, black, ruff, rustfmt) takes lines.

### 2.7 One configuration, found per file

The formatter's settings are the `format:` section of `dgdtools.yml`,
resolved for each file as [config.md](config.md) describes: nearest
settings scope, then the project's config, then `.editorconfig`, then
the defaults; command-line flags (`--indent-style`, `--indent-width`,
`--line-endings`) over all of them. Values are validated when the
config is loaded; an unknown key or value is an error, not a default.

The `unformatted` lint rule calls the same function with the same
per-file settings, so `dgdtools lint` and `dgdtools format --check`
cannot disagree.

## 3. The library surface

```go
out, err := format.Source(project, path, text, nil)      // a file's text, by that file's settings
out, err := format.Source(project, path, text, ranges)   // only these lines ([]format.LineRange)
edits := format.Edits(file, out)                         // []diag.TextEdit, one per changed gap
```

`Source` is all a front end does to format a text: the file's settings
(`project.Settings(path)`), the project's dialect and macro conventions,
the lexer, the engine, the gate. It is what `dgdtools format` calls and
what an editor's formatting request will call. Underneath are
`format.Format(file, options)` and `format.Lines(file, options, ranges)`
over a `*token.File` and `format.OptionsFor(config, settings.Format)`,
which is what the `unformatted` rule uses on the file it has already
lexed. A result depends on the file's bytes and the options only: no
other file is read, and the same bytes give the same result in an editor
buffer, in a partial checkout and in the whole lib.

Errors are values (`ErrIllegal`, `ErrGateFailure`, `ErrNotIdempotent`,
`ErrInternal`): a file that cannot be formatted is reported and left
alone, in a run over many files and in an editor session alike.
`ErrIllegal` comes with the line and column of the first token that does
not lex, and what is wrong with it.

A backslash at the end of a line outside a directive is a line splice,
which DGD removes before it reads tokens; the lexer makes it trivia. The
formatter leaves the gap that holds one as it is written (but for the
line endings): where the two lines break and how the second begins.

## 4. Measured

VikingMUD lib, the mud's own configuration (110,993 `.c`/`.h` files in
scope after excludes), braces kept on their lines:

| | |
|---|---|
| reformatted | 104,494 |
| already formatted | 5,744 |
| refused: does not tokenize | 755 |
| gate failures | 0 |
| changed by a second pass | 0 |
| a non-blank line joined or split | 0 files |
| wall time, `format --check lib` | 8 s (12 CPUs) |
