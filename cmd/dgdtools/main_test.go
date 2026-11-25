package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tree writes files to a temp dir and returns the directory.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	// macOS temp dirs are symlinked; resolve so paths compare equal.
	dir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	write(t, dir, files)
	return dir
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, src := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(src), 0o644))
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// dgdtools runs the command with the given standard input. The user's
// own configuration directory is kept out of it.
func dgdtools(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	if os.Getenv("DGDTOOLS_TEST_HOME") == "" {
		home := t.TempDir()
		t.Setenv("DGDTOOLS_TEST_HOME", home)
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		t.Setenv("AppData", filepath.Join(home, ".config"))
	}
	var out, errb bytes.Buffer
	code = run(context.Background(), args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestCommands(t *testing.T) {
	// No command, or one that does not exist: usage, exit 2.
	code, out, errs := dgdtools(t, "")
	assert.Equal(t, 2, code)
	assert.Empty(t, out)
	assert.Contains(t, errs, "usage: dgdtools <command>")
	code, _, errs = dgdtools(t, "", "frobnicate")
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, `unknown command "frobnicate"`)
	// A path is not a command: there is no default one.
	dir := tree(t, map[string]string{"a.c": "int x;\n"})
	code, _, errs = dgdtools(t, "", filepath.Join(dir, "a.c"))
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, "unknown command")
	// Commands have one name each.
	for _, alias := range []string{"fmt", "check"} {
		code, _, _ = dgdtools(t, "", alias)
		assert.Equal(t, 2, code, alias)
	}

	// Help, for the command and for each subcommand.
	code, out, _ = dgdtools(t, "", "help")
	assert.Equal(t, 0, code)
	for _, want := range []string{"lint", "format", "rules", "explain RULE", "config", "profile build", "profile show", "lsp", "version"} {
		assert.Contains(t, out, want)
	}
	for _, sub := range []string{"lint", "format", "rules", "explain", "config", "profile"} {
		code, _, errs = dgdtools(t, "", sub, "-h")
		assert.Equal(t, 0, code, sub)
		assert.Contains(t, errs, "usage: dgdtools "+sub, sub)
		code, _, errs2 := dgdtools(t, "", "help", sub)
		assert.Equal(t, 0, code, sub)
		assert.Equal(t, errs, errs2, sub)
		code, _, _ = dgdtools(t, "", sub, "--no-such-flag")
		assert.Equal(t, 2, code, sub)
	}

	code, out, _ = dgdtools(t, "", "version")
	assert.Equal(t, 0, code)
	assert.Equal(t, "dgdtools dev\n", out)
	code, out2, _ := dgdtools(t, "", "--version")
	assert.Equal(t, 0, code)
	assert.Equal(t, out, out2)

	// rename is a command like the others: advertised, and its usage
	// without its arguments.
	assert.Contains(t, usage, "rename SYMBOL NEW")
	code, _, errs = dgdtools(t, "", "rename", "x")
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, "dgdtools rename: want SYMBOL (or FILE) and NEW")
}

func TestRulesAndExplain(t *testing.T) {
	code, out, _ := dgdtools(t, "", "rules")
	assert.Equal(t, 0, code)
	for _, want := range []string{"syntax:", "runtime:", "suspicious:", "convention:", "E001 syntax-error", "R001 callable-not-found"} {
		assert.Contains(t, out, want)
	}
	code, _, _ = dgdtools(t, "", "rules", "extra")
	assert.Equal(t, 2, code)

	code, out, _ = dgdtools(t, "", "explain", "E005")
	assert.Equal(t, 0, code)
	assert.Contains(t, out, "declaration-after-statement")
	assert.Contains(t, out, "/* dgdtools-ignore lint/declaration-after-statement */")
	assert.NotContains(t, out, "Options")
	_, opts, _ := dgdtools(t, "", "explain", "raw-inherit-path")
	assert.Contains(t, opts, "Options, in lint.rules under raw-inherit-path:\n    deny: a list of strings; ")
	code, out2, _ := dgdtools(t, "", "explain", "declaration-after-statement")
	assert.Equal(t, 0, code)
	assert.Equal(t, out, out2)
	code, _, errs := dgdtools(t, "", "explain", "nope")
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, "dgdtools explain: unknown rule \"nope\" (see `dgdtools rules`)")
	code, _, _ = dgdtools(t, "", "explain")
	assert.Equal(t, 2, code)
}

