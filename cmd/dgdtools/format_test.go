package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/profile"
)

const (
	messy     = "void f() {\nif (x)\ny();\n}\n"
	formatted = "void f() {\n    if (x)\n        y();\n}\n"
	indent2   = "void f() {\n  if (x)\n    y();\n}\n"
	tabbed    = "void f() {\n\tif (x)\n\t\ty();\n}\n"
)

func TestFormatModes(t *testing.T) {
	dir := tree(t, map[string]string{
		"a.c":       messy,
		"b.c":       formatted,
		"sub/c.h":   messy,
		"sub/d.txt": "not source\n",
	})
	a, b := filepath.Join(dir, "a.c"), filepath.Join(dir, "b.c")
	t.Chdir(dir)

	// --check lists what would change, by the name the user gave, writes
	// nothing and exits 1; a directory stands for the .c and .h files
	// under it.
	code, out, errs := dgdtools(t, "", "format", "--check", ".")
	assert.Equal(t, 1, code)
	assert.Equal(t, "a.c\nsub/c.h\n", filepath.ToSlash(out))
	assert.Equal(t, "2 files would be reformatted, 1 file already formatted\n", errs)
	assert.Equal(t, messy, read(t, a))
	code, out, errs = dgdtools(t, "", "format", "--check", "-q", "b.c")
	assert.Equal(t, 0, code)
	assert.Empty(t, out+errs)

	// --diff prints unified diffs, writes nothing and exits 1.
	code, out, _ = dgdtools(t, "", "format", "--diff", "a.c", "b.c")
	assert.Equal(t, 1, code)
	assert.True(t, strings.HasPrefix(out, "--- a.c\n+++ a.c\n@@ -1,4 +1,4 @@\n void f() {\n-if (x)\n-y();\n+    if (x)\n+        y();\n }\n"), out)
	assert.NotContains(t, out, "b.c")
	assert.Equal(t, messy, read(t, a))
	code, out, _ = dgdtools(t, "", "format", "--diff", "b.c")
	assert.Equal(t, 0, code)
	assert.Empty(t, out)

	// The default rewrites the files that change and only those.
	before, err := os.Stat(b)
	require.NoError(t, err)
	code, out, errs = dgdtools(t, "", "format", ".")
	assert.Equal(t, 0, code)
	assert.Empty(t, out)
	assert.Equal(t, "2 files reformatted, 1 file unchanged\n", errs)
	assert.Equal(t, formatted, read(t, a))
	assert.Equal(t, formatted, read(t, filepath.Join(dir, "sub/c.h")))
	assert.Equal(t, "not source\n", read(t, filepath.Join(dir, "sub/d.txt")))
	after, err := os.Stat(b)
	require.NoError(t, err)
	assert.Equal(t, before.ModTime(), after.ModTime())
	code, out, errs = dgdtools(t, "", "format", "--check", ".")
	assert.Equal(t, 0, code, "formatting is a fixed point")
	assert.Empty(t, out)
	assert.Equal(t, "0 files would be reformatted, 3 files already formatted\n", errs)

	// Without a path nothing is rewritten.
	code, _, errs = dgdtools(t, "", "format")
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, "nothing to format")
}

