package lint_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

func minilibProject(t *testing.T) *config.Project {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "minilib"))
	require.NoError(t, err)
	cfg := &config.Config{}
	cfg.Root = root
	cfg.IncludeDirs = []string{"/include", "/dgd/include"}
	cfg.IncludeFile = "/dgd/include/Std.h"
	cfg.AutoObjects = []string{"/dgd/lib/object"}
	object := 0
	cfg.Conventions.CallRegistry = map[string]profile.CallForms{
		"register_handler": {{Function: 0}},
		"store_fp":         {{Object: &object, Function: 1}, {Function: 0}},
	}
	cfg.Conventions.AutosaveMarkers = []string{"set_auto_save"}
	cfg.Conventions.VirtualPaths = []string{"virtual/**"}
	return &config.Project{Config: cfg, Root: root}
}

func run(t *testing.T, prj *config.Project, fsys vfs.FS, req lint.Request) *lint.Result {
	t.Helper()
	l, err := lint.New(prj, fsys)
	require.NoError(t, err)
	res, err := l.Run(context.Background(), req)
	require.NoError(t, err)
	return res
}

func noteKinds(notes []diag.Note) []diag.NoteKind {
	var out []diag.NoteKind
	for _, n := range notes {
		out = append(out, n.Kind)
	}
	return out
}

// keys renders findings as "rule file:line".
func keys(ds []diag.Diagnostic) []string {
	var out []string
	for _, d := range ds {
		out = append(out, fmt.Sprintf("%s %s:%d", d.Rule, filepath.Base(d.Path), d.Line))
	}
	return out
}

func TestMinilibSeededBugs(t *testing.T) {
	res := run(t, minilibProject(t), vfs.OS{}, lint.Request{Options: lint.Options{Enable: []string{"static-autosave-var"}}})
	got := keys(res.Diagnostics)

	for _, want := range []string{
		"callable-not-found user.c:10",    // call_out to missing function
		"callable-not-found user.c:11",    // config registrar to missing function
		"callable-not-found user.c:19",    // this_object()->missing_fn()
		"static-crossobj user.c:27",       // static cross-object
		"static-crossobj user.c:21",       // private via ->
		"callable-not-found user.c:24",    // call_other missing
		"callable-not-found user.c:26",    // literal-path -> missing
		"static-autosave-var saver.c:4",   //
		"lifecycle-chain messy.c:6",       //
		"undefined-prototype user.c:28",   // call to declared-never-defined function
		"target-object-missing user.c:29", // clone_object of missing file
		"target-object-missing user.c:30", // -> on missing object
		"target-object-missing broken.c:3",
		"syntax-error broken.c:4", // inherit of a macro nobody defines
		"include-not-found vet.c:1",
		"assignment-in-condition vet.c:12",
		"assignment-in-condition vet.c:16",
		"no-effect-statement vet.c:18",
		"sscanf-format vet.c:19",
		"callback-arity vet.c:21",
		"callback-arity vet.c:22",
	} {
		assert.Contains(t, got, want)
	}
	for _, k := range got {
		for _, clean := range []string{
			"user.c:31", // virtual path is exempt
			"thing.c:3", // forward decl with definition
			"cond.c",    // #ifdef-split brackets; registration arity unknown
			"vet.c:14",  // ((x = 1)) idiom
			"vet.c:25",  // assign-and-test
			"vet.c:20",  // %% is not a conversion
			"vet.c:23",  // correct arity
			"vet.c:24",  // ellipsis accepts extras
			"mixinuser.c",
			"user.c:20",   // static + same object is legal
			"user.c:18",   // real function
			"user.c:22",   // auto object provides it
			"user.c:23",   //
			"user.c:25",   //
			"user.c:12",   // registered handler exists
			"broken.c:10", // partial chain: unknown, not missing
		} {
			assert.NotContains(t, k, clean)
		}
	}

	// Diagnostics carry codes, ranges and related locations.
	for _, d := range res.Diagnostics {
		assert.NotEmpty(t, d.Code)
		assert.LessOrEqual(t, d.Start, d.End)
		if d.Rule == "static-crossobj" {
			require.NotEmpty(t, d.Related)
			assert.Equal(t, "thing.c", filepath.Base(d.Related[0].Path))
		}
	}
	// Unknowns are counted, not reported.
	assert.Greater(t, res.Stats.UnknownReceivers, 0)
	assert.Greater(t, res.Stats.UnresolvedInherits, 0)
}

// A linter that only lints keeps no Uses, and finds what any other
// does: with every rule on the minilib, the same findings in the same
// order.
func TestLintOnly(t *testing.T) {
	prj := minilibProject(t)
	req := lint.Request{Options: lint.Options{Select: []string{"all"}}}
	want := run(t, prj, vfs.OS{}, req)
	l, err := lint.New(prj, vfs.OS{}, lint.LintOnly())
	require.NoError(t, err)
	got, err := l.Run(context.Background(), req)
	require.NoError(t, err)
	require.NotEmpty(t, got.Diagnostics)
	assert.Equal(t, want.Diagnostics, got.Diagnostics)
	assert.Equal(t, want.Stats, got.Stats)
	u := l.Index().Snapshot().Unit("/obj/user")
	require.NotNil(t, u)
	assert.False(t, u.HasUses())
}

func TestOptInRulesAndSuppression(t *testing.T) {
	res := run(t, minilibProject(t), vfs.OS{}, lint.Request{Options: lint.Options{Enable: []string{"missing-visibility", "raw-inherit-path", "unresolved-inherit", "C006"}}})
	got := keys(res.Diagnostics)
	assert.Contains(t, got, "missing-visibility messy.c:13")
	assert.Contains(t, got, "raw-inherit-path messy.c:1")
	assert.Contains(t, got, "unresolved-inherit broken.c:4")
	assert.NotContains(t, got, "missing-visibility messy.c:21", "the comment above it suppresses")
	for _, k := range got {
		assert.NotContains(t, k, "missing-visibility thing.c", "public counts as a specifier")
		assert.NotContains(t, k, "unused-suppression", "the one suppression comment is used")
	}
	assert.Equal(t, 1, res.Stats.Suppressed)

	// With the rule off, whether the comment is needed cannot be told.
	res = run(t, minilibProject(t), vfs.OS{}, lint.Request{Options: lint.Options{Enable: []string{"unused-suppression"}}})
	assert.NotContains(t, strings.Join(keys(res.Diagnostics), "\n"), "unused-suppression")
}

