# The language server

`dgdtools lsp` speaks the Language Server Protocol (3.17, and the one
request of 3.18 it needs) on standard input and output. This document
says how it is built and why. It builds on the library described in
[architecture.md](architecture.md) (sections 6 and 10),
[config.md](config.md) and [formatter.md](formatter.md).

The server is a thin layer: transport, document versions, scheduling and
position conversion. Every answer is the library's: diagnostics are
`lint.Document`'s, fixes `diag.Fix`, formatting `format.Source`, what a
name refers to `pkg/resolve`, rename `pkg/rename`. Anything the server
would need that a script or the CLI could also use goes into the library
first, and the server's tests hold each answer to the library's.

## 1. Shape

- **A subcommand of the one binary**, not a separate server: one thing to
  install, and the server cannot be another version than the linter and
  the formatter. stdio only; `--log FILE` (a `log/slog` text log at debug
  level: each lint and request with its duration, panics with their
  stacks) and `--trace` (every message added to that log; needs
  `--log`). Standard output is the protocol; standard error is written
  only on a startup failure.
- **`pkg/lsp`** is the server, `pkg/lsp/lsptest` a client in the test's
  own process over a pipe; `cmd/dgdtools` only wires the streams to it.

| file | |
|---|---|
| `server.go` | `Serve`, the connection (the only file that knows `sourcegraph/jsonrpc2`), the order of things, `initialize`, `shutdown`, `exit`, `$/cancelRequest`, panics, the queue of outgoing notifications, what "idle" is |
| `sync.go` | open documents and their incremental changes; watched files; workspace folders and settings; the watchers asked of the client |
| `diagnostics.go` | when a document is linted and what is published: timers, the lint a newer change stops, the first whole-lib scan |
| `actions.go` | `codeAction`, `formatting`, `rangeFormatting` |
| `definition.go`, `hover.go`, `completion.go`, `symbols.go`, `references.go`, `tokens.go` | one feature each, the library's answer in the protocol's terms |
| `rename.go`, `files.go` | `prepareRename`, `rename`; `workspace/willRenameFiles` |
| `content.go` | `workspace/textDocumentContent`: the text of a profile's stub |
| `position.go`, `protocol.go` | `PositionMapper`, URIs and paths; the protocol's types as far as used |

Underneath is one type above all: `session.Session` (`pkg/session`),
which holds what a server holds (the open documents' texts, the
projects, a linter for each) and applies every change to all of it in
one step:

```go
s := session.New(vfs.OS{}, config.DiscoverOptions{ProfileDirs: dirs, UserConfig: user, Folders: folders})
a := s.Change(path, text)          // didOpen, didChange: the buffer is the file
a  = s.Change(path, nil)           // didClose: the disk is the file again
a  = s.DiskChanged(paths...)       // didChangeWatchedFiles: files and directories
// a.Files: whose diagnostics may differ; a.All: configuration changed, anybody's may
doc, err := s.Lint(ctx, path, lint.Options{NoScan: true})
if err == nil && s.Current(doc) { /* publish doc.Diagnostics */ }
```

- **One step.** A change sets or drops the buffer, tells the workspace (a
  `dgdtools.yml`, an `.editorconfig`, a profile, a driver configuration:
  the projects resolved from it are forgotten, their linters with them),
  tells every linter (the index starts its next generation), and returns
  the union of what the linters say is affected.
- **No lint mixes two texts.** A lint reads one generation of the index,
  and a unit knows the text it was built from: `Snapshot.Parse` refuses
  to pair it with another (`lint.Stale`). A lint that a change overtook
  either fails that way or is not `Current`; the server makes it again,
  which it was going to do anyway.
- **A document is self-contained.** `lint.Document` has the diagnostics,
  tokens, tree, `#define` environment, index unit and the `Snapshot` it
  was linted against, so hover and definition ask about the lib as the
  diagnostics saw it, from any goroutine.
- **Configuration is not special-cased.** Every watched path goes to
  `DiskChanged`; the server acts on `Affected`. A changed project is
  resolved and indexed again when one of its documents is next linted.

## 2. Transport and dispatch

**JSON-RPC is `github.com/sourcegraph/jsonrpc2`**, not hand-rolled. The
framing is easy; request/response correlation in both directions (the
server also sends requests), concurrent writers and orderly shutdown are
what goes wrong, and the library has years of production use. Its
websocket transport is in a package that is not imported.
`golang.org/x/tools/internal/jsonrpc2` cannot be imported, `x/exp`'s copy
promises nothing, and `go.lsp.dev/jsonrpc2` comes with
`go.lsp.dev/protocol`. Only `server.go` names the library.

**Protocol types are hand-written** (`protocol.go`), for the subset used.
Generated full-spec packages lag the spec, are large, and make every
optional field a pointer.

**Order.** Notifications that change state (`didOpen`, `didChange`,
`didClose`, `didChangeWatchedFiles`, `didChangeConfiguration`,
`didChangeWorkspaceFolders`) are applied one at a time in arrival order,
each as one call on the session. Requests run concurrently against a
`lint.Document`, each with a context that `$/cancelRequest` cancels. The
goroutine that reads never writes: a client may wait for its own message
to be read before it reads the server's. Outgoing notifications go
through one queue, so what was published last for a document is what the
client has.

A request is not cancelled by a newer version of its document; it is
answered `ContentModified` when the document changed meanwhile, which
clients pass over. A request has 5 s; one that takes longer is answered
with nothing, not with an error the editor would show.

The server has one request of its own, `dgdtools/idle`, answered when
nothing is scheduled, running or unsent. It is for tests and scripts
(section 12); an editor has no use for it.

## 3. Capabilities

