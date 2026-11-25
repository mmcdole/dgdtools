# LPC (DGD) for Vim and Neovim

This directory is a plugin for both editors: the filetype `dgdlpc`
(detection, syntax, filetype plugin, indent) and, in Neovim 0.11 and
later, the language server `dgdtools lsp`, started by itself when
`dgdtools` is on `PATH` ([install it](../../README.md#installation)).

## Installing

The plugin is the subdirectory `editors/vim` of the repository.

```vim
" vim-plug
Plug 'mmcdole/dgdtools', { 'rtp': 'editors/vim' }
```

```lua
-- lazy.nvim
{
  'mmcdole/dgdtools',
  config = function(plugin)
    vim.opt.rtp:append(plugin.dir .. '/editors/vim')
    -- lazy.nvim has loaded the plugins of the runtime path by now.
    vim.cmd.runtime({ 'ftdetect/dgdlpc.lua', 'plugin/dgdtools.lua', bang = true })
  end,
}
```

```lua
-- vim.pack (Neovim 0.12), in init.lua
vim.pack.add({ 'https://github.com/mmcdole/dgdtools' })
vim.opt.rtp:append(vim.pack.get({ 'dgdtools' })[1].path .. '/editors/vim')
```

Without a plugin manager, as a package (for Neovim the directory is
`~/.local/share/nvim/site/pack/dgdtools/start`):

```sh
git clone https://github.com/mmcdole/dgdtools ~/src/dgdtools
mkdir -p ~/.vim/pack/dgdtools/start
ln -s ~/src/dgdtools/editors/vim ~/.vim/pack/dgdtools/start/dgdtools
```

Vim needs `filetype plugin indent on` and `syntax on` in its vimrc;
Neovim has both by default.

## Which files are LPC

Nothing here claims `*.c` or `*.h` by name: they stay C, except under a
directory that has a `dgdtools.yml` or a `.dgdtools.yml` (the file's own
directory or any above it), or under a directory listed in
`g:dgdlpc_roots`:

```lua
vim.g.dgdlpc_roots = { vim.fs.normalize('~/mud/players/me') }
```

In Vim such a file is given the filetype `c` first and `dgdlpc` right
after, because Vim's own detection runs first; in Neovim it is `dgdlpc`
from the start.

## The filetype

- **Syntax:** the driver's reserved words and no others, `({ })` and
  `([ ])`, DGD's escapes, `1..2`, `->`, `<-` and `::`, directives to the
  end of their logical line.
- **Filetype plugin:** `commentstring` and `comments`; `gf`, `[i` and
  `[d` on `#include`, `inherit` and `#define` lines, a path beginning
  with `/` looked up from the directories above the file; `%` between
  `#if`, `#else` and `#endif` with matchit.
- **Indent:** C indenting set to what `dgdtools format` writes, with
  the lines of an array or mapping literal one level in from the line
  that opens it, however deeply literals nest. Three shapes still come
  out otherwise, and the formatter settles them: a block after a case
  label on the same line, an element of a literal continued over
  several lines, and the statement after a goto label. `shiftwidth` and
  `expandtab` are yours (or your `.editorconfig`'s); the formatter's
  default is four spaces.

Variables (`b:` is asked before `g:`, so a mudlib can differ):

| | default | |
|---|---|---|
| `g:dgdlpc_slash_slash` | `1` | `//` begins a comment, as with `dialect.slash_slash: true`. `0`: it is two operators |
| `g:dgdlpc_closures` | `0` | `1`: `function` is a type keyword, as with `dialect.closures: true` |
| `g:dgdlpc_roots` | `[]` | directories whose `*.c` and `*.h` are LPC without a `dgdtools.yml` above them |
| `g:dgdlpc_no_functions` | `0` | `1`: do not highlight a name before `(` |
| `g:dgdlpc_minlines` | unset | synchronize the syntax on comments within this many lines, instead of reading the file from its start |

The two dialect variables are set by hand to what the mudlib's
`dgdtools.yml` says; the runtime files do not read it.

## The language server in Neovim

With the plugin installed and `dgdtools` on `PATH`, a `dgdlpc` buffer
gets the server: `:checkhealth vim.lsp` shows it attached. What it does
is in [editors/README.md](../README.md#the-language-server); in Neovim
the usual keys reach it: `gra` for the fixes, `grn` to rename, `grr` for
references, `K` for the hover, `CTRL-]` to go to a definition, `gO` for
the outline, `CTRL-X CTRL-O` to complete, `gq` and
`:lua vim.lsp.buf.format()` to format.

The configuration is `lsp/dgdtools.lua`, under the name `dgdtools`.
Change any of it with `vim.lsp.config`; for a folder with no
`dgdtools.yml`, say where it is and what it is:

```lua
vim.lsp.config('dgdtools', {
  root_dir = vim.fs.normalize('~/mud/players/me'),
  settings = { dgdtools = { profile = 'mylib', libPath = '/players/me' } },
})
```

The plugin decides when the first LPC file opens, so a `cmd` set this
way by then (in `init.lua`, or in a plugin manager's `config` function)
is the command it looks for, and a `dgdtools` that is not on `PATH` is
started too. To start the server yourself, or not at all, set this
before the first LPC file opens:

```lua
vim.g.dgdtools_lsp = false
vim.lsp.enable('dgdtools') -- if and when you want it
```

nvim-lspconfig has no entry for dgdtools and needs none: the plugin
supplies `lsp/dgdtools.lua`, and `vim.lsp.config('dgdtools', ...)`
works the same with nvim-lspconfig installed.

Semantic tokens are shown by themselves (`:Inspect` on a name). The
standard types are linked already; the modifiers can be given colours:

```lua
vim.api.nvim_set_hl(0, '@lsp.typemod.function.defaultLibrary.dgdlpc', { link = '@function.builtin' })
vim.api.nvim_set_hl(0, '@lsp.typemod.method.defaultLibrary.dgdlpc', { link = '@function.builtin' })
vim.api.nvim_set_hl(0, '@lsp.typemod.function.inherited.dgdlpc', { link = '@function.call' })
vim.api.nvim_set_hl(0, '@lsp.typemod.type.macro.dgdlpc', { link = 'Type' })
vim.api.nvim_set_hl(0, '@lsp.typemod.modifier.macro.dgdlpc', { link = 'StorageClass' })
```

To format on save:

```lua
vim.api.nvim_create_autocmd('BufWritePre', {
  pattern = { '*.c', '*.h' },
  callback = function(ev)
    if vim.bo[ev.buf].filetype == 'dgdlpc' then vim.lsp.buf.format({ bufnr = ev.buf }) end
  end,
})
```

To try it from a checkout without touching your configuration:

```sh
make build    # bin/dgdtools
PATH=$PWD/bin:$PATH nvim --clean --cmd 'set rtp^=editors/vim' /path/to/your/lib/std/room.c
```

## The language server in Vim

Vim has no client of its own; these are the lines for the common ones.
The root is the directory of the `dgdtools.yml` (`dgdlpc#Root`), else
the file's own. With [vim-lsp](https://github.com/prabirshrestha/vim-lsp):

```vim
if executable('dgdtools')
  autocmd User lsp_setup call lsp#register_server({
      \ 'name': 'dgdtools',
      \ 'cmd': ['dgdtools', 'lsp'],
      \ 'allowlist': ['dgdlpc'],
      \ 'root_uri': {server_info -> lsp#utils#path_to_uri(dgdlpc#Root(expand('%:p:h')) ?? expand('%:p:h'))},
      \ })
endif
```

With [ALE](https://github.com/dense-analysis/ale), in the vimrc (after
`plug#end()` if vim-plug installs ALE):

```vim
call ale#linter#Define('dgdlpc', {
    \ 'name': 'dgdtools',
    \ 'lsp': 'stdio',
    \ 'executable': 'dgdtools',
    \ 'command': '%e lsp',
    \ 'project_root': {buffer -> dgdlpc#Root(fnamemodify(bufname(buffer), ':p:h')) ?? fnamemodify(bufname(buffer), ':p:h')},
    \ })
```

With [coc.nvim](https://github.com/neoclide/coc.nvim), in
`coc-settings.json`:

```json
{
  "languageserver": {
    "dgdtools": {
      "command": "dgdtools",
      "args": ["lsp"],
      "filetypes": ["dgdlpc"],
      "rootPatterns": ["dgdtools.yml", ".dgdtools.yml"]
    }
  }
}
```

A folder without a `dgdtools.yml` (a wizard's checkout of a lib, say)
needs the server told its profile and its lib path, as Neovim's
`settings` above tell it. Every client can give them as initialization
options, which hold for the root it starts the server in; that root
must then be the checkout's directory, not the file's (`root_uri` in
vim-lsp, `project_root` in ALE, `rootPatterns` in coc). In vim-lsp's
`lsp#register_server({...})` and in ALE's `ale#linter#Define(...)`
above:

```vim
      \ 'initialization_options': {'profile': 'mylib', 'libPath': '/players/me'},
```

In coc's `dgdtools` entry:

```json
"initializationOptions": { "profile": "mylib", "libPath": "/players/me" }
```

A client that answers the server's questions about its settings can
give them for each folder instead, under the section `dgdtools`
(vim-lsp's `workspace_config`, coc's `settings`).

### Files changed outside the editor

The server learns of a file changed on disk when the client tells it.
vim-lsp and ALE do not; coc.nvim does where watchman is installed, and
Neovim where it watches files (not by default on Linux). With a client
that does not, a file changed outside the editor (by `git pull`, or by
`dgdtools rename --write`) is seen when it is opened in the editor or
the server restarts; until then what the server says of the files that
use it may be out of date. The server says so once in its log when the
client does not watch files.

## Tests

`go test ./editors` runs these files in the `vim` and `nvim` on `PATH`,
when there are any: detection, options, syntax and indent, and, in
Neovim 0.11 and later, the language server (`TestNeovim*`), the plugin's
starting it among them (`TestNeovimPlugin`).