func TestRuleSelection(t *testing.T) {
	prj := minilibProject(t)

	res := run(t, prj, vfs.OS{}, lint.Request{Options: lint.Options{Disable: []string{"static-crossobj"}}})
	for _, d := range res.Diagnostics {
		assert.NotEqual(t, "static-crossobj", d.Rule)
	}

	// --select replaces the set; codes and groups work.
	res = run(t, prj, vfs.OS{}, lint.Request{Options: lint.Options{Select: []string{"R001"}}})
	require.NotEmpty(t, res.Diagnostics)
	for _, d := range res.Diagnostics {
		assert.Equal(t, "callable-not-found", d.Rule)
	}
	res = run(t, prj, vfs.OS{}, lint.Request{Options: lint.Options{Select: []string{"suspicious"}}})
	for _, d := range res.Diagnostics {
		assert.Contains(t, []string{"assignment-in-condition", "no-effect-statement", "sscanf-format", "unreachable-code"}, d.Rule)
	}

	// Config: scalar severity enables and sets severity; off disables;
	// group settings are overridden by rule settings.
	prj = minilibProject(t)
	prj.Config.Lint.Rules = map[string]config.RuleSettings{
		"runtime":            {Severity: "off"},
		"callable-not-found": {Severity: "warning"},
		"missing-visibility": {Severity: "info"},
	}
	res = run(t, prj, vfs.OS{}, lint.Request{})
	seen := map[string]diag.Severity{}
	for _, d := range res.Diagnostics {
		seen[d.Rule] = d.Severity
	}
	assert.Equal(t, diag.Warning, seen["callable-not-found"])
	assert.Equal(t, diag.Info, seen["missing-visibility"])
	assert.NotContains(t, seen, "static-crossobj")

	// Per-path overrides.
	prj = minilibProject(t)
	prj.Config.Lint.Overrides = []config.Override{
		{Paths: []string{"obj/**"}, Rules: map[string]config.RuleSettings{"callable-not-found": {Severity: "off"}}},
		{Paths: []string{"daemons/**"}, Rules: map[string]config.RuleSettings{"static-autosave-var": {Severity: "off"}}},
	}
	res = run(t, prj, vfs.OS{}, lint.Request{Options: lint.Options{Enable: []string{"static-autosave-var"}}})
	for _, d := range res.Diagnostics {
		assert.NotEqual(t, "callable-not-found", d.Rule)
		assert.NotEqual(t, "static-autosave-var", d.Rule)
	}

	// Unknown rules are configuration errors, not silently ignored.
	prj = minilibProject(t)
	l, err := lint.New(prj, vfs.OS{})
	require.NoError(t, err)
	_, err = l.Run(context.Background(), lint.Request{Options: lint.Options{Enable: []string{"no-such-rule"}}})
	assert.ErrorContains(t, err, "unknown rule")
	prj.Config.Lint.Rules = map[string]config.RuleSettings{"nope": {Severity: "off"}}
	_, err = lint.New(prj, vfs.OS{})
	assert.ErrorContains(t, err, "unknown rule")
	prj.Config.Lint.Rules = map[string]config.RuleSettings{"syntax": {Severity: "loud"}}
	_, err = lint.New(prj, vfs.OS{})
	assert.ErrorContains(t, err, "unknown severity")
}

// A rule's mapping takes the options of that rule and no others, and a
// rule is set once in a mapping.
func TestRuleOptionsAreChecked(t *testing.T) {
	load := func(rules string) error {
		m := vfs.Mem{"/p/dgdtools.yml": "lint:\n  rules:\n" + rules, "/p/a.c": ""}
		prj, err := config.Discover(m, []string{"/p"}, config.DiscoverOptions{})
		if err != nil {
			return err
		}
		_, err = lint.New(prj, m)
		return err
	}
	require.NoError(t, load("    lifecycle-chain: {severity: error, names: [create, reset]}\n"))
	require.NoError(t, load("    raw-inherit-path: {deny: [/std/]}\n    R006: {names: [create]}\n    syntax: {severity: warning}\n"))

	for rules, want := range map[string]string{
		"    lifecycle-chain: {nmaes: [create]}\n": `line 3: lint.rules: rule lifecycle-chain has no option "nmaes" (its options: names)`,
		"    lifecycle-chain: {deny: [/std/]}\n":   `rule lifecycle-chain has no option "deny" (its options: names)`,
		"    syntax-error: {deny: [/std/]}\n":      `rule syntax-error has no option "deny" (it has no options)`,
		"    lifecycle-chain: {names: create}\n":   "rule lifecycle-chain: option names: want a list of strings",
		"    raw-inherit-path: {deny: {a: b}}\n":   "rule raw-inherit-path: option deny: want a list of strings",
		"    runtime: {names: [create]}\n":         `"runtime" is a group of rules, and "names" is not a setting of a group`,
		"    all: {deny: []}\n":                    `"all" is a group of rules`,
		"    syntax-error: off\n    E001: error\n": `line 4: lint.rules: rule syntax-error is set twice, as "syntax-error" and as "E001"`,
		"    e001: off\n    E001: error\n":         `rule syntax-error is set twice, as "e001" and as "E001"`,
		"    no-such-rule: off\n":                  `line 3: lint.rules: unknown rule "no-such-rule"`,
		"    syntax-error: loud\n":                 `line 3: lint.rules: syntax-error: unknown severity "loud" (want off, info, warning or error)`,
		"    syntax-error: {severity: [a]}\n":      "severity: want off, info, warning or error",
		"    syntax-error: {max_per_file: 5}\n":    "line 3: lint.rules.syntax-error.max_per_file removed: pass --max-per-file to dgdtools lint",
		"    syntax-error: [error]\n":              "line 3: a rule is set to a severity (off, info, warning, error) or to a mapping",
	} {
		assert.ErrorContains(t, load(rules), want, rules)
	}

	// The same for overrides and for nested configuration files.
	m := vfs.Mem{
		"/p/dgdtools.yml":     "root: .\nlint:\n  overrides:\n    - paths: [\"x/**\"]\n      rules: {lifecycle-chain: {deny: [a]}}\n",
		"/q/dgdtools.yml":     "root: .\n",
		"/q/sub/dgdtools.yml": "root: false\nlint:\n  rules: {raw-inherit-path: {names: [a]}}\n",
		"/q/sub/a.c":          "",
	}
	prj, err := config.Discover(m, []string{"/p"}, config.DiscoverOptions{})
	require.NoError(t, err)
	_, err = lint.New(prj, m)
	assert.ErrorContains(t, err, `lint.overrides[0].rules: rule lifecycle-chain has no option "deny"`)
	prj, err = config.Discover(m, []string{"/q"}, config.DiscoverOptions{})
	require.NoError(t, err)
	l, err := lint.New(prj, m)
	require.NoError(t, err)
	_, err = l.Run(context.Background(), lint.Request{})
	assert.ErrorContains(t, err, `/q/sub/dgdtools.yml: line 3: lint.rules: rule raw-inherit-path has no option "names" (its options: deny)`)
}

// Options set in several layers add up key by key.
func TestRuleSettingsLayer(t *testing.T) {
	src := "inherit \"/std/base\";\ninherit \"/obj/x\";\nvoid a() { x == 1; y == 2; z == 3; }\n"
	prj, m := memProject(map[string]string{"obj/t.c": src, "obj/x.c": "", "d/t.c": src})
	prj.Config.Lint.Rules = map[string]config.RuleSettings{
		"raw-inherit-path": {Severity: "warning", Options: map[string]any{"deny": []string{"/std/"}}},
	}
	prj.Config.Lint.Overrides = []config.Override{
		{Paths: []string{"d/**"}, Rules: map[string]config.RuleSettings{
			"raw-inherit-path":    {Severity: "info"},
			"no-effect-statement": {Severity: "error"},
		}},
		{Paths: []string{"d/**"}, Rules: map[string]config.RuleSettings{"C001": {Options: map[string]any{"deny": []string{"/obj/"}}}}},
	}
	res := run(t, prj, m, lint.Request{Paths: []string{"/lib/obj/t.c", "/lib/d/t.c"}})
	var got []string
	for _, d := range res.Diagnostics {
		got = append(got, fmt.Sprintf("%s:%d %s %s", strings.TrimPrefix(d.Path, "/lib/"), d.Line, d.Severity, d.Rule))
	}
	assert.Equal(t, []string{
		"d/t.c:2 info raw-inherit-path", // the override's severity, the later override's prefix
		"d/t.c:3 error no-effect-statement",
		"d/t.c:3 error no-effect-statement",
		"d/t.c:3 error no-effect-statement",
		"obj/t.c:1 warning raw-inherit-path",
		"obj/t.c:3 warning no-effect-statement",
		"obj/t.c:3 warning no-effect-statement",
		"obj/t.c:3 warning no-effect-statement",
	}, got)
}