func TestConfigCommand(t *testing.T) {
	dir := tree(t, map[string]string{
		"dgdtools.yml":                 "root: lib\ndriver_config: mud.dgd\nformat: {line_endings: lf}\nlint:\n  rules: {missing-visibility: warning}\n",
		"mud.dgd":                      "include_dirs = ({ \"/include\" });\ntypechecking = 1;\n",
		"lib/a.c":                      "",
		"lib/.editorconfig":            "[*.c]\nindent_style = tab\n",
		"lib/players/ann/dgdtools.yml": "root: false\nformat: {indent_width: 2}\nlint:\n  rules: {missing-visibility: off, unreachable-code: error}\n",
		"lib/players/ann/b.c":          "",
	})
	cfg := filepath.Join(dir, "dgdtools.yml")
	code, out, errs := dgdtools(t, "", "config", filepath.Join(dir, "lib/a.c"))
	assert.Equal(t, 0, code, errs)
	assert.Contains(t, out, "config file:   "+cfg)
	assert.Contains(t, out, "mudlib root:   "+filepath.Join(dir, "lib"))
	assert.Contains(t, out, "typechecking:  1")
	assert.Contains(t, out, "incomplete", "the configured include dir does not exist")
	// The settings of the file, and where each is from.
	assert.Contains(t, out, "settings of "+filepath.Join(dir, "lib/a.c"))
	assert.Contains(t, out, "indent style:  tab       ("+filepath.Join(dir, "lib/.editorconfig")+")")
	assert.Contains(t, out, "indent width:  4         (default)")
	assert.Contains(t, out, "line endings:  lf        ("+cfg+")")
	assert.Contains(t, out, "missing-visibility           warning")
	assert.NotContains(t, out, "scope:")

	// Under a settings scope: the same project, other settings.
	scope := filepath.Join(dir, "lib/players/ann/dgdtools.yml")
	code, out, _ = dgdtools(t, "", "config", filepath.Join(dir, "lib/players/ann/b.c"))
	assert.Equal(t, 0, code)
	assert.Contains(t, out, "config file:   "+cfg)
	assert.Contains(t, out, "scope:         "+scope)
	assert.Contains(t, out, "indent width:  2         ("+scope+")")
	assert.Contains(t, out, "unreachable-code             error")
	assert.NotContains(t, out, "missing-visibility")
	// A directory, from inside it, with no argument.
	t.Chdir(filepath.Join(dir, "lib/players/ann"))
	code, out, _ = dgdtools(t, "", "config")
	assert.Equal(t, 0, code)
	assert.Contains(t, out, "settings of "+filepath.Join(dir, "lib/players/ann", "*.c"))
	assert.Contains(t, out, "indent style:  tab ")

	// The command line over everything.
	code, out, _ = dgdtools(t, "", "config", "--no-config", "b.c")
	assert.Equal(t, 0, code)
	assert.Contains(t, out, "config file:   (none)")
	assert.Contains(t, out, "indent style:  space     (default)")

	// A scope that is not valid is an error where it applies.
	write(t, dir, map[string]string{"lib/players/ann/dgdtools.yml": "root: false\nprofile: mymud\n"})
	code, _, errs = dgdtools(t, "", "config", "b.c")
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, "dgdtools config: "+scope+": profile belongs in the project's root config")
	code, _, _ = dgdtools(t, "", "config", filepath.Join(dir, "lib/a.c"))
	assert.Equal(t, 0, code)
}

