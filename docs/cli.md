# CLI Guide

`dgdtools` packages a linter, a formatter, a refactoring tool, a mudlib profile generator, and an LSP server into a single binary.

## Commands

```text
dgdtools lint [path ...]        Lint files and directories (defaults to current directory)
dgdtools format path ... | -    Format files in place (--check and --diff only report)
dgdtools rename SYMBOL NEW      Rename a symbol across the mudlib (diffs by default; --write applies)
dgdtools rename FILE NEW        Move a file or directory and rewrite references to it
dgdtools rules                  List all lint rules
dgdtools explain RULE           Show details and options for a rule
dgdtools config [path]          Validate config files and show resolved settings for a path
dgdtools profile build | show   Build or inspect mudlib profiles
dgdtools lsp                    Start the language server on stdio
dgdtools version                Print the version
```

Run `dgdtools <command> -h` to see all flags for a command. Flags can be placed before or after file paths.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Success (no lint failures, no formatting changes needed, or no unresolved rename candidates) |
| `1` | Findings or pending items: lint findings at or above `--fail-on` (default `error`), files that `format --check` would modify, or unconfirmed possible matches left by `rename` |
| `2` | Usage, configuration, I/O, or internal error, or a refused rename |
| `130` | Interrupted |

---

## `dgdtools lint`

`dgdtools lint` checks DGD LPC source files for compile-time errors, broken runtime calls, suspicious code, and optional mudlib conventions.

```sh
dgdtools lint                                 # Lint the current directory within its project
dgdtools lint dir/ file.c                     # Lint specific files or directories
dgdtools lint --output-format json dir/       # Machine-readable output: json, sarif, github
dgdtools lint --stdin-filename obj/key.c      # Lint stdin as if it were obj/key.c
dgdtools lint -                               # Lint unnamed stdin (file-local rules only)
dgdtools lint --select syntax,S               # Run only selected rules (names, codes, groups, or letters)
dgdtools lint --enable RULES --ignore RULES   # Adjust the configured rule set
dgdtools lint --fix dir/                      # Apply safe fixes and report remaining findings (--diff previews)
dgdtools lint --write-baseline                # Snapshot existing findings into a baseline file
dgdtools lint --stats dir/                    # Print per-rule counts and unverified stats
dgdtools lint --fail-on warning               # Exit 1 on warnings too (default: errors only)
dgdtools lint --max-per-file 5                # At most 5 findings per rule per file
dgdtools rules                                # List all rules with codes, defaults, and scope
dgdtools explain RULE                         # Explain a specific rule and its options
```

Passing a directory lints all `.c` files under it (along with their included headers), skipping paths matched by `exclude` in `dgdtools.yml`. Naming a `.h` file directly lints that header on its own. Running `dgdtools lint` with no arguments lints the current working directory while still resolving the project root above it—so running it inside `players/ann` checks only that folder against the full mudlib.

If a file can't be read, `dgdtools` reports a warning and continues with the rest of the files.

### How analysis works

`dgdtools` uses an error-tolerant DGD 1.7 parser that analyzes source code as written. Rather than fully expanding macros, it classifies `#define`s (as types, specifiers, statements, expressions, etc.) from their definitions.

The linter follows a **prove it or stay silent** policy: it only reports an issue when it can verify it from the code it can see. If an `#include` is missing, an `inherit` path can't be evaluated statically, a call target is held in a variable, an `#ifdef` condition is unknown, or an opaque macro gets in the way, the affected facts are marked *unknown* and no false positive is emitted. Pass `--stats` to see how much code couldn't be verified.