// ------------------------------------------------------------ mem libs

// memProject builds an in-memory lib under /lib with a config that mirrors
// a small driver setup.
func memProject(files map[string]string) (*config.Project, vfs.Mem) {
	m := vfs.Mem{
		"/lib/include/std.h":   "#define public\n#define status int\n#define STD \"/std/\"\n",
		"/lib/sys/auto.c":      "string object_name() { return \"x\"; }\n",
		"/lib/sys/forced.h":    "#define TRUE 1\n",
		"/lib/std/base.c":      "static void create() { }\nint query(int a) { return a; }\nstatic int hidden() { return 1; }\n",
		"/lib/placeholder.txt": "",
	}
	for k, v := range files {
		m["/lib/"+k] = v
	}
	cfg := &config.Config{}
	cfg.Root = "/lib"
	cfg.IncludeDirs = []string{"/include"}
	cfg.IncludeFile = "/sys/forced.h"
	cfg.AutoObjects = []string{"/sys/auto"}
	one := 1
	cfg.Dialect.Typechecking = &one
	return &config.Project{Config: cfg, Root: "/lib"}, m
}

// lintMem lints a single file "t.c" with the given source and returns
// "rule line:col message" strings.
func lintMem(t *testing.T, src string, req lint.Request, extra map[string]string) []string {
	t.Helper()
	files := map[string]string{"obj/t.c": src}
	for k, v := range extra {
		files[k] = v
	}
	prj, m := memProject(files)
	req.Paths = []string{"/lib/obj/t.c"}
	res := run(t, prj, m, req)
	var out []string
	for _, d := range res.Diagnostics {
		out = append(out, fmt.Sprintf("%s %d:%d %s", d.Rule, d.Line, d.Col, d.Message))
	}
	return out
}

func fn(body string) string { return "void f() {\n" + body + "\n}\n" }

func assertFinding(t *testing.T, got []string, prefix string) {
	t.Helper()
	for _, g := range got {
		if strings.HasPrefix(g, prefix) {
			return
		}
	}
	t.Errorf("no finding with prefix %q in %v", prefix, got)
}

func TestSyntaxRules(t *testing.T) {
	t.Run("castle", func(t *testing.T) {
		got := lintMem(t, "#include <std.h>\nmapping c;\nvoid create() {\n c[\"a\"] = ({\n  STD + \"x\", \"se\",\n  })\n c[\"b\"] = ({ });\n}\n", lint.Request{}, nil)
		require.Len(t, got, 1, "%v", got)
		assert.Equal(t, "syntax-error 6:5 missing ';' before 'c'", got[0])
	})
	t.Run("legacy idioms", func(t *testing.T) {
		got := lintMem(t, "int g = 5;\n"+fn("int x = 1;\n write(\"a\" \"b\"\n  \"c\");\n add(\"x\", (: this_object(), \"fn\" :));"), lint.Request{}, nil)
		assertFinding(t, got, "declaration-initializer 1:5")
		assertFinding(t, got, "declaration-initializer 3:5")
		assertFinding(t, got, "adjacent-strings 4:12")
		assertFinding(t, got, "legacy-closure 6:11")
		assert.Len(t, got, 4, "%v", got)
	})
	t.Run("adjacent strings fix", func(t *testing.T) {
		prj, m := memProject(map[string]string{"obj/t.c": fn(`write("a" "b" "c");`)})
		res := run(t, prj, m, lint.Request{Paths: []string{"/lib/obj/t.c"}})
		require.Len(t, res.Diagnostics, 1)
		d := res.Diagnostics[0]
		require.Len(t, d.Fixes, 1)
		src := []byte(m["/lib/obj/t.c"])
		edits := d.Fixes[0].Edits
		for i := len(edits) - 1; i >= 0; i-- { // apply back to front
			e := edits[i]
			src = append(src[:e.Start], append([]byte(e.NewText), src[e.End:]...)...)
		}
		assert.Contains(t, string(src), `write("a" + "b" + "c");`)
	})
	t.Run("missing semicolon fix", func(t *testing.T) {
		fixed := func(src string) (string, bool) {
			prj, m := memProject(map[string]string{"obj/t.c": src})
			res := run(t, prj, m, lint.Request{Paths: []string{"/lib/obj/t.c"}})
			require.Len(t, res.Diagnostics, 1, "%q", src)
			d := res.Diagnostics[0]
			if len(d.Fixes) == 0 {
				return "", false
			}
			require.Len(t, d.Fixes, 1)
			assert.Equal(t, diag.Safe, d.Fixes[0].Applicability)
			e := d.Fixes[0].Edits[0]
			return src[:e.Start] + e.NewText + src[e.End:], true
		}
		// Where the parser assumed the ';': the fixed file has no error.
		for _, src := range []string{fn(" a = 1\n b = 2;"), fn(" a = 1 /* c */\n b = 2;"), "int f() {\n return 1\n}\n", "int a\nint b;\n"} {
			out, ok := fixed(src)
			require.True(t, ok, "%q", src)
			assert.Empty(t, lintMem(t, out, lint.Request{}, nil), "%q", out)
		}
		// Not where a directive stands between: the ';' would be in
		// every branch.
		_, ok := fixed(fn(" a = 1\n#ifdef X\n b = 2;\n#endif"))
		assert.False(t, ok)
		// Not where the parser gave up on the statement.
		_, ok = fixed(fn(" a = 1 b = 2;"))
		assert.False(t, ok)
	})
	t.Run("declaration after statement", func(t *testing.T) {
		got := lintMem(t, fn("int a;\n a = 1;\n int b;\n { int ok; ok = 1; }\n ;\n if (a) { int fine; fine = 2; }"), lint.Request{}, nil)
		require.Len(t, got, 1, "%v", got)
		assertFinding(t, got, "declaration-after-statement 4:2")

		got = lintMem(t, "#define DECLS int a; int b;\n"+fn("DECLS\n int c;\n c = 1;"), lint.Request{}, nil)
		assert.Empty(t, got, "a macro before the declaration may itself declare")
	})
	t.Run("bad directive", func(t *testing.T) {
		got := lintMem(t, "#ifdef X\n#else X\n#endif X\n#endif /* X */\n#foo\n#\n#pragma whatever\nint a;\n", lint.Request{}, nil)
		assertFinding(t, got, "bad-directive 2:1")
		assertFinding(t, got, "bad-directive 3:1")
		assertFinding(t, got, "bad-directive 5:1 unknown preprocessor directive '#foo'")
		assert.Len(t, got, 3, "%v", got)

		got = lintMem(t, "#include <std.h>;\n#include <std.h> /* fine */\nint a;\n", lint.Request{}, nil)
		require.Len(t, got, 1, "%v", got)
		assertFinding(t, got, "bad-directive 1:1 ';' after the #include target")
	})
	t.Run("undecided branches are not judged", func(t *testing.T) {
		src := "#ifdef FEATURE\nfoo(a) { return 1; }\nvoid g() { \"/std/base\"->nope(); x = 1 y = 2; }\n#endif\n#ifdef __DGD__\nbar(b) { }\n#endif\n"
		got := lintMem(t, src, lint.Request{}, nil)
		require.Len(t, got, 1, "%v", got)
		assertFinding(t, got, "missing-parameter-type 6:5")
	})
	t.Run("inherit path", func(t *testing.T) {
		got := lintMem(t, "#include <std.h>\ninherit STD \"base\";\ninherit NOWHERE;\n", lint.Request{}, nil)
		assertFinding(t, got, "adjacent-strings 2:13")
		assertFinding(t, got, "syntax-error 3:9 inherit path must be a string constant")
	})
	t.Run("inherit after declaration", func(t *testing.T) {
		got := lintMem(t, "inherit \"/std/base\";\nint x;\ninherit b \"/std/base\";\n", lint.Request{}, nil)
		assertFinding(t, got, "inherit-after-declaration 3:1")

		extra := map[string]string{"obj/protos.h": "int helper(int x);\n", "obj/defs.h": "#define X 1\n"}
		got = lintMem(t, "#include \"defs.h\"\n#include \"protos.h\"\ninherit \"/std/base\";\n", lint.Request{}, extra)
		require.Len(t, got, 1, "%v", got)
		assertFinding(t, got, "inherit-after-declaration 3:1 inherit must precede all declarations, but \"protos.h\"")
		got = lintMem(t, "#include \"defs.h\"\ninherit \"/std/base\";\n#include \"protos.h\"\n", lint.Request{}, extra)
		assert.Empty(t, got)

		// A declaration the header only makes under an undecided #ifdef
		// proves nothing; one behind an include guard is certain.
		extra["obj/protos.h"] = "#ifdef EXEC\nint one() { return 1; }\n#endif\n"
		got = lintMem(t, "#include \"protos.h\"\ninherit \"/std/base\";\n", lint.Request{}, extra)
		assert.Empty(t, got)
		extra["obj/protos.h"] = "#ifndef PROTOS_H\n#define PROTOS_H\nint helper(int x);\n#endif\n"
		got = lintMem(t, "#include \"protos.h\"\ninherit \"/std/base\";\n", lint.Request{}, extra)
		assert.Len(t, got, 1, "%v", got)
	})
	t.Run("typeless functions under typechecking", func(t *testing.T) {
		src := "foo(a, int b) { }\nbar() { return 1; }\nbaz() { return; }\nvoid v() { return 2; }\nint i() { return; }\nstatus s() { return; }\npublic ok() { }\n"
		got := lintMem(t, "#include <std.h>\n"+src, lint.Request{}, nil)
		assertFinding(t, got, "missing-parameter-type 2:5 missing type for parameter 'a'")
		assertFinding(t, got, "return-value 3:9 value returned from 'bar'")
		assertFinding(t, got, "return-value 5:12 value returned from void function")
		assertFinding(t, got, "return-value 6:11 function must return a value")
		assert.Len(t, got, 4, "%v", got)

		// Typechecking unknown: the typeless rules are silent.
		prj, m := memProject(map[string]string{"obj/t.c": src})
		prj.Config.Dialect.Typechecking = nil
		res := run(t, prj, m, lint.Request{Paths: []string{"/lib/obj/t.c"}, Options: lint.Options{Select: []string{"E009", "E010"}}})
		for _, d := range res.Diagnostics {
			assert.NotContains(t, d.Message, "bar")
			assert.NotContains(t, d.Message, "parameter")
		}
	})
	t.Run("missing include is fatal for syntax", func(t *testing.T) {
		got := lintMem(t, "#include \"gone.h\"\nvoid f() { x = 1 y = 2; }\n", lint.Request{}, nil)
		require.Len(t, got, 1, "%v", got)
		assertFinding(t, got, "include-not-found 1:1")
	})
	t.Run("lexical", func(t *testing.T) {
		got := lintMem(t, fn("write(\"abc);\n x = 1 @ 2;"), lint.Request{}, nil)
		assertFinding(t, got, "syntax-error 2:7 unterminated string literal")
		assertFinding(t, got, "syntax-error 3:8 illegal character")
	})
}

