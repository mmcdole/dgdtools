package lint_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// A file is linted by the rules of its own place in the tree: the
// project's, as adjusted by each nested dgdtools.yml above it.
func TestSettingsScopes(t *testing.T) {
	const bare = "int f() { return 1; }\n" // missing-visibility
	m := vfs.Mem{
		"/mud/dgdtools.yml": "root: lib\nlint:\n  rules: {missing-visibility: warning}\n" +
			"  overrides:\n    - paths: [\"players/ann/**\"]\n      rules: {missing-visibility: info}\n",
		"/mud/lib/std/a.c":              bare,
		"/mud/lib/players/dgdtools.yml": "root: false\nlint:\n  rules: {missing-visibility: error}\n",
		"/mud/lib/players/ann/b.c":      bare,
		"/mud/lib/players/bob/dgdtools.yml": "root: false\nlint:\n  rules: {convention: off}\n" +
			"  overrides:\n    - paths: [\"keep/**\"]\n      rules: {missing-visibility: info}\n",
		"/mud/lib/players/bob/c.c":      bare,
		"/mud/lib/players/bob/keep/d.c": bare,
		// A wizard's own project, met inside the whole lib.
		"/mud/lib/players/cyd/dgdtools.yml": "profile: nowhere\nlint:\n  rules: {missing-visibility: info}\n",
		"/mud/lib/players/cyd/e.c":          bare,
	}
	prj, err := config.Discover(m, []string{"/mud/lib"}, config.DiscoverOptions{})
	require.NoError(t, err)
	l, err := lint.New(prj, m)
	require.NoError(t, err)
	findings := func(opt lint.Options) []string {
		t.Helper()
		res, err := l.Run(context.Background(), lint.Request{Options: opt})
		require.NoError(t, err)
		// The project below is passed over, and the run says so.
		require.Len(t, res.Notes, 2)
		assert.Equal(t, diag.NoteDriverUnset, res.Notes[0].Kind)
		assert.Equal(t, diag.Note{
			Kind: diag.NoteNestedProject, Path: "/mud/lib/players/cyd/dgdtools.yml",
			Message: "1 file under /mud/lib/players/cyd is a project of their own (dgdtools.yml) and was not linted: lint that directory by itself",
		}, res.Notes[1])
		var out []string
		for _, d := range res.Diagnostics {
			out = append(out, fmt.Sprintf("%s %s %s", strings.TrimPrefix(d.Path, "/mud/lib/"), d.Rule, d.Severity))
		}
		sort.Strings(out)
		return out
	}
	assert.Equal(t, []string{
		"players/ann/b.c missing-visibility error",     // the scope, after the project's override
		"players/bob/keep/d.c missing-visibility info", // the nearer scope's override, relative to it
		"std/a.c missing-visibility warning",           // the project's own
	}, findings(lint.Options{}))

	// The command line still has the last word, and the first.
	assert.Empty(t, findings(lint.Options{Disable: []string{"missing-visibility"}}))
	assert.Equal(t, []string{
		"players/ann/b.c missing-visibility error",
		"players/bob/keep/d.c missing-visibility info",
		"std/a.c missing-visibility warning",
	}, findings(lint.Options{Select: []string{"missing-visibility"}}))

	// One document, and the question "what runs here".
	doc, err := l.Lint(context.Background(), "/mud/lib/players/ann/b.c", lint.Options{})
	require.NoError(t, err)
	require.Len(t, doc.Diagnostics, 1)
	assert.Equal(t, "error", doc.Diagnostics[0].Severity.String())
	severity := func(path string) string {
		rules, err := l.EffectiveRules(path)
		require.NoError(t, err)
		for _, r := range rules {
			if r.Name == "missing-visibility" {
				return r.Severity.String()
			}
		}
		return "off"
	}
	assert.Equal(t, "warning", severity(""))
	assert.Equal(t, "warning", severity("/mud/lib/std/a.c"))
	assert.Equal(t, "error", severity("/mud/lib/players/ann/b.c"))
	assert.Equal(t, "off", severity("/mud/lib/players/bob/c.c"))
	assert.Equal(t, "info", severity("/mud/lib/players/bob/keep/d.c"))
	_, err = l.EffectiveRules("/mud/lib/players/cyd/e.c")
	assert.EqualError(t, err, "/mud/lib/players/cyd/e.c belongs to the project of /mud/lib/players/cyd/dgdtools.yml")
	_, err = l.Lint(context.Background(), "/mud/lib/players/cyd/e.c", lint.Options{})
	assert.ErrorContains(t, err, "belongs to the project of")

	// A scope that names a rule that does not exist, or is not a valid
	// file, fails the run instead of linting its files by other rules.
	again := func() {
		t.Helper()
		prj, err = config.Discover(m, []string{"/mud/lib"}, config.DiscoverOptions{})
		require.NoError(t, err)
		l, err = lint.New(prj, m)
		require.NoError(t, err)
	}
	m["/mud/lib/players/bob/dgdtools.yml"] = "root: false\nlint:\n  rules: {no-such-rule: off}\n"
	again()
	_, err = l.Run(context.Background(), lint.Request{})
	assert.ErrorContains(t, err, "/mud/lib/players/bob/dgdtools.yml: line 3: lint.rules: unknown rule \"no-such-rule\"")
	_, err = l.Lint(context.Background(), "/mud/lib/players/bob/c.c", lint.Options{})
	assert.ErrorContains(t, err, "unknown rule")
	_, err = l.Lint(context.Background(), "/mud/lib/std/a.c", lint.Options{})
	assert.NoError(t, err)
	m["/mud/lib/players/bob/dgdtools.yml"] = "root: false\nexclude: [\"x\"]\n"
	again()
	_, err = l.Run(context.Background(), lint.Request{})
	assert.ErrorContains(t, err, "exclude belongs in the project's root config")
}

