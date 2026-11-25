;;; dgdlpc-mode.el --- Major mode for LPC as the DGD driver compiles it -*- lexical-binding: t; -*-

;; Copyright (C) 2026 Matthew McDole

;; Author: Matthew McDole
;; URL: https://github.com/mmcdole/dgdtools
;; Version: 0.1.0
;; Package-Requires: ((emacs "29.1"))
;; Keywords: languages
;; SPDX-License-Identifier: MIT

;; This file is not part of GNU Emacs.

;;; Commentary:

;; `dgdlpc-mode' edits LPC as the DGD driver compiles it: highlighting
;; of DGD's words and no others, `({ })' and `([ ])', `<-', `..', `::'
;; and inherit labels, comments, and indentation that puts a line where
;; `dgdtools format' would.  It is part of dgdtools
;; (https://github.com/mmcdole/dgdtools, editors/emacs), whose language
;; server `dgdtools lsp' it registers with Eglot.
;;
;; A .c or .h file is opened in `dgdlpc-mode' only when a directory
;; above it has a dgdtools.yml or a .dgdtools.yml, or when it lies under
;; one of `dgdlpc-roots'.  Every other .c and .h file stays C.
;;
;; To start the language server in every LPC buffer:
;;
;;   (add-hook 'dgdlpc-mode-hook #'eglot-ensure)
;;
;; The keyword lists below are checked against
;; editors/common/dgdlpc.json by `go test ./editors'.

;;; Code:

;; Why prog-mode and not cc-mode: a mode derived from `c-mode' runs
;; `c-mode-common-hook' and `c-mode-hook' in its buffers, so whatever a
;; user has put there for C (`eglot-ensure' with clangd, a C style)
;; would run on LPC files too, and Eglot would match its C entry for the
;; derived mode.  It would also bring cc-mode's C keywords (`struct',
;; `char', `unsigned'), which DGD does not have, and read `({' as a GCC
;; statement expression.  Removing all of that means a cc-mode language
;; definition (`c-lang-defconst' for every keyword class), which is much
;; more code than the little a C-like language needs from prog-mode:
;; a syntax table, a list of words, and an indentation function held to
;; the formatter.

(defgroup dgdlpc nil
  "LPC as the DGD driver compiles it."
  :group 'languages
  :prefix "dgdlpc-"
  :link '(url-link "https://github.com/mmcdole/dgdtools"))

(defcustom dgdlpc-roots nil
  "Directories whose .c and .h files are DGD LPC without a dgdtools.yml.
A file under one of these is opened in `dgdlpc-mode' and shares one
language server with the rest of the directory.

A remote directory may be listed too, written as the names of its
files begin, as in \"/ssh:me@host:/mud/lib\".  It is matched as
text, without asking the server anything, and gets highlighting and
indentation; it is not a project for the language server."
  :type '(repeat directory))

(defcustom dgdlpc-indent-offset 4
  "Number of columns for each level of indentation.
The formatter's default is 4.  `editorconfig-mode' sets this from
indent_size."
  :type 'integer
  :safe #'integerp)

(defcustom dgdlpc-slash-slash t
  "Non-nil means `//' begins a comment, as with dialect.slash_slash.
Set it per lib in .dir-locals.el when the lib's dgdtools.yml turns
the dialect off."
  :type 'boolean
  :safe #'booleanp)

(defcustom dgdlpc-closures nil
  "Non-nil means `function' is a type keyword, as with dialect.closures."
  :type 'boolean
  :safe #'booleanp)

;;;; Words

;; Checked against editors/common/dgdlpc.json by go test ./editors: the
;; lists below are its keywords, types, specifiers, closures and
;; directives.  Change the word list first.

(defconst dgdlpc-keywords
  '("break" "case" "catch" "continue" "default" "do" "else" "for" "goto"
    "if" "inherit" "new" "nil" "operator" "return" "rlimits" "switch"
    "try" "while")
  "Reserved words of DGD LPC that are neither types nor specifiers.")

(defconst dgdlpc-types
  '("float" "int" "mapping" "mixed" "object" "string" "void")
  "Type keywords of DGD LPC.")

(defconst dgdlpc-specifiers
  '("atomic" "nomask" "private" "static" "varargs")
  "Class specifier keywords of DGD LPC.")

(defconst dgdlpc-closures-keywords
  '("function")
  "Type keywords reserved only with dialect.closures.")

(defconst dgdlpc-directives
  '("define" "elif" "else" "endif" "error" "if" "ifdef" "ifndef"
    "include" "line" "pragma" "undef")
  "Preprocessor directive names.")

;;;; Finding a lib

(defun dgdlpc-root (dir)
  "Return the nearest directory, DIR or one above it, with a dgdtools.yml.
A .dgdtools.yml counts too.  Return nil when there is none."
  (locate-dominating-file
   dir (lambda (d)
         (or (file-exists-p (expand-file-name "dgdtools.yml" d))
             (file-exists-p (expand-file-name ".dgdtools.yml" d))))))

(defun dgdlpc--listed-root (dir remote)
  "Return the directory of `dgdlpc-roots' that holds DIR, or nil.
With REMOTE non-nil only the remote entries are asked, and they are
compared as text; otherwise only the local ones.  A local file is thus
never the cause of a connection."
  (let (found)
    (dolist (root dgdlpc-roots found)
      (when (and (not found)
                 (eq (not remote) (not (file-remote-p root)))
                 (if remote
                     ;; "host:" is the remote home: a slash after the
                     ;; colon would name the server's root instead.
                     (string-prefix-p (if (string-match-p "[/:]\\'" root)
                                          root
                                        (concat root "/"))
                                      dir)
                   (file-in-directory-p dir root)))
        (setq found root)))))

(defun dgdlpc--lib-root (dir)
  "Return the root of the lib DIR is in, or nil.
That is the nearest directory with a dgdtools configuration, else the
directory of `dgdlpc-roots' that holds DIR.  A remote directory is in
a lib only when listed there: nothing above it is looked up."
  (if (file-remote-p dir)
      (dgdlpc--listed-root dir t)
    (or (dgdlpc-root dir)
        (dgdlpc--listed-root dir nil))))

;;;###autoload
(defun dgdlpc-lib-file-p (file)
  "Return non-nil if FILE lies in a DGD lib.
That is, a directory above it has a dgdtools.yml or a .dgdtools.yml,
or it is under one of `dgdlpc-roots'.  A remote file is in a lib only
under a remote entry of `dgdlpc-roots': its directories are not looked
up."
  (and (dgdlpc--lib-root
        ;; Expanding a remote name can ask the server where home is.
        (file-name-directory (if (file-remote-p file)
                                 file
                               (expand-file-name file))))
       t))

;; The rule runs before `auto-mode-alist' (as `magic-mode-alist' does),
;; so that a .c file of a lib is never put in `c-mode' first, with the C
;; hooks run on it.  The test of the name is copied into the autoloads,
;; so that the package is loaded the first time a .c or .h file is
;; opened, not the first time any file is.
;;;###autoload
(progn
  (defun dgdlpc-lib-buffer-p ()
    "Return non-nil if the current buffer visits a .c or .h file of a lib.
See `dgdlpc-lib-file-p'.  This is the test `magic-mode-alist' asks."
    (and buffer-file-name
         (string-match-p "\\.[ch]\\'" buffer-file-name)
         (dgdlpc-lib-file-p buffer-file-name)))
  (add-to-list 'magic-mode-alist '(dgdlpc-lib-buffer-p . dgdlpc-mode)))

;;;; Syntax

(defvar dgdlpc-mode-syntax-table
  (let ((st (make-syntax-table)))
    (modify-syntax-entry ?_ "_" st)
    (modify-syntax-entry ?\\ "\\" st)
    (modify-syntax-entry ?\" "\"" st)
    (modify-syntax-entry ?\' "\"" st)
    (modify-syntax-entry ?/ ". 124b" st)
    (modify-syntax-entry ?* ". 23" st)
    (modify-syntax-entry ?\n "> b" st)
    (modify-syntax-entry ?\r "> b" st)
    (dolist (c '(?+ ?- ?= ?% ?< ?> ?& ?| ?^ ?! ?~ ?? ?: ?# ?. ?, ?\;))
      (modify-syntax-entry c "." st))
    st)
  "Syntax table of `dgdlpc-mode', with `//' comments.")

(defvar dgdlpc--no-slash-slash-syntax-table
  (let ((st (make-syntax-table dgdlpc-mode-syntax-table)))
    (modify-syntax-entry ?/ ". 14" st)
    (modify-syntax-entry ?\n " " st)
    (modify-syntax-entry ?\r " " st)
    st)
  "Syntax table of `dgdlpc-mode' without `//' comments.")

(defconst dgdlpc--literal-re
  (concat "\"\\(?:[^\"\\\\\n]\\|\\\\\\(?:.\\|\n\\)\\)*\""
          "\\|'\\(?:[^'\\\\\n]\\|\\\\\\(?:[0-7]\\{1,3\\}\\|x[0-9A-Fa-f]+\\|.\\)\\)'")
  "A string literal or a character constant.
A string may go on after a backslash and a newline.")

(defun dgdlpc--logical-line-start ()
  "Move point to the start of its line, and of the lines it continues.
A line that ends in a backslash goes on in the next one."
  (forward-line 0)
  (while (and (> (point) 2) (eq (char-before (1- (point))) ?\\))
    (forward-line -1)))

(defun dgdlpc--extend-region (start end)
  "Extend the region from START to END to whole logical lines.
For `syntax-propertize-extend-region-functions': a quote is decided
by the rest of its logical line."
  (save-excursion
    (goto-char start)
    (dgdlpc--logical-line-start)
    (when (< (point) start)
      (cons (point) end))))

(defun dgdlpc--syntax-propertize (start end)
  "Mark the quotes from START to END that begin no literal as punctuation.
A `\"' or `\\='' that the line does not close (an apostrophe in an
#error, a string left open) is no quote: the lexer ends such a
literal with its line, and so the highlighting must."
  (goto-char start)
  (while (and (< (point) end) (re-search-forward "[\"']" end t))
    (let ((q (match-beginning 0)))
      (unless (nth 8 (save-excursion (syntax-ppss q)))
        (goto-char q)
        (if (looking-at dgdlpc--literal-re)
            (goto-char (match-end 0))
          (put-text-property q (1+ q) 'syntax-table (string-to-syntax "."))
          (goto-char (1+ q)))))))

(defun dgdlpc--apply-dialect ()
  "Use the syntax table that `dgdlpc-slash-slash' asks for.
Run again after directory and file local variables are set."
  (let ((table (if dgdlpc-slash-slash
                   dgdlpc-mode-syntax-table
                 dgdlpc--no-slash-slash-syntax-table)))
    (unless (eq (syntax-table) table)
      (set-syntax-table table)
      (syntax-ppss-flush-cache (point-min))
      (when font-lock-mode
        (font-lock-flush)))))

;;;; Highlighting

(defun dgdlpc--match-closures (limit)
  "Find the next `function' before LIMIT when it is a keyword.
It is one with `dgdlpc-closures'."
  (and dgdlpc-closures
       (re-search-forward (regexp-opt dgdlpc-closures-keywords 'symbols) limit t)))

(defun dgdlpc--match-number (limit)
  "Find the next number before LIMIT.
In 1..2 the number is 1, not the float 1., and ..2 is no float either."
  (let (found)
    (while (and (not found)
                (re-search-forward "\\_<[0-9]\\|\\.[0-9]" limit t))
      (let ((start (match-beginning 0)))
        (goto-char start)
        (cond
         ((and (eq (char-after) ?.) (eq (char-before) ?.))
          (goto-char (1+ start)))
         ((looking-at (concat "0[xX][0-9A-Fa-f]+"
                              "\\|\\([0-9]+\\)\\.\\."
                              "\\|[0-9]*\\.[0-9]+\\(?:[eE][-+]?[0-9]+\\)?"
                              "\\|[0-9]+\\.\\(?:[eE][-+]?[0-9]+\\)?"
                              "\\|[0-9]+[eE][-+]?[0-9]+"
                              "\\|[0-9]+"))
          (when (match-beginning 1)
            (set-match-data (list (match-beginning 1) (match-end 1))))
          (goto-char (match-end 0))
          (setq found t)))))
    found))

(defun dgdlpc--match-literal-bracket (limit)
  "Find the next bracket of an array or a mapping literal before LIMIT.
That is `({', `([', and the `})' or `])' that closes one; the `])' of
f(a[i]) closes no literal."
  (let (found)
    (while (and (not found)
                (re-search-forward "([[{]\\|[]}])" limit t))
      (let ((ppss (save-excursion (syntax-ppss (match-beginning 0)))))
        (unless (nth 8 ppss)
          (setq found
                (or (eq (char-after (match-beginning 0)) ?\()
                    (let ((open (nth 1 ppss)))
                      (and open (eq (char-before open) ?\())))))))
    found))

(defvar dgdlpc-font-lock-keywords
  `(;; Directives: the name, the header of an #include, the macro of
    ;; a #define.  The rest of the line is highlighted as code.
    (,(concat "^[ \t]*\\(#[ \t]*" (regexp-opt dgdlpc-directives) "\\)\\_>")
     1 'font-lock-preprocessor-face)
    ("^[ \t]*#[ \t]*include[ \t]*\\(<[^>\n]*>\\)" 1 'font-lock-string-face)
    ("^[ \t]*#[ \t]*define[ \t]+\\(\\(?:\\sw\\|\\s_\\)+\\)"
     1 'font-lock-variable-name-face)
    ;; Inherit labels: inherit label "/path", label::fn().
    ("\\_<inherit[ \t]+\\([A-Za-z_][A-Za-z0-9_]*\\)[ \t]+[\"A-Za-z_]"
     1 'font-lock-constant-face)
    ("\\_<\\([A-Za-z_][A-Za-z0-9_]*\\)::" 1 'font-lock-constant-face)
    ;; Reserved words, the driver's and no more.
    ("\\_<nil\\_>" 0 'font-lock-constant-face)
    (,(regexp-opt (append (remove "nil" dgdlpc-keywords) dgdlpc-specifiers)
                  'symbols)
     0 'font-lock-keyword-face)
    (,(regexp-opt dgdlpc-types 'symbols) 0 'font-lock-type-face)
    (dgdlpc--match-closures 0 'font-lock-type-face)
    (dgdlpc--match-number 0 'font-lock-number-face)
    ;; ({ }) and ([ ]), and the operators DGD has and C does not.
    (dgdlpc--match-literal-bracket 0 'font-lock-bracket-face)
    ("\\.\\.\\.?\\|<-\\|->\\|::" 0 'font-lock-operator-face)
    ;; A name before `(': defined or declared at the top level, called
    ;; anywhere else.
    ("\\_<\\([A-Za-z_][A-Za-z0-9_]*\\)[ \t]*("
     1 (if (zerop (car (save-excursion (syntax-ppss (match-beginning 1)))))
           'font-lock-function-name-face
         'font-lock-function-call-face)))
  "Highlighting of `dgdlpc-mode'.")

;;;; Indentation

;; One level for each open bracket, counted from the line that holds
;; it, as `dgdtools format' does: the elements of ({ }) and ([ ]) one
;; level in from the line with the `({' or `([', its closer at that
;; line's level; a block's lines one level in from the statement that
;; opens it; case labels at the level of their switch; a statement
;; continued on the next line (an if without braces, an expression
;; broken after an operator) one level in from its first line.
;; Directives go to the margin, and their continuation lines, and the
;; lines of a string, are left alone.  Two shapes come out differently
;; from the formatter, which settles them when it runs: an element of a
;; literal continued over several lines, and nested if statements
;; without braces.

(defun dgdlpc--directive-start ()
  "Return the start of the directive whose logical line has point, or nil."
  (save-excursion
    (dgdlpc--logical-line-start)
    (and (looking-at-p "[ \t]*#")
         (not (nth 8 (syntax-ppss)))
         (point))))

(defun dgdlpc--backward-code ()
  "Move point back over white space, comments and directives.
Return the character then before point, nil at the start of the buffer."
  (let ((again t))
    (while again
      (forward-comment (- (point)))
      (let ((d (and (not (bobp)) (dgdlpc--directive-start))))
        (setq again (and d (< d (point))))
        (when again
          (goto-char d)))))
  (char-before))

(defun dgdlpc--block-p (open)
  "Return non-nil if the bracket at OPEN opens a block.
A block's `{' is any but the `{' of `({'."
  (and (eq (char-after open) ?{)
       (save-excursion
         (goto-char open)
         (skip-chars-backward " \t\n")
         (not (eq (char-before) ?\()))))

(defun dgdlpc--anchor (open)
  "Return the indentation of the line the bracket at OPEN belongs to.
That is the bracket's own line, but for a block's `{' after `)': then
it is the line of the `(' (the line of if, while or the function)."
  (save-excursion
    (goto-char open)
    (when (dgdlpc--block-p open)
      (when (eq (dgdlpc--backward-code) ?\))
        (condition-case nil
            (backward-sexp)
          (scan-error (goto-char open)))))
    (current-indentation)))

(defun dgdlpc--continued-p ()
  "Return non-nil if the line at point continues a statement.
It does when the code before it ends in none of `;', `{', `}', `:'."
  (save-excursion
    (let ((c (dgdlpc--backward-code)))
      (and c (not (memq c '(?\; ?{ ?} ?:)))))))

(defun dgdlpc--statement-indent (open)
  "Return the indentation of the statement the line at point continues.
OPEN is the `{' of the block the statement is in."
  (save-excursion
    (while (dgdlpc--continued-p)
      (dgdlpc--backward-code)
      ;; Out of the brackets opened in the block, to the line of the
      ;; outermost.
      (let ((opens (nth 9 (syntax-ppss))))
        (while (and opens (<= (car opens) open))
          (setq opens (cdr opens)))
        (when opens
          (goto-char (car opens))))
      (back-to-indentation))
    (current-indentation)))

(defun dgdlpc--comment-indent (start)
  "Return the indentation of a line inside the comment begun at START.
A line that begins with `*' goes under the `*' of `/*'; any other
under the comment's first word."
  (let ((star (looking-at-p "\\*")))
    (save-excursion
      (goto-char start)
      (cond (star (1+ (current-column)))
            ((looking-at "/\\*+[ \t]*\\([^ \t\n]\\)")
             (goto-char (match-beginning 1))
             (current-column))
            (t (+ (current-column) 3))))))

(defun dgdlpc--calculate-indent ()
  "Return the column the line at point belongs at, or nil to leave it.
Point is at the first non-blank character of the line."
  (let* ((ppss (syntax-ppss))
         (open (nth 1 ppss)))
    (cond
     ((nth 3 ppss) nil)
     ((save-excursion
        (forward-line 0)
        (and (not (bobp))
             (eq (char-before (1- (point))) ?\\)
             (dgdlpc--directive-start)))
      nil)
     ((nth 4 ppss) (dgdlpc--comment-indent (nth 8 ppss)))
     ((looking-at-p "#") 0)
     ((null open) 0)
     ((looking-at-p "[])}]") (dgdlpc--anchor open))
     ((not (dgdlpc--block-p open))
      (+ (dgdlpc--anchor open) dgdlpc-indent-offset))
     ((looking-at-p "\\(?:case\\|default\\)\\_>") (dgdlpc--anchor open))
     ((and (not (looking-at-p "{")) (dgdlpc--continued-p))
      (+ (dgdlpc--statement-indent open) dgdlpc-indent-offset))
     (t (+ (dgdlpc--anchor open) dgdlpc-indent-offset)))))

(defun dgdlpc-indent-line ()
  "Indent the current line as `dgdtools format' would."
  (interactive)
  (let ((indent (save-excursion
                  (back-to-indentation)
                  (dgdlpc--calculate-indent))))
    (when indent
      (if (<= (current-column) (current-indentation))
          (indent-line-to indent)
        (save-excursion (indent-line-to indent))))))

;;;; Eglot

(defvar eglot-lsp-context)
(defvar eglot-server-programs)
(defvar editorconfig-indent-size-vars)

(defun dgdlpc-project-find (dir)
  "Return the project of DIR for Eglot: the lib it is in.
The lib is where `dgdlpc-root' or `dgdlpc-roots' say, so that the files
of one lib share one language server, rooted where the server finds
the lib's dgdtools.yml.  For anything but Eglot this returns nil, and
`project-current' finds what it would otherwise."
  (when (and (bound-and-true-p eglot-lsp-context)
             (not (file-remote-p dir)))
    (let ((root (dgdlpc--lib-root dir)))
      (when root
        (cons 'transient (file-name-as-directory (expand-file-name root)))))))

;; Registered when Eglot loads, which this does not make happen.  The
;; form is copied into the autoloads, so it is in place before the
;; first LPC file opens.
;;;###autoload
(with-eval-after-load 'eglot
  (add-to-list 'eglot-server-programs
               '((dgdlpc-mode :language-id "dgdlpc") "dgdtools" "lsp")))

;;;; The mode

;;;###autoload
(define-derived-mode dgdlpc-mode prog-mode "LPC (DGD)"
  "Major mode for LPC as the DGD driver compiles it.

A .c or .h file is opened in this mode when it lies in a lib: see
`dgdlpc-lib-file-p'.  Indentation follows `dgdtools format', with
`dgdlpc-indent-offset' columns a level.  The language server is
registered with Eglot; to start it in every LPC buffer, add
`eglot-ensure' to `dgdlpc-mode-hook'.

\\{dgdlpc-mode-map}"
  :group 'dgdlpc
  (setq-local comment-start "/* ")
  (setq-local comment-end " */")
  (setq-local comment-start-skip "\\(?://+\\|/\\*+\\)[ \t]*")
  (setq-local comment-end-skip "[ \t]*\\(?:\n\\|\\*+/\\)")
  (setq-local parse-sexp-ignore-comments t)
  (setq-local syntax-propertize-function #'dgdlpc--syntax-propertize)
  (add-hook 'syntax-propertize-extend-region-functions
            #'dgdlpc--extend-region nil t)
  (setq-local font-lock-defaults '(dgdlpc-font-lock-keywords))
  (setq-local indent-line-function #'dgdlpc-indent-line)
  (setq-local electric-indent-chars
              (append "{}]):#" electric-indent-chars))
  (setq-local editorconfig-indent-size-vars '(dgdlpc-indent-offset))
  (add-hook 'project-find-functions #'dgdlpc-project-find nil t)
  (dgdlpc--apply-dialect)
  (add-hook 'hack-local-variables-hook #'dgdlpc--apply-dialect nil t))

(provide 'dgdlpc-mode)

;;; dgdlpc-mode.el ends here