func TestAssignmentInCondition(t *testing.T) {
	tests := []struct {
		name, body string
		want       int
		message    string
	}{
		{"integer constant", `if (x = 1) x = 2;`, 1, "assignment of a constant"},
		{"float constant", `if (x = 1.5) x = 2;`, 1, "assignment of a constant"},
		{"string constant", `if (s = "yes") x = 2;`, 1, "assignment of a constant"},
		{"character constant", `if (x = 'y') x = 2;`, 1, "assignment of a constant"},
		{"nil constant", `if (value = nil) x = 2;`, 1, "assignment of a constant"},
		{"negative constant", `if (x = -1) x = 2;`, 1, "assignment of a constant"},
		{"logical and RHS", `if (x = next() && ready) x = 2;`, 1, "stores the result of &&/||"},
		{"logical or RHS", `if (x = next() || ready) x = 2;`, 1, "stores the result of &&/||"},
		{"constant assignment chain", `if (x = y = 0) x = 2;`, 1, "assignment of a constant"},
		{"logical assignment chain", `if (x = y = next() && ready) x = 2;`, 1, "stores the result of &&/||"},
		{"do-while", `do x++; while (x = 0);`, 1, "assignment of a constant"},
		{"for middle clause", `for (i = 0; i = 5; i++) x++;`, 1, "assignment of a constant"},
		{"comma segments", `if (y = next(), x = 0) x = 2;`, 1, "assignment of a constant"},
		{"index target", `if (a[0] = 1) x = 2;`, 1, "assignment of a constant"},
		{"assign and test", `if (x = sizeof(values)) x = 2;`, 0, ""},
		{"while assign and test", `while (value = next()) x++;`, 0, ""},
		{"parenthesized constant", `if ((x = 1)) x = 2;`, 0, ""},
		{"parenthesized assignment before logical", `if ((x = next()) && ready) x = 2;`, 0, ""},
		{"parenthesized assignment after logical", `if (ready && (x = next())) x = 2;`, 0, ""},
		{"parenthesized logical RHS", `if (x = (next() && ready)) x = 2;`, 0, ""},
		{"ternary RHS", `if (x = ready ? 1 : 0) x = 2;`, 0, ""},
		{"compiler owns unary LHS", `if (!x = next() || ready) x = 2;`, 0, ""},
		{"compiler owns logical LHS", `if (ready && x = 0) x = 2;`, 0, ""},
		{"compiler owns chained logical LHS", `if (x = ready && y = next()) x = 2;`, 0, ""},
		{"for init and post", `for (i = 0; i < 5; i = 2) x++;`, 0, ""},
		{"compound assignment", `if (x += 1) x = 2;`, 0, ""},
		{"comparison", `if (x == 1) x = 2;`, 0, ""},
		{"assignment in call argument", `if (check(x = 1)) x = 2;`, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lintMem(t, fn(tt.body), lint.Request{Options: lint.Options{Select: []string{"assignment-in-condition"}}}, nil)
			require.Len(t, got, tt.want, "%v", got)
			if tt.want > 0 {
				assert.Contains(t, got[0], tt.message)
			}
		})
	}
}

