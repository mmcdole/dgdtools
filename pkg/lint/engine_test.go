package lint_test

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// A per-file limit is applied to what would be shown: after the findings
// in undecided branches are withheld, after suppression comments and
// after the baseline. What it holds back is counted.
func TestMaxPerFileCountsWhatIsShown(t *testing.T) {
	src := `void f() {
    a == 1;   /* dgdtools-ignore lint/no-effect-statement */
    b == 2;   /* dgdtools-ignore lint/no-effect-statement */
    c == 3;
    d == 4;
    e == 5;
    g == 6;
}
`
	prj, m := memProject(map[string]string{"obj/t.c": src})
	two := lint.Options{MaxPerFile: 2}
	lines := func(res *lint.Result) (out []int) {
		for _, d := range res.Diagnostics {
			out = append(out, d.Line)
		}
		return out
	}
	res := run(t, prj, m, lint.Request{Paths: []string{"/lib/obj/t.c"}, Options: two})
	assert.Equal(t, []int{4, 5}, lines(res), "the first two that no comment suppresses")
	assert.Equal(t, 2, res.Stats.Suppressed)
	assert.Equal(t, 2, res.Stats.Capped)

	res = run(t, prj, m, lint.Request{Paths: []string{"/lib/obj/t.c"}, Options: lint.Options{MaxPerFile: 3}})
	assert.Equal(t, []int{4, 5, 6}, lines(res))
	assert.Equal(t, 1, res.Stats.Capped)
	// No limit, unless one is given.
	res = run(t, prj, m, lint.Request{Paths: []string{"/lib/obj/t.c"}})
	assert.Equal(t, []int{4, 5, 6, 7}, lines(res))
	assert.Zero(t, res.Stats.Capped)

	// With the first shown findings in the baseline, the limit is for
	// the ones after them.
	m["/lib/dgdtools.yml"] = ""
	prj.Config.Path, prj.Config.Dir = "/lib/dgdtools.yml", "/lib"
	l, err := lint.New(prj, m)
	require.NoError(t, err)
	data, err := lint.NewBaseline(l.BaselinePath(), res.Diagnostics[:3]).Marshal()
	require.NoError(t, err)
	m[l.BaselinePath()] = string(data)
	res = run(t, prj, m, lint.Request{Paths: []string{"/lib/obj/t.c"}, Options: two})
	assert.Equal(t, []int{7}, lines(res), "one finding is new; the limit of two does not hide it")
	assert.Equal(t, 3, res.Stats.Baselined)
	assert.Zero(t, res.Stats.Capped)

	// A finding in a branch that could not be decided is not counted
	// against the limit either.
	guess := "#include \"gone.h\"\nvoid f() {\n#ifdef UNKNOWN\n    x = \"a\" \"b\";\n#endif\n    y = \"c\" \"d\";\n    z = \"e\" \"f\";\n}\n"
	prj, m = memProject(map[string]string{"obj/g.c": guess})
	prj.Config.Lint.Rules = map[string]config.RuleSettings{"include-not-found": {Severity: "off"}}
	res = run(t, prj, m, lint.Request{Paths: []string{"/lib/obj/g.c"}, Options: lint.Options{MaxPerFile: 1}})
	assert.Equal(t, []int{6}, lines(res))
	assert.Equal(t, 1, res.Stats.GuessedBranch)
	assert.Equal(t, 1, res.Stats.Capped)
}

// brokenFS fails to read some files and crashes on others.
type brokenFS struct {
	vfs.FS
	unreadable, crashing string
}

func (b brokenFS) ReadFile(p string) ([]byte, error) {
	switch p {
	case b.unreadable:
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrPermission}
	case b.crashing:
		panic("the lexer met its match")
	}
	return b.FS.ReadFile(p)
}