func TestFormatStdin(t *testing.T) {
	dir := tree(t, map[string]string{
		"dgdtools.yml":     "root: .\nformat: {indent_width: 2}\n",
		"sub/dgdtools.yml": "root: false\nformat: {indent_style: tab}\n",
	})
	t.Chdir(dir)
	// "-": standard input to standard output, by the settings of the
	// working directory.
	code, out, errs := dgdtools(t, messy, "format", "-")
	assert.Equal(t, 0, code)
	assert.Equal(t, indent2, out)
	assert.Empty(t, errs)
	// --stdin-filename: by the settings of that path, which need not
	// exist; it implies "-".
	code, out, _ = dgdtools(t, messy, "format", "--stdin-filename", "sub/new.c")
	assert.Equal(t, 0, code)
	assert.Equal(t, tabbed, out)
	code, out, _ = dgdtools(t, messy, "format", "--stdin-filename", filepath.Join(dir, "sub/new.c"), "-")
	assert.Equal(t, 0, code)
	assert.Equal(t, tabbed, out)

	// --check and --diff name it, and print no source.
	code, out, _ = dgdtools(t, messy, "format", "--check", "-")
	assert.Equal(t, 1, code)
	assert.Equal(t, "<stdin>\n", out)
	code, out, _ = dgdtools(t, messy, "format", "--diff", "--stdin-filename", "sub/new.c")
	assert.Equal(t, 1, code)
	assert.Contains(t, out, "--- sub/new.c\n+++ sub/new.c\n")
	assert.Contains(t, out, "+\tif (x)\n")
	code, out, _ = dgdtools(t, indent2, "format", "--check", "-")
	assert.Equal(t, 0, code)
	assert.Empty(t, out)

	// What cannot be formatted is an error, and nothing is printed.
	code, out, errs = dgdtools(t, "int x = \"unterminated;\n", "format", "--stdin-filename", "bad.c")
	assert.Equal(t, 2, code)
	assert.Empty(t, out)
	assert.Contains(t, errs, "bad.c:1:9: file contains unlexable content")

	code, _, errs = dgdtools(t, messy, "format", "-", "sub")
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, "cannot be combined with paths")
	code, _, errs = dgdtools(t, messy, "format", "--stdin-filename", "x.c", "sub")
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, "cannot be combined with paths")
}