// A statement that only exists because the parser resynchronized in the
// middle of a broken construct must not feed other rules.
func TestNoFindingsFromResynchronizedCode(t *testing.T) {
	got := lintMem(t, fn("for (int i = 0; i < n; i++) x();\n ok();\n q == 4;"), lint.Request{}, nil)
	require.Len(t, got, 2, "%v", got)
	assertFinding(t, got, "syntax-error 2:6")
	assertFinding(t, got, "no-effect-statement 4:4")

	// The statement right after a skip is hidden; the one after it counts.
	got = lintMem(t, fn("x = foo(1, 2;\n y == 3;\n q == 4;"), lint.Request{}, nil)
	require.Len(t, got, 2, "%v", got)
	assertFinding(t, got, "syntax-error 2:13")
	assertFinding(t, got, "no-effect-statement 4:4")
}

func TestSuspiciousRules(t *testing.T) {
	got := lintMem(t, fn("x == f();\n x != 1;\n x < 2;\n x = 1;\n f();\n x == 1 ? a() : b();\n a() && b();"),
		lint.Request{Options: lint.Options{Select: []string{"no-effect-statement"}}}, nil)
	assert.Len(t, got, 3, "%v", got)

	got = lintMem(t, fn("sscanf(s, \"%d %s\", a, b, c);\n sscanf(s, \"%d %*s %s\", a, b);\n sscanf(s, \"%d\");\n sscanf(s, fmt, a, b, c);\n ob->sscanf(s, \"\", a);"),
		lint.Request{Options: lint.Options{Select: []string{"sscanf-format"}}}, nil)
	require.Len(t, got, 1, "%v", got)
	assertFinding(t, got, "sscanf-format 2:11")

	got = lintMem(t, "int g(int x) {\n switch (x) {\n case 1: return 1; break;\n case 2: return 2;\n default: break;\n }\n return 0;\n x = 1;\n}\n",
		lint.Request{Options: lint.Options{Select: []string{"unreachable-code"}}}, nil)
	assertFinding(t, got, "unreachable-code 3:20")
	assertFinding(t, got, "unreachable-code 8:2")
	assert.Len(t, got, 2, "%v", got)
}

func TestRuntimeRulesOnTree(t *testing.T) {
	extra := map[string]string{
		"std/paths.h": "#define BASE \"/std/base\"\n#define ME this_object()\n#define GONE \"/std/gone\"\n",
	}
	src := `#include "/std/paths.h"
inherit BASE;
static void create() {
    ::create();
    BASE->query(1);
    BASE->nope();
    BASE->hidden();
    ME->nope2();
    (BASE)->query(1, 2);
    ("/std/" + "base")->nope3();
    GONE->x();
    unknown()->whatever();
    call_other(BASE, "nope4", 1);
    BASE->query(args...);
}
`
	got := lintMem(t, src, lint.Request{}, extra)
	assertFinding(t, got, "callable-not-found 6:11 function 'nope' is not defined in /std/base")
	assertFinding(t, got, "static-crossobj 7:11")
	assertFinding(t, got, "callable-not-found 8:9 function 'nope2'")
	assertFinding(t, got, "callback-arity 9:13")
	assertFinding(t, got, "callable-not-found 10:25 function 'nope3'")
	assertFinding(t, got, "target-object-missing 11:11 object /std/gone does not exist")
	assertFinding(t, got, "callable-not-found 13:22 function 'nope4'")
	assert.Len(t, got, 7, "%v", got)

	// Object paths without a leading slash are resolved from the lib
	// root, as the driver does.
	got = lintMem(t, "inherit \"std/base\";\nstatic void create() {\n ::create();\n \"std/base\"->query(1);\n clone_object(\"std/base\");\n \"obj/nothere\"->x();\n}\n", lint.Request{}, nil)
	require.Len(t, got, 1, "%v", got)
	assertFinding(t, got, "target-object-missing 6:17 object /obj/nothere does not exist")

	// lifecycle-chain sees ::create() through the tree, and stays silent
	// when part of the body could not be parsed.
	got = lintMem(t, "inherit \"/std/base\";\nstatic void create() { x = 1; }\n", lint.Request{Options: lint.Options{Select: []string{"lifecycle-chain"}}}, nil)
	assert.Len(t, got, 1)
	got = lintMem(t, "#define INIT ::create();\ninherit \"/std/base\";\nstatic void create() { INIT }\n", lint.Request{Options: lint.Options{Select: []string{"lifecycle-chain"}}}, nil)
	assert.Empty(t, got)
}