// A file that cannot be linted is reported with the reason, and the two
// reasons are told apart: the file cannot be read, or the tools crashed.
func TestFileErrors(t *testing.T) {
	prj, m := memProject(map[string]string{
		"obj/ok.c":     "void f() { x == 1; }\n",
		"obj/locked.c": "void f() { }\n",
		"obj/bug.c":    "void f() { }\n",
		"obj/child.c":  "inherit \"/obj/bug\";\nvoid g() { this_object()->nowhere(); }\n",
	})
	fsys := brokenFS{FS: m, unreadable: "/lib/obj/locked.c", crashing: "/lib/obj/bug.c"}
	res := run(t, prj, fsys, lint.Request{})
	require.Len(t, res.Errors, 2)
	assert.Equal(t, lint.FileError{Path: "/lib/obj/bug.c", Kind: lint.Internal, Message: "internal error (please report): the lexer met its match"}, *res.Errors[0])
	assert.Equal(t, lint.FileError{Path: "/lib/obj/locked.c", Kind: lint.Unreadable, Message: "cannot be read: permission denied"}, *res.Errors[1])
	assert.Equal(t, []string{"no-effect-statement ok.c:1"}, keys(res.Diagnostics), "the rest of the run stands; the crashed parent makes its child unknown")
	assert.Equal(t, 4, res.Stats.Files, "the files that were linted")

	// A file that crashed on another's behalf is reported too.
	l, err := lint.New(prj, fsys)
	require.NoError(t, err)
	res, err = l.Run(context.Background(), lint.Request{Paths: []string{"/lib/obj/child.c"}})
	require.NoError(t, err)
	require.Len(t, res.Errors, 1)
	assert.Equal(t, "/lib/obj/bug.c", res.Errors[0].Path)
	assert.Equal(t, lint.Internal, res.Errors[0].Kind)

	// One document: the error is the file's.
	_, err = l.Lint(context.Background(), "/lib/obj/locked.c", lint.Options{})
	var fe *lint.FileError
	require.True(t, errors.As(err, &fe))
	assert.Equal(t, lint.Unreadable, fe.Kind)
	assert.EqualError(t, err, "/lib/obj/locked.c: cannot be read: permission denied")
	_, err = l.Lint(context.Background(), "/lib/obj/none.c", lint.Options{})
	require.True(t, errors.As(err, &fe))
	assert.Equal(t, lint.Unreadable, fe.Kind)
	_, err = l.Lint(context.Background(), "/lib/obj/bug.c", lint.Options{})
	require.True(t, errors.As(err, &fe))
	assert.Equal(t, lint.Internal, fe.Kind)
}

// What a diagnostic says besides where and what: the kind of code it is
// about, and how far its fix can be trusted.
func TestDiagnosticModel(t *testing.T) {
	src := "void f() {\n    string s;\n    s = \"a\" \"b\";\n    return;\n    s = \"c\";\n}\n"
	prj, m := memProject(map[string]string{"obj/t.c": src})
	res := run(t, prj, m, lint.Request{Paths: []string{"/lib/obj/t.c"}, Options: lint.Options{Enable: []string{"unreachable-code"}}})
	require.Len(t, res.Diagnostics, 2)
	adj, dead := res.Diagnostics[0], res.Diagnostics[1]
	assert.Equal(t, "adjacent-strings", adj.Rule)
	assert.Empty(t, adj.Tags)
	require.Len(t, adj.Fixes, 1)
	assert.Equal(t, diag.Safe, adj.Fixes[0].Applicability)
	assert.Equal(t, "unreachable-code", dead.Rule)
	assert.Equal(t, []diag.Tag{diag.Unnecessary}, dead.Tags)
	assert.Equal(t, "unsafe", diag.Fix{}.Applicability.String(), "a fix is not safe unless it says so")

	// Whether a finding is a claim about compiled code is the rule's to
	// say: the preprocessor reads directives in skipped branches too.
	compiledOnly := map[string]bool{}
	for _, r := range lint.Rules() {
		assert.Equal(t, "https://github.com/mmcdole/dgdtools/blob/main/docs/rules.md#"+r.Name, r.URL("dev"))
		compiledOnly[r.Name] = r.CompiledOnly
	}
	// A release links to the page as it was tagged; any other build to main.
	for v, ref := range map[string]string{"v1.2.3": "v1.2.3", "v1.0.0-rc.1": "v1.0.0-rc.1", "dev": "main", "": "main",
		"v1.2.3-4-gabc1234": "main", "v1.2.3-dirty": "main", "v1.2.4-SNAPSHOT-abc1234": "main", "test": "main"} {
		assert.Equal(t, "https://github.com/mmcdole/dgdtools/blob/"+ref+"/docs/rules.md", lint.DocsURL(v), v)
	}
	assert.True(t, compiledOnly["syntax-error"])
	assert.True(t, compiledOnly["callable-not-found"])
	assert.False(t, compiledOnly["bad-directive"])
	assert.False(t, compiledOnly["no-effect-statement"])
	assert.False(t, compiledOnly["missing-visibility"])
}
