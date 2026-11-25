# Editor support for LPC (DGD)

What an editor needs to show LPC (DGD). The language is called
**`dgdlpc`** everywhere: the VS Code language id, the Vim filetype, the
Helix language. (`lpc` is taken by tooling for other drivers, which
colours as keywords words DGD does not have and knows none of `nil`,
`atomic` and `rlimits`.)

| | |
|---|---|
| [VS Code](vscode/README.md) | the extension: a TextMate grammar, the language configuration, and the client that starts the language server |
| [Vim and Neovim](vim/README.md) | one plugin for both: filetype detection, syntax, filetype plugin, indent; in Neovim 0.11 and later the language server, started by itself |
| [Helix](helix/README.md) | the language server, with C's highlighting until there is a tree-sitter grammar |
| Zed | a new language needs a Zed extension, and none is written |
| `common/` | what the grammars share: the word list (`dgdlpc.json`) and the sample files the tests run over |

Any other editor with an LSP client can run the server: the command is
`dgdtools lsp`, on standard input and output, for the files of the
language `dgdlpc`, with `dgdtools.yml` and `.dgdtools.yml` as root
markers.

There are two layers ([lsp.md](../docs/design/lsp.md), section 10). The
first is highlighting that works with no server, offline, on a file that
does not parse: it knows the tokens of the language, exactly as the
tool's own lexer reads them. The second is the language server,
`dgdtools lsp`, which knows what a grammar cannot: the linter's
findings as you type, their fixes, the formatter, where a name is
declared and what it is, and the colours of what only analysis knows
(semantic tokens) over the grammar's ([below](#the-language-server)).

## A `.c` file is LPC only where you say so

Nothing here claims `*.c` or `*.h` by name: on a machine that has C
sources too, they stay C. A file is `dgdlpc` because of where it lies:

