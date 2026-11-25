// Package editors holds what an editor needs to show DGD LPC: the VS
// Code extension (vscode/: the TextMate grammar, the language
// configuration, and the client of the language server, in TypeScript),
// the Vim and Neovim plugin (vim/: the runtime files, and Neovim's
// entry for the language server), the Emacs major mode (emacs/:
// dgdlpc-mode.el, which registers the server with Eglot, and its ERT
// tests), and what the grammars share (common/): the word list and the
// sample files.
//
// There is no Go code here to import. The package exists for its tests,
// which keep the grammars from drifting away from the tool:
//
//   - common/dgdlpc.json is written from pkg/token, pkg/lexer and pkg/pp
//     (go test ./editors -update), and each grammar's word lists are
//     compared with it;
//   - the TextMate grammar is loaded, checked rule by rule, and run over
//     the samples by a small interpreter of the grammar format; what it
//     scopes is compared with the tokens pkg/lexer finds in the same
//     bytes, and with a snapshot beside each sample;
//   - the Vim files are run by the vim and nvim on PATH, when there are
//     any: filetype detection, the syntax groups of the samples (same
//     comparison, same kind of snapshot), and indentation;
//   - the Emacs mode is run by the emacs on PATH, when it is 29.1 or
//     later: it byte-compiles without a warning, its ERT tests pass
//     (detection, comments, the dialect, indentation against the
//     formatter, Eglot, the autoloads), its faces of the samples are
//     compared with pkg/lexer's tokens and a snapshot, and an Eglot
//     session has the server built from the tree lint and format;
//   - the language server is run under the nvim on PATH, when it is one
//     with vim.lsp.config (0.11 and later): the command is built, and
//     editor sessions have it lint, fix, format, navigate and rename,
//     and the plugin start it, or not when told not to;
//   - the manifest of the VS Code extension is compared with the code
//     of its client: the commands, the settings, the names both share
//     with the server; and the platforms it is packaged for, with the
//     binary inside, are compared with those a release is built for.
//     The client's own checks (its types, its tests, its build) need
//     Node and are run by npm in vscode/.
//
// See docs/design/lsp.md, sections 10.1 and 11.
package editors
