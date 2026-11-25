# LPC (DGD) for Helix

Helix highlights only through tree-sitter, and there is no tree-sitter
grammar for DGD LPC yet ([lsp.md](../../docs/design/lsp.md), 10.1), so
LPC files can have C's highlighting at best (below), which gets `({ })`
and `::` wrong. The language server, `dgdtools lsp`, works now:
diagnostics, fixes, formatting, navigation, completion, rename.

With `dgdtools` on `PATH` ([install it](../../README.md#installation)),
put this in the mudlib's own `.helix/languages.toml`, so that its `.c`
and `.h` files are LPC and those of every other project stay C:

```toml
[language-server.dgdtools]
command = "dgdtools"
args = ["lsp"]

[[language]]
name = "dgdlpc"
scope = "source.dgdlpc"
file-types = ["c", "h"]
roots = ["dgdtools.yml", ".dgdtools.yml"]
comment-tokens = "//"
block-comment-tokens = { start = "/*", end = "*/" }
indent = { tab-width = 4, unit = "    " }
grammar = "c"
language-servers = ["dgdtools"]
```

Helix looks for a language's queries under its own name, so `dgdlpc`
has no highlighting until it is told to use C's. Once per machine
(Helix reads queries from its runtime directories, not from a project):

```sh
dir=~/.config/helix/runtime/queries/dgdlpc   # %AppData%\helix\runtime\queries\dgdlpc on Windows
mkdir -p "$dir"
for q in highlights indents injections locals textobjects; do
  echo '; inherits: c' > "$dir/$q.scm"
done
```

`; inherits: c` is how Helix's own C++ queries take C's. `hx --health
dgdlpc` says whether the server and the queries are found. These lines
were not run in Helix.