// `dgdtools config` is how a configuration is verified: whatever is wrong
// with the files that apply to the path, or lie below it, is said, with
// exit code 2.
func TestConfigVerifies(t *testing.T) {
	dir := tree(t, map[string]string{
		"dgdtools.yml":             "root: lib\nexclude: [\"attic/**\"]\n",
		"lib/a.c":                  "",
		"lib/d/x/dgdtools.yml":     "root: false\nlint:\n  rules: {lifecycle-chain: {names: [create, reset]}}\n",
		"lib/d/x/a.c":              "",
		"lib/attic/y/dgdtools.yml": "root: false\nnonsense: 1\n",
	})
	cfg, nested := filepath.Join(dir, "dgdtools.yml"), filepath.Join(dir, "lib/d/x/dgdtools.yml")
	code, out, errs := dgdtools(t, "", "config", dir)
	assert.Equal(t, 0, code, errs)
	assert.Contains(t, out, "below:         "+nested+" (valid)")
	assert.NotContains(t, out, "attic", "an excluded directory is not the project's")

	bad := func(file, content, want string) {
		t.Helper()
		was := read(t, file)
		write(t, "/", map[string]string{file: content})
		code, out, errs := dgdtools(t, "", "config", dir)
		assert.Equal(t, 2, code, content)
		assert.Empty(t, out)
		assert.Contains(t, errs, "dgdtools config: ", content)
		assert.Contains(t, errs, want, content)
		write(t, "/", map[string]string{file: was})
	}
	bad(cfg, "root: lib\ndriver_confg: mud.dgd\n", `line 2: unknown key "driver_confg" (did you mean "driver_config"?)`)
	bad(cfg, "root: lib\ninclude_dirs: [/x]\n", cfg+": line 2: include_dirs removed: dgdtools reads it from driver_config")
	bad(cfg, "root: lib\nprofile_build:\n  driver_config: mud.dgd\n", cfg+": line 3: profile_build.driver_config moved: use top-level driver_config")
	bad(cfg, "root: lib\nconventions:\n  specifier_macros: [public]\n", "conventions.specifier_macros removed: use conventions.macros: { public: specifier }")
	bad(cfg, "root: lib\nrelative_object_paths: here\n", cfg+": relative_object_paths: unknown value \"here\"")
	bad(cfg, "root: lib\nconventions:\n  macros: {X: thing}\n", "conventions.macros: X: unknown class \"thing\"")
	bad(cfg, "root: lib\nconventions:\n  call_registry: {f: 0}\n", "line 3: a call registry entry is {function: N}")
	bad(cfg, "root: lib\nlint:\n  rules: {no-such: off}\n", "line 3: lint.rules: unknown rule \"no-such\"")
	bad(cfg, "root: lib\nlint:\n  rules:\n    syntax-error: {deny: [x]}\n", "rule syntax-error has no option \"deny\"")
	bad(cfg, "root: lib\nlint:\n  rules: {E001: off, syntax-error: error}\n", "rule syntax-error is set twice")
	bad(cfg, "root: lib\nlint: {fail_on: never}\n", "lint.fail_on removed: pass --fail-on to dgdtools lint")
	bad(cfg, "root: lib\nformat: {indent_width: wide}\n", "line 2: format.indent_width: wide is not a whole number")
	bad(cfg, "root: lib\nprofile: nowhere\n", "profile \"nowhere\" not found")
	bad(nested, "root: false\nlint:\n  rules: {lifecycle-chain: {nams: [create]}}\n", nested+": line 3: lint.rules: rule lifecycle-chain has no option \"nams\"")
	bad(nested, "root: false\nexclude: [x]\n", nested+": exclude belongs in the project's root config")

	// Valid again; a file's own configuration is what is checked for a file.
	code, _, errs = dgdtools(t, "", "config", filepath.Join(dir, "lib/a.c"))
	assert.Equal(t, 0, code, errs)
}

