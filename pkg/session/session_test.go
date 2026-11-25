package session_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/session"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

var bg = context.Background()

// lib is a small lib with every kind of edge between its files: a
// force-included header, an auto object, includes by both forms, inherits
// through a macro, calls by path, callbacks its inheritors provide.
func lib() vfs.Mem {
	return vfs.Mem{
		"/lib/dgdtools.yml":   "root: .\ndriver_config: mud.dgd\n",
		"/lib/mud.dgd":        "include_dirs = ({ \"/include\" });\ninclude_file = \"/include/auto.h\";\nauto_object = \"/sys/auto\";\n",
		"/lib/include/auto.h": "#define AUTO 1\n",
		"/lib/include/std.h":  "#define STD \"/std/base\"\n",
		"/lib/sys/auto.c":     "void autof() {}\n",
		"/lib/std/base.c":     "void create() {}\nint foo(int a) { return a; }\nstatic void sfn() {}\n",
		"/lib/std/mid.c":      "inherit \"/std/base\";\nvoid create() { ::create(); }\nvoid bar() {}\n",
		"/lib/std/mixin.c":    "void go() { this_object()->provided(); call_out(\"later\", 1); }\n",
		"/lib/obj/a.c":        "#include <std.h>\ninherit STD;\nvoid create() { ::create(); \"/obj/b\"->bar(); \"/obj/b\"->nofn(); this_object()->foo(1,2); call_out(\"cb\", 1); autof(); }\n",
		"/lib/obj/b.c":        "inherit \"/std/mid\";\nvoid create() { }\n",
		"/lib/obj/c.c":        "#include \"local.h\"\nvoid x() { \"/obj/missing\"->f(); LOCAL->bar(); \"/obj/b\"->sfn(); \"/sys/auto\"->autof(1); }\n",
		"/lib/obj/local.h":    "#define LOCAL \"/obj/b\"\n",
		"/lib/obj/d.c":        "inherit \"/std/mixin\";\nvoid provided() {}\n",
	}
}

// findings lints path as a server does and renders what it found.
func findings(t testing.TB, s *session.Session, path string) string {
	t.Helper()
	doc, err := s.Lint(bg, path, lint.Options{})
	if err != nil {
		return "error: " + err.Error()
	}
	require.True(t, s.Current(doc))
	var b strings.Builder
	for _, d := range doc.Diagnostics {
		fmt.Fprintln(&b, d.String())
	}
	return b.String()
}

// A change to a buffer is one step: no lint sees the new text with what
// the index knew of the old.
func TestChangeIsOneStep(t *testing.T) {
	const path = "/lib/obj/b.c"
	texts := []string{
		"inherit \"/std/nope\";\nvoid create() { }\n",
		"\n\n\n\ninherit \"/std/mid\";\nvoid create() { ::create(); }\n",
	}
	s := session.New(lib(), config.DiscoverOptions{})
	var want []string
	for _, text := range texts {
		assert.Contains(t, s.Change(path, []byte(text)).Files, path)
		want = append(want, findings(t, s, path))
	}
	require.Contains(t, want[0], "/lib/obj/b.c:1:9: error: inherited object /std/nope does not exist")
	require.Empty(t, want[1])

	doc, err := s.Lint(bg, path, lint.Options{})
	require.NoError(t, err)
	assert.Equal(t, texts[1], string(doc.File.Src))
	s.Change(path, []byte(texts[0]))
	assert.False(t, s.Current(doc), "a change overtook it")

	// Lints beside changes: each is of one text, start to end.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				doc, err := s.Lint(bg, path, lint.Options{})
				var fe *lint.FileError
				if errors.As(err, &fe) {
					assert.Equal(t, lint.Stale, fe.Kind, "%v", err)
					continue
				}
				if !assert.NoError(t, err) {
					return
				}
				var b strings.Builder
				for _, d := range doc.Diagnostics {
					fmt.Fprintln(&b, d.String())
				}
				i := slices.Index(texts, string(doc.File.Src))
				if assert.GreaterOrEqual(t, i, 0) {
					assert.Equal(t, want[i], b.String(), "the findings of text %d", i)
				}
			}
		}()
	}
	for i := range 300 {
		s.Change(path, []byte(texts[i%2]))
	}
	close(stop)
	wg.Wait()

	// Closed, the file on disk is what there is.
	assert.Contains(t, s.Change(path, nil).Files, path)
	assert.Contains(t, findings(t, s, path), "/lib/obj/b.c:2:6: warning: create() overrides inherited /std/mid::create()")
}