// A callback by name never reaches a private function: DGD leaves
// private functions out of the table call_out and the registrars search.
// Reported only when that is so in the whole chain and in every object
// the code may run as.
func TestPrivateCallback(t *testing.T) {
	files := map[string]string{
		"std/priv.c":    "private void tick() { }\nvoid shown() { }\n",
		"std/pub.c":     "void tick() { }\n",
		"std/stat.c":    "static void tick() { }\n",
		"std/redef.c":   "inherit \"/std/pub\";\nprivate void tick() { }\nvoid f() { call_out(\"tick\", 1); }\n",
		"std/hidden.c":  "private inherit \"/std/pub\";\nvoid f() { call_out(\"tick\", 1); }\n",
		"std/both.c":    "inherit \"/std/pub\";\ninherit \"/std/priv\";\nvoid f() { call_out(\"tick\", 1); }\n",
		"std/base2.c":   "private void tick() { }\nvoid f() { call_out(\"tick\", 1); }\n",
		"obj/leaf2.c":   "inherit \"/std/base2\";\nvoid tick() { }\n",
		"std/base3.c":   "private void tick() { }\nvoid f() { call_out(\"tick\", 1); }\n",
		"obj/leaf3.c":   "inherit \"/std/base3\";\nvoid other() { }\n",
		"std/base4.c":   "private void tick() { }\nvoid f() { call_out(\"tick\", 1); }\n",
		"obj/leaf4.c":   "inherit \"/std/base4\";\ninherit \"/std/nowhere\";\n",
		"std/broken.c":  "void tick() { }\nint lost( { return 2; }\n",
		"std/partial.c": "inherit \"/std/broken\";\nprivate void tick() { }\nvoid f() { call_out(\"tick\", 1); }\n",
		"std/unequal.c": "void tick();\nprivate void tick() { }\nvoid f() { call_out(\"tick\", 1); }\n",
		"std/nomask.c":  "nomask private void tick() { }\nvoid f() { call_out(\"tick\", 1); }\n",
	}
	sel := lint.Request{Options: lint.Options{Select: []string{"private-callback"}}}
	lintAt := func(file string, extra map[string]string) []string {
		t.Helper()
		all := map[string]string{}
		for k, v := range files {
			all[k] = v
		}
		for k, v := range extra {
			all[k] = v
		}
		prj, m := memProject(all)
		prj.Config.Conventions.CallRegistry = map[string]profile.CallForms{"store_fp": {{Function: 0}}}
		req := sel
		req.Paths = []string{"/lib/" + file}
		var out []string
		for _, d := range run(t, prj, m, req).Diagnostics {
			out = append(out, fmt.Sprintf("%d:%d %s", d.Line, d.Col, d.Message))
		}
		return out
	}

	// Its own private function, by call_out and by a registrar of the lib;
	// the -> and call_other forms are static-crossobj's.
	got := lintAt("obj/t.c", map[string]string{"obj/t.c": "private void tick() { }\nvoid f() {\n call_out(\"tick\", 1);\n store_fp(\"tick\");\n this_object()->tick();\n call_other(this_object(), \"tick\");\n}\n"})
	assert.Equal(t, []string{
		"3:11 call_out() never calls 'tick', which is private in /obj/t: DGD calls no private function by name",
		"4:11 store_fp() never calls 'tick', which is private in /obj/t: DGD calls no private function by name",
	}, got)

	// A private function of a parent, and one inherited privately.
	got = lintAt("obj/t.c", map[string]string{"obj/t.c": "inherit \"/std/priv\";\nvoid f() { call_out(\"tick\", 1); call_out(\"shown\", 1); }\n"})
	assert.Equal(t, []string{"2:21 call_out() never calls 'tick', which is private in /std/priv: DGD calls no private function by name"}, got)
	got = lintAt("std/hidden.c", nil)
	assert.Equal(t, []string{"2:21 call_out() never calls 'tick', which is inherited privately from /std/pub: DGD calls no private function by name"}, got)

	// Each with a related location at the definition.
	prj, m := memProject(files)
	res := run(t, prj, m, lint.Request{Paths: []string{"/lib/std/base3.c"}, Options: lint.Options{Select: []string{"private-callback"}}})
	require.Len(t, res.Diagnostics, 1)
	require.Len(t, res.Diagnostics[0].Related, 1)
	assert.Equal(t, 1, res.Diagnostics[0].Related[0].Line)
	assert.Equal(t, "R007", res.Diagnostics[0].Code)

	for name, file := range map[string]string{
		"a static function is reached":                           "obj/t.c",
		"a private redefinition leaves the inherited function":   "std/redef.c",
		"another inherited object has it not private":            "std/both.c",
		"an inheritor has it, and this_object() may be that one": "std/base2.c",
		"an inheritor's chain is not known in full":              "std/base4.c",
		"the file's own chain is not known in full":              "std/partial.c",
		"DGD refuses a prototype that is not private":            "std/unequal.c",
		"DGD refuses private and nomask":                         "std/nomask.c",
	} {
		extra := map[string]string{"obj/t.c": "private void tick() { }\nstatic void tock() { }\nvoid f() { call_out(\"tock\", 1); }\n"}
		assert.Empty(t, lintAt(file, extra), name)
	}
	assert.Len(t, lintAt("std/base3.c", nil), 1, "inheritors that do not have it")

	// A name made at run time, or a receiver that is not this_object().
	src := "private void tick() { }\nvoid f(string s, object ob) {\n call_out(s, 1);\n call_out(\"ti\" + \"ck\", 1);\n store_fp(s);\n ob->tick();\n}\n"
	assert.Empty(t, lintAt("obj/t.c", map[string]string{"obj/t.c": src}))

	// The auto object has a function of the name; code of the auto object.
	got = lintAt("obj/t.c", map[string]string{
		"sys/auto.c": "string object_name() { return \"x\"; }\nvoid tick() { }\n",
		"obj/t.c":    "private void tick() { }\nvoid f() { call_out(\"tick\", 1); }\n",
	})
	assert.Empty(t, got)
	got = lintAt("sys/auto.c", map[string]string{"sys/auto.c": "private void tick() { }\nvoid f() { call_out(\"tick\", 1); }\n"})
	assert.Empty(t, got)

	// A suppression comment, as for every rule.
	got = lintAt("obj/t.c", map[string]string{"obj/t.c": "private void tick() { }\nvoid f() {\n call_out(\"tick\", 1); // dgdtools-ignore lint/private-callback\n}\n"})
	assert.Empty(t, got)
}

// Code that is part of the auto object runs as every object in the lib,
// so a self-call there proves nothing about any one object.
func TestAutoObjectSelfCallsAreUnknown(t *testing.T) {
	prj, m := memProject(map[string]string{
		"sys/auto.c":  "inherit \"/sys/timer\";\n#include \"/sys/part.c\"\nvoid a() { this_object()->reset(); }\n",
		"sys/part.c":  "void p() { this_object()->init(); }\n",
		"sys/timer.c": "void tick() { this_object()->heart_beat(); call_out(\"heart_beat\", 1); }\n",
		"obj/leaf.c":  "void f() { this_object()->nowhere(); }\n",
	})
	res := run(t, prj, m, lint.Request{})
	assert.Equal(t, []string{"callable-not-found leaf.c:1"}, keys(res.Diagnostics))
}

// A parent whose top level did not fully parse has an incomplete symbol
// table; nothing may be concluded from a name missing in it.
func TestPartialParentIsUnknown(t *testing.T) {
	extra := map[string]string{
		"std/broken.c": "int there() { return 1; }\nint lost( { return 2; }\nint after() { return 3; }\n",
		"std/unbal.c":  "int there() { if (1) { return 1; }\nint lost() { return 2; }\n",
	}
	src := "void f() {\n \"/std/broken\"->there();\n \"/std/broken\"->lost();\n \"/std/broken\"->nowhere();\n \"/std/unbal\"->lost();\n \"/std/unbal\"->nowhere();\n}\n"
	got := lintMem(t, src, lint.Request{}, extra)
	assert.Empty(t, got, "broken parents make lookups unknown")

	// The broken files themselves do report their syntax errors.
	prj, m := memProject(extra)
	res := run(t, prj, m, lint.Request{Paths: []string{"/lib/std/unbal.c"}})
	require.NotEmpty(t, res.Diagnostics)
	assert.Equal(t, "syntax-error", res.Diagnostics[0].Rule)
	assert.Contains(t, res.Diagnostics[0].Message, "function 'lost' begins inside the body of 'there'")
	require.NotEmpty(t, res.Diagnostics[0].Related)
}

// ------------------------------------------- degraded and zero-config

func TestFileLocalModeWithoutRoot(t *testing.T) {
	m := vfs.Mem{"/any/where/castle.c": "#include <std.h>\ninherit ROOM;\nmapping c;\nvoid create() {\n c[\"a\"] = ({ IW + \"x\" })\n c[\"b\"] = 1;\n NOPE->gone();\n \"/std/gone\"->x();\n}\n"}
	prj, err := config.Discover(m, []string{"/any/where/castle.c"}, config.DiscoverOptions{})
	require.NoError(t, err)
	assert.Empty(t, prj.Root)
	res := run(t, prj, m, lint.Request{Paths: []string{"/any/where/castle.c"}})
	require.Len(t, res.Diagnostics, 1, "%v", res.Diagnostics)
	assert.Equal(t, "syntax-error", res.Diagnostics[0].Rule)
	assert.Equal(t, 5, res.Diagnostics[0].Line)
	require.NotEmpty(t, res.Notes)
	assert.Equal(t, diag.NoteNoLib, res.Notes[0].Kind)
	assert.Contains(t, res.Notes[0].Message, "file-local rules only")
}

func TestIncompleteLibDegradesToUnknown(t *testing.T) {
	// Only one domain is checked out: /include, the force-include and the
	// auto object are absent.
	m := vfs.Mem{"/lib/d/Foo/room.c": "#include <std.h>\n#include \"local.h\"\ninherit \"/std/room\";\nvoid create() {\n ::create();\n \"/std/thing\"->x();\n clone_object(\"/obj/y\");\n a = 1\n b = 2;\n}\n"}
	cfg := &config.Config{}
	cfg.Root = "/lib"
	cfg.IncludeDirs = []string{"/include"}
	cfg.IncludeFile = "/sys/forced.h"
	cfg.AutoObjects = []string{"/sys/auto"}
	prj := &config.Project{Config: cfg, Root: "/lib"}
	res := run(t, prj, m, lint.Request{})
	got := keys(res.Diagnostics)
	assert.Equal(t, []string{"syntax-error room.c:8"}, got, "only the provable finding survives")
	require.NotEmpty(t, res.Notes)
	assert.Contains(t, noteKinds(res.Notes), diag.NoteIncompleteLib)
	assert.Greater(t, res.Stats.UnknownIncludes, 0)
}