func TestUserLevelConfig(t *testing.T) {
	home := t.TempDir()
	home, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	t.Setenv("DGDTOOLS_TEST_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, ".config"))
	user := filepath.Join(home, ".config/dgdtools/dgdtools.yml")
	write(t, home, map[string]string{".config/dgdtools/dgdtools.yml": "format: {indent_width: 2}\n"})
	dir := tree(t, map[string]string{
		"bare/a.c":          messy,
		"proj/dgdtools.yml": "root: .\n",
		"proj/b.c":          messy,
	})
	// It is the configuration of a path that has no project's...
	code, out, _ := dgdtools(t, messy, "format", "--stdin-filename", filepath.Join(dir, "bare/a.c"))
	assert.Equal(t, 0, code)
	assert.Equal(t, indent2, out)
	code, out, _ = dgdtools(t, "", "config", filepath.Join(dir, "bare/a.c"))
	assert.Equal(t, 0, code)
	assert.Contains(t, out, "config file:   "+user+" (user-level: no project config found)")
	// ...and of no other.
	code, out, _ = dgdtools(t, messy, "format", "--stdin-filename", filepath.Join(dir, "proj/b.c"))
	assert.Equal(t, 0, code)
	assert.Equal(t, formatted, out)
	code, out, _ = dgdtools(t, messy, "format", "--no-config", "--stdin-filename", filepath.Join(dir, "bare/a.c"))
	assert.Equal(t, 0, code)
	assert.Equal(t, formatted, out)
}

// An interrupted command says so and exits the way a shell reports an
// interrupt, not as an error of the command.
func TestInterrupted(t *testing.T) {
	dir := tree(t, map[string]string{"dgdtools.yml": "root: .\n", "a.c": "void f() { }\n"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, args := range [][]string{{"lint", dir}, {"format", "--check", dir}} {
		var out, errb bytes.Buffer
		code := run(ctx, args, strings.NewReader(""), &out, &errb)
		assert.Equal(t, 130, code, args)
		assert.Equal(t, "dgdtools "+args[0]+": interrupted\n", errb.String())
	}
}

// A directory below the project with a root config of its own is another
// project to every command: lint passes over it and says so, format
// formats it by its own settings, config names it.
func TestNestedProject(t *testing.T) {
	const bare = "int f() {\nreturn 1;\n}\n"
	dir := tree(t, map[string]string{
		"dgdtools.yml":     "root: .\nlint:\n  rules: {missing-visibility: warning}\n",
		"a.c":              bare,
		"own/dgdtools.yml": "root: .\nlint:\n  rules: {missing-visibility: error}\nformat: {indent_style: tab}\n",
		"own/b.c":          bare,
	})
	own := filepath.Join(dir, "own")
	code, out, errs := dgdtools(t, "", "lint", dir)
	assert.Equal(t, 0, code, errs)
	assert.Contains(t, out, "a.c:1:5: warning")
	assert.NotContains(t, out, "b.c")
	assert.Contains(t, errs, "1 file under "+own+" is a project of their own (dgdtools.yml) and was not linted")
	code, out, _ = dgdtools(t, "", "lint", own)
	assert.Equal(t, 1, code)
	assert.Contains(t, out, "b.c:1:5: error")
	code, out, _ = dgdtools(t, "", "lint", filepath.Join(own, "b.c"))
	assert.Equal(t, 1, code)
	assert.Contains(t, out, "b.c:1:5: error")

	code, out, errs = dgdtools(t, "", "config", dir)
	assert.Equal(t, 0, code, errs)
	assert.Contains(t, out, "below:         "+filepath.Join(own, "dgdtools.yml")+" (a project of its own)")

	code, _, errs = dgdtools(t, "", "format", "-q", dir)
	assert.Equal(t, 0, code, errs)
	assert.Equal(t, "int f() {\n    return 1;\n}\n", read(t, filepath.Join(dir, "a.c")))
	assert.Equal(t, "int f() {\n\treturn 1;\n}\n", read(t, filepath.Join(own, "b.c")))
}