Announced: `positionEncoding` (`utf-8` when offered, else `utf-16`);
`textDocumentSync` (open/close, incremental); `codeActionProvider`
(`quickfix`, `refactor.rewrite`, `source.fixAll.dgdtools`, only to a
client with code action literals); `completionProvider` (triggers `>`,
`:`, `"`, `/`, `<`; with `completionItem/resolve`);
`signatureHelpProvider` (`(`, `,`, retrigger `)`); `definition`,
`references`, `documentHighlight`, `hover`, `documentSymbol`,
`workspaceSymbol`, `documentFormatting`, `documentRangeFormatting`;
`semanticTokensProvider` (`full` with `delta`, and `range`);
`renameProvider` (with `prepareProvider` to a client with
`prepareSupport`); `workspace.workspaceFolders` with change
notifications; `workspace.textDocumentContent` for the scheme
`dgdtools-profile`; `workspace.fileOperations.willRename` for
`**/*.{c,h}` files and `**` folders.

Asked of the client when it says it can: `workspace/configuration`,
`client/registerCapability` for `workspace/didChangeWatchedFiles`,
`window/workDoneProgress/create`, `workspace/semanticTokens/refresh`.

**Everything optional is gated on what the client says**: related
information, tags and code descriptions on diagnostics, link support for
definitions, Markdown in hovers, label offsets and per-signature active
parameters, snippets, hierarchical symbols, `documentChanges` and change
annotations in edits. A plain client gets the same facts in the plainer
form; `lsptest` runs a rich and a plain client over every feature.

## 4. Projects, settings and watched files

- **A document finds its project from its own path**, exactly like the
  CLI: the nearest root config upward, settings scopes passed over. Two
  workspace folders under one lib share a project; one folder holding two
  libs has two. A file with no project gets the shared one without a
  config (file-local rules; the user configuration when there is one).
- **Settings say two things**, `dgdtools.profile` and `dgdtools.libPath`,
  for a folder without a `dgdtools.yml`: a wizard with no file of their
  own. They become `DiscoverOptions.Folders` (directory, profile, lib
  path), which hold for paths under the folder that no config claims, and
  never `DiscoverOptions.Profile/LibPath`, which are a command's flags and
  would win over every config in the workspace. A config file always
  wins; an editor cannot make the server disagree with the command line.
  They are read with `workspace/configuration` per folder, or from
  `initializationOptions` and `didChangeConfiguration`'s `dgdtools`
  section by a client without it. When they change, the session is made
  again and the open documents linted; otherwise never. No profile is
  ever assumed. The server passes what the library does not find itself:
  `ProfileDirs` (`profile.UserDirs`) and `UserConfig`.
- **Watching.** One registration of `workspace/didChangeWatchedFiles` for
  `**/*.{c,h}`, `**/dgdtools.yml`, `**/.dgdtools.yml`,
  `**/.editorconfig`, `**/*.profile`, `**/dgdtools-baseline.json`, plus
  each project's `Project.Inputs()` and `Linter.BaselinePath()` that the
  patterns miss (outside every folder, or an extended config, the driver
  configuration), as relative patterns where supported. It is registered
  again when that set changes. Every event goes to `DiskChanged`, a
  directory as the directory; the open documents in `Affected.Files` (all
  of them on `Affected.All`) are linted again.
- **A client without dynamic registration** of watched files is not asked,
  and the server then sees the buffers and nothing else that changes on
  disk (a `git pull`, `dgdtools rename --write`), as gopls does in the
  same case. It says so once, in the log (`window/logMessage`), so the
  stale answer is explained; the Vim README says the same.
- The baseline applies in the editor as on the command line.

## 5. Documents and scheduling

- **Incremental sync** (full text accepted too). Each range edit is
  applied with the negotiated encoding; the line table is then made again
  from the whole text, which cannot be wrong where an edit makes or
  unmakes a line break. `didOpen` makes the buffer the file even when it
  equals the disk; `didClose` returns the file to the disk and clears its
  diagnostics: diagnostics are of open documents only (gopls,
  rust-analyzer), and nothing would keep a closed file's true. `didSave`
  is not asked for. A document with no file (`untitled:`) is kept in step
  and nothing is said about it.
- **One timer and one running lint per document.** A change restarts the
  timer (200 ms) and stops the lint; a change elsewhere that affects the
  document schedules it after 1 s, so typing in a header does not re-lint
  twenty rooms per keystroke, but never postpones a lint for the
  document's own change. Lints of different documents run beside each
  other. A result is published only if it is of the current version and
  `Current`; what is published is compared with what was published last
  and not sent twice.
- **The scan.** Documents are linted with `Options.NoScan`. The first lint
  that needs the whole lib (`NeedsScan`: who inherits this, a self-call
  the chain does not provide) starts the lib's first scan in the
  background, with `$/progress` when it takes over 300 ms, and its end in
  the log (`mudlib scanned: 5205 files in 2.1 s (/path)`, or a warning).
  Only that first scan runs beside the lints: a later document linted
  with `NoScan` right after a change elsewhere would say less than before
  for a moment, and findings would blink. Once the scan is complete, lints
  run without `NoScan` and complete it themselves, a file or two after a
  change. In a partial checkout the scan covers the checkout.
- **A lint that fails on the configuration** is shown once
  (`window/showMessage`) and clears the document's diagnostics.

## 6. Positions and encodings

**What a column counts.** If the client offers `utf-8`, a column is a byte
offset in the line, which is what the lexer counts. Otherwise UTF-16, via
`diag.UTF16Col` and `diag.ByteCol` on the one line involved. Both live in
`PositionMapper`, used for every position in and every range out. The
mapper counts lines as the protocol does (`\n`, `\r\n`, a lone `\r`),
not by the lexer's table: the two differ about a lone `\r` inside an
unterminated string, which the random sessions found.