func TestFileOutsideRoot(t *testing.T) {
	prj, m := memProject(nil)
	m["/elsewhere/x.c"] = "inherit \"/std/nothere\";\nvoid f() { \"/std/gone\"->x(); y = 1 z = 2; }\n"
	res := run(t, prj, m, lint.Request{Paths: []string{"/elsewhere/x.c"}})
	assert.Equal(t, []string{"syntax-error x.c:2"}, keys(res.Diagnostics))
	assert.Contains(t, noteKinds(res.Notes), diag.NoteOutsideLib)
}

func TestExcludesAndExplicitFiles(t *testing.T) {
	prj, m := memProject(map[string]string{
		"attic/old.c": "void f() { x = 1 y = 2; }\n",
		"obj/ok.c":    "void f() { }\n",
	})
	prj.Config.Exclude = []string{"attic/**"}
	res := run(t, prj, m, lint.Request{})
	assert.Empty(t, res.Diagnostics, "excluded directories are not walked")
	res = run(t, prj, m, lint.Request{Paths: []string{"/lib/attic/old.c"}})
	assert.Len(t, res.Diagnostics, 1, "a file named explicitly is linted")

	l, err := lint.New(prj, m)
	require.NoError(t, err)
	_, err = l.Run(context.Background(), lint.Request{Paths: []string{"/lib/nope.c"}})
	assert.ErrorContains(t, err, "no such file")
}

// ------------------------------------------------- library behaviours

func TestOverlayAndInvalidate(t *testing.T) {
	prj, m := memProject(map[string]string{
		"std/lib.c": "int helper() { return 1; }\n",
		"obj/a.c":   "inherit \"/std/lib\";\nvoid f() { this_object()->helper(); this_object()->extra(); }\n",
		"obj/b.c":   "void g() { }\n",
	})
	ov := vfs.NewOverlay(m)
	l, err := lint.New(prj, ov)
	require.NoError(t, err)
	ctx := context.Background()

	res, err := l.Run(ctx, lint.Request{Paths: []string{"/lib/obj/a.c"}})
	require.NoError(t, err)
	require.Len(t, res.Diagnostics, 1)
	assert.Contains(t, res.Diagnostics[0].Message, "'extra'")

	// An unsaved edit to the parent adds extra() and removes helper().
	ov.Set("/lib/std/lib.c", []byte("int extra() { return 2; }\n"))
	dropped := l.Invalidate("/lib/std/lib.c")
	assert.Contains(t, dropped, "/lib/std/lib.c")
	res, err = l.Run(ctx, lint.Request{Paths: []string{"/lib/obj/a.c"}})
	require.NoError(t, err)
	require.Len(t, res.Diagnostics, 1)
	assert.Contains(t, res.Diagnostics[0].Message, "'helper'")

	// An unsaved syntax error in the file itself.
	ov.Set("/lib/obj/a.c", []byte("void f() { x = 1 y = 2; }\n"))
	l.Invalidate("/lib/obj/a.c")
	res, err = l.Run(ctx, lint.Request{Paths: []string{"/lib/obj/a.c"}})
	require.NoError(t, err)
	require.Len(t, res.Diagnostics, 1)
	assert.Equal(t, "syntax-error", res.Diagnostics[0].Rule)

	// Dropping the buffer restores the disk view.
	ov.Delete("/lib/obj/a.c")
	ov.Delete("/lib/std/lib.c")
	l.Invalidate("/lib/obj/a.c", "/lib/std/lib.c")
	res, err = l.Run(ctx, lint.Request{Paths: []string{"/lib/obj/a.c"}})
	require.NoError(t, err)
	require.Len(t, res.Diagnostics, 1)
	assert.Contains(t, res.Diagnostics[0].Message, "'extra'")
}

func TestInvalidateFollowsIncludes(t *testing.T) {
	prj, m := memProject(map[string]string{
		"obj/defs.h": "#define TARGET \"/std/base\"\n",
		"obj/a.c":    "#include \"defs.h\"\nvoid f() { TARGET->query(1); }\n",
	})
	ov := vfs.NewOverlay(m)
	l, err := lint.New(prj, ov)
	require.NoError(t, err)
	res, err := l.Run(context.Background(), lint.Request{Paths: []string{"/lib/obj/a.c"}})
	require.NoError(t, err)
	assert.Empty(t, res.Diagnostics)

	ov.Set("/lib/obj/defs.h", []byte("#define TARGET \"/std/gone\"\n"))
	dropped := l.Invalidate("/lib/obj/defs.h")
	assert.Contains(t, dropped, "/lib/obj/a.c", "a.c was built from defs.h")
	res, err = l.Run(context.Background(), lint.Request{Paths: []string{"/lib/obj/a.c"}})
	require.NoError(t, err)
	require.Len(t, res.Diagnostics, 1)
	assert.Equal(t, "target-object-missing", res.Diagnostics[0].Rule)
}