// A change to what a project is configured by replaces the project and
// its linter, and only those; a baseline and an .editorconfig are read
// again by the linter there is.
func TestConfigurationChanges(t *testing.T) {
	m := lib()
	m["/lib/obj/plain.c"] = "int f() { return 1; }\nvoid g() { x == 1; }\n"
	m["/other/dgdtools.yml"] = "lint:\n  rules: {missing-visibility: warning}\n"
	m["/other/o.c"] = "int f() { return 1; }\n"
	s := session.New(m, config.DiscoverOptions{})
	const plain, other = "/lib/obj/plain.c", "/other/o.c"
	require.Contains(t, findings(t, s, plain), "no-effect-statement")
	require.NotContains(t, findings(t, s, plain), "missing-visibility")
	require.Contains(t, findings(t, s, other), "warning: function 'f' has no visibility specifier")
	libLinter, _ := s.Linter(plain)
	otherLinter, _ := s.Linter(other)
	require.True(t, libLinter.Scan(bg))
	doc, err := s.Lint(bg, plain, lint.Options{})
	require.NoError(t, err)

	// The root config.
	m["/lib/dgdtools.yml"] += "lint:\n  rules: {missing-visibility: error}\n"
	a := s.DiskChanged("/lib/dgdtools.yml")
	assert.True(t, a.All)
	assert.False(t, s.Current(doc))
	assert.Contains(t, findings(t, s, plain), "error: function 'f' has no visibility specifier")
	l, _ := s.Linter(plain)
	assert.NotSame(t, libLinter, l)
	o, _ := s.Linter(other)
	assert.Same(t, otherLinter, o, "another project keeps its linter and what it has indexed")
	libLinter = l

	// A settings scope appears; then it is a root config, and the
	// directory a project of its own.
	m["/lib/obj/dgdtools.yml"] = "root: false\nlint:\n  rules: {missing-visibility: info}\n"
	assert.True(t, s.DiskChanged("/lib/obj/dgdtools.yml").All)
	assert.Contains(t, findings(t, s, plain), "info: function 'f' has no visibility specifier")
	m["/lib/obj/dgdtools.yml"] = "root: .\nlint:\n  rules: {missing-visibility: warning}\n"
	assert.True(t, s.DiskChanged("/lib/obj/dgdtools.yml").All)
	assert.Contains(t, findings(t, s, plain), "warning: function 'f' has no visibility specifier")
	prj, err := s.Project(plain)
	require.NoError(t, err)
	assert.Equal(t, "/lib/obj/dgdtools.yml", prj.ConfigPath)
	delete(m, "/lib/obj/dgdtools.yml")
	assert.True(t, s.DiskChanged("/lib/obj/dgdtools.yml").All)
	assert.Contains(t, findings(t, s, plain), "error: function 'f' has no visibility specifier")

	// A baseline: written, then removed.
	libLinter, _ = s.Linter(plain)
	doc, err = s.Lint(bg, plain, lint.Options{})
	require.NoError(t, err)
	data, err := lint.NewBaseline(libLinter.BaselinePath(), doc.Diagnostics).Marshal()
	require.NoError(t, err)
	m[libLinter.BaselinePath()] = string(data)
	a = s.DiskChanged(libLinter.BaselinePath())
	assert.True(t, a.All)
	assert.Empty(t, findings(t, s, plain))
	l, _ = s.Linter(plain)
	assert.Same(t, libLinter, l, "the linter stays")
	delete(m, libLinter.BaselinePath())
	assert.True(t, s.DiskChanged(libLinter.BaselinePath()).All)
	assert.NotEmpty(t, findings(t, s, plain))

	// A configuration that is not valid is the error of the files it is
	// for, until it is mended.
	m["/other/dgdtools.yml"] = "lint:\n  rules: {no-such-rule: off}\n"
	s.DiskChanged("/other/dgdtools.yml")
	assert.Contains(t, findings(t, s, other), `error: /other/dgdtools.yml: line 2: lint.rules: unknown rule "no-such-rule"`)
	assert.NotContains(t, findings(t, s, plain), "error: /")
	m["/other/dgdtools.yml"] = ""
	s.DiskChanged("/other/dgdtools.yml")
	assert.Empty(t, findings(t, s, other))

	// A source file is nobody's configuration.
	assert.False(t, s.Change(plain, []byte("void h() { }\n")).All)
}