**What bytes are analyzed.** The lexer is byte-oriented and never
transcodes; real libs have Latin-1 files.

- An **open document** is the text the editor sent, as UTF-8. Positions on
  it are what the editor shows, and edits go back as ranges and Unicode
  text that the editor writes in whatever encoding it opened the file
  with.
- A file that is **not open** is read as disk bytes. Its columns count an
  invalid UTF-8 byte as one unit: exact when the editor decodes the file
  as a single-byte encoding, and for all-ASCII lines regardless.
- The hazard is the editor's: a Latin-1 file opened as UTF-8 shows
  replacement characters, and saving corrupts it. The VS Code extension
  sets `files.autoGuessEncoding` for `dgdlpc` and documents
  `files.encoding`; the server warns once when a file on disk is not
  valid UTF-8 and its opened text has U+FFFD, and a rename or move whose
  edits land in a file on disk that is not valid UTF-8 says so in its
  message.
- The server never writes source files; the client applies every edit.

Latin-1 libs stay as they are; converting one is its owner's decision.

## 7. Robustness and logging

- Every handler runs under `recover`: a panic becomes an `InternalError`
  (or is dropped for a notification), is logged with its stack, and the
  server goes on. The library already isolates a panic in one file
  (`lint.Internal`) and in one rule (`Document.Errors`: the rest of the
  document stands); both are logged, neither is a diagnostic.
- `shutdown` and `exit` per the spec; exit status 1 without `shutdown`.
  The server exits when its input closes.
- Memory: the index keeps facts, not trees; trees live as long as a
  request. Open documents keep one buffer each.
- **Messages.** `window/showMessage` is for what changes what the user
  should do: a configuration that fails, a refusal, a rename or move that
  leaves possible places. Everything informational goes to
  `window/logMessage`: which project a document belongs to and its notes,
  the scan's end, a text the formatter refuses, a partial answer to
  references, a rename's warnings that need no action (base files other
  objects inherit, which must be recompiled), the client without file
  watching. gopls and rust-analyzer do not pop up on a successful rename;
  neither does this server. `$/logTrace` gets each request's time.

## 8. Features

The rule for every answer is the linter's: **prove it or stay silent**.
Where the tools cannot tell (an unknown receiver, an object the index
cannot see, code that does not parse or is not compiled), the answer is
nothing or is marked as unknown, never the likeliest guess.

### 8.1 Diagnostics and code actions

A diagnostic's `code` is the rule's code (`R001`), as in every other
output; `codeDescription` links the rule's section of `docs/rules.md`;
`source` is `dgdtools`; related locations and tags (`unnecessary` for
unreachable code and unused suppressions) go to a client that reads
them. A fix is a `quickfix` with its finding, preferred when safe; all
safe fixes of a document are `source.fixAll.dgdtools`, which is what
"fix all on save" asks for. The two suppression actions are
`lint.Suppress`: a line comment above the finding at its indentation (or
into the comment already about that line), not offered where no line can
go; a file range, not offered where one exists. An "explain rule"
command is not built: the link is on the diagnostic.

### 8.2 Formatting

`formatting` and `rangeFormatting` are `format.Source(prj, path, text,
lines)` behind the formatter's gate; a refusal returns no edits and a
log line. `format.Edits` gives one edit per changed gap, so cursors and
folds survive. The project's settings win over the request's `tabSize`
and `insertSpaces`. A range that ends at column 0 does not include that
line. Format-on-save of modified lines is the client's use of
`rangeFormatting`.

### 8.3 Definition