func TestFormatSettingsPerFile(t *testing.T) {
	dir := tree(t, map[string]string{
		"two/dgdtools.yml":                "root: lib\nformat: {indent_width: 2}\nexclude: [\"d/old/**\"]\n",
		"two/lib/d/a.c":                   messy,
		"two/lib/d/old/gone.c":            messy,
		"two/lib/d/sub/dgdtools.yml":      "root: false\nformat: {indent_width: 8}\n",
		"two/lib/d/sub/deep.c":            messy,
		"two/lib/d/own/dgdtools.yml":      "root: .\nformat: {indent_style: tab}\nexclude: [\"attic/**\"]\n",
		"two/lib/d/own/mine.c":            messy,
		"two/lib/d/own/attic/x.c":         messy,
		"lost/dgdtools.yml":               "root: .\n",
		"lost/a.c":                        messy,
		"lost/own/dgdtools.yml":           "profile: not-installed\n",
		"lost/own/mine.c":                 messy,
		"tabs/dgdtools.yml":               "root: .\nformat: {indent_style: tab}\n",
		"tabs/b.c":                        messy,
		"none/c.c":                        messy,
		"editor/.editorconfig":            "root = true\n[*.c]\nindent_style = tab\n[*.h]\nindent_size = 3\n",
		"editor/d.c":                      messy,
		"editor/d.h":                      messy,
		"editor/sub/dgdtools.yml":         "root: .\nformat: {indent_style: space}\n",
		"editor/sub/e.c":                  messy,
		"editor/sub/.editorconfig":        "[*]\nindent_size = 1\n",
		"editor/sub/deeper/f.c":           messy,
		"editor/sub/deeper/.dgdtools.yml": "root: false\nformat: {indent_width: 2}\n",
	})
	show := func(args ...string) string {
		t.Helper()
		code, out, errs := dgdtools(t, "", append([]string{"format", "--diff", "-q"}, args...)...)
		assert.Contains(t, []int{0, 1}, code, errs)
		return out
	}
	// From an unrelated working directory: the project of a file is
	// found from the file, not from where the command runs.
	t.Chdir(filepath.Join(dir, "none"))
	out := show(filepath.Join(dir, "two/lib/d/a.c"), filepath.Join(dir, "tabs/b.c"), "c.c")
	assert.Contains(t, out, "a.c\n@@ -1,4 +1,4 @@\n void f() {\n-if (x)\n-y();\n+  if (x)\n+    y();\n")
	assert.Contains(t, out, "b.c\n@@ -1,4 +1,4 @@\n void f() {\n-if (x)\n-y();\n+\tif (x)\n+\t\ty();\n")
	assert.Contains(t, out, "+++ c.c\n@@ -1,4 +1,4 @@\n void f() {\n-if (x)\n-y();\n+    if (x)\n+        y();\n")

	// A directory: the excludes of its project are matched from the lib
	// root wherever the walk starts; a settings scope adjusts the files
	// under it; a root config in the tree is a project of its own, with
	// its own settings and excludes.
	code, listed, errs := dgdtools(t, "", "format", "--check", "-q", filepath.Join(dir, "two/lib/d"))
	assert.Equal(t, 1, code, errs)
	assert.Equal(t, []string{"a.c", "own/mine.c", "sub/deep.c"}, relLines(listed, filepath.Join(dir, "two/lib/d")))
	out = show(filepath.Join(dir, "two/lib/d"))
	assert.Contains(t, out, "deep.c\n@@ -1,4 +1,4 @@\n void f() {\n-if (x)\n-y();\n+        if (x)\n+                y();\n")
	assert.Contains(t, out, "mine.c\n@@ -1,4 +1,4 @@\n void f() {\n-if (x)\n-y();\n+\tif (x)\n+\t\ty();\n")
	// A profile that is not there is what lint needs: a file is still
	// formatted by its settings, with a note.
	code, listed, errs = dgdtools(t, "", "format", "--check", filepath.Join(dir, "lost"))
	assert.Equal(t, 1, code)
	assert.Contains(t, errs, `note: formatting by the settings without it: profile "not-installed" not found`)
	assert.Equal(t, []string{"a.c", "own/mine.c"}, relLines(listed, filepath.Join(dir, "lost")))
	// A file named explicitly is formatted even if excluded.
	assert.Contains(t, show(filepath.Join(dir, "two/lib/d/old/gone.c")), "+  if (x)\n")

	// .editorconfig gives what no dgdtools.yml sets, per file name.
	assert.Contains(t, show(filepath.Join(dir, "editor/d.c")), "+\tif (x)\n")
	assert.Contains(t, show(filepath.Join(dir, "editor/d.h")), "+   if (x)\n")
	assert.Contains(t, show(filepath.Join(dir, "editor/sub/e.c")), "+ if (x)\n+  y();\n")
	assert.Contains(t, show(filepath.Join(dir, "editor/sub/deeper/f.c")), "+  if (x)\n+    y();\n")

	// Flags win over every file; --config is the whole configuration;
	// --no-config is none.
	two, tabs := filepath.Join(dir, "two/lib/d/sub/deep.c"), filepath.Join(dir, "tabs/b.c")
	out = show("--indent-width", "1", two, tabs)
	assert.Contains(t, out, "+ if (x)\n+  y();\n")
	assert.Contains(t, out, "+\tif (x)\n")
	out = show("--indent-style", "space", "--indent-width", "3", "--line-endings", "crlf", tabs)
	assert.Contains(t, out, "+   if (x)\r\n")
	assert.Contains(t, show("--config", filepath.Join(dir, "tabs/dgdtools.yml"), two), "+\tif (x)\n")
	assert.Contains(t, show("--no-config", two, filepath.Join(dir, "editor/d.c")), "+    if (x)\n")
	assert.NotContains(t, show("--no-config", two, filepath.Join(dir, "editor/d.c")), "\t")
}

func relLines(out, base string) []string {
	var rel []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		r, _ := filepath.Rel(base, l)
		rel = append(rel, filepath.ToSlash(r))
	}
	return rel
}

func TestFormatSkipMarkers(t *testing.T) {
	src := "int   a;\n/* dgdtools-ignore-start format */\nint   table;\n/* dgdtools-ignore-end format */\nint   b;\n"
	code, out, _ := dgdtools(t, src, "format", "--no-config", "-")
	assert.Equal(t, 0, code)
	assert.Equal(t, "int a;\n/* dgdtools-ignore-start format */\nint   table;\n/* dgdtools-ignore-end format */\nint b;\n", out)
}