func TestCancellation(t *testing.T) {
	prj, m := memProject(map[string]string{"obj/a.c": "void f() { }\n"})
	l, err := lint.New(prj, m)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = l.Run(ctx, lint.Request{})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestBaseline(t *testing.T) {
	src := "void f() {\n a = 1\n b = 2;\n}\n"
	prj, m := memProject(map[string]string{"obj/a.c": src})
	m["/lib/dgdtools.yml"] = ""
	prj.Config.Path, prj.Config.Dir = "/lib/dgdtools.yml", "/lib"
	l, err := lint.New(prj, m)
	require.NoError(t, err)
	ctx := context.Background()
	res, err := l.Run(ctx, lint.Request{})
	require.NoError(t, err)
	require.Len(t, res.Diagnostics, 1)

	data, err := lint.NewBaseline(l.BaselinePath(), res.Diagnostics).Marshal()
	require.NoError(t, err)
	assert.Contains(t, string(data), `"path": "obj/a.c"`)
	m[l.BaselinePath()] = string(data)

	// A linter reads its baseline once: a new baseline is a new linter.
	l, err = lint.New(prj, m)
	require.NoError(t, err)
	res, err = l.Run(ctx, lint.Request{})
	require.NoError(t, err)
	assert.Empty(t, res.Diagnostics)
	assert.Equal(t, 1, res.Stats.Baselined)

	// Edits above the finding do not resurrect it; a new finding shows.
	m["/lib/obj/a.c"] = "/* a comment */\n\nvoid f() {\n a = 1\n b = 2;\n c = 3\n}\n"
	l.Invalidate("/lib/obj/a.c")
	res, err = l.Run(ctx, lint.Request{})
	require.NoError(t, err)
	require.Len(t, res.Diagnostics, 1)
	assert.Equal(t, 6, res.Diagnostics[0].Line)

	res, err = l.Run(ctx, lint.Request{Options: lint.Options{NoBaseline: true}})
	require.NoError(t, err)
	assert.Len(t, res.Diagnostics, 2)
	for _, d := range res.Diagnostics {
		assert.NotEmpty(t, d.Fingerprint)
	}

	// The baseline applies to a single document as it does to a run.
	doc, err := l.Lint(ctx, "/lib/obj/a.c", lint.Options{})
	require.NoError(t, err)
	require.Len(t, doc.Diagnostics, 1)
	assert.Equal(t, 6, doc.Diagnostics[0].Line)

	m[l.BaselinePath()] = "not json"
	l, err = lint.New(prj, m)
	require.NoError(t, err)
	_, err = l.Run(ctx, lint.Request{})
	assert.ErrorContains(t, err, "dgdtools-baseline.json")
	_, err = l.Run(ctx, lint.Request{Options: lint.Options{NoBaseline: true}})
	assert.NoError(t, err, "a broken baseline does not stop a run that ignores it")

	// A run may name another baseline file; the project's is not read.
	m["/elsewhere/known.json"] = strings.Replace(string(data), `"path": "obj/a.c"`, `"path": "../lib/obj/a.c"`, 1)
	res, err = l.Run(ctx, lint.Request{Options: lint.Options{Baseline: "/elsewhere/known.json"}})
	require.NoError(t, err)
	require.Len(t, res.Diagnostics, 1, "paths in a baseline are relative to the baseline")
	assert.Equal(t, 6, res.Diagnostics[0].Line)
	res, err = l.Run(ctx, lint.Request{Options: lint.Options{Baseline: "/elsewhere/none.json"}})
	require.NoError(t, err)
	assert.Len(t, res.Diagnostics, 2, "no such file: nothing is known")
	other, err := lint.NewBaseline("/elsewhere/known.json", res.Diagnostics).Marshal()
	require.NoError(t, err)
	assert.Contains(t, string(other), `"path": "../lib/obj/a.c"`)
	assert.Empty(t, prj.Config.Lint.Baseline, "the project's configuration is not written to")
}

func TestRuleTableIsStable(t *testing.T) {
	codes, names := map[string]bool{}, map[string]bool{}
	for _, r := range lint.Rules() {
		assert.False(t, codes[r.Code], "duplicate code %s", r.Code)
		assert.False(t, names[r.Name], "duplicate name %s", r.Name)
		codes[r.Code], names[r.Name] = true, true
		assert.NotEmpty(t, r.Doc, r.Name)
		assert.NotEmpty(t, r.Explain, r.Name)
		assert.Regexp(t, `^[ERSC]\d{3}$`, r.Code)
		for _, o := range r.OptionList() {
			assert.Regexp(t, `^[a-z_]+$`, o.Key, r.Name)
			assert.NotEmpty(t, o.Doc, "%s: %s", r.Name, o.Key)
			assert.NotContains(t, o.Takes, ".", "%s: %s takes a type the configuration can spell", r.Name, o.Key)
		}
	}
	// Names that suppression comments in existing libs rely on.
	for _, n := range []string{"callable-not-found", "static-crossobj", "undefined-prototype",
		"target-object-missing", "callback-arity", "include-not-found", "assignment-in-condition",
		"no-effect-statement", "sscanf-format", "lifecycle-chain", "static-autosave-var",
		"unresolved-inherit", "raw-inherit-path", "missing-visibility", "unformatted"} {
		assert.True(t, names[n], n)
	}
	// Mutating a returned table does not affect the next one.
	lint.Rules()[0].Default = false
	assert.True(t, lint.Rules()[0].Default)
}

// Nothing about one lib is built in: what marks a saving object, what is
// a specifier macro and which function the driver calls in a new object
// come from the configuration.
func TestConventionsComeFromTheConfiguration(t *testing.T) {
	files := map[string]string{
		"std/base.c":  "void create() { }\nvoid setup() { }\n",
		"obj/save.c":  "static int n;\nvoid f() { set_auto_save(1); }\n",
		"obj/spec.c":  "visible int g() { return 1; }\n",
		"obj/child.c": "inherit \"/std/base\";\nvoid create() { }\nvoid setup() { }\n",
	}
	find := func(prj *config.Project, m vfs.Mem, file string) []string {
		return keys(run(t, prj, m, lint.Request{
			Paths:   []string{"/lib/obj/" + file},
			Options: lint.Options{Enable: []string{"static-autosave-var"}},
		}).Diagnostics)
	}

	prj, m := memProject(files)
	assert.Empty(t, find(prj, m, "save.c"), "no markers: no object is known to save itself")
	assert.Equal(t, []string{"syntax-error spec.c:1"}, find(prj, m, "spec.c"))
	assert.Equal(t, []string{"lifecycle-chain child.c:2"}, find(prj, m, "child.c"), "create, by custom, when the driver's is not known")

	prj, m = memProject(files)
	prj.Config.Conventions.AutosaveMarkers = []string{"set_auto_save"}
	prj.Config.Conventions.Macros = map[string]string{"visible": "specifier"}
	prj.Config.Conventions.Create = "setup"
	assert.Equal(t, []string{"static-autosave-var save.c:1"}, find(prj, m, "save.c"))
	assert.Empty(t, find(prj, m, "spec.c"))
	assert.Equal(t, []string{"lifecycle-chain child.c:3"}, find(prj, m, "child.c"), "conventions.create")

	prj.Config.Lint.Rules = map[string]config.RuleSettings{"lifecycle-chain": {Options: map[string]any{"names": []string{"create", "setup"}}}}
	assert.Equal(t, []string{"lifecycle-chain child.c:2", "lifecycle-chain child.c:3"}, find(prj, m, "child.c"), "the rule's own list wins")
}

// The example configurations name only rules and options that exist.
func TestExampleConfigsAreValid(t *testing.T) {
	for _, file := range []string{"../../dgdtools.example.yml", "../../examples/viking/dgdtools.yml"} {
		path, err := filepath.Abs(file)
		require.NoError(t, err)
		cfg, err := config.Load(vfs.OS{}, path)
		require.NoError(t, err, file)
		_, err = lint.New(&config.Project{Config: cfg}, vfs.Mem{})
		assert.NoError(t, err, file)
	}
}

// A private function called on this_object() by -> or call_other is
// reported as private-callback has it: not when it redefines an
// inherited function DGD keeps, nor when an inheritor has it.
func TestStaticCrossObjOnThisObject(t *testing.T) {
	files := map[string]string{
		"std/pub.c":   "void tick() { }\n",
		"std/redef.c": "inherit \"/std/pub\";\nprivate void tick() { }\nvoid f() { this_object()->tick(); call_other(this_object(), \"tick\"); }\n",
		"std/base.c":  "private void tick() { }\nvoid f() { this_object()->tick(); }\n",
		"obj/leaf.c":  "inherit \"/std/base\";\nvoid tick() { }\n",
		"std/alone.c": "private void tick() { }\nvoid f() { this_object()->tick(); call_other(this_object(), \"tick\"); }\n",
	}
	prj, m := memProject(files)
	lintAt := func(file string) []string {
		var out []string
		req := lint.Request{Paths: []string{"/lib/" + file}, Options: lint.Options{Select: []string{"static-crossobj"}}}
		for _, d := range run(t, prj, m, req).Diagnostics {
			out = append(out, fmt.Sprintf("%d:%d %s", d.Line, d.Col, d.Message))
		}
		return out
	}
	assert.Empty(t, lintAt("std/redef.c"), "the inherited public function is what is called")
	assert.Empty(t, lintAt("std/base.c"), "this_object() may be /obj/leaf, which has it")
	assert.Equal(t, []string{
		"2:27 'tick' in /std/alone is private — call_other cannot reach it and returns nil",
		"2:61 'tick' in /std/alone is private — call_other cannot reach it and returns nil",
	}, lintAt("std/alone.c"))
}