`lint.Document.Resolve(off)`: `#include` targets, `inherit` paths (the
whole statement goes to the object, even when written with a macro,
since 95% of a lib's inherits are), inherit labels, calls of every form
(`fn()`, `::fn()`, `label::fn()`, `->fn` and `call_other` on
`this_object()` or a constant path, callbacks of `call_out` and
`conventions.call_registry`), macros (to their `#define`), globals,
locals and parameters, object paths handed to `clone_object` and the
like. A client with `linkSupport` gets `LocationLink`s with the origin
range; others `Location`s. A declaration in a profile stub is at
`dgdtools-profile://NAME/std/room.c`, whose text the server serves
(section 11.1).

Deliberately nothing for: an unknown receiver; a function only an
inheritor has (which inheritor runs is nobody's to say); a kfun (no
source; hover has it); the auto object's function where the chain has
an object the index cannot see, unless it is `nomask` (the unseen
object may redefine it: the corpus found 26 of 224,000 references going
wrong before this rule); something private elsewhere; code not compiled
or not parsed; from inside a stub (a stub is shown, not analyzed).
`declaration` and `typeDefinition` are not offered.

### 8.4 Hover and signature help

**Hover** is the declaration as written without its body, the comment
directly above it (block or a run of `//` lines ending on the line
before; read as Latin-1 when not UTF-8), and where it is (file and line,
`auto object`, `profile NAME`, the kfun's origin). An unknown receiver
says so; a macro shows its body and the class the parser takes it for.
Markdown (a `dgdlpc` code block, the comment escaped to show as typed)
to a client that reads it, plain text otherwise.

**Signature help reads tokens; hover reads the tree.** A call being
written does not parse, so the callee and the argument are found by
walking the compiled tokens left of the cursor to the open `(`. The
receiver is read from the tokens as `calls.Extract` reads the tree, and
tests hold the one to the other. The arguments `call_other` and
`call_out` pass on show the called function's signature, not the
registrar's `mixed...`: that is the function being written to.

**The kfun table** (`pkg/kfun`) is read out of the driver's source by a
test that also writes it (`TestDriverSource`, given `$DGD_SRC`), not
typed from the manual: the prototypes are the driver's own, and a new
driver version is a diff of one file. The price is no parameter names.
A driver's extension kfuns are the project's data (`dialect.kfuns`,
carried by a profile), never the table's. A kfun is the meaning of a
name only where the whole program is known and the auto object does not
redefine it: showing the driver's `clone_object(object)` for a lib's
`clone_object(string)` would be worse than nothing.

### 8.5 Symbols

**The outline** has one level (LPC nests no declarations; locals are
not listed, as in clangd, gopls and rust-analyzer): inherit statements
(by the object's lib path, kind `Class`), functions and prototypes,
global variables, `#define`s read by the preprocessor.

**`workspace/symbol`** searches the functions and globals of the lib's
units and the macros of headers, walking the units' own symbol tables
with no table of names beside them (the units are shared across
generations; a second table would be rebuilt at every change): 50 ms
on 108,000 files. It runs the scan first for at most 2 s, answering from
what is indexed. Matching is `internal/fuzzy`: a subsequence, smart about
case, in four ranks, then shorter names first — gopls' and
rust-analyzer's behaviour as a user sees it, without their scoring
tables. An empty query finds nothing; at most 200 results. In a partial
checkout the profile's stubs are searched too.

### 8.6 Completion

From the compiled tokens left of the cursor, so it works while the
statement does not parse, with the scopes of `syntax.Scope` (DGD's own:
parameters and the outermost block are one scope; a variable is in scope
from its declarator on). Offered: locals, globals, callable functions,
macros, kfuns where a name means one, keywords by position; after `::`
and `label::` the inherited functions; after `->` on `this_object()` or
a constant path the reachable functions, and on any other receiver an
empty list, not a guess; callback strings; `#include` and `inherit`
paths, from disk and from a profile's stubs. Items sort by nearness
(local, own, inherited, macro, auto, kfun, keyword); a comment is read
only in `completionItem/resolve`. A function comes with its parentheses
to a client with snippets. Over 2,000 items the list is the best 2,000
by fuzzy match and incomplete. A trigger character that begins nothing
(a `>` that compares, a `/` that divides) answers `null`.

### 8.7 References and highlights

**The reverse index is the units' own.** What a file alone says of each
place that names something another file can name (a function, global,
macro, object, header) is an `index.Use`, read with the unit
(`index.WithUses(resolve.Extractor(registry))`), so the whole-lib scan
makes them and they are invalidated with their unit and nothing else.
What a Use names is not stored, because it depends on other files; a
search looks each candidate up in the asking generation, which is what
`At` does, and so is the same answer by construction. A Use is 16 bytes;
on the VikingMUD lib (110,086 files) that is 4.47 million Uses. Lookups
go through `Snapshot.ChainFunc` and `ChainUnseen` over shared parent
chains rather than flattening each object's chain (850 MB down to 60 MB
after 51 searches).

Found: definitions and prototypes (with `includeDeclaration`), every
call that reaches a function (a redefinition's callers are its own), a
global's uses in its object and inheritors, a macro's uses in code,
directives, other macros' bodies and `inherit` statements, an object's
inherits and path arguments, a header's includes. A call on an object
nobody can tell is not listed and is counted (`Possible`); the log says
so, and in a partial checkout that the answer covers the checkout only.
`add_item`'s 140,161 places in 25,627 files take 60 ms.

**Highlights** are the document's references, `Write` where declared,
assigned or incremented, `Read` elsewhere (an indexed assignment reads
the variable, as in gopls).

### 8.8 Semantic tokens

Made in the lint that publishes the diagnostics, kept with the document,
so a request finds them made; a request ahead of the scheduled lint
lints the document, and the scheduled lint reuses that. `full` keeps the
array with an id; `full/delta` answers one edit between the common
prefix and suffix; `range` keeps nothing. When a lint after a change
elsewhere makes other tokens for the same version,
`workspace/semanticTokens/refresh` is sent. One walk per file
(`syntax.PathsAt`, memoized lookups): 7 µs median per file on the
VikingMUD lib, 6 ms the slowest, so there is no size budget. The legend
is in section 10.2.

## 9. Rename and refactorings

A mud renames a function mud-wide, in one sweep, and the old name is
gone that day; there is no deprecation machinery here (no forwarding
stubs, no two-step migration). What the tool adds is the judgment grep
lacks: for every place the name occurs, whether it is the symbol, might
be, or is not.

**One engine, two front ends.** `pkg/rename` returns a plan and writes
nothing; the server and `dgdtools rename` show the same plan, so a
rename previewed in an editor and one made from a shell cannot differ.

```go
t, err := rename.Prepare(ctx, f, off)          // *rename.Target, or a *rename.Refusal
plan, err := rename.Rename(ctx, f, off, "new")  // or a *rename.Refusal
plan.Occurrences   // Path, Start, End, Line, Col, Class (Certain, Possible, Excluded), Decl, Reason
plan.Edits         // the Certain ones
plan.PossibleEdits // the Possible ones, made only when asked for
plan.Warnings, plan.Dynamic, plan.Files, plan.Partial
plan, err = rename.Files(ctx, snap, []rename.Move{{From: old, To: new}})  // files and directories moved
```

### 9.1 What is renamed

- **What one file decides**: locals, parameters, `catch` variables,
  private functions and variables, inherit labels, macros a `.c` file
  defines for itself. Every identifier of the name is asked of `At`; one
  in code not compiled or not parsed makes the rename a refusal, since
  in one file that is a place it would get wrong. A macro is renamed as
  a whole, in every branch, so the file means what it meant.
- **Across files**: a function that is not private, a global that is not
  private, a header's macro, and what a file other files include
  declares. A function is renamed with its **family**: DGD calls by name
  in the object's table, which holds the latest definition, so a function
  and its redefinitions up and down the hierarchy (siblings included) are
  one function by the driver's rules, and renaming one alone changes what
  the lib does. A private function redefines nothing and joins nothing.
  The declarations of the files one program is made of (a prototype in a
  header, the definition in a file included as code) are one function.
  Here possible places make a plan, not a refusal: one unparsable file of
  110,000 would otherwise stop every rename of the lib.

**DGD and private functions.** `call_other`, `->`, `call_out` and every
registrar's callback find a function in the object's table, which has no
private function (`Control::makeSymbols`). So `call_out("fn")` and
`this_object()->fn()` do not call a private `fn`: they are left out of
its rename and named in a warning. The linter reports them
(`static-crossobj`, `private-callback`).

### 9.2 Certain, possible, excluded

Each Use of the old name is looked up where it is:

- **certain**: it reaches one of the declarations renamed and nothing
  else: definitions, prototypes, direct, `::` and `label::` calls, `->`
  and `call_other` on `this_object()` or a constant path, callbacks with
  a known object;
- **possible**: a call on an object nobody can tell; a callback on
  `this_object()` where the object lacks the name (an inheritor may have
  it); a chain with a file that cannot be found; a name several inherited
  objects have; the name in code not compiled or not parsed, or in a
  `#define` body, where its meaning depends on the use;
- **excluded**: it reaches another declaration, or nothing in a complete
  chain.

Calls whose function name is made at run time (`"do_" + verb`) cannot be
searched. The plan lists them (`plan.Dynamic`, with their locations),
those whose constant part the old name begins or ends with first. A
rename is called complete only when nothing is left possible.

Occurrences in comments and in strings that are no callback or path are
not renamed and not listed, as in gopls and rust-analyzer: listing every
word would bury the possible places that matter.

### 9.3 Refusals and warnings

Refused, each with its reason: a kfun, or a function whose name is also
one (a call the rename cannot see would reach the driver's); the create
function and the driver object's entry points (`initialize`,
`compile_error`, ...), which the driver calls by name, as old names and
as new ones; a family with a declaration in a profile stub ("defined in
the mudlib profile"); a macro the driver or configuration defines; a
variable `save_object` writes (found from `save_object`,
`restore_object` and `conventions.autosave_markers` in the object, its
parents and inheritors), since renaming it loses data at the next restore
with nothing in any log — `--force` makes this a warning, and the
editor's error points there; a lib not yet indexed in full, or files
changed while planning. Of the new name: not a name, a keyword, a macro
where an edit lands, a function or variable the objects that see the
symbol already have (`ChainFunc`, `ChainVar`), a kfun, a local it would
capture or be hidden by, a macro used where an edit lands whose body
names either name, an argument of a stringizing macro.

Warnings: the edited files other objects inherit or include, with
counts (those objects keep the program they were compiled with until
they are recompiled); the functions the driver calls in a connection
object (`open`, `close`, `receive_message`, ...), renamed with a warning
because which objects are connections is the lib's decision; a partial
checkout.

### 9.4 Partial checkouts

A wizard's checkout with a profile has the rest of the lib's
declarations and none of its call sites. Exact: what one file decides,
and every place among the checkout's files. Within the checkout with a
warning: a public function or global, and anything of a file code
outside may inherit. Refused: a symbol whose definition is a stub.

### 9.5 Files moved

`rename.Files` collects the places that name a moved file: the reverse
index's `UseObject` and `UseInclude` Uses in every unit (every
`inherit`, `#include` and object path argument), the calls made by path
(`"/std/room"->fn()`), and the moved files' own references, which name
other files from where they were. Each is rewritten as it was written
(absolute stays absolute, `.c` kept or not, a relative include stays
relative where it can) and resolved as the driver will see it once the
files have moved (`Snapshot.ResolveIncludeIf`, `ObjectPath`); a text that
would not resolve to the moved file is not made, and one that does not
depend on the file's location is left alone. A path made by a macro or
an expression is Possible, never edited: a macro's string is every
user's. A file moved out of the lib leaves its places Possible. Edits
are of the files as they are, before the move.

### 9.6 Code actions

- **Add the prototype** (`Document.Prototype`): DGD compiles a call to a
  function not yet declared as a call to an undefined one, which
  typechecking refuses; a prototype before the call fixes that. Offered
  as `refactor.rewrite`, safe, only where a prototype can be written
  exactly.
- **Fix a path** (`Document.PathFixes`) for `include-not-found` and a
  missing object written as one string: the one file of the lib with the
  missing base name, written as the path was. None when no file or
  several have the name (the choice is the person's). Unsafe, so not
  preferred and not in "fix all"; computed when asked for, since the
  search reads directories.
- **Suppress** (section 8.1).

### 9.7 In the server

- `prepareRename` answers the range and placeholder, `null` where nothing
  is named, or `RequestFailed` with the reason, which VS Code and Neovim
  show as is. On an `#include` or `inherit` path it says to rename the
  file. A symbol other files can name makes the whole-lib scan run first
  (up to the request's budget).
- `rename` answers one `TextDocumentEdit` per file in `documentChanges`
  (an open document's version, `null` for a file not open), or
  `changes`. To a client with `changeAnnotationSupport` every edit is
  annotated: the certain ones under `certain`, the possible ones under
  one annotation per reason with `needsConfirmation`, so VS Code's preview
  shows them unticked by reason. Other clients get the certain edits
  only. A forcible refusal's error names `dgdtools rename --force`.
- `workspace/willRenameFiles` answers the move's `WorkspaceEdit` the same
  way. A refusal is `null` and a message, never an error: the editor's
  move goes ahead whatever the server says. VS Code sends the request
  from its explorer; Neovim's core does not, file explorer plugins
  (oil.nvim, nvim-tree) do.
- **Messages** follow section 7: possible places left (unticked in the
  preview, or not renamed, the first few listed with
  `dgdtools rename --include-possible` named), refusals and the
  dynamic calls that may reach the function go to `window/showMessage`;
  so do edits in a file on disk that is not valid UTF-8; warnings that
  need no action (inherited base files) go to `window/logMessage`. A successful rename with
  nothing to say says nothing. The wording is the library's
  (`rename.Plan`'s summary), the same as the command's.

## 10. Syntax highlighting: two layers

A grammar in the editor for the lexical layer, which works with no
server, offline, on a file that does not parse; semantic tokens from the
server for what only analysis knows, an overlay that may arrive late or
not at all.

### 10.1 Editor-side grammars

Everything lives under `editors/`, so a grammar, a client and the server
they talk to are one commit. The grammars are checked against one word
list (`editors/common/dgdlpc.json`), which `go test ./editors -update`
writes from `pkg/token`, `pkg/lexer` and `pkg/pp`, and against shared
samples with a snapshot per grammar.

**The TextMate grammar** (`source.dgdlpc`, VS Code and anything else
that reads TextMate) covers comments, directives to the end of the
logical line, DGD's strings, numbers (`1..2` is not a float), exactly
the driver's keywords, the `({ })` and `([ ])` brackets, every operator,
function names at definition and call by pattern, and inherit labels. It
does not tell macros from identifiers beyond the ALL_CAPS convention and
does not list kfuns: those are known by the server, not guessed. One
fixed file cannot follow a project's dialect: `//` is always a comment
(harmless where the dialect has it off: there it is a syntax error the
linter reports), and `function` is never a keyword (it is a plain name
without `dialect.closures`; the server's tokens colour it where it is
one). `])` closes a mapping only where one is open; the language
configuration lists `({ })` but not `([ ])`, which as a text pair would
unbalance every `f(a[i])`.

**Tested by running the grammar from Go**, not by a Node tool: a small
interpreter of the format tokenizes the samples, compared byte for byte
with `pkg/lexer`'s tokens and with the snapshots. The price is a regular
expression syntax Go and Oniguruma read alike, which the test enforces.

**Vim: a separate `dgdlpc` filetype**, not a DGD mode in the stock
`lpc.vim`, which targets MudOS (colouring `foreach`, `class`, `(: :)` as
valid, knowing none of `nil`, `atomic`, `rlimits`), would have to be
mostly subtracted, and has been unmaintained since 2016. A separate
filetype lets detection be precise: `*.c`/`*.h` are `dgdlpc` only under a
`dgdtools.yml` or a directory of `g:dgdlpc_roots`, never globally, and a
buffer whose name is a URL (`fugitive://`, `scp://`) is left to Vim. In
Neovim the rule runs ahead of the built-in detection, so no C server
starts on an LPC file. The ftplugin makes `gf` and `[i` work without the
server; indent is `cindent` with options that match the formatter, and
the shapes that differ are listed in `indent/dgdlpc.vim` and left to the
formatter.

**tree-sitter is deferred**: a second grammar with an external scanner
for directives anywhere, query files and each editor's packaging (6 to 8
days, and maintenance) for tree-sitter-only editors such as Helix and
Zed. It would be for highlighting only, never the linter's parser
([architecture.md](architecture.md) 2.1). Until there are users asking,
Neovim uses the Vim syntax plus semantic tokens.

### 10.2 Semantic tokens

Tokens add only what analysis knows and never restate the grammar (no
keywords, strings, comments, operators), which keeps payloads small and
avoids flicker. Standard types only, so themes work unconfigured, plus
three custom modifiers (`inherited`, `local`, `macro`):

| Token | Type | Modifiers |
|---|---|---|
| inherit label | `namespace` | `declaration` where declared |
| macro the parser takes for a type / for specifiers (or empty) | `type` / `modifier` | `macro` |
| any other macro, and the name at its `#define` | `macro` | `declaration` at the `#define` |
| function at its prototype / definition | `function` | `declaration` / `definition`; `static` |
| call of the object's own / an inherited function | `function` | — / `inherited`; `static` |
| call of the auto object's function or a kfun | `function` | `defaultLibrary` |
| `->fn`, `call_other` | `method` | as a call, by the receiver's lookup; none for an unknown receiver |
| global / local variable | `variable` | `inherited`, `static` / `local`; `declaration` where declared |
| parameter | `parameter` | `declaration` |
| `function` where the lib has closures | `keyword` | |

"Unresolved" is not a token: a finding says that, and a name nobody
knows keeps the grammar's colour. Of several declarations a name may
have, a modifier is given when all have it. The tokens claim nothing the
grammars claim otherwise. VS Code maps types and modifiers to the
grammar's scopes for themes without semantic rules
(`semanticTokenScopes`); Neovim links the standard types itself, and
`editors/vim/README.md` has links for the modifiers.

## 11. Editor clients

The language is `dgdlpc` everywhere (VS Code id, Vim filetype, Emacs `dgdlpc-mode`). `lpc` is taken in both ecosystems by tooling for other
drivers, and a file handed to two grammars and two servers under one id
is worse than a new word.

### 11.1 VS Code

`editors/vscode`: the manifest, the grammar, the language configuration,
and `src/`, TypeScript over `vscode-languageclient` 11 (LSP 3.18, VS Code
1.91 or later), bundled into one file by esbuild, as rust-analyzer,
vscode-go, Biome and Ruff do: the types are the first test of calls made
without a VS Code at hand, and one bundled file is what a package should
be. No linter, formatter or test framework beyond `tsc --strict` and
Node's test runner.

- **Activation**: `workspaceContains` for the two config names, and what
  VS Code adds (a document of the language, a command). The server
  starts then.
- **Settings**: `dgdtools.path` (empty: the package's binary, else `PATH`;
  a bare name on `PATH`; else a file), `dgdtools.profile` and
  `dgdtools.libPath` (resource scope, synchronized),
  `dgdtools.logFile`, `dgdtools.trace.server`. `path` and `logFile` are
  `machine-overridable` and restricted in untrusted workspaces: a
  workspace cannot choose which program runs or which file is written
  (vscode-go's arrangement). No executable found: one message saying what
  was looked for, with buttons to the setting and the README; nothing is
  searched elsewhere.
- **Which files are LPC is a workspace setting the user is asked
  about.** `files.associations` in the workspace's settings, never the
  user's, with patterns that name the folder (the setting has window
  scope, so a nested or multi-root folder gets `**/DIR/**/*.c`). The
  commands "Use LPC (DGD) for This Folder's .c/.h Files" and its inverse
  write and remove exactly those keys, and list (or unlist) Microsoft's C
  extensions as unwanted recommendations in `.vscode/extensions.json`,
  per folder. On activation, for each outermost config directory not yet
  associated, the extension asks once (Yes, Never remembered, dismissed
  asks again). The extension never sets a document's language behind the
  user's back, but for a profile's stub.
- **Stubs** are served by the server through `workspace/textDocumentContent`
  (LSP 3.18): the client library registers the content provider, so the
  extension has no code for it and any 3.18 client gets the same. The
  server answers from the profile of that name a project in use has,
  comparing case-insensitively when nothing matches exactly (VS Code
  lower-cases a URI's host). Stubs are read-only by their scheme. Not
  built: `workspace/textDocumentContent/refresh` when a profile changes.
- **Checked** by `npm run check`, `npm test` (the server lookup, the
  setting's values, the association patterns) and `npm run build`, all
  in CI, and by `TestVSCodeClient`, which holds the manifest to the code
  and to the server (commands, settings and their scopes, the stub
  scheme, the token legend, the engines, the license). No test starts VS
  Code: `@vscode/test-electron` downloads VS Code at every run and needs
  a display.

### 11.2 How a release carries the binary

A wizard has no Go toolchain, so the model is rust-analyzer's and
Ruff's: **a package of the extension for each platform, with the binary
inside**, served by the Marketplace and Open VSX by platform, plus one
package without a binary for the rest.

- **Which binary runs**: `dgdtools.path` when set; else the package's;
  else `PATH`. The package's before `PATH` because client and server are
  one commit and one release, so the client never meets an older server.
  The packaged file is made executable if needed; the output channel
  shows the path, which rule chose it, and the server's version from
  `serverInfo`.
- **`editors/vscode/scripts/vsix.mts`** packs from GoReleaser's
  `dist/artifacts.json` (or one `--binary`): six targets, a table that
  `TestVSCodePackages` holds to `.goreleaser.yaml`. The version is the
  tag, given to vsce; `package.json` is never written. `@vscode/vsce` and
  `ovsx` are pinned devDependencies.
- **The release job** (`vscode` in `release.yml`) takes GoReleaser's
  binaries from the same run as an artifact, packs, attaches the seven
  packages to the release, and publishes only when the token secret
  exists and the tag is not a pre-release; `--publishable` first refuses
  a manifest nobody made ready. **CI** (`vsix` in `ci.yml`) runs the same
  packing on every push and checks that the runner's package has a
  binary that runs.
- **No downloading, by decision.** Fetching a binary at first run costs
  network code in an editor (proxies, rate limits, a first run that can
  fail), a checksum that proves little from the same source, a place to
  keep and update the binary, and an extension that fetches and runs
  programs. A package per platform is delivered and updated by the
  marketplace like any other extension and works offline.

### 11.3 Vim, Neovim, Emacs, others

- **Vim and Neovim**: `editors/vim` is one plugin for both (Vim reads
  neither `lsp/` nor `plugin/*.lua`). For Neovim 0.11+,
  `lsp/dgdtools.lua` is the `vim.lsp.config` entry, and
  `plugin/dgdtools.lua` enables it for `dgdlpc` buffers when the
  configured command is executable, unless `g:dgdtools_lsp` is false;
  nothing needs to be in nvim-lspconfig. For Vim, `editors/vim/README.md`
  has the lines for vim-lsp, ALE and coc.nvim, including how to pass the
  profile and lib path. Vim clients and Neovim on Linux usually do not
  watch files (section 4).
- **Emacs**: `editors/emacs/dgdlpc-mode.el`, a major mode for Emacs
  29.1+ derived from `prog-mode`, not `c-mode`: a mode derived from
  `c-mode` runs the user's C hooks (`eglot-ensure` with clangd, a C
  style) on LPC files, matches Eglot's C entry and brings cc-mode's C
  keywords; undoing that is a cc-mode language definition, more code
  than the syntax table, word lists and indentation function a C-like
  language needs. Detection is the Vim rule, asked from
  `magic-mode-alist` (before `auto-mode-alist`, so no C hook runs
  first), with `dgdlpc-roots`; remote files are left to C, as URL
  buffers are in Vim, unless a remote directory is listed there (matched
  as text, for highlighting and indentation only). The name test is in the autoloads, so the package
  loads with the first `.c`/`.h` file. The server is added to
  `eglot-server-programs` when Eglot loads (not before, and Eglot is
  never loaded for it); it is started by the user's `eglot-ensure` in
  `dgdlpc-mode-hook`, the Emacs norm. For Eglot only (`eglot-lsp-context`),
  a buffer-local `project-find-functions` entry roots the project at the
  nearest `dgdtools.yml` or `dgdlpc-roots` entry, as Neovim's root
  markers do. Indentation is an indent function that counts brackets as
  the formatter does, not cc-mode's engine. lsp-mode gets lines in the
  README, not code.
- Helix, Sublime (LSP), Kate: a line of configuration each,
  documented by the README for any client, not maintained.

## 12. Testing

- **Under the server**, in `pkg/session`: a change is one step while
  lints run; configuration, profile, baseline and `.editorconfig` changes
  reach their files and no other project; `TestDifferential` holds what
  a front end would show after random edits, creations and deletions to
  what a fresh session finds.
- **Transcripts**: `pkg/lsp/testdata/*.txt`, played by `TestTranscripts`
  (`-update` rewrites them): a lib's files, a script of client messages,
  the expected server messages with paths and versions normalized. There
  is no `wait`: a script asks for what was published once the server is
  idle (`dgdtools/idle`), which makes a transcript the same on every run.
  Code actions, definition, hover and rename have a plain-client
  transcript beside the rich one; positions are played in both
  encodings.
- **The server is the library**: `TestDiagnosticsAreTheLinters`,
  `TestDefinitionIsResolve`, `TestHoverIsResolve`,
  `TestCompletionIsResolve`, `TestReferencesAreTheLibrarys`,
  `TestTokensAreTheLibrarys`, `TestRenameIsTheLibrarys` ask at every
  offset of every file of a fixture lib, in both encodings, and hold the
  answer to the library's; `TestLSPMatchesLint` in `cmd/dgdtools` holds
  published diagnostics to `dgdtools lint --output-format json`.
- **Sync and order**: `TestIncrementalChanges` and `FuzzChange` (edits in
  both encodings against a plain string; the mapper against positions
  worked out the long way); `TestRandomSession` (edits, opens, closes,
  disk changes; after each step what is published is what a fresh lint
  finds); `TestBurst` (200 changes publish only the last); `TestCancel`,
  `TestPanics`, `TestEndings`, `TestLatin1`, `TestDependents`,
  `TestWatchedFiles`, `TestSettings`. All under `-race`.
- **Editors**: `TestNeovim*` in `editors` drive the `nvim` on `PATH`
  through each feature, and `TestEmacsEglot` drives Eglot in the
  `emacs` on `PATH`; CI installs both so they run there.
- **Corpus** (`-tags corpus`, `$DGDTOOLS_CONFIG`, the VikingMUD lib):
  `TestCorpusSession` opens 2,003 files (9 Latin-1), types a character
  into each and takes it out: what is published equals a whole-lib
  linter's findings, 4 ms median from open to diagnostics. The
  `TestCorpus*` tests of `pkg/resolve` and `pkg/rename` hold every answer
  of a sample to the files (resolve, hover, completion, references,
  tokens) and every rename to a re-lint of the whole renamed lib (the
  diagnostics unchanged but for the new name and the possible places
  left). `TestCorpusMemory` measures what a server holds after the scan
  (architecture.md 4.1).

## 13. Not built

- Pull diagnostics and whole-project diagnostics: on a lib with hundreds
  of thousands of baselined findings, open documents are what matters.
- Inlay hints, call hierarchy, `implementation` (the reverse index could
  answer it).
- Receiver flow (`ob = clone_object("/obj/x"); ob->fn()`), as the linter
  lacks it too; a macro after `->`.
- Anything from inside a stub: stubs are shown, not analyzed.
- An "explain rule" command, a "show baselined findings" setting.
- Renaming an inherit label of a file others include (no header in the
  VikingMUD lib has one); occurrences only in uncompiled code of files
  that neither see the symbol nor are read again (the index keeps no Uses
  of such code).
- The tree-sitter grammar (10.1) and a Zed extension.

## 14. Decisions

Each with the alternative it was chosen over.

- **`sourcegraph/jsonrpc2`**, not a hand-rolled transport; protocol
  types hand-written, not generated (section 2).
- **A subcommand of the one binary**, not a separate server.
- **Diagnostics for open documents only**, and none for a closed one.
- **Latin-1 files stay as they are**; editors are configured for them
  (section 6).
- **File association is workspace-scoped**, never claimed globally.
- **The language is `dgdlpc`**, not `lpc`.
- **The editor's settings say two things** (profile, lib path) for a
  folder without a config, and a config wins.
- **Profiles come from the search path** `profile.Resolve` uses; dgdtools
  has no network code.
- **Informational messages go to the log**, popups only for what needs
  the user (section 7).
- **Prove it or stay silent**: a definition, a hover, a token, a
  completion after `->` is given only where it is known (section 8).
- **The kfun table is read from the driver's source**; extension kfuns
  are the project's data.
- **Signature help reads tokens, hover reads the tree.**
- **An outline has one level; a search by name walks the units**;
  fuzzy matching is a subsequence in four ranks.
- **One rename engine for editor and command line, a function renamed
  with its family, no deprecation machinery** (section 9).
- **Stubs open as read-only virtual documents** served with
  `workspace/textDocumentContent`.
- **Editor support lives in this repository**, and the grammars are
  tested by `go test`.
- **A separate Vim filetype**; **tree-sitter deferred**.
- **The VS Code client is TypeScript bundled into one file**; published
  as `mmcdole.dgdtools` to the Marketplace and Open VSX.
- **A release carries the binary in a package per platform**; the
  extension downloads nothing (11.2).
