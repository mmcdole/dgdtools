# Editor Support & Language Server

`dgdtools` provides editor support for **LPC (DGD)** across two layers:

1. **Syntax grammars** that work offline without a running server, matching `dgdtools`'s own lexer tokens.
2. **The language server (`dgdtools lsp`)**, which adds diagnostics, quick fixes, formatting, navigation, completion, refactoring, and semantic highlighting.

The language ID / filetype is **`dgdlpc`** everywhere (VS Code, Vim/Neovim, and Emacs's `dgdlpc-mode`). This avoids conflicting with existing `lpc` plugins built for LDMud or FluffOS, which highlight keywords DGD doesn't have and miss DGD keywords like `nil`, `atomic`, and `rlimits`.

| Editor | Status |
|---|---|
| [VS Code](vscode/README.md) | Extension with TextMate grammar, language config, and bundled `dgdtools` LSP server |
| [Vim & Neovim](vim/README.md) | Plugin with filetype detection, syntax, ftplugin, indent, and built-in LSP setup for Neovim 0.11+ |
| [Emacs](emacs/README.md) | `dgdlpc-mode` with detection, highlighting, and indentation, registered with Eglot (Emacs 29+) |
| Other LSP editors | Run `dgdtools lsp` over stdio for language `dgdlpc`, using `dgdtools.yml` / `.dgdtools.yml` as root markers |

Shared grammar word lists (`dgdlpc.json`) and snapshot test fixtures live in `common/`.

## Scoped `.c` and `.h` detection

None of the editor integrations claim `*.c` or `*.h` globally, so C projects on the same machine remain C:

- **VS Code:** Opt-in per folder via workspace `files.associations` (the extension offers a command and prompt when `dgdtools.yml` is present).
- **Vim / Neovim:** Activated only under directories containing `dgdtools.yml` / `.dgdtools.yml`, or directories listed in `g:dgdlpc_roots`.
- **Emacs:** The same rule, from `magic-mode-alist`, with `dgdlpc-roots` for the listed directories.

## The language server (`dgdtools lsp`)

`dgdtools lsp` runs over standard input and output and requires no separate server configuration. It discovers the nearest `dgdtools.yml` for each file just like the CLI, so diagnostics and formatting match `dgdtools lint` and `dgdtools format`.

### Capabilities

- **Diagnostics:** Live lint results for open files with rule codes, documentation links, and related locations across files. Editing an included header, inherited object, `dgdtools.yml`, `.editorconfig`, profile, or baseline updates dependent open files automatically.
- **Code actions & refactorings:** Quick fixes for diagnostics, single-line or file-wide `dgdtools-ignore` suppression comments, *Fix All* (`source.fixAll.dgdtools`) for safe fixes, path suggestions for unresolved `#include`/`inherit` statements when a unique match exists, and *Add prototype* (`refactor.rewrite`) for functions called before they are defined.
- **File moves (`workspace/willRenameFiles`):** Renaming or moving a `.c`/`.h` file or directory updates all referencing `inherit`, `#include`, and object paths, matching `dgdtools rename FILE NEW`. Macro-defined paths are left untouched with a notification. Supported natively in VS Code's Explorer and in Neovim explorer plugins such as `oil.nvim` and `nvim-tree`.
- **Formatting:** Whole-file and range formatting using project settings (`format:` in `dgdtools.yml`, falling back to `.editorconfig`).
- **Go to definition:** Jumps from `#include` to headers, `inherit` to parent objects, function calls (`fn()`, `::fn()`, `label::fn()`, `->fn()`, `call_other`, and string callbacks on known receivers) to definitions, macros to `#define`s, and variables/parameters to declarations. Symbols from a mudlib profile open as read-only stubs. Calls on unknown receivers (such as an object in a variable) do nothing instead of guessing.
- **Hover:** Shows declarations (specifiers, types, macro definitions and classifications), leading doc comments, and origin (file and line, `auto object`, profile, or `kfun` for built-in and `dialect.kfuns` extension functions).
- **Signature help:** Parameter hints while typing calls (`(` and `,`), including `call_other`, `call_out`, and built-in/custom kfuns—even on incomplete lines.
- **Document & workspace symbols:** File outlines (inherits, functions, global variables, macros) and fuzzy workspace symbol search across the mudlib and active profile.
- **Completion:** Context-aware completion for locals, parameters, object variables, own/inherited/auto-object functions, macros, kfuns, keywords, `::` and `->` targets, `#include` and `inherit` paths, and callback string arguments in `call_out`, `call_other`, and `conventions.call_registry`.
- **Find all references & highlights:** Workspace-wide references for functions (all statically resolvable call forms), globals, macros, inherited objects, and included headers, plus in-file read/write highlights. Ambiguous dynamic calls are omitted and counted in the server log.
- **Rename symbol:** Same semantics as `dgdtools rename`—renames locals, parameters, private symbols, inherit labels, macros, globals, and functions (along with overrides and all confirmed calls across the mudlib). Ambiguous matches (like calls on variable receivers or inside macro bodies) are annotated as needing confirmation so VS Code shows them unchecked; clients without edit confirmation receive only confirmed edits plus a summary message of skipped matches. Refuses unsafe renames (kfuns, profile symbols, `create`, `save_object` globals, name collisions) with an explanation.
- **Semantic tokens:** Highlights details a syntax grammar can't know: macros (including type-like macros such as `status` and specifier macros such as `public`), function calls by origin (own, `inherited`, auto object, `defaultLibrary` kfun), methods (`->fn`), globals vs. `local` variables and parameters, inherit labels, and `function` when `dialect.closures` is enabled. Uses standard LSP token types plus `inherited`, `local`, and `macro` modifiers.

### Logging and editor settings

Pass `--log /tmp/dgdtools-lsp.log` to write debug logs, and `--trace` to log all JSON-RPC messages. The server never writes to stderr.

For wizard checkouts without a `dgdtools.yml`, the server accepts two per-workspace-folder settings:

- `dgdtools.profile` — Mudlib profile name or file path (same as `profile:` in `dgdtools.yml`).
- `dgdtools.libPath` — Checkout path within the mudlib, e.g. `/players/me` (same as `lib_path:`).

If a `dgdtools.yml` is present, it always takes precedence over editor settings.

---

## Building the VS Code extension

Each GitHub release includes platform-specific `.vsix` packages with `dgdtools` bundled (`darwin-arm64`, `darwin-x64`, `linux-x64`, `linux-arm64`, `win32-x64`, `win32-arm64`) plus a universal `.vsix` without a bundled binary. Install any `.vsix` with `code --install-extension FILE`.

Building from a local checkout requires Node.js 22.18+ (and Go to bundle the binary):

```sh
# Build a .vsix for the current platform with bin/dgdtools bundled
make vsix
code --install-extension editors/vscode/dgdtools-darwin-arm64-0.0.1.vsix
```

To package for a specific target platform or version:

```sh
GOOS=linux GOARCH=amd64 go build -o /tmp/dgdtools ./cmd/dgdtools
cd editors/vscode
npm ci
node scripts/vsix.mts --binary /tmp/dgdtools --target linux-x64 --version 0.4.0 --out /tmp
```

Pass `--dist DIR` to build all seven release packages from GoReleaser output (`goreleaser build --snapshot --clean`).

To run the unpacked extension in development mode (using `dgdtools` from `PATH`):

```sh
cd editors/vscode
npm ci
npm run build
code --extensionDevelopmentPath=$PWD /path/to/your/lib
```

Run `npm run check` for TypeScript typechecking, `npm test` for extension unit tests, and `go test ./editors` to verify manifest consistency and release targets.

---

## Testing and updating grammars

Run `go test ./editors` to test the TextMate grammar, the Vim syntax, and the Emacs mode:

- `common/dgdlpc.json` is generated from `pkg/token`, `pkg/lexer`, and `pkg/pp`; tests verify every grammar stays in sync with every keyword and directive.
- The TextMate grammar is evaluated against `common/samples/` and compared byte-for-byte against the Go lexer and `*.tm.snap` snapshots. Patterns are restricted to the regex subset shared by Go's `regexp` and Oniguruma (plus trailing lookaheads).
- When `vim` or `nvim` is on `PATH`, tests run Vim/Neovim directly to verify filetype detection, options, syntax groups (`*.vim.snap`), indentation (matching `dgdtools format`), and Neovim 0.11+ LSP integration.
- When `emacs` (29.1+) is on `PATH`, tests byte-compile `dgdlpc-mode`, run its ERT tests (detection, comments, indentation, Eglot registration, autoloads), compare its faces with the lexer and `*.emacs.snap`, and run an Eglot session against the server.

After intentional grammar changes, regenerate snapshots and review the diff:

```sh
go test ./editors -update
```