- **Vim, Neovim:** under a directory that has a `dgdtools.yml` or a
  `.dgdtools.yml` (the file's own directory or any above it), or under a
  directory listed in `g:dgdlpc_roots`.
- **VS Code:** in a workspace whose settings associate the files, which
  the extension writes when asked to.
- **Helix:** in a mudlib whose own `.helix/languages.toml` says so.

## The language server

`dgdtools lsp` is the server: the one binary, started by the editor,
talking on its standard input and output. It needs no configuration of
its own. A file's project is the nearest `dgdtools.yml` above it, as for
every command, so what the editor shows is what `dgdtools lint` prints
and what it formats is what `dgdtools format` writes. What it does:

- **Diagnostics** for the files that are open, a moment after you stop
  typing, with the rule's code, a link to the rule's documentation and
  the related places in other files. A change to a header or an
  inherited file reaches the open files that depend on it; a change to
  a `dgdtools.yml`, an `.editorconfig`, a profile or a baseline reaches
  all of them.
- **Code actions** on a finding: its fix when it has one, a comment
  that suppresses it for the line (`/* dgdtools-ignore lint/RULE */`) or
  for the file, and "fix all" (`source.fixAll.dgdtools`) for every fix
  that is safe to apply unseen. For a missing `#include` or `inherit`,
  the path of the one file of the mudlib with that name, when there is
  one (not preferred: it may not be the file meant). At a call of a
  function the file defines later, and nothing declares before it, "Add
  the prototype" (`refactor.rewrite`), which typechecking needs.
- **Files moved**: renaming or moving a `.c` or `.h` file or a folder
  in the editor (`workspace/willRenameFiles`) rewrites every `inherit`,
  `#include` and object path that names it, as `dgdtools rename FILE
  NEW` does; a path made by a macro is left, and a message says where.
  VS Code asks for it in its explorer; Neovim's core does not, its file
  explorer plugins (oil.nvim, nvim-tree) do.
- **Formatting** of a file or a selection, with the project's settings
  (`format:` in `dgdtools.yml`, else `.editorconfig`), not the editor's.
- **Go to definition**: from an `#include` to the header, from an
  `inherit` statement to the object, from a call to the function (the
  file's, an inherited one, the auto object's; `::fn` and `label::fn`;
  `->fn`, `call_other` and string callbacks when the receiver is
  `this_object()` or a path), from a macro to its `#define`, from a
  variable, a local or a parameter to its declaration. With a profile,
  what is not in your folder opens as the profile's read-only stub.
  Where the tools cannot know (a call on an object held in a variable),
  nothing happens rather than a guess.
- **Hover** on any of those names: the declaration (a function's with
  its specifiers and types, a variable's type, a macro's `#define` and
  what the parser takes it for), the comment directly above it, and
  where it is: the file and line, `auto object`, the profile, or `kfun`
  for a function of the driver's, of which the server has DGD's table
  (the kfuns of your driver's extension modules are `dialect.kfuns` in
  `dgdtools.yml`). On `ob->fn()` with `ob` a variable it says that the
  receiver is not known, and no more.
- **Signature help** in the arguments of a call, as you type `(` and
  `,`: the function's declaration with the parameter you are at, for
  `fn()`, `::fn()`, `label::fn()`, `->fn()` on `this_object()` or a
  path, and kfuns; in `call_other(ob, "fn", ...)` and `call_out("fn",
  delay, ...)` the arguments that are passed on are `fn`'s. It works
  while the line is half written. A call on an unknown receiver has
  none.
- **Symbols**: the outline of a file (its inherit statements, by the
  object inherited, its functions, global variables and macros), and
  the search for a name in the whole mudlib: the functions and variables
  of its objects and the macros of its headers that have the letters
  you type, in their order, the closest match first. With a profile
  the names of the profile's stubs are found too, and say so.
- **Completion** as you type: the parameters and locals in scope, the
  object's variables and functions (its own, inherited, the auto
  object's), the macros in view, kfuns and keywords, each with its
  declaration and its comment; after `::` and `label::` the inherited
  functions; after `->` on `this_object()` or a path the functions
  that can be called there, and nothing on a receiver nobody knows; the
  files of an `#include` and the objects of an `inherit` path, on disk
  and in a profile; the function's name in the string of `call_out`,
  `call_other` and your mudlib's registrars. It is asked for by itself
  after `->`, `::`, and in a path at `"`, `<` and `/`.
- **References** of a name, in the whole mudlib: the calls of a
  function in every form that reaches it (`fn()`, `::fn()`,
  `label::fn()`, `->fn` and string callbacks on `this_object()` or a
  path), the uses of a global variable in its object and those that
  inherit it, the uses of a macro (in code, in directives, in other
  macros, in an `inherit` statement), the `inherit` statements and
  object paths of an object, the `#include`s of a header; a local's or
  a parameter's in its function. The declaration is among them when
  the editor asks for it. A call on an object nobody can tell is not
  listed, and the server's log says how many there were. In a folder
  with a profile the search covers your folder, and the log says so:
  the profile has the rest of the mudlib's declarations, not its code.
  The first search waits for the mudlib to be indexed, a few seconds on
  a big one.
- **Highlights** of the name under the cursor in the file, a variable,
  a local or a parameter marked where it is read and where it is
  written (declared, assigned, incremented).
- **Rename**, the same as `dgdtools rename`: a local, a parameter (a
  `catch` variable among them), a private function or variable, an
  inherit label, a macro in its file; a function with its
  redefinitions in the objects that inherit it and those it inherits,
  and every call that reaches one of them, a global variable in its
  object and its inheritors, a header's macro in every file that
  includes it, across the mudlib. The edits come as one change to every
  file, each open file's for the version it was made from. The places
  that may be the name and nobody can tell (a call on an object held in
  a variable, the name in a macro's body, code not compiled or not
  parsed) come under an annotation that needs confirming, so that VS
  Code's preview shows them unticked; an editor that cannot confirm
  edits (Neovim) gets the certain ones, and a message lists the others.
  The editor first asks whether the name under the cursor can be
  renamed, and is told why not: a kfun, the profile's declaration, the
  create function, a variable `save_object` writes (`dgdtools rename
  --force` renames it), a name in code that is not compiled or does not
  parse. A new name that would hide another, be hidden, or take the
  place of a function or a macro is refused, naming it. A message says
  what to know beside: the edited files other objects inherit (those
  objects must be recompiled for the rename to take effect), a checkout
  whose rest of the mudlib is not renamed, the calls that make a
  function's name when they run. DGD calls no private function by name,
  so the string of `call_out("fn")` for a private `fn` is no call of
  it: it is left, and the server says so.
- **Semantic tokens**: the colours of what only analysis knows, over
  the grammar's. A macro as a macro, one that stands for a type
  (`status`) as a type and one that stands for specifiers or for
  nothing (`public`) as a specifier; a function where it is declared
  and defined, and in a call whether it is the object's own, inherited
  (`inherited`), the auto object's or the driver's (`defaultLibrary`);
  `->fn` as a method; global variables, locals (`local`) and
  parameters; inherit labels; `function` where the mudlib has closures
  (`dialect.closures`), which no grammar can tell. A name the tools know
  nothing of keeps the grammar's colour: nothing is guessed. The types
  are the protocol's own, so a theme colours them unconfigured; the
  modifiers `inherited`, `local` and `macro` are dgdtools'.

It keeps a log when asked to: `dgdtools lsp --log /tmp/dgdtools-lsp.log`,
and with `--trace` every message in both directions. It never writes to
standard error.

**Settings.** There are two, for a wizard whose folder has no
`dgdtools.yml`: `dgdtools.profile` (the mudlib profile to work against:
a name or a file, as `profile:` in a config) and `dgdtools.libPath`
(where the folder sits in the mudlib, as `lib_path:`). They are per
workspace folder, a config file always wins over them, and nothing else
about the tools is set from an editor.


## Building the VS Code extension

A release of dgdtools has the extension as files, one package for each
platform with that platform's `dgdtools` inside
(`dgdtools-darwin-arm64-0.4.0.vsix`, and so on for `darwin-x64`,
`linux-x64`, `linux-arm64`, `win32-x64` and `win32-arm64`) and one
without the program for every other platform (`dgdtools-0.4.0.vsix`);
`code --install-extension FILE` installs one.

From a checkout it is built before it is used, which needs Node.js
(22.18 or later) and installs nothing outside the folder. **As a
package**, for the machine you are on and with the `dgdtools` of the
checkout inside it, which needs Go too:

```
cd /path/to/dgdtools
make vsix        # go build, npm ci, and vsce package --target for this platform
code --install-extension editors/vscode/dgdtools-darwin-arm64-0.0.1.vsix
```

The file's name has the platform (`linux-x64`, `win32-arm64`, ...) and
the version of `package.json`, which a release replaces with its own.
`make vsix` is `scripts/vsix.mts --binary bin/dgdtools`, and the script
takes another program and another platform, a version, and a directory
to write to:

```
cd /path/to/dgdtools
GOOS=linux GOARCH=amd64 go build -o /tmp/dgdtools ./cmd/dgdtools
cd editors/vscode
npm ci
node scripts/vsix.mts --binary /tmp/dgdtools --target linux-x64 --version 0.4.0 --out /tmp
```

`--dist DIR` makes all seven packages of a release from what GoReleaser
wrote to `DIR` (`goreleaser build --snapshot --clean` writes `dist/`),
which is what the release workflow runs. The packing is `vsce`'s, the
one `package-lock.json` pins; nothing here publishes anything.

**From the folder as it is**, without a package and without a program
in it (the `dgdtools` on `PATH` is run):

```
cd /path/to/dgdtools/editors/vscode
npm ci           # the client library and the build tools, as package-lock.json pins them
npm run build    # writes dist/extension.js, the one file VS Code runs
code --extensionDevelopmentPath=$PWD /path/to/your/lib
```

To work on the extension: `npm run check` checks the types, `npm test`
runs the tests of what needs no VS Code (finding the executable, the
association patterns, what the packaging script makes of a release),
and `go test ./editors` holds the manifest to the code and the
script's platforms to GoReleaser's. None of them starts VS Code: the
client has not been run in one here, which is the first thing to do
with it.

## Changing a grammar

The grammars are tested by `go test ./editors`, which needs nothing
installed:

- `common/dgdlpc.json` is written from the tool's own tables
  (`pkg/token`, `pkg/lexer`, `pkg/pp`), and the keyword lists and
  directive names of both grammars are compared with it. A keyword added
  to the language fails the test until both grammars have it.
- The TextMate grammar is run, by an interpreter of the grammar format
  that is part of the test, over `common/samples/`. Its tokens are
  compared with the lexer's, byte for byte, and with the snapshot beside
  each sample (`*.tm.snap`). Because the test runs it with Go's regular
  expressions and an editor with Oniguruma, the grammar keeps to the
  syntax the two share (no `\d \w \s`, no `$`, no lookbehind), plus a
  lookahead at the end of a pattern; the test turns away anything else.
- The Vim files are run by the `vim` and the `nvim` on `PATH`, when
  there are any: detection, the options, the syntax groups of the samples
  (compared with the lexer's tokens and with `*.vim.snap`), and the
  indent script, which must lay out `samples/object.c` as the formatter
  does.

After a change that is meant: `go test ./editors -update`, and read the
diff of the snapshots.