func TestFormatLines(t *testing.T) {
	const src = "void f() {\n  a=1;\n  b=2;\n  c=3;\n}\nint   g;\n"
	dir := tree(t, map[string]string{"a.c": src, "b.c": src})
	a := filepath.Join(dir, "a.c")
	// Only the lines asked for change; they follow the file's own
	// indentation.
	code, out, _ := dgdtools(t, src, "format", "--no-config", "--lines", "3", "-")
	assert.Equal(t, 0, code)
	assert.Equal(t, "void f() {\n  a=1;\n  b = 2;\n  c=3;\n}\nint   g;\n", out)
	code, _, errs := dgdtools(t, "", "format", "--lines", "2-3", "--lines", "6", a)
	assert.Equal(t, 0, code)
	assert.Equal(t, "1 file reformatted, 0 files unchanged\n", errs)
	assert.Equal(t, "void f() {\n  a = 1;\n  b = 2;\n  c=3;\n}\nint g;\n", read(t, a))
	// With --check, lines that are fine are fine.
	code, _, _ = dgdtools(t, "", "format", "--check", "--lines", "2-3", a)
	assert.Equal(t, 0, code)
	code, out, _ = dgdtools(t, "", "format", "--check", "--lines", "4", a)
	assert.Equal(t, 1, code)
	assert.Equal(t, a+"\n", out)

	for _, bad := range [][]string{
		{"--lines", "0-3", a},
		{"--lines", "5-2", a},
		{"--lines", "x", a},
		{"--lines", "1-2", a, filepath.Join(dir, "b.c")},
		{"--lines", "1-2", dir},
		{"--lines", "1-2", "--changed", a},
	} {
		code, _, _ = dgdtools(t, "", append([]string{"format"}, bad...)...)
		assert.Equal(t, 2, code, bad)
	}
}

func TestFormatChanged(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	const old = "void f() {\n  a=1;\n  b=2;\n  c=3;\n}\n\nvoid g() {\n  d=4;\n}\n"
	dir := tree(t, map[string]string{
		"lib/a.c":     old,
		"lib/same.c":  old,
		"lib/sub/b.c": old,
		".gitignore":  "ignored.c\n",
	})
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "legacy")
	git("tag", "start")

	// Edit one line of one function, stage another edit, add a file.
	write(t, dir, map[string]string{
		"lib/a.c":       strings.Replace(old, "  b=2;", "  b=22;", 1),
		"lib/sub/b.c":   strings.Replace(old, "  d=4;", "  d=44;\n  e=5;", 1),
		"lib/new.c":     "int   n;\n",
		"lib/ignored.c": "int   i;\n",
	})
	git("add", "lib/sub/b.c")
	t.Chdir(filepath.Join(dir, "lib"))

	// --check with --changed: only what was touched is looked at.
	code, out, errs := dgdtools(t, "", "format", "--changed", "--check", ".")
	assert.Equal(t, 1, code, errs)
	assert.Equal(t, "a.c\nnew.c\nsub/b.c\n", filepath.ToSlash(out))
	assert.Equal(t, "3 files would be reformatted, 0 files already formatted\n", errs)

	code, _, errs = dgdtools(t, "", "format", "--changed", ".")
	assert.Equal(t, 0, code, errs)
	// The changed line is formatted, the rest of the file is as it was.
	assert.Equal(t, strings.Replace(old, "  b=2;", "  b = 22;", 1), read(t, "a.c"))
	// Staged changes count: the comparison is with the revision. (Every
	// line of this block changed, so there is no line in it to follow.)
	assert.Equal(t, strings.Replace(old, "  d=4;", "    d = 44;\n    e = 5;", 1), read(t, "sub/b.c"))
	// A file git does not know is new as a whole; an ignored one is not
	// git's business, and an unchanged one is not read.
	assert.Equal(t, "int n;\n", read(t, "new.c"))
	assert.Equal(t, "int   i;\n", read(t, "ignored.c"))
	assert.Equal(t, old, read(t, "same.c"))
	code, out, _ = dgdtools(t, "", "format", "--changed", "--check", "-q", ".")
	assert.Equal(t, 0, code)
	assert.Empty(t, out)

	// Against another revision: everything since.
	git("add", ".")
	git("commit", "-q", "-m", "work")
	code, out, _ = dgdtools(t, "", "format", "--changed", "--check", "-q", ".")
	assert.Equal(t, 0, code)
	assert.Empty(t, out)
	write(t, dir, map[string]string{"lib/a.c": strings.Replace(read(t, "a.c"), "  c=3;", "  c=33;", 1)})
	code, out, _ = dgdtools(t, "", "format", "--changed", "--base", "start", "--diff", "-q", "a.c")
	assert.Equal(t, 1, code)
	assert.Contains(t, out, "-  c=33;\n+  c = 33;\n")
	assert.Equal(t, 1, strings.Count(out, "\n-"), "one line changes")

	// Errors: a revision git does not know, a directory that is not in a
	// work tree, flags that do not go together.
	code, _, errs = dgdtools(t, "", "format", "--changed", "--base", "no-such-rev", ".")
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, "dgdtools format: --changed: ")
	outside := tree(t, map[string]string{"x.c": messy})
	code, _, errs = dgdtools(t, "", "format", "--changed", outside)
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, "--changed: ")
	assert.Equal(t, messy, read(t, filepath.Join(outside, "x.c")))
	code, _, errs = dgdtools(t, "", "format", "--base", "start", ".")
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, "--base goes with --changed")
	code, _, _ = dgdtools(t, messy, "format", "--changed", "-")
	assert.Equal(t, 2, code)
}

