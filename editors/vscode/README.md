# LPC (DGD)

Language support for LPC as compiled by the [DGD](https://github.com/dworkin/dgd)
driver, powered by the [dgdtools](https://github.com/mmcdole/dgdtools#readme)
language server.

## Features

- **Syntax highlighting** for DGD-specific syntax: `({ })` and `([ ])`
  literals, `<-`, `..`, `nil`, `atomic`, `rlimits` and labeled inherits.
  Works without the server.
- **Diagnostics as you type**: syntax errors, constructs DGD rejects,
  missing includes, and calls by name to functions or objects that don't
  exist. Same results as `dgdtools lint`.
- **Quick fixes**, including suppression comments and *Fix All*
  (`source.fixAll.dgdtools`).
- **Formatting** for whole files and selections. Whitespace only; line
  breaks are kept.
- **Go to Definition** for includes, inherits, function calls (`::fn`,
  `label::fn`, `->fn`, string callbacks), macros and variables.
- **Hover** and **Signature Help** for mudlib functions, variables,
  macros and kfuns.
- **Document and workspace symbols.**
- **Completion** for locals, functions, variables, macros, kfuns,
  keywords, `#include` and `inherit` paths, and `call_out` names.
- **Find All References** and **document highlights** across the whole
  mudlib.
- **Rename Symbol** (F2) across the mudlib, including overrides in
  inheriting objects. Places that can't be confirmed (such as a call on
  an object held in a variable) appear unchecked in the preview.
- **File moves**: renaming or moving a `.c`/`.h` file or folder in the
  Explorer updates the `inherit`, `#include` and object paths that refer
  to it.
- **Refactorings**: fix an unresolved include or inherit path, and add a
  missing prototype.
- **Semantic highlighting** for macros, kfuns, inherited functions,
  globals, locals and parameters.

When a result can't be determined statically, the server stays quiet
instead of guessing.

## Installation

Install **LPC (DGD)** from the Marketplace. Packages for
macOS, Linux and Windows (x64 and arm64) include the `dgdtools` binary,
so there's nothing else to install. On other platforms,
[install dgdtools](https://github.com/mmcdole/dgdtools#installation)
and put it on your `PATH`.

Each [release](https://github.com/mmcdole/dgdtools/releases) also has
the `.vsix` files, which can be installed with
*Extensions: Install from VSIX…*.

## Getting started

`.c` and `.h` files are C by default, so the extension only treats them
as LPC in folders you choose. Open your mudlib and run
**dgdtools: Use LPC (DGD) for This Folder's .c/.h Files** from the command
palette or the Explorer context menu. If the folder has a `dgdtools.yml`,
the extension offers to do this for you.

The command adds this to the workspace settings:

```json
{
  "files.associations": { "*.c": "dgdlpc", "*.h": "dgdlpc" }
}
```

It also lists Microsoft's C/C++ extensions under
`unwantedRecommendations` in `.vscode/extensions.json`, so VS Code stops
suggesting them. **dgdtools: Use C for This Folder's .c/.h Files**
undoes both.

To switch a single file, use *Change Language Mode* → *LPC (DGD)*.

Project settings (rules, formatting, mudlib root, profiles) live in
`dgdtools.yml`, shared with the command line. See the
[dgdtools README](https://github.com/mmcdole/dgdtools#readme).

## Recommended settings

Format and fix on save:

```json
{
  "[dgdlpc]": {
    "editor.formatOnSave": true,
    "editor.codeActionsOnSave": { "source.fixAll.dgdtools": "explicit" }
  }
}
```

The extension turns on `files.autoGuessEncoding` for LPC files, since
many older mudlibs use Latin-1. If all of yours do, set the encoding
explicitly to avoid wrong guesses:

```json
{
  "[dgdlpc]": { "files.encoding": "iso88591" }
}
```

Word-based suggestions are off for LPC files, because the server provides
completion.

## Commands

| Command | Description |
|---|---|
| **dgdtools: Use LPC (DGD) for This Folder's .c/.h Files** | Associate the folder's `.c`/`.h` files with LPC |
| **dgdtools: Use C for This Folder's .c/.h Files** | Undo the association |
| **dgdtools: Restart Server** | Restart the language server |

## Settings

| Setting | Description |
|---|---|
| `dgdtools.path` | Path to the `dgdtools` executable. Defaults to the bundled binary, then `dgdtools` on `PATH`. |
| `dgdtools.profile` | Mudlib profile to use when the folder has no `dgdtools.yml`. |
| `dgdtools.libPath` | The folder's path within the mudlib (e.g. `/players/me`) when it has no `dgdtools.yml`. |
| `dgdtools.logFile` | File for the server's debug log. |
| `dgdtools.trace.server` | Trace messages between VS Code and the server (`messages` or `verbose`). Set the *dgdtools* output channel's log level to *Trace* to see them. |

A `dgdtools.yml` takes precedence over `dgdtools.profile` and
`dgdtools.libPath`. In untrusted workspaces, workspace values of
`dgdtools.path` and `dgdtools.logFile` are ignored.

## Known limitations

- The grammar always treats `//` as a comment. If your driver doesn't
  support `//` comments, the linter reports them.
- `function` is highlighted as a type only via semantic highlighting,
  when `dialect.closures` is enabled.

## Links

- [dgdtools](https://github.com/mmcdole/dgdtools#readme): rules,
  formatter, configuration, profiles
- [Editor support](https://github.com/mmcdole/dgdtools/blob/main/editors/README.md):
  Vim, Neovim, Helix and building the extension
- [Issues](https://github.com/mmcdole/dgdtools/issues)