- **Without configuration**, file-local rules (syntax errors, suspicious constructs) still work: `dgdtools lint castle.c`.
- **Cross-file rules** (missing inherits/includes, broken `call_other`/`call_out` targets, etc.) require a mudlib root (`root:` in `dgdtools.yml` or `--root`) or a [mudlib profile](configuration.md#mudlib-profiles). `dgdtools` never guesses the mudlib root; files outside the root run file-local rules only.

### Rule groups

Rules are organized into four groups identified by their code prefix:

| Group | Prefix | Description |
|---|---|---|
| `syntax` | `E` | Errors the DGD compiler rejects; the object will not load |
| `runtime` | `R` | Code that compiles but fails or silently returns `nil` at runtime (e.g., `->`, `call_other`, `call_out`, or custom registrars targeting missing functions or objects) |
| `suspicious` | `S` | Valid LPC that is likely a bug |
| `convention` | `C` | Mudlib-specific style or policy rules (off by default) |

See [docs/rules.md](rules.md) for the complete reference of every rule, its default severity, and its options. Rule names and codes are stable.

The `E` rules were validated against DGD 1.7.9 across a 108,000-file mudlib: out of 30,747 files flagged by an `E` rule, none compiled under DGD.

### Wizard quickstart (partial checkouts)

You don't need a copy of the entire mudlib to lint your own directory. Download your wizard folder, get your mud's `.profile` file, and add a one-line `dgdtools.yml`:

```sh
$ cat usr/ann/dgdtools.yml
profile: mymud

$ cd usr/ann && dgdtools lint
room/hall.c:14:21: error: missing ';' before 'add_exit' [E001 syntax-error]
obj/key.c:9:19: error: function 'do_unlok' is not defined in /usr/ann/obj/key (this_object) ... [R001 callable-not-found]
```

You can also pass `--profile mymud` (and `--lib-path /usr/ann` if your local directory path doesn't match the mudlib layout) on the command line without creating a config file, or set `profile:` in `~/.config/dgdtools/dgdtools.yml`. See [Mudlib profiles](configuration.md#mudlib-profiles) for profile search paths and how maintainers build profiles with `dgdtools profile build`.

### Adopting on an existing mudlib

Older mudlibs often have thousands of findings in legacy or unmaintained files. You can record existing findings in a baseline and fail only on new ones:

```sh
dgdtools lint --write-baseline   # Writes dgdtools-baseline.json next to dgdtools.yml
dgdtools lint                    # Reports only findings not in the baseline
```

Baseline entries match by file path, rule, and offending source line text rather than line number, so unrelated edits in a file don't resurface old warnings. You can also tune rules per path with `lint.overrides` or per-directory config files (`root: false`); see [Configuration](configuration.md#per-directory-settings).

### Suppressing findings and ignore directives

All inline directives start with `dgdtools-ignore` and work in both `/* ... */` and `// ...` comments:

```text
dgdtools-ignore TARGETS [: reason]         Applies to this line, or the next line if standalone
dgdtools-ignore-start TARGETS [: reason]   Starts an ignored range
dgdtools-ignore-end [TARGETS]              Ends an ignored range (omit targets to end all)

TARGETS:
  lint/RULE, ...   One or more lint rules, by name or code
  lint             All lint rules (single-line only)
  format           Formatter (ranges only)
```

Examples:

```c
x = "a" "b";                 /* dgdtools-ignore lint/adjacent-strings */

/* dgdtools-ignore lint/callable-not-found: defined dynamically at boot */
"/sys/d"->late();

/* dgdtools-ignore-start lint/lifecycle-chain, lint/R001 */
...
/* dgdtools-ignore-end lint/lifecycle-chain, lint/R001 */
```

- A comment only acts as a directive if it begins with `dgdtools-ignore`; comments that merely mention it in prose are ignored.
- Range suppressions must name specific rules—you can't disable all lint rules for a range.
- Malformed directives (unknown rule names, missing targets, unmatched start/end) are reported by `invalid-suppression` (enabled by default). Enable `unused-suppression` to catch stale suppressions.
- Run `grep -rn dgdtools-ignore` to audit every exemption in a mudlib.

### Rule notes

**Conditional assignments:** Assign-and-test is a common LPC idiom and is allowed when the intent is clear:

```c
if (sz = sizeof(items)) {              /* allowed */
    process(items);
}
if (sz = 0) {                          /* warning: probably meant == */
    reset_items();
}
if (sz = load_items() && ready) {      /* warning: sz receives 0 or 1 */
    process(items);
}
if ((sz = load_items()) && ready) {    /* allowed: parenthesized explicitly */
    process(items);
}
```

Invalid assignment targets that DGD rejects at compile time (such as `!ob = present(...)`) are left to DGD rather than duplicated as lint rules.

**Lifecycle chaining (`lifecycle-chain`):** Checks that overridden lifecycle functions call their inherited implementation. By default, this checks the project's `create` function (`conventions.create` in `dgdtools.yml` or the profile, defaulting to `create`). You can add more functions via rule options: `lifecycle-chain: { names: [create, reset] }`.

Files with no explicit `inherit`, parents that don't define the function, and unresolved parents are skipped. When multiple inherited parents define the function, use labeled inherits to call each one:

```c
inherit room I_ROOM;
inherit commands I_COMMAND;

static void create() {
    room::create();
    commands::create();
}
```

In diamond inheritance hierarchies, chaining both direct parents may invoke a shared ancestor's initializer twice, since DGD deduplicates inherited programs but not explicit calls. The rule doesn't attempt to detect diamonds or enforce call ordering—use a targeted `dgdtools-ignore lint/lifecycle-chain` comment when skipping a call is intentional.

### Output formats

Default text output prints one finding per line in compiler format:

```text
obj/key.c:9:19: error: function 'do_unlok' is not defined in /obj/key (this_object) (called via add_action) [R001 callable-not-found]
```

Paths under the working directory are printed relative to it; paths outside are absolute. Column numbers count bytes. Declarations coming from a profile use `dgdtools-profile://NAME/std/room.c` URIs.

- `--output-format json`: Versioned JSON document containing diagnostics (rule name/code, severity, line/column, byte range, line fingerprint, tags, related locations, and safe/unsafe text edits), referenced rule metadata with documentation URLs, file-level errors (`unreadable`, `internal`, `stale`), and unverified counters.
- `--output-format sarif`: SARIF 2.1.0 for code-scanning integrations (such as GitHub Code Scanning), with `helpUri` links and partial fingerprints.
- `--output-format github`: GitHub Actions workflow annotations.

`dgdtools lint` never modifies files unless `--fix` is passed.

---

## `dgdtools format`

`dgdtools format` is an opinionated, whitespace-only formatter in the spirit of `gofmt`.

```sh
dgdtools format dir/ file.c                     # Format files in place
dgdtools format --check dir/                    # List files that need formatting; exit 1 if any
dgdtools format --diff file.c                   # Print unified diff without writing
dgdtools format -                               # Format stdin to stdout
dgdtools format --stdin-filename usr/ann/room.c # Format stdin using that path's settings
dgdtools format --lines 120-180 file.c          # Format only lines 120-180
dgdtools format --changed dir/                  # Format only lines changed since git HEAD
```

At least one path is required (use `.` for the current directory). Directories match all `.c` and `.h` files not excluded by `dgdtools.yml`. Formatting depends only on a file's own text and its formatting settings, so output is identical whether run in an editor buffer, a partial checkout, or the full mudlib. Missing profiles or driver configs don't block formatting.

### Formatting style

Only three options are configurable (in `dgdtools.yml`, `.editorconfig`, or via `--indent-style`, `--indent-width`, and `--line-endings`): spaces vs. tabs, indent width, and line endings. Everything else follows a fixed style:

- **Line breaks are preserved:** The formatter never wraps long lines or joins/splits statements (other than collapsing runs of more than two blank lines). Return types on their own line stay there, single-line functions (`string query_code() { return code; }`) stay on one line, and both `if (x) {` and next-line braces are kept as written.
- **Indentation:** Block bodies, braceless `if`/`else`/`for`/`while`/`do` bodies, and statement continuation lines are indented one level. `case` labels align with `switch`, and their bodies indent one level deeper.
- **Horizontal spacing:** Normalized consistently (`a+b` → `a + b`, `f( a,b )` → `f(a, b)`).
- **Preprocessor branches:** Every branch of `#if` / `#else` / `#elif` is formatted independently.
- **Unparseable regions:** Code with syntax errors keeps its line breaks and falls back to brace-based indentation; surrounding valid code formats normally.
- **Comments & whitespace:** Comments are never reflowed. Multi-line block comments shift as a unit, and function headers with embedded comments keep their layout. Trailing whitespace is stripped, at most two consecutive blank lines are kept, and files end with a single newline.

### Skipping blocks

Wrap regions you want left untouched with `dgdtools-ignore-start format` and `dgdtools-ignore-end format`:

```c
/* dgdtools-ignore-start format */
mapping exits = ([ "north" : "/room/hall",
                   "south" : "/room/yard",   ]);
/* dgdtools-ignore-end format */
```

Every byte inside the range is preserved verbatim. Omitting the end comment ignores formatting through the end of the file.

### Partial formatting

- `--lines A-B` (repeatable, single file) formats only the specified line ranges.
- `--changed` queries git for lines modified since `HEAD` (or `--base REV`) and formats only those lines. Untracked files are formatted completely; unchanged files are skipped.

Both flags work with `--check` and `--diff`, making it easy to enforce formatting on changed lines in CI without reformatting entire legacy files. Bytes outside the selected ranges are untouched, and reformatted lines match the indentation style of neighboring lines when editing legacy files.

### Safety guarantee

`dgdtools format` is designed to be provably behavior-preserving. The lexer is byte-lossless, and the formatter only edits whitespace between tokens. Before writing output, it re-lexes the result to verify that the token stream is identical to the input and checks idempotency. If a file fails tokenization or the safety check, it is left untouched and `dgdtools` exits with status `2`.

---

## `dgdtools rename`

`dgdtools rename` renames symbols across a mudlib or moves files/directories while updating every reference. By default it prints unified diffs without modifying any files; pass `--write` to apply changes.

### Renaming a symbol

```sh
dgdtools rename /std/living::query_name query_short_name                   # Preview diffs
dgdtools rename --write /std/living::query_name query_short_name           # Apply certain edits
dgdtools rename --write --include-possible /std/living::query_name query_short_name
dgdtools rename /include/std.h::ROOM ROOM_OB                               # Rename a header macro
dgdtools rename obj/sword.c:42:9 weight                                    # Rename symbol at file:line:col
dgdtools rename --output-format json /std/room::add_item add_detail        # Print rename plan as JSON
```

Supported `SYMBOL` formats:

| Format | Target |
|---|---|
| `/std/living::query_name` | Function or global variable on an object (by mudlib path; `.c` is optional), whether defined directly or inherited |
| `/include/std.h::ROOM` | Macro defined in a header or file (by mudlib path) |
| `FILE:LINE:COL` | Symbol at a 1-based line and column in a file on disk (useful for locals, parameters, and inherit labels) |

Every occurrence is classified as **certain**, **possible**, or **excluded**:

- **Functions:** Renames the function along with all overrides up and down the inheritance hierarchy (since DGD dispatches calls in base objects to overrides), plus all certain calls: `fn()`, `::fn()`, `label::fn()`, `ob->fn()` and `call_other` on known targets (literal paths, path macros, `this_object()`), `call_out`, and registrars configured in `conventions.call_registry`. Unrelated objects defining the same name are left untouched.
- **Global variables:** Renamed in the defining object and all objects that inherit it.
- **Macros:** Renamed in every file that includes the defining header.
- **File-scoped symbols:** Locals, parameters, private functions/variables, inherit labels, and file-local macros are renamed within their file.
- **Possible matches:** Calls on unknown receivers (e.g., `ob->fn()` where `ob` is a parameter), `this_object()` callbacks that might target a subclass override, occurrences inside macro bodies, and code inside inactive `#if` branches or unparseable regions. These are skipped unless `--include-possible` is set. Dynamic function names built at runtime (like `call_other(ob, "do_" + verb)`) can't be rewritten automatically; matching prefixes/suffixes are listed in the summary.

When the project defines checkout patterns (`profile_build.checkouts` or from a profile), diffs and stderr summaries are grouped by owner (e.g., wizard or domain).

**Safety checks and refusals:** `dgdtools rename` refuses (exiting `2`) if:
- The new name collides with or shadows another visible function, variable, macro, local, or kfun.
- The source or target name is the driver's `create` function or a driver object callback (`path_read`, `compile_error`, etc.).
- The symbol is defined in a read-only mudlib profile.
- The symbol is a global variable persisted by `save_object` (renaming it breaks saved state on reboot; pass `--force` to override).

It also warns when edited files are inherited or included elsewhere (since DGD requires recompiling dependent objects for changes to take effect) or when renaming inside a partial wizard checkout with a profile.

### Moving a file or directory

Pass a file or directory path instead of a symbol to move it and update references:

```sh
dgdtools rename /std/room.c /std/base_room.c      # Preview diffs
dgdtools rename --write /std/room.c base_room.c   # Rename within the same directory
dgdtools rename lib/std/room.c lib/std/place.c    # Move using filesystem paths
dgdtools rename /d/Ruins /d/Old                   # Move an entire directory
```

If the source matches a path on disk relative to the working directory, filesystem paths are used; otherwise mudlib paths are used.

Every `inherit`, `#include`, and object path (`clone_object("/std/room")`, `"/std/room"->fn()`) referring to the moved file is rewritten while preserving its original style (root-relative, file-relative, or include-dir relative), along with relative paths inside the moved file itself. Paths expanded from macros (`inherit ROOM;`) and plain string literals in unrecognized contexts (`add_exit("/std/room", ...)`, `#define`s) are reported as possible matches; pass `--include-possible` to rewrite plain strings too. `--write` updates all referencing files first and moves the file or directory last.

---

## Using `dgdtools` as a Go library

`cmd/dgdtools` is a thin CLI wrapper around reusable Go packages:

- `pkg/lint` — `Linter` runs against a project and virtual filesystem (`pkg/vfs`), supports incremental file updates and `context` cancellation, and returns structured diagnostics.
- `pkg/format` — `Source` formats whole files or line ranges; `Edits` computes minimal text edits for editors.
- `pkg/config` — `Project.Settings(path)` handles config discovery and per-file setting resolution.
- `pkg/session` — Manages editor state (unsaved buffers, projects, and per-project linters).

For internal architecture and design rationale, see [docs/design/architecture.md](design/architecture.md), [docs/design/config.md](design/config.md), [docs/design/formatter.md](design/formatter.md), and [docs/design/lsp.md](design/lsp.md).