func TestFormatProfileDialectAndConventions(t *testing.T) {
	p := &profile.Profile{Manifest: profile.Manifest{
		Format: profile.Format, Name: "test",
		Dialect:     profile.Dialect{Language: "lpc", Driver: "dgd", SlashSlash: true, Closures: true},
		Conventions: profile.Conventions{Macros: map[string]string{"visible": "specifier"}},
	}}
	data, err := p.Marshal()
	require.NoError(t, err)
	const src = "visible function*make() {\nreturn 0;\n}\n"
	dir := tree(t, map[string]string{
		"mine/test.profile": string(data),
		"mine/dgdtools.yml": "profile: test\n",
		"bare/a.c":          src,
	})
	// With the profile `function` is a type and `visible` a specifier:
	// this is a function definition.
	code, out, errs := dgdtools(t, src, "format", "--stdin-filename", filepath.Join(dir, "mine/a.c"))
	assert.Equal(t, 0, code, errs)
	assert.Equal(t, "visible function *make() {\n    return 0;\n}\n", out)
	// Without it the same text is not one.
	code, out, _ = dgdtools(t, src, "format", "--stdin-filename", filepath.Join(dir, "bare/a.c"))
	assert.Equal(t, 0, code)
	assert.Equal(t, "visible function * make() {\n    return 0;\n}\n", out)
	// The profile can be named on the command line.
	code, out, _ = dgdtools(t, src, "format", "--profile", filepath.Join(dir, "mine/test.profile"), "--stdin-filename", filepath.Join(dir, "bare/a.c"))
	assert.Equal(t, 0, code)
	assert.Contains(t, out, "function *make()")
}

