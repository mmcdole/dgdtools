;;; dgdlpc-mode-tests.el --- Tests of dgdlpc-mode -*- lexical-binding: t; -*-

;; Copyright (C) 2026 Matthew McDole
;; SPDX-License-Identifier: MIT

;; This file is not part of GNU Emacs.

;;; Commentary:

;; Run by `go test ./editors' (TestEmacsMode), or by hand from
;; editors/emacs:
;;
;;   `emacs -Q --batch -L . -l dgdlpc-mode-tests.el -f ert-run-tests-batch-and-exit'
;;
;; The samples are those of the other grammars, in ../common/samples.
;; What the highlighting makes of them is compared with the lexer of
;; dgdtools by the Go test.

;;; Code:

(require 'ert)
(require 'dgdlpc-mode)
(require 'project)

(defvar eglot-lsp-context)
(defvar eglot-server-programs)

(defconst dgdlpc-test--samples
  (expand-file-name "../common/samples/"
                    (file-name-directory (or load-file-name buffer-file-name)))
  "The directory of the shared sample files.")

(defun dgdlpc-test--sample (name)
  "Return the text of the sample file NAME."
  (with-temp-buffer
    (insert-file-contents (expand-file-name name dgdlpc-test--samples))
    (buffer-string)))

(defmacro dgdlpc-test--with-tree (files &rest body)
  "Run BODY in a new directory holding FILES, an alist of names and texts.
The directory, with symbolic links resolved, is bound to `dir', and
buffers opened in it are killed after."
  (declare (indent 1) (debug t))
  `(let* ((dir (file-name-as-directory
                (file-truename (make-temp-file "dgdlpc" t))))
          (before (buffer-list)))
     (unwind-protect
         (progn
           (dolist (f ,files)
             (let ((path (expand-file-name (car f) dir)))
               (make-directory (file-name-directory path) t)
               (with-temp-file path (insert (cdr f)))))
           ,@body)
       (dolist (b (buffer-list))
         (unless (memq b before)
           (with-current-buffer b (set-buffer-modified-p nil))
           (kill-buffer b)))
       (delete-directory dir t))))

(defun dgdlpc-test--mode-of (file)
  "Return the major mode FILE is opened in."
  (with-current-buffer (find-file-noselect file)
    major-mode))

(defun dgdlpc-test--faces (text)
  "Return TEXT highlighted in `dgdlpc-mode', as a string with faces."
  (with-temp-buffer
    (insert text)
    (dgdlpc-mode)
    (font-lock-ensure)
    (buffer-string)))

(defun dgdlpc-test--face-at (text needle &optional nth)
  "Return the face list in TEXT at the NTH (0-based) occurrence of NEEDLE."
  (let ((pos -1))
    (dotimes (_ (1+ (or nth 0)))
      (setq pos (string-search needle text (1+ pos))))
    (should pos)
    (ensure-list (get-text-property pos 'face text))))

;;;; Detection

(ert-deftest dgdlpc-detection ()
  "A .c or .h file is LPC under a dgdtools.yml or one of `dgdlpc-roots'."
  (dgdlpc-test--with-tree '(("lib/dgdtools.yml" . "")
                            ("lib/std/tokens.c" . "int x;\n")
                            ("lib/std/directives.h" . "int x;\n")
                            ("lib/std/notes.txt" . "notes\n")
                            ("lib/std/cookie.c" . "/* -*- mode: c -*- */\n")
                            ("alt/.dgdtools.yml" . "")
                            ("alt/a/b/x.h" . "int x;\n")
                            ("listed/x.c" . "int x;\n")
                            ("plain/x.c" . "int x;\n")
                            ("plain/x.h" . "int x;\n")
                            ("plain/cookie.c" . "/* -*- mode: dgdlpc -*- */\n")
                            ("own/.dir-locals.el"
                             . "((auto-mode-alist . ((\"\\\\.[ch]\\\\'\" . dgdlpc-mode))))\n")
                            ("own/x.c" . "int x;\n"))
    (let ((dgdlpc-roots (list (expand-file-name "listed" dir))))
      (dolist (f '("lib/std/tokens.c" "lib/std/directives.h"
                   "lib/std/new.c"      ; a file that does not exist yet
                   "alt/a/b/x.h" "listed/x.c"))
        (should (equal (list f (dgdlpc-test--mode-of (expand-file-name f dir)))
                       (list f 'dgdlpc-mode))))
      (should (eq (dgdlpc-test--mode-of (expand-file-name "plain/x.c" dir)) 'c-mode))
      (should-not (eq (dgdlpc-test--mode-of (expand-file-name "plain/x.h" dir))
                      'dgdlpc-mode))
      (should-not (eq (dgdlpc-test--mode-of (expand-file-name "lib/std/notes.txt" dir))
                      'dgdlpc-mode))
      ;; The file's own mode line wins, and so does .dir-locals.el.
      (should (eq (dgdlpc-test--mode-of (expand-file-name "lib/std/cookie.c" dir))
                  'c-mode))
      (should (eq (dgdlpc-test--mode-of (expand-file-name "plain/cookie.c" dir))
                  'dgdlpc-mode))
      (should (eq (dgdlpc-test--mode-of (expand-file-name "own/x.c" dir))
                  'dgdlpc-mode))
      ;; A remote file is not looked up, wherever Emacs runs.
      (let ((default-directory (expand-file-name "lib/std/" dir)))
        (should-not (dgdlpc-lib-file-p "/ssh:nohost:/lib/std/x.c"))
        (should-not (dgdlpc-lib-file-p (concat "/ssh:nohost:" dir "lib/std/x.c"))))
      (should (equal (dgdlpc-root (expand-file-name "lib/std/" dir))
                     (expand-file-name "lib/" dir)))
      (should-not (dgdlpc-root (expand-file-name "plain/" dir))))))

(ert-deftest dgdlpc-detection-remote ()
  "A remote file is LPC only under a remote entry of `dgdlpc-roots'."
  (let ((dgdlpc-roots '("/ssh:nohost:/mud/lib" "/ssh:other:")))
    (should (dgdlpc-lib-file-p "/ssh:nohost:/mud/lib/std/x.c"))
    (should (dgdlpc-lib-file-p "/ssh:other:obj/x.h"))
    ;; Not a neighbour with a longer name, nor anything unlisted.
    (should-not (dgdlpc-lib-file-p "/ssh:nohost:/mud/library/x.c"))
    (should-not (dgdlpc-lib-file-p "/ssh:elsewhere:/mud/lib/x.c"))
    ;; Eglot gets no project from a remote entry.
    (let ((eglot-lsp-context t))
      (should-not (dgdlpc-project-find "/ssh:nohost:/mud/lib/std/")))))

;;;; Comments, highlighting and the dialect

(ert-deftest dgdlpc-comments ()
  "Comments are /* */, and // where the dialect has it."
  (with-temp-buffer
    (insert "x = 1;\n")
    (dgdlpc-mode)
    (should (equal comment-start "/* "))
    (should (equal comment-end " */"))
    (comment-region (point-min) (point-max))
    (should (equal (buffer-string) "/* x = 1; */\n"))
    (uncomment-region (point-min) (point-max))
    (should (equal (buffer-string) "x = 1;\n")))
  (let ((text (dgdlpc-test--faces "x = 1; // a comment\nint y;\n")))
    (should (memq 'font-lock-comment-face (dgdlpc-test--face-at text "a comment")))
    (should (memq 'font-lock-type-face (dgdlpc-test--face-at text "int")))))

(ert-deftest dgdlpc-dialect ()
  "Without dialect.slash_slash // is no comment; with closures `function' is a type."
  (let ((dgdlpc-slash-slash nil)
        (dgdlpc-closures t))
    (let ((text (dgdlpc-test--faces "x = 1; // not a comment\nfunction f;\n")))
      (should-not (memq 'font-lock-comment-face (dgdlpc-test--face-at text "not")))
      (should (memq 'font-lock-type-face (dgdlpc-test--face-at text "function")))))
  (let ((text (dgdlpc-test--faces "string function;\n")))
    (should-not (dgdlpc-test--face-at text "function")))
  ;; As a lib says it in .dir-locals.el.
  (dgdlpc-test--with-tree
      '(("lib/dgdtools.yml" . "")
        ("lib/.dir-locals.el" . "((dgdlpc-mode . ((dgdlpc-slash-slash . nil))))\n")
        ("lib/a.c" . "x = 1; // not a comment\n"))
    (with-current-buffer (find-file-noselect (expand-file-name "lib/a.c" dir))
      (should (eq major-mode 'dgdlpc-mode))
      (should-not dgdlpc-slash-slash)
      (font-lock-ensure)
      (goto-char (point-min))
      (search-forward "not")
      (should-not (nth 4 (syntax-ppss))))))

(ert-deftest dgdlpc-highlighting ()
  "DGD's brackets, operators, numbers, labels and quotes."
  (let ((text (dgdlpc-test--faces
               (concat "inherit room \"/std/room\";\n"
                       "x = f(a[i]) + g(([ 1: ({ 2 }) ]));\n"
                       "y = a[1..2] + a[..3] + ob <- \"/x\";\n"
                       "void f() { room::create(); }\n"
                       "#error can't be \"right\n"
                       "return nil;\n"))))
    (should (memq 'font-lock-constant-face (dgdlpc-test--face-at text "room")))
    (should (memq 'font-lock-keyword-face (dgdlpc-test--face-at text "inherit")))
    ;; ({ }) and ([ ]), and not the ]) of an index in a call.
    (should-not (dgdlpc-test--face-at text "])"))
    (should (memq 'font-lock-bracket-face (dgdlpc-test--face-at text "([")))
    (should (memq 'font-lock-bracket-face (dgdlpc-test--face-at text "({")))
    (should (memq 'font-lock-bracket-face (dgdlpc-test--face-at text "})")))
    (should (memq 'font-lock-bracket-face (dgdlpc-test--face-at text "])" 1)))
    ;; 1..2 is two numbers and a range.
    (should (memq 'font-lock-number-face (dgdlpc-test--face-at text "1..")))
    (should (memq 'font-lock-operator-face (dgdlpc-test--face-at text "..2")))
    (should (memq 'font-lock-number-face (dgdlpc-test--face-at text "2]")))
    (should (memq 'font-lock-number-face (dgdlpc-test--face-at text "3]")))
    (should (memq 'font-lock-operator-face (dgdlpc-test--face-at text "<-")))
    (should (memq 'font-lock-constant-face (dgdlpc-test--face-at text "room::")))
    (should (memq 'font-lock-operator-face (dgdlpc-test--face-at text "::")))
    (should (memq 'font-lock-function-call-face (dgdlpc-test--face-at text "create")))
    (should (memq 'font-lock-preprocessor-face (dgdlpc-test--face-at text "#error")))
    ;; Neither the apostrophe nor the open string runs past their line.
    (should (memq 'font-lock-keyword-face (dgdlpc-test--face-at text "return")))
    (should (memq 'font-lock-constant-face (dgdlpc-test--face-at text "nil")))))

;;;; Indentation

(defun dgdlpc-test--flatten (text)
  "Return TEXT with the indentation taken off every line."
  (replace-regexp-in-string "^[ \t]+" "" text))

(ert-deftest dgdlpc-indentation ()
  "The indentation of object.c, a fixed point of the formatter, is redone."
  (let ((want (dgdlpc-test--sample "object.c")))
    (with-temp-buffer
      (insert (dgdlpc-test--flatten want))
      (dgdlpc-mode)
      (setq indent-tabs-mode nil)
      (indent-region (point-min) (point-max))
      (let ((have (split-string (buffer-string) "\n"))
            (lines (split-string want "\n"))
            (n 0))
        (should (= (length have) (length lines)))
        (while lines
          (setq n (1+ n))
          (should (equal (list n (car have)) (list n (car lines))))
          (setq have (cdr have) lines (cdr lines)))))))

(ert-deftest dgdlpc-indentation-other ()
  "Indentation keeps directives' continuations and strings, and follows the offset."
  (with-temp-buffer
    (insert "#define X(a) \\\n        (a)\nvoid f()\n{\nx = \"a\\\n  b\";\nif (x)\nreturn;\n}\n")
    (dgdlpc-mode)
    (setq indent-tabs-mode nil)
    (setq-local dgdlpc-indent-offset 2)
    (indent-region (point-min) (point-max))
    (should (equal (buffer-string)
                   "#define X(a) \\\n        (a)\nvoid f()\n{\n  x = \"a\\\n  b\";\n  if (x)\n    return;\n}\n")))
  ;; The other samples, which are not laid out as the formatter would,
  ;; are indented without an error.
  (dolist (name '("tokens.c" "directives.h" "stray.c"))
    (with-temp-buffer
      (insert (dgdlpc-test--sample name))
      (dgdlpc-mode)
      (indent-region (point-min) (point-max)))))

;;;; Eglot

(ert-deftest dgdlpc-eglot ()
  "The server is registered with Eglot without loading it, rooted at the lib."
  (dgdlpc-test--with-tree '(("lib/dgdtools.yml" . "")
                            ("lib/std/a.c" . "int x;\n")
                            ("listed/x.c" . "int x;\n"))
    (with-current-buffer (find-file-noselect (expand-file-name "lib/std/a.c" dir))
      (should (eq major-mode 'dgdlpc-mode))
      (should-not (featurep 'eglot))
      (require 'eglot)
      (should (member '((dgdlpc-mode :language-id "dgdlpc") "dgdtools" "lsp")
                      eglot-server-programs))
      ;; Eglot's project is the lib; anybody else's is what it was.
      (let ((eglot-lsp-context t))
        (should (equal (project-root (project-current))
                       (expand-file-name "lib/" dir))))
      (should-not (project-current)))
    (let ((dgdlpc-roots (list (expand-file-name "listed" dir))))
      (with-current-buffer (find-file-noselect (expand-file-name "listed/x.c" dir))
        (let ((eglot-lsp-context t))
          (should (equal (project-root (project-current))
                         (expand-file-name "listed/" dir))))))))

;;;; Autoloads

(ert-deftest dgdlpc-autoloads ()
  "As installed by package.el: opening other files does not load the package."
  (dgdlpc-test--with-tree '(("lib/dgdtools.yml" . "")
                            ("lib/a.c" . "int x;\n")
                            ("notes.txt" . "notes\n"))
    (let* ((pkg (expand-file-name "pkg/" dir))
           (loaddefs (expand-file-name "dgdlpc-mode-autoloads.el" pkg)))
      (make-directory pkg)
      (copy-file (locate-library "dgdlpc-mode.el") pkg)
      (loaddefs-generate pkg loaddefs)
      (with-temp-buffer
        (should (zerop (call-process
                        (expand-file-name invocation-name invocation-directory)
                        nil t nil "-Q" "--batch" "-L" pkg "-l" loaddefs "--eval"
                        (format "%S"
                                `(progn
                                   (find-file ,(expand-file-name "notes.txt" dir))
                                   (princ (format "%s " (featurep 'dgdlpc-mode)))
                                   (find-file ,(expand-file-name "lib/a.c" dir))
                                   (princ (format "%s %s" major-mode (featurep 'eglot))))))))
        (should (equal (car (last (split-string (buffer-string) "\n" t)))
                       "nil dgdlpc-mode nil"))))))

(provide 'dgdlpc-mode-tests)

;;; dgdlpc-mode-tests.el ends here
