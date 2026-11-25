# dgdtools

CLI tooling and a language server for the [DGD](https://github.com/dworkin/dgd) dialect of LPC, with editor clients for VS Code and Vim/Neovim.

Built on a native DGD 1.7 parser rather than adapted from LDMud or FluffOS, `dgdtools` ships as a single binary where the CLI and editor share the same `dgdtools.yml` configuration.

## CLI Tooling

Run against a full mudlib, in CI, or inside a single wizard checkout using a mudlib profile.

| Command | Purpose |
|---|---|
| `dgdtools lint` | Catch DGD compile errors and broken runtime calls, without guessing |
| `dgdtools format` | Whitespace-only formatter (`gofmt` style) that preserves line breaks |
| `dgdtools rename` | Rename symbols or move files across the mudlib with diff previews |
| `dgdtools profile` | Build compact `.profile` interface files for partial checkouts |

See the **[CLI Guide](docs/cli.md)**, **[Configuration & Profiles](docs/configuration.md)**, and **[Lint Rules](docs/rules.md)**.

## Language Server & Editors

`dgdtools lsp` brings live diagnostics, quick fixes, formatting, navigation, completion, references, and rename to any LSP-capable editor. Editor integrations use the `dgdlpc` filetype and never claim `.c`/`.h` files outside your mudlib.

| Editor | What's included |
|---|---|
| **[VS Code](editors/vscode/README.md)** | Marketplace extension with syntax highlighting and bundled `dgdtools` binary |
| **[Vim & Neovim](editors/vim/README.md)** | Syntax, indent, filetype detection, and auto-LSP for Neovim 0.11+ |

See **[Editor Support & LSP](editors/README.md)** for full server capabilities and generic LSP client setup.

## Installation

Install with Go 1.25 or later:

```sh
go install github.com/mmcdole/dgdtools/cmd/dgdtools@latest
```

Prebuilt binaries and VS Code `.vsix` packages are also available on the [Releases](https://github.com/mmcdole/dgdtools/releases) page (the VS Code extension bundles `dgdtools`, so no separate install is needed there).

<details>
<summary>Building from source &amp; corpus tests</summary>

```sh
git clone https://github.com/mmcdole/dgdtools
cd dgdtools
make build          # Outputs bin/dgdtools
make test
```

Run full-tree invariants (byte-exact lexer round-trip, token comparator, and formatter safety gate) against a real mudlib:

```sh
make corpus LPC_CORPUS=/path/to/lib
```

</details>

## Quick Start

File-local linting and formatting work with no configuration:

```sh
dgdtools lint path/to/file.c
dgdtools format --diff path/to/file.c
```

For cross-file checks, navigation, and refactoring, add a `dgdtools.yml`:

**Full mudlib** (placed beside `lib/` and your driver config):

```yaml
root: lib
driver_config: etc/mud.dgd
profile_build:               # only needed to publish a profile
  name: mymud
  share: ["kernel/**", "lib/**"]
  checkouts: ["usr/*"]
```

**Wizard checkout** (using your mud's published `.profile`):

```yaml
profile: mymud
```

Then run `dgdtools lint` or open the folder in your editor:

```text
$ dgdtools lint
room/hall.c:14:21: error: missing ';' before 'add_exit' [E001 syntax-error]
obj/key.c:9:19: error: function 'do_unlok' is not defined in /usr/ann/obj/key (this_object) [R001 callable-not-found]
```

See [dgdtools.example.yml](dgdtools.example.yml) and [examples/viking/dgdtools.yml](examples/viking/dgdtools.yml) for complete examples.

## Documentation

| Document | Contents |
|---|---|
| [CLI Guide](docs/cli.md) | `lint`, `format`, `rename`, exit codes, suppressions, output formats, and Go library overview |
| [Configuration & Profiles](docs/configuration.md) | `dgdtools.yml` reference, per-directory scopes, `.editorconfig`, and mudlib profiles |
| [Lint Rules](docs/rules.md) | Complete reference of all lint rules, codes, defaults, and options |
| [Editor Support & LSP](editors/README.md) | `dgdtools lsp` features, editor setup overview, VSIX builds, and grammar tests |
| [VS Code](editors/vscode/README.md) · [Vim / Neovim](editors/vim/README.md) | Per-editor installation and configuration |
| [Design Docs](docs/design/architecture.md) | Architecture ([architecture.md](docs/design/architecture.md), [config.md](docs/design/config.md), [formatter.md](docs/design/formatter.md), [lsp.md](docs/design/lsp.md)) |

## License Note

The DGD dialect details implemented here (keywords, operators, literal and escape forms, preprocessor behavior, and call semantics) are derived from DGD's grammar, documentation, and observable behavior. No DGD source code (AGPLv3) has been copied or translated into this repository.
