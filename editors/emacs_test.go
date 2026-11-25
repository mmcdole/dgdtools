package editors

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/format"
	"github.com/mmcdole/dgdtools/pkg/lexer"
)

const (
	emacsDir   = "emacs"
	emacsMode  = emacsDir + "/dgdlpc-mode.el"
	emacsTests = emacsDir + "/dgdlpc-mode-tests.el"
)

// emacsWords reads the word lists of the mode: each defconst NAME
// followed by a quoted list of strings.
func emacsWords(t *testing.T) map[string][]string {
	t.Helper()
	data, err := os.ReadFile(emacsMode)
	require.NoError(t, err)
	out := map[string][]string{}
	def := regexp.MustCompile(`(?s)\(defconst (dgdlpc-[a-z-]+)\s+'\(([^)]*)\)`)
	str := regexp.MustCompile(`"([^"\\]*)"`)
	for _, m := range def.FindAllStringSubmatch(string(data), -1) {
		var words []string
		for _, s := range str.FindAllStringSubmatch(m[2], -1) {
			words = append(words, s[1])
		}
		out[m[1]] = words
	}
	return out
}

// TestEmacsWords: the mode's keyword lists and directive names are the
// shared word list's.
func TestEmacsWords(t *testing.T) {
	w := loadWordList(t)
	lists := emacsWords(t)
	assert.Equal(t, w.Keywords, sorted(lists["dgdlpc-keywords"]), "the mode's keywords")
	assert.Equal(t, w.Types, sorted(lists["dgdlpc-types"]), "the mode's types")
	assert.Equal(t, w.Specifiers, sorted(lists["dgdlpc-specifiers"]), "the mode's specifiers")
	assert.Equal(t, w.Closures, sorted(lists["dgdlpc-closures-keywords"]), "the mode's closures keywords")
	assert.Equal(t, w.Directives, sorted(lists["dgdlpc-directives"]), "the mode's directive names")
}

// emacsPath returns the emacs on PATH, when it is one the mode is for
// (29.1 and later, which have Eglot). The test is skipped otherwise,
// except in CI.
func emacsPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("emacs")
	if err != nil {
		noEditor(t, "no emacs on PATH")
	}
	out, err := exec.Command(path, "-Q", "--batch", "--eval",
		`(princ (if (version<= "29.1" emacs-version) "ok" emacs-version))`).Output()
	if err != nil || string(out) != "ok" {
		noEditor(t, "the emacs on PATH is older than 29.1: %s %v", out, err)
	}
	return path
}

// emacsFaces dumps the faces of the samples: for each, a .faces file in
// $DGDLPC_OUT with the line, byte column (1-based), byte length and
// faces of every run of characters with the same faces.
const emacsFaces = `
(dolist (file (split-string (getenv "DGDLPC_SAMPLES") ","))
  (with-temp-buffer
    (insert-file-contents file)
    (dgdlpc-mode)
    (font-lock-ensure)
    (let ((lnum 1) lines)
      (goto-char (point-min))
      (while (not (eobp))
        (let ((bol (point)) (eol (line-end-position)) (pos (point)))
          (while (< pos eol)
            (let ((faces (get-text-property pos 'face))
                  (next (next-single-property-change pos 'face nil eol)))
              (push (format "%d\t%d\t%d\t%s" lnum
                            (1+ (- (position-bytes pos) (position-bytes bol)))
                            (- (position-bytes next) (position-bytes pos))
                            (mapconcat #'symbol-name (ensure-list faces) " "))
                    lines)
              (setq pos next))))
        (forward-line 1)
        (setq lnum (1+ lnum)))
      (with-temp-file (expand-file-name (concat (file-name-nondirectory file) ".faces")
                                        (getenv "DGDLPC_OUT"))
        (insert (mapconcat #'identity (nreverse lines) "\n") "\n")))))
`

// emacsClass puts the faces of a run of text in the lexer's words.
// Emacs has one face for keywords and specifiers, and one for strings
// and characters, so the text says which.
func emacsClass(w wordList, faces []string, text string) byteClass {
	has := func(name string) bool {
		for _, f := range faces {
			if f == name {
				return true
			}
		}
		return false
	}
	word := func() byteClass {
		for _, x := range w.Specifiers {
			if x == text {
				return clSpecifier
			}
		}
		for _, x := range w.Types {
			if x == text {
				return clType
			}
		}
		return clKeyword
	}
	switch {
	case has("font-lock-comment-face"), has("font-lock-comment-delimiter-face"):
		return clComment
	case has("font-lock-string-face"):
		if strings.HasPrefix(text, "'") {
			return clChar
		}
		return clString
	case has("font-lock-preprocessor-face"):
		return clDirective
	case has("font-lock-number-face"):
		lower := strings.ToLower(text)
		if strings.Contains(lower, ".") || (!strings.HasPrefix(lower, "0x") && strings.Contains(lower, "e")) {
			return clFloat
		}
		return clInt
	case has("font-lock-keyword-face"), has("font-lock-type-face"):
		return word()
	case has("font-lock-constant-face") && text == "nil":
		return clKeyword
	case has("font-lock-operator-face"), has("font-lock-bracket-face"):
		return clOperator
	}
	return clOther
}