// A directory is deleted and the watcher says so, with the directory.
func TestDirectoryChanges(t *testing.T) {
	m := lib()
	s := session.New(m, config.DiscoverOptions{})
	const b = "/lib/obj/b.c"
	require.Contains(t, findings(t, s, b), "overrides inherited /std/mid::create()")
	for p := range m {
		if strings.HasPrefix(p, "/lib/std/") {
			delete(m, p)
		}
	}
	assert.Contains(t, s.DiskChanged("/lib/std").Files, b)
	got := findings(t, s, b)
	assert.Equal(t, findings(t, session.New(maps.Clone(m), config.DiscoverOptions{}), b), got)
	assert.Contains(t, got, "inherited object /std/mid does not exist")
}

var seeds = flag.Int("seeds", 30, "the number of random edit sequences TestDifferential runs")

// variants are the texts each file takes in TestDifferential; "" is no
// file.
var variants = map[string][]string{
	"/lib/include/auto.h": {"#define AUTO 1\n", "#define AUTO 1\n#define STD \"/std/mid\"\n", "#include <std.h>\n", ""},
	"/lib/include/std.h":  {"#define STD \"/std/base\"\n", "#define STD \"/std/mid\"\n", "#define STD \"/std/nope\"\n", "", "#define STD \"/std/base\"\nint decl;\n"},
	"/lib/sys/auto.c":     {"void autof() {}\n", "void autof(int a) {}\nvoid cb() {}\n", "inherit \"/std/base\";\nvoid autof() {}\n", ""},
	"/lib/std/base.c":     {"void create() {}\nint foo(int a) { return a; }\nstatic void sfn() {}\n", "int foo(int a, int b) { return a; }\nvoid sfn() {}\n", "void create() {}\nvoid cb() {}\nvoid later() {}\n", "", "void create() {\n"},
	"/lib/std/mid.c":      {"inherit \"/std/base\";\nvoid create() { ::create(); }\nvoid bar() {}\n", "inherit \"/std/base\";\nvoid create() { }\nprivate void bar() {}\n", "void nofn() {}\n", ""},
	"/lib/std/mixin.c":    {"void go() { this_object()->provided(); call_out(\"later\", 1); }\n", "void go() { this_object()->provided(); }\nvoid later();\nvoid z() { later(); }\n"},
	"/lib/std/nope.c":     {"", "void create() {}\n"},
	"/lib/obj/a.c":        {"#include <std.h>\ninherit STD;\nvoid create() { ::create(); \"/obj/b\"->bar(); \"/obj/b\"->nofn(); this_object()->foo(1,2); call_out(\"cb\", 1); autof(); }\n", "inherit \"/std/mid\";\nvoid create() { }\n", ""},
	"/lib/obj/b.c":        {"inherit \"/std/mid\";\nvoid create() { }\n", "inherit \"/std/base\";\nvoid nofn() {}\nvoid bar(int x) {}\n", "", "inherit \"/obj/d\";\nvoid bar() {}\n"},
	"/lib/obj/c.c":        {"#include \"local.h\"\nvoid x() { \"/obj/missing\"->f(); LOCAL->bar(); \"/obj/b\"->sfn(); \"/sys/auto\"->autof(1); }\n", "#include \"local.h\"\n#include \"other.h\"\ninherit LOCAL;\nvoid x() { OTHER->bar(); }\n"},
	"/lib/obj/local.h":    {"#define LOCAL \"/obj/b\"\n", "#define LOCAL \"/std/mid\"\n", ""},
	"/lib/obj/other.h":    {"", "#define OTHER \"/obj/d\"\n", "#define OTHER \"/obj/b\"\nvoid proto();\n"},
	"/lib/obj/missing.c":  {"", "void f() {}\n", "static void f() {}\n"},
	"/lib/obj/d.c":        {"inherit \"/std/mixin\";\nvoid provided() {}\n", "inherit \"/std/mixin\";\nvoid later() {}\n", "", "void provided() {}\n"},
	"/lib/obj/e.c":        {"", "inherit \"/std/mixin\";\nvoid provided() {}\nvoid later() {}\n"},
}

