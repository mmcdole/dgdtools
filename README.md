# dgdtools

Source tooling for the [DGD](https://github.com/dworkin/dgd) dialect of LPC:

- **dgdfmt** — a code formatter in the gofmt tradition
- **dgdlint** — a linter focused on the failures the DGD compiler cannot
  see: string-dispatched calls, missing objects, silent argument mismatches
- **dgdcmp** — compares two files by significant token stream, proving a
  diff is formatting-only

Existing LPC tooling targets LDMud or FluffOS and mishandles DGD syntax:
`({ })` / `([ ])` literals, the `<-` instance-of operator, `..` ranges,
labeled inherits, K&R-style typeless functions. These tools implement the
DGD 1.7 grammar, with driver build variants (`SLASHSLASH` line comments,
`CLOSURES`) available as configuration.

## Installation

With Go 1.25 or later:

```
go install github.com/mmcdole/dgdtools/cmd/...@latest
```

Or from a clone:

```
git clone https://github.com/mmcdole/dgdtools
cd dgdtools
make build          # binaries in ./bin
make test
```

`make corpus LPC_CORPUS=/path/to/lib` runs the full-tree invariants
against a real mudlib: byte-exact lexer round-trip, comparator
self-compare, and the formatter gate on every file.

## Configuration

All three tools read `dgdtools.yml`, found by searching upward from the
working directory (or given with `-config`). Every field is optional; the
tools work with no config file at all. A natural setup is one file next to
the mudlib:

```
mud/
├── dgdtools.yml
└── lib/            ← root: lib
```

```yaml
dialect:
  slash_slash: true          # // comments (driver SLASHSLASH build flag)
  closures: false            # `function` keyword (driver CLOSURES flag)

root: lib                    # filesystem directory that is the lib's "/"

exclude:                     # glob patterns, ** crosses directories
  - "players/**"
  - "**/old/**"
  - "**/*.retired"
  - "**/var/**"              # generated runtime data

format:
  indent: 4
  line_endings: preserve     # preserve | lf | crlf
  max_blank_lines: 2
  function_headers: split    # split (name at column 0) | joined

lint:
  # Mirror the driver config so #include and inherit macros resolve.
  include_dirs: ["/dgd/include", "/std/include"]
  include_file: "/dgd/include/Std.h"   # force-included everywhere
  auto_objects: ["/dgd/lib/object"]    # implicitly inherited by everything

  # Identifiers accepted in specifier position (empty visibility macros).
  specifier_macros: [public]

  # Functions whose string argument names a callback on this_object();
  # call_other and call_out are built in. Value = 0-based argument index.
  call_registry:
    store_fp: 0
    add_action: 0

  # Functions taking a literal object path; clone_object, compile_object,
  # and find_object are built in.
  object_registry: {}

  # scanf/printf-style functions and their format-string argument;
  # sscanf (arg 1) is built in.
  format_registry: {}

  # Function/macro names marking an object as auto-saving.
  autosave_markers: [set_auto_save]

  # Lib-path globs served by virtual-object daemons (no backing .c file).
  virtual_paths: []

  # Rules run by their built-in defaults (`dgdlint -rules` lists them);
  # enable adds, disable removes. The rules: section only tunes settings —
  # listing a rule there does not enable it.
  enable: []
  disable: []
  rules:
    raw-inherit-path: { severity: warning, deny: ["/std/", "/obj/"] }
    lifecycle-chain: { names: [create] }
  path_rules:
    - paths: ["legacy/**"]
      disable: [lifecycle-chain]
  fail_on: error             # exit 1 at this severity or above
```

The `root`, `include_dirs`, `include_file`, and `auto_objects` values
mirror the DGD driver configuration. Without them the tier-2 lint rules
cannot resolve inherit chains and stay quiet rather than guess.

## dgdfmt

```
dgdfmt file.c               # print formatted source to stdout
dgdfmt -l dir/              # list files whose formatting differs
dgdfmt -d file.c            # show a unified diff
dgdfmt -w dir/              # rewrite files in place
```

The style is fixed, with a small number of toggles (`-indent`,
`-line-endings`, `-max-blank-lines`, `-func-headers`); flags override the
config file. The default style:

- Function definitions in KNF: specifiers and return type on one line,
  the name at column 0, braces on their own lines. `-func-headers joined`
  keeps the header on one line instead.
- Definitions written entirely on one line stay on one line
  (`string query_code() { return code; }`).
- Control-flow braces are cuddled (`if (x) {`, `} else {`) when the
  source uses Allman layout; one-line brace blocks keep their own lines.
- `case` labels at the `switch` level, bodies one deeper.
- Horizontal spacing is normalized (`a+b` → `a + b`, `f( a,b )` →
  `f(a, b)`, `({"x"})` → `({ "x" })`); the author's line breaks are
  preserved, and long lines are never re-wrapped.
- Comments are never reflowed: line comments keep their bytes, multi-line
  block comments shift as a unit, and function headers containing
  comments are left in their original layout.
- Trailing whitespace is removed, blank-line runs are capped, files end
  with exactly one newline, and line endings are preserved per file
  unless a policy is set.

### Safety

The formatter must be safe to run across a live game's codebase, so it is
built to be provably behavior-preserving:

1. The lexer is byte-lossless. Whitespace and comments are tokens;
   reassembling the token stream reproduces the input byte for byte.
   Bytes are never transcoded — Latin-1 text, ANSI escapes inside string
   literals, and CRLF endings pass through untouched.
2. The formatter emits significant tokens verbatim and synthesizes only
   the whitespace between them.
3. Every output is gated before it is returned: re-lexed, required to be
   token-identical to the input, and required to be idempotent. A file
   that trips the gate is left untouched and reported.
4. Files that do not fully tokenize are refused, never guessed at.

`dgdcmp a.c b.c` exposes the gate's comparison directly: exit 0 means the
two files differ only in formatting. `dgdcmp -dump file.c` prints the
token stream.

## dgdlint

```
dgdlint dir/ file.c ...     # lint; paths default to the config root
dgdlint -rules              # list rules with tier, default, and severity
dgdlint -enable r1,r2 ...   # enable rules beyond the defaults
dgdlint -f json ...         # machine-readable output
```

Exit codes: `0` clean, `1` findings at or above `fail_on`, `2` error.

Rules are independent analyzers. Tier 1 rules inspect one file at a time.
Tier 2 rules build an index of the whole lib — every object's functions,
resolved inherit chains (following `#define` path macros through their
includes), and every string-referenced call site — because in DGD every
cross-object call is late-bound by name and a missing function silently
returns nil. The index only reports what it can prove: unknown call
targets and unresolvable chains are skipped, and a module's callbacks are
checked against its inheritors and includers before being called missing.

| Rule | Tier | Default | Finds |
|---|---|---|---|
| `callable-not-found` | 2 | on | a string-referenced function (`obj->fn()`, `call_other`, `call_out`, registrars) that does not exist in the target's inherit chain — the call silently returns nil |
| `static-crossobj` | 2 | on | a call_other-style call to a function it cannot reach: `private` functions always, `static` functions from another object — silently returns nil |
| `undefined-prototype` | 2 | on | a function declared but never defined anywhere in the chain, and called — a runtime error ("Undefined function") |
| `target-object-missing` | 2 | on | a literal object path (inherit, `clone_object`, call target) with no backing file — a runtime load error |
| `callback-arity` | 2 | on | a dispatched call passing an argument count the target cannot accept — silently padded with nil / dropped under non-strict typechecking |
| `include-not-found` | 2 | on | a `#include` target that cannot be found — a compile error at load |
| `assignment-in-condition` | 1 | on | `if (x = 1)` — a bare assignment in an `if`/`while`/`for` condition; `((x = y))` marks intent and is accepted |
| `no-effect-statement` | 1 | on | a comparison used as a statement (`x == 1;`) |
| `sscanf-format` | 1 | on | more variables supplied than `%`-conversions in the format string |
| `lifecycle-chain` | 1 | on | a lifecycle function (default `create`) in an inheriting object that never chains `::create()` |
| `static-autosave-var` | 2 | off | a `static` global in an auto-saving object — excluded from `save_object`, the state silently does not persist; enable when reviewing specifier changes |
| `unresolved-inherit` | 2 | off | an inherit path the macro evaluator could not resolve; affected chains cannot be verified |
| `raw-inherit-path` | 1 | off | an inherit using a literal path string where the lib mandates macros |
| `missing-visibility` | 1 | off | a function with no visibility specifier |
| `unformatted` | 1 | off | a file that is not dgdfmt-formatted |

Suppress findings inline from any comment:

```
/* dgdlint:disable-next-line callable-not-found */
/* dgdlint:disable-line rule-a,rule-b */
/* dgdlint:disable rule-a */   ... /* dgdlint:enable */
```

File-scoped `disable` requires an explicit rule list.

## License note

The DGD dialect facts implemented here (keywords, operators, literal and
escape forms, preprocessor and call semantics) were taken as facts from
DGD's grammar, documentation, and observable behavior. No DGD source code
(AGPLv3) has been copied or translated into this repository.