// The unformatted rule formats a file as `dgdtools format` would: with
// the file's own settings, and leaving alone what skip markers protect.
func TestUnformattedFollowsTheFormatter(t *testing.T) {
	const spaces = "void\nf()\n{\n    x = 1;\n}\n"
	const tabs = "void\nf()\n{\n\tx = 1;\n}\n"
	m := vfs.Mem{
		"/lib/dgdtools.yml":            "root: .\nlint:\n  rules: {unformatted: warning}\n",
		"/lib/a.c":                     spaces,
		"/lib/b.c":                     tabs,
		"/lib/tabs/dgdtools.yml":       "root: false\nformat: {indent_style: tab}\n",
		"/lib/tabs/c.c":                tabs,
		"/lib/tabs/d.c":                spaces,
		"/lib/ec/.editorconfig":        "[*.c]\nindent_style = tab\n",
		"/lib/ec/e.c":                  tabs,
		"/lib/ec/f.c":                  spaces,
		"/lib/skip.c":                  "/* dgdtools-ignore-start format */\nint   *table,   *other;\n/* dgdtools-ignore-end format */\nint x;\n",
		"/lib/skip2.c":                 "/* dgdtools-ignore-start format */\nint   *table,   *other;\n/* dgdtools-ignore-end format */\nint   x;\n",
		"/lib/tabs/quiet/dgdtools.yml": "root: false\nlint:\n  rules: {unformatted: off}\n",
		"/lib/tabs/quiet/g.c":          spaces,
	}
	prj, err := config.Discover(m, []string{"/lib"}, config.DiscoverOptions{})
	require.NoError(t, err)
	res := run(t, prj, m, lint.Request{})
	var got []string
	for _, d := range res.Diagnostics {
		got = append(got, strings.TrimPrefix(d.Path, "/lib/")+" "+d.Rule)
	}
	assert.Equal(t, []string{"b.c unformatted", "ec/f.c unformatted", "skip2.c unformatted", "tabs/d.c unformatted"}, got)
}