// TestDifferential holds a session that is told of every change to what
// a session made afresh finds: after each of a random sequence of edits,
// creations and deletions of files and of a whole directory, the
// diagnostics a front end would be showing (those it had, with the files
// the change is said to affect linted again) are those of a new session
// over the same files. -seeds N runs more sequences.
func TestDifferential(t *testing.T) {
	names := slices.Sorted(maps.Keys(variants))
	sources := func(m vfs.Mem) []string {
		var out []string
		for _, p := range slices.Sorted(maps.Keys(m)) {
			if strings.HasSuffix(p, ".c") || strings.HasSuffix(p, ".h") {
				out = append(out, p)
			}
		}
		return out
	}
	for seed := range int64(*seeds) {
		rng := rand.New(rand.NewSource(seed))
		m := lib()
		s := session.New(m, config.DiscoverOptions{})
		shown := map[string]string{}
		for _, p := range sources(m) {
			shown[p] = findings(t, s, p)
		}
		var log []string
		for step := range 25 {
			var changed []string
			if rng.Intn(8) == 0 {
				// A directory goes, or comes back as it first was.
				dir := []string{"/lib/std", "/lib/include", "/lib/sys"}[rng.Intn(3)]
				gone := true
				for p, text := range lib() {
					if strings.HasPrefix(p, dir+"/") {
						if _, there := m[p]; there {
							delete(m, p)
						} else {
							m[p], gone = text, false
						}
					}
				}
				for p := range m {
					if strings.HasPrefix(p, dir+"/") && gone {
						delete(m, p)
					}
				}
				changed = []string{dir}
				log = append(log, fmt.Sprintf("%s gone=%v", dir, gone))
			} else {
				for range 1 + rng.Intn(2) {
					p := names[rng.Intn(len(names))]
					text := variants[p][rng.Intn(len(variants[p]))]
					if text == "" {
						delete(m, p)
					} else {
						m[p] = text
					}
					changed = append(changed, p)
					log = append(log, fmt.Sprintf("%s = %q", p, text))
				}
			}
			affected := s.DiskChanged(changed...)
			require.False(t, affected.All)
			for _, p := range affected.Files {
				delete(shown, p)
				if _, is := m[p]; is {
					shown[p] = findings(t, s, p)
				}
			}
			fresh := session.New(maps.Clone(m), config.DiscoverOptions{})
			for _, p := range sources(m) {
				got, linted := shown[p]
				if !linted {
					got = "(new, and not said to be affected)"
				}
				if want := findings(t, fresh, p); got != want {
					t.Fatalf("seed %d step %d: %s after %v (affected %v)\nshown:\n%sa new session finds:\n%sedits:\n  %s",
						seed, step, p, changed, affected.Files, got, want, strings.Join(log, "\n  "))
				}
			}
		}
	}
}