// A file is replaced whole: the new text is written beside it and renamed
// over it. Its mode stays, a link to it stays a link, nothing is left
// behind.
func TestFormatReplacesTheFile(t *testing.T) {
	dir := tree(t, map[string]string{"real/a.c": messy, "b.c": messy})
	require.NoError(t, os.Chmod(filepath.Join(dir, "b.c"), 0o600))
	link := filepath.Join(dir, "link.c")
	if err := os.Symlink(filepath.Join("real", "a.c"), link); err != nil {
		t.Skip("no symbolic links here:", err)
	}
	code, _, errs := dgdtools(t, "", "format", "-q", link, filepath.Join(dir, "b.c"))
	require.Equal(t, 0, code, errs)
	assert.Equal(t, formatted, read(t, filepath.Join(dir, "real/a.c")))
	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink)
	info, err = os.Stat(filepath.Join(dir, "b.c"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	for _, d := range []string{dir, filepath.Join(dir, "real")} {
		entries, err := os.ReadDir(d)
		require.NoError(t, err)
		for _, e := range entries {
			assert.False(t, strings.HasPrefix(e.Name(), "."), "%s was left behind", e.Name())
		}
	}
}

func TestFormatErrors(t *testing.T) {
	dir := tree(t, map[string]string{
		"ok.c":                  messy,
		"bad.c":                 "int x = \"unterminated;\n",
		"cfg/dgdtools.yml":      "root: .\nformat: {line_endings: dos}\n",
		"cfg/a.c":               messy,
		"typo/dgdtools.yml":     "root: .\nfromat: {indent_width: 2}\n",
		"typo/a.c":              messy,
		"key/dgdtools.yml":      "root: .\nformat: {line_width: 80}\n",
		"key/a.c":               messy,
		"scope/dgdtools.yml":    "root: .\n",
		"scope/in/dgdtools.yml": "root: false\nexclude: [x]\n",
		"scope/in/a.c":          messy,
		"scope/b.c":             messy,
	})
	// A file that cannot be formatted is reported and left alone; the
	// others are still done; the exit code says something went wrong.
	code, out, errs := dgdtools(t, "", "format", filepath.Join(dir, "bad.c"), filepath.Join(dir, "ok.c"), filepath.Join(dir, "missing.c"))
	assert.Equal(t, 2, code)
	assert.Empty(t, out)
	assert.Contains(t, errs, "bad.c:1:9: file contains unlexable content")
	assert.Contains(t, errs, "missing.c: no such file or directory")
	assert.Contains(t, errs, "1 file reformatted, 0 files unchanged, 1 file could not be formatted")
	assert.Equal(t, formatted, read(t, filepath.Join(dir, "ok.c")))
	assert.Equal(t, "int x = \"unterminated;\n", read(t, filepath.Join(dir, "bad.c")))
	// An error wins over "would change".
	code, _, _ = dgdtools(t, "", "format", "--check", filepath.Join(dir, "bad.c"), filepath.Join(dir, "typo/a.c"))
	assert.Equal(t, 2, code)

	// Configuration that does not make sense is an error, not a default.
	for file, want := range map[string]string{
		"cfg/a.c":      "unknown line endings \"dos\"",
		"typo/a.c":     "fromat",
		"key/a.c":      "line_width",
		"scope/in/a.c": "exclude belongs in the project's root config",
	} {
		code, _, errs = dgdtools(t, "", "format", filepath.Join(dir, file))
		assert.Equal(t, 2, code, file)
		assert.Contains(t, errs, want, file)
		assert.Equal(t, messy, read(t, filepath.Join(dir, file)), file)
	}
	// A scope that is not valid stops its own files, not its project's.
	code, _, errs = dgdtools(t, "", "format", filepath.Join(dir, "scope"))
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, "exclude belongs in the project's root config")
	assert.Equal(t, formatted, read(t, filepath.Join(dir, "scope/b.c")))
	assert.Equal(t, messy, read(t, filepath.Join(dir, "scope/in/a.c")))

	// So are flags.
	for _, bad := range [][]string{
		{"--indent-style", "sideways", "-"},
		{"--indent-width", "40", "-"},
		{"--line-endings", "mac", "-"},
		{"--nonsense", "-"},
		{"-w", "-"},
	} {
		code, _, _ = dgdtools(t, messy, append([]string{"format"}, bad...)...)
		assert.Equal(t, 2, code, bad)
	}
}