// TestEmacsMode runs the mode in the emacs on PATH: it byte-compiles
// without a warning, its ERT tests pass (detection, comments, the
// dialect, indentation against the formatter, Eglot, the autoloads),
// and the faces it gives the samples agree with pkg/lexer's tokens and
// with the snapshots beside them. It is skipped where there is no
// emacs, except in CI.
func TestEmacsMode(t *testing.T) {
	emacs := emacsPath(t)
	modeDir, err := filepath.Abs(emacsDir)
	require.NoError(t, err)

	t.Run("compile", func(t *testing.T) {
		dir := t.TempDir()
		var files []string
		for _, f := range []string{emacsMode, emacsTests} {
			data, err := os.ReadFile(f)
			require.NoError(t, err)
			writeFile(t, filepath.Join(dir, filepath.Base(f)), string(data))
			files = append(files, filepath.Base(f))
		}
		cmd := exec.Command(emacs, append([]string{"-Q", "--batch", "-L", ".",
			"--eval", "(setq byte-compile-error-on-warn t)", "-f", "batch-byte-compile"}, files...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	})

	t.Run("ert", func(t *testing.T) {
		// The indentation test redoes object.c, which must be what the
		// formatter writes: the mode is held to the formatter, not to a
		// file somebody laid out.
		src, err := os.ReadFile(indentSample)
		require.NoError(t, err)
		formatted, err := format.Format(lexer.Lex(indentSample, src, lexer.Default), format.Options{})
		require.NoError(t, err)
		require.Equal(t, string(src), string(formatted), "%s must be a fixed point of the formatter", indentSample)

		cmd := exec.Command(emacs, "-Q", "--batch", "-L", modeDir, "-l", filepath.Join(modeDir, filepath.Base(emacsTests)),
			"-f", "ert-run-tests-batch-and-exit")
		cmd.Dir = t.TempDir()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	})

	t.Run("faces", func(t *testing.T) {
		w := loadWordList(t)
		dir := t.TempDir()
		var paths []string
		for path := range samples(t) {
			abs, err := filepath.Abs(path)
			require.NoError(t, err)
			paths = append(paths, abs)
		}
		cmd := exec.Command(emacs, "-Q", "--batch", "-L", modeDir, "-l", "dgdlpc-mode", "--eval", emacsFaces)
		cmd.Env = append(os.Environ(), "DGDLPC_SAMPLES="+strings.Join(paths, ","), "DGDLPC_OUT="+dir)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)

		directive := regexp.MustCompile(`^\s*#\s*([a-z]+)`)
		for path, src := range samples(t) {
			lines := sourceLines(src)
			spans := readDump(t, filepath.Join(dir, filepath.Base(path)+".faces"), len(lines))
			lexed := lexSample(t, path, src)
			class := make([]byteClass, len(src)+1)
			boundary := make([]bool, len(src)+1)
			off := 0
			for i, text := range strings.SplitAfter(src, "\n") {
				if i == len(lines) {
					break
				}
				for _, s := range spans[i] {
					c := emacsClass(w, strings.Fields(s.label), text[s.start:s.end])
					for j := s.start; j < s.end; j++ {
						class[off+j] = c
					}
					boundary[off+s.start] = true
				}
				// A directive is highlighted as C modes do: its name, and
				// the rest of the line as code. The name is checked here,
				// and the line left out of the comparison below.
				if m := directive.FindStringSubmatchIndex(text); m != nil && lexed.class[off+m[2]] == clDirective {
					name := text[m[2]:m[3]]
					known := false
					for _, d := range w.Directives {
						known = known || d == name
					}
					got := ""
					for _, s := range spans[i] {
						if s.start <= m[2] && m[2] < s.end {
							got = s.label
						}
					}
					if known {
						assert.Contains(t, got, "font-lock-preprocessor-face", "%s:%d: #%s", path, i+1, name)
					} else {
						assert.NotContains(t, got, "font-lock-preprocessor-face", "%s:%d: #%s", path, i+1, name)
					}
				}
				off += len(text)
			}
			for i := range lexed.class {
				if lexed.class[i] == clDirective {
					class[i] = clDirective
				}
			}
			lexed.agree(t, path, class, boundary, false)
			compareSnapshot(t, path+".emacs.snap", renderSnapshot(lines, spans))
		}
	})
}

// emacsSession is what the emacs on PATH does with the language server,
// as a user who has eglot-ensure in dgdlpc-mode-hook: it opens a file of
// a lib, waits for the diagnostics, mends one finding by typing, has
// the file formatted, and writes down what it saw.
const emacsSession = `;;; -*- lexical-binding: t; -*-
(require 'json)
(let ((dir (getenv "DGDLPC_DIR")) (out nil))
  (condition-case err
      (progn
        (require 'dgdlpc-mode)
        (setq eglot-sync-connect t)
        (add-hook 'dgdlpc-mode-hook #'eglot-ensure)
        (find-file (expand-file-name "lib/obj/a.c" dir))
        ;; eglot-ensure connects when the command that opened the file
        ;; is done.
        (run-hooks 'post-command-hook)
        (push (cons 'mode (symbol-name major-mode)) out)
        (cl-flet ((wait (what cond)
                    (let ((until (+ (float-time) 20)))
                      (while (and (not (funcall cond)) (< (float-time) until))
                        (accept-process-output nil 0.05))
                      (unless (funcall cond) (error "Waited in vain for %s" what))))
                  (found ()
                    (sort (mapcar (lambda (d)
                                    (save-excursion
                                      (goto-char (flymake-diagnostic-beg d))
                                      (format "%d:%d-%d %s"
                                              (1- (line-number-at-pos)) (current-column)
                                              (progn (goto-char (flymake-diagnostic-end d)) (current-column))
                                              (flymake-diagnostic-text d))))
                                  (flymake-diagnostics))
                          #'string<)))
          (wait "the server" (lambda () (eglot-current-server)))
          (let ((server (eglot-current-server)))
            (push (cons 'server (plist-get (eglot--server-info server) :name)) out)
            (push (cons 'root (project-root (eglot--project server))) out))
          (wait "diagnostics" (lambda () (= (length (found)) 2)))
          (push (cons 'first (found)) out)
          ;; Typing: the comparison becomes an assignment.
          (goto-char (point-min))
          (search-forward "==")
          (replace-match "=")
          ;; Eglot sends changes, and Flymake asks for its findings, when
          ;; Emacs is idle, which it never is in batch mode.
          (eglot--signal-textDocument/didChange)
          (flymake-start)
          (wait "one finding less" (lambda () (= (length (found)) 1)))
          (push (cons 'second (found)) out)
          ;; Formatting, of a line that needs it.
          (goto-char (point-min))
          (forward-line 2)
          (delete-region (point) (line-end-position))
          (insert "string     s;")
          (eglot-format-buffer)
          (push (cons 'text (buffer-substring-no-properties (point-min) (point-max))) out)))
    (error (push (cons 'error (error-message-string err)) out)))
  (with-temp-file (expand-file-name "result.json" dir)
    (insert (json-encode out)))
  (kill-emacs 0))
`

// TestEmacsEglot runs the language server built from this tree under
// Eglot in the emacs on PATH, found as dgdtools on PATH the way the mode
// registers it, and started by eglot-ensure.
func TestEmacsEglot(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the command and starts an editor")
	}
	emacs := emacsPath(t)
	modeDir, err := filepath.Abs(emacsDir)
	require.NoError(t, err)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	bin := filepath.Join(dir, "bin", "dgdtools")
	if out, err := exec.Command("go", "build", "-o", bin, "../cmd/dgdtools").CombinedOutput(); err != nil {
		t.Fatalf("building the command: %v\n%s", err, out)
	}
	const text = "void f()\n{\n    string s;\n\n    s == \"x\";\n    s = \"one\" \"two\";\n}\n"
	writeFile(t, filepath.Join(dir, "lib/dgdtools.yml"), "root: .\n")
	writeFile(t, filepath.Join(dir, "lib/obj/a.c"), text)
	writeFile(t, filepath.Join(dir, "session.el"), emacsSession)

	cmd := exec.Command(emacs, "-Q", "--batch", "-L", modeDir, "-l", filepath.Join(dir, "session.el"))
	cmd.Env = append(os.Environ(), "DGDLPC_DIR="+dir, "PATH="+filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	require.NoError(t, err, "%s", out)
	var got struct {
		Error, Mode, Server, Root, Text string
		First, Second                   []string
	}
	require.NoError(t, json.Unmarshal(data, &got), "%s", data)
	require.Empty(t, got.Error, "%s", out)

	assert.Equal(t, "dgdlpc-mode", got.Mode)
	assert.Equal(t, "dgdtools", got.Server)
	assert.Equal(t, filepath.Join(dir, "lib")+"/", got.Root)
	codes := func(list []string) []string {
		var out []string
		for _, d := range list {
			pos, msg, _ := strings.Cut(d, " ")
			code := regexp.MustCompile(`\b[A-Z][0-9]{3}\b`).FindString(msg)
			out = append(out, pos+" "+code)
		}
		return out
	}
	assert.Equal(t, []string{"4:6-8 S002", "5:14-19 E002"}, codes(got.First), "%q", got.First)
	assert.Equal(t, []string{"5:14-19 E002"}, codes(got.Second), "%q", got.Second)
	assert.Equal(t, strings.Replace(text, "s == ", "s = ", 1), got.Text)
}
