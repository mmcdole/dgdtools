# LPC (DGD) for Emacs

A major mode, `dgdlpc-mode`, for Emacs 29.1 and later: detection, syntax highlighting, comments, and indentation, plus registration of `dgdtools lsp` with Eglot, the LSP client built into Emacs. The language server needs `dgdtools` on `PATH` ([installation instructions](../../README.md#installation)).

## Installation

The package lives in the `editors/emacs` subdirectory of this repository.

**use-package (Emacs 30+):**

```elisp
(use-package dgdlpc-mode
  :vc (:url "https://github.com/mmcdole/dgdtools" :lisp-dir "editors/emacs")
  :hook (dgdlpc-mode . eglot-ensure))
```

**package-vc (Emacs 29+):**

```elisp
(package-vc-install
 '(dgdlpc-mode :url "https://github.com/mmcdole/dgdtools" :lisp-dir "editors/emacs"))
(add-hook 'dgdlpc-mode-hook #'eglot-ensure)
```

**Manual:**

```sh
git clone https://github.com/mmcdole/dgdtools ~/src/dgdtools
```

```elisp
(add-to-list 'load-path "~/src/dgdtools/editors/emacs")
(require 'dgdlpc-mode)
(add-hook 'dgdlpc-mode-hook #'eglot-ensure)
```

Installed with package-vc or use-package, the package is loaded the first time a `.c` or `.h` file is opened; nothing else loads it, and it does not load Eglot.

## Detection

The mode does not claim `*.c` or `*.h` files globally—normal C files stay in `c-mode` (or `c-ts-mode`, if you use it). A `.c` or `.h` file opens in `dgdlpc-mode` only if:

- Its directory or any ancestor directory contains `dgdtools.yml` or `.dgdtools.yml`, or
- It is inside a directory listed in `dgdlpc-roots`:

```elisp
(setq dgdlpc-roots '("~/mud/players/me"))
```

The check runs from `magic-mode-alist`, before `auto-mode-alist`, so an LPC file is never put in `c-mode` first and your C hooks (a C language server, a C style) do not run on it.

Remote files (TRAMP, or another handler that answers `file-remote-p`) are never searched for a `dgdtools.yml`, and stay C unless their directory is listed in `dgdlpc-roots`. Write the entry as the buffer's file names begin (`C-h v buffer-file-name` shows one):

```elisp
(setq dgdlpc-roots '("/ssh:me@host:/mud/lib"))
```

A listed remote directory gets highlighting and indentation. The mode does not set up the language server for it, so if you start Eglot from the mode hook, leave remote buffers out:

```elisp
(add-hook 'dgdlpc-mode-hook
          (lambda ()
            (unless (file-remote-p default-directory)
              (eglot-ensure))))
```

To choose the mode yourself, put a mode line at the top of a file:

```c
/* -*- mode: dgdlpc -*- */
```

or claim the `.c` and `.h` files of a directory tree in its `.dir-locals.el`:

```elisp
((auto-mode-alist . (("\\.[ch]\\'" . dgdlpc-mode))))
```

## Mode features & settings

- **Highlighting:** DGD keywords only, `nil`, `({ })` and `([ ])` literals, `..`, `->`, `<-`, `::`, inherit labels, numbers (`1..2` is a range, not a float), function names, and preprocessor directives. A string or character constant left open ends with its line, as in the compiler. Numbers, operators, brackets, and calls use the faces Emacs 29 added for them (`font-lock-number-face`, `font-lock-operator-face`, `font-lock-bracket-face`, `font-lock-function-call-face`), which many themes leave plain.
- **Comments:** `M-;` and `comment-region` write `/* */`, the form every driver compiles; `//` is a comment where the dialect has it.
- **Indentation:** Tuned to match `dgdtools format`, including nested array and mapping literals, `case` labels at the level of their `switch`, and continued statements. Two shapes are left for `dgdtools format` to settle: an element of a literal continued over several lines, and nested `if` statements without braces. `indent-tabs-mode` follows your own setting; with `editorconfig-mode` (built in since Emacs 30), `.editorconfig` sets both it and the offset.

Variables can be set globally, and all but `dgdlpc-roots` per lib in `.dir-locals.el`:

| Variable | Default | Description |
|---|---|---|
| `dgdlpc-roots` | `nil` | Directories whose `.c`/`.h` files should be treated as LPC without a `dgdtools.yml` |
| `dgdlpc-indent-offset` | `4` | Columns per indentation level |
| `dgdlpc-slash-slash` | `t` | Treat `//` as a line comment (`t`) or as two `/` operators (`nil`) |
| `dgdlpc-closures` | `nil` | Highlight `function` as a type keyword (`t`) when DGD `CLOSURES` is enabled |

For example, for a lib whose `dgdtools.yml` turns `dialect.slash_slash` off:

```elisp
((dgdlpc-mode . ((dgdlpc-slash-slash . nil))))
```

Note: these are set by hand; the mode does not parse `dgdtools.yml`.

## Language server with Eglot

The package adds `dgdtools lsp` to `eglot-server-programs` for `dgdlpc-mode` once Eglot loads. Start it with `M-x eglot` in an LPC buffer, or in every LPC buffer with:

```elisp
(add-hook 'dgdlpc-mode-hook #'eglot-ensure)
```

All the files of one lib share one server. For Eglot, the lib is the nearest directory with `dgdtools.yml` / `.dgdtools.yml`, or the entry of `dgdlpc-roots` that holds the file; other uses of `project.el` are not affected.

Eglot's usual commands work: `M-.` (go to definition), `M-?` (references), `C-h .` or ElDoc (hover and signatures), `completion-at-point`, `eglot-rename`, `eglot-code-actions`, `eglot-format-buffer`, and Flymake for diagnostics. In Emacs 31, Eglot also paints the server's semantic tokens. See [editors/README.md](../README.md#the-language-server-dgdtools-lsp) for the full list of server features.

If `dgdtools` isn't on `PATH`, set `exec-path`, or replace the entry after Eglot loads:

```elisp
(with-eval-after-load 'eglot
  (add-to-list 'eglot-server-programs
               '((dgdlpc-mode :language-id "dgdlpc") "~/go/bin/dgdtools" "lsp")))
```

### Partial checkouts without `dgdtools.yml`

List the checkout in `dgdlpc-roots`, and pass the profile and lib path in the checkout's `.dir-locals.el`:

```elisp
((dgdlpc-mode . ((eglot-workspace-configuration
                  . (:dgdtools (:profile "mylib" :libPath "/players/me"))))))
```

### Format on save

```elisp
(add-hook 'eglot-managed-mode-hook
          (lambda ()
            (when (derived-mode-p 'dgdlpc-mode)
              (add-hook 'before-save-hook #'eglot-format-buffer nil t))))
```

### External file changes

The server asks the client to watch the lib's files, and Eglot does (through `file-notify`), so changes made outside Emacs (a `git pull`, `dgdtools rename --write`) reach it. Where file notification is not available, open the changed file or restart the server with `M-x eglot-reconnect`.

## Language server with lsp-mode

The package does not register with lsp-mode. These lines do:

```elisp
(with-eval-after-load 'lsp-mode
  (add-to-list 'lsp-language-id-configuration '(dgdlpc-mode . "dgdlpc"))
  (lsp-register-client
   (make-lsp-client :new-connection (lsp-stdio-connection '("dgdtools" "lsp"))
                    :activation-fn (lsp-activate-on "dgdlpc")
                    :server-id 'dgdtools)))
(add-hook 'dgdlpc-mode-hook #'lsp-deferred)
```

## Tests

Run `go test ./editors` to test the word lists, byte compilation, detection, highlighting (`*.emacs.snap`), indentation, the autoloads, and (with the server built from the tree) an Eglot session (`TestEmacs*`). The ERT tests also run on their own from `editors/emacs`:

```sh
emacs -Q --batch -L . -l dgdlpc-mode-tests.el -f ert-run-tests-batch-and-exit
```
