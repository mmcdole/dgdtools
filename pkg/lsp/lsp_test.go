package lsp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/lsp"
	"github.com/mmcdole/dgdtools/pkg/lsp/lsptest"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// tree writes files below a new directory and returns it.
func tree(t testing.TB, files map[string]string) string {
	t.Helper()
	// macOS temp dirs are symlinked; resolve so paths compare equal.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	write(t, dir, files)
	return dir
}

func write(t testing.TB, dir string, files map[string]string) {
	t.Helper()
	for rel, src := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(src), 0o644))
	}
}

// start runs a server that waits as briefly as a test wants to.
func start(t testing.TB) *lsptest.Client {
	t.Helper()
	return lsptest.Start(t, lsp.Options{Version: "test", Debounce: 2 * time.Millisecond, DependentDebounce: 10 * time.Millisecond})
}

// lib is a small lib with every kind of edge between its files.
func lib() map[string]string {
	return map[string]string{
		"dgdtools.yml":   "root: .\ndriver_config: mud.dgd\n",
		"mud.dgd":        "include_dirs = ({ \"/include\" });\ninclude_file = \"/include/auto.h\";\nauto_object = \"/sys/auto\";\n",
		"include/auto.h": "#define AUTO 1\n",
		"include/std.h":  "#define STD \"/std/base\"\n",
		"sys/auto.c":     "void autof() {}\n",
		"std/base.c":     "void create() {}\nint foo(int a) { return a; }\nstatic void sfn() {}\n",
		"std/mid.c":      "inherit \"/std/base\";\nvoid create() { ::create(); }\nvoid bar() {}\n",
		"std/mixin.c":    "void go() { this_object()->provided(); call_out(\"later\", 1); }\n",
		"obj/a.c":        "#include <std.h>\ninherit STD;\nvoid create() { ::create(); \"/obj/b\"->bar(); \"/obj/b\"->nofn(); this_object()->foo(1,2); call_out(\"cb\", 1); autof(); }\n",
		"obj/b.c":        "inherit \"/std/mid\";\nvoid create() { }\n",
		"obj/c.c":        "#include \"local.h\"\nvoid x() { \"/obj/missing\"->f(); LOCAL->bar(); \"/obj/b\"->sfn(); \"/sys/auto\"->autof(1); }\n",
		"obj/local.h":    "#define LOCAL \"/obj/b\"\n",
		"obj/d.c":        "inherit \"/std/mixin\";\nvoid provided() {}\n",
	}
}

// buffers are the texts an editor has open beside the one being looked
// at, by path: a test that opens several files that refer to each other
// sets it, since they are what the tools read then.
var buffers map[string]string

// fresh lints the file at path as `dgdtools lint` does, by a linter made
// for the purpose, with text in place of what the disk has.
func fresh(t testing.TB, path, text string) []diag.Diagnostic {
	t.Helper()
	fsys := vfs.NewOverlay(vfs.OS{})
	for p, text := range buffers {
		fsys.Set(p, []byte(text))
	}
	fsys.Set(path, []byte(text))
	prj, err := config.Discover(fsys, []string{path}, config.DiscoverOptions{})
	require.NoError(t, err)
	l, err := lint.New(prj, fsys)
	require.NoError(t, err)
	res, err := l.Run(context.Background(), lint.Request{Paths: []string{path}})
	require.NoError(t, err)
	require.Empty(t, res.Errors)
	return res.Diagnostics
}

// A finding is a diagnostic as both sides can say it, in a line:
// "L:C-L:C severity rule: message (+related)".
func finding(r lsp.Range, severity int, rule, message string, related int) string {
	return fmt.Sprintf("%d:%d-%d:%d %d %s: %s (+%d)", r.Start.Line, r.Start.Character, r.End.Line, r.End.Character, severity, rule, message, related)
}

// linted are the findings of a fresh lint of text, with their positions
// worked out the long way in the given encoding.
func linted(t testing.TB, path, text, encoding string) []string {
	out := []string{}
	for _, d := range fresh(t, path, text) {
		r := lsp.Range{Start: lsptest.PositionOf(text, int(d.Start), encoding), End: lsptest.PositionOf(text, int(d.End), encoding)}
		out = append(out, finding(r, map[diag.Severity]int{diag.Error: 1, diag.Warning: 2, diag.Info: 3}[d.Severity], d.Rule, wire(t, d.Message), len(d.Related)))
	}
	return out
}

// wire returns a message as it arrives: one can quote bytes of the file
// that are no UTF-8 (a Latin-1 file, a character cut in two), and on the
// wire it is JSON, where each such byte is U+FFFD.
func wire(t testing.TB, message string) string {
	data, err := json.Marshal(message)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &message))
	return message
}

// published are the findings the server last published for the file.
func published(t testing.TB, c *lsptest.Client, path string) []string {
	t.Helper()
	p := c.Diagnostics(path)
	require.NotNil(t, p, "%s: nothing was published", path)
	require.NotNil(t, p.Version)
	require.Equal(t, c.Doc(path).Version, *p.Version, "%s: the version the diagnostics are of", path)
	out := []string{}
	for _, d := range p.Diagnostics {
		assert.Equal(t, "dgdtools", d.Source)
		out = append(out, finding(d.Range, d.Severity, ruleOf(t, d.Code), d.Message, len(d.RelatedInformation)))
	}
	return out
}

// ruleOf returns the name of the rule with the given code, which is what
// a diagnostic of the protocol carries.
func ruleOf(t testing.TB, code string) string {
	r := lint.RuleByName(code)
	require.NotNil(t, r, code)
	require.Equal(t, code, r.Code, "a diagnostic's code is the rule's code, not its name")
	return r.Name
}

// rules are the rules of the findings, in order.
func rules(fs []string) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, strings.TrimSuffix(strings.Fields(f)[2], ":"))
	}
	return out
}

// For every file of a lib, in both position encodings, what is published
// when it is opened is what `dgdtools lint` finds in it, range for
// range; and after an edit and its undo it is that again.
func TestDiagnosticsAreTheLinters(t *testing.T) {
	files := lib()
	files["obj/e.c"] = "/* blåbær \U0001F600 */ void f() { \"grøt \U0001F600\"->x(); a == 1; \"€\" \"å\"; }\n" +
		"void g() { \"/obj/b\"->nofn(); /* \U0001F600 */ \"/obj/b\"->nofn(); }\r\n\rvoid h() { b == 2; }"
	dir := tree(t, files)
	total := 0
	for _, enc := range [][]string{nil, {lsp.UTF8, lsp.UTF16}} {
		c := start(t)
		c.Initialize(lsptest.Capabilities{Encodings: enc}, dir)
		require.Equal(t, enc != nil, c.Encoding == lsp.UTF8)
		for rel, text := range files {
			if strings.HasSuffix(rel, ".c") {
				c.Open(filepath.Join(dir, rel), text)
			}
		}
		for rel, text := range files {
			if !strings.HasSuffix(rel, ".c") {
				continue
			}
			path := filepath.Join(dir, rel)
			want := linted(t, path, text, c.Encoding)
			assert.Equal(t, want, published(t, c, path), "%s (%s)", rel, c.Encoding)
			total += len(want)

			edited := "int added_at_the_top;\n" + text
			c.Change(path, lsptest.Edit{Text: "int added_at_the_top;\n"})
			assert.Equal(t, linted(t, path, edited, c.Encoding), published(t, c, path), "%s, edited", rel)
			c.Change(path, lsptest.Edit{End: len("int added_at_the_top;\n")})
			assert.Equal(t, want, published(t, c, path), "%s, the edit undone", rel)
		}
		require.NoError(t, c.Shutdown())
	}
	assert.Greater(t, total, 20, "the lib has findings of every kind")
	// Among them one that only the scan of the whole lib finds: a
	// callback that no object inheriting the file provides.
	assert.Contains(t, fmt.Sprint(linted(t, filepath.Join(dir, "std/mixin.c"), files["std/mixin.c"], lsp.UTF8)), "'later' is not defined")
}

// The scan of the whole lib, which the first question about who inherits
// a file starts, is shown to a client that shows progress, and its end is
// logged.
func TestScanProgress(t *testing.T) {
	defer func(d time.Duration) { *lsp.ProgressDelay = d }(*lsp.ProgressDelay)
	*lsp.ProgressDelay = 0
	dir := tree(t, lib())
	path := filepath.Join(dir, "std/mixin.c")
	files := 0
	for rel := range lib() {
		if strings.HasSuffix(rel, ".c") {
			files++
		}
	}
	for _, plain := range []bool{false, true} {
		c := start(t)
		c.Initialize(lsptest.Capabilities{Plain: plain}, dir)
		c.Open(path, lib()["std/mixin.c"])
		assert.Contains(t, fmt.Sprint(published(t, c, path)), "'later' is not defined", "what only the scan finds")
		c.Idle()
		var logged strings.Builder
		for _, m := range c.Received("window/logMessage") {
			var p lsp.LogMessageParams
			require.NoError(t, json.Unmarshal(m.Params, &p))
			fmt.Fprintln(&logged, p.Type, p.Message)
		}
		assert.Regexp(t, fmt.Sprintf(`(?m)^3 mudlib scanned: %d files in \d+\.\d s \(%s\)$`, files, regexp.QuoteMeta(dir)), logged.String())
		var shown []string
		for _, m := range c.Received("$/progress") {
			var p struct {
				Token string
				Value lsp.WorkDoneProgress
			}
			require.NoError(t, json.Unmarshal(m.Params, &p))
			shown = append(shown, p.Value.Kind+" "+p.Value.Title)
		}
		if plain {
			assert.Empty(t, shown)
			assert.Empty(t, c.Received("window/workDoneProgress/create"))
		} else {
			assert.Equal(t, []string{"begin dgdtools: indexing the mudlib", "end "}, shown)
			assert.Len(t, c.Received("window/workDoneProgress/create"), 1)
		}
		require.NoError(t, c.Shutdown())
	}
}

// A session that does not end as the protocol has it.
func TestEndings(t *testing.T) {
	c := start(t)
	c.Initialize(lsptest.Capabilities{})
	assert.ErrorIs(t, c.Exit(), lsp.ErrNoShutdown, "exit without shutdown")

	// The editor goes away: the stream ends.
	near, far := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- lsp.Serve(context.Background(), far, lsp.Options{}) }()
	near.Close()
	assert.ErrorIs(t, <-done, lsp.ErrNoShutdown)

	// The process is told to stop.
	ctx, cancel := context.WithCancel(context.Background())
	_, far = net.Pipe()
	go func() { done <- lsp.Serve(ctx, far, lsp.Options{}) }()
	cancel()
	assert.ErrorIs(t, <-done, lsp.ErrNoShutdown)
}

// count returns how often diagnostics were published for the file.
func count(c *lsptest.Client, path string) int {
	n := 0
	for _, m := range c.Received("textDocument/publishDiagnostics") {
		var p lsp.PublishDiagnosticsParams
		json.Unmarshal(m.Params, &p)
		if p.URI == lsptest.URI(path) {
			n++
		}
	}
	return n
}

// A change to a file reaches the open documents that depend on it, and
// no others: a header, on disk and as a buffer.
func TestDependents(t *testing.T) {
	dir := tree(t, lib())
	path := func(rel string) string { return filepath.Join(dir, rel) }
	c := start(t)
	c.Initialize(lsptest.Capabilities{}, dir)
	for _, rel := range []string{"obj/a.c", "obj/c.c", "obj/d.c"} {
		c.Open(path(rel), lib()[rel])
	}
	before := published(t, c, path("obj/c.c"))
	require.Contains(t, rules(before), "static-crossobj")
	counts := func() [3]int {
		c.Idle()
		return [3]int{count(c, path("obj/a.c")), count(c, path("obj/c.c")), count(c, path("obj/d.c"))}
	}
	was := counts()

	// The header c.c includes changes on disk: LOCAL is another object.
	write(t, dir, map[string]string{"obj/local.h": "#define LOCAL \"/obj/d\"\n"})
	c.WatchedFiles(2, path("obj/local.h"))
	after := published(t, c, path("obj/c.c"))
	assert.Equal(t, linted(t, path("obj/c.c"), lib()["obj/c.c"], c.Encoding), after)
	assert.NotEqual(t, before, after)
	assert.Equal(t, [3]int{was[0], was[1] + 1, was[2]}, counts(), "c.c is published again, and it alone")

	// The header is opened and edited: the buffer is the file.
	c.Open(path("obj/local.h"), "#define LOCAL \"/obj/d\"\n")
	c.Replace(path("obj/local.h"), "#define LOCAL \"/obj/b\"\n")
	assert.Equal(t, before, published(t, c, path("obj/c.c")))
	assert.Equal(t, [3]int{was[0], was[1] + 2, was[2]}, counts())
	// Closed, the disk is the file again, and the disk has the other text.
	c.Close(path("obj/local.h"))
	assert.Equal(t, after, published(t, c, path("obj/c.c")))
	// An event for a file that is open changes nothing: the buffer is the file.
	write(t, dir, map[string]string{"obj/c.c": "int nothing;\n"})
	c.WatchedFiles(2, path("obj/c.c"))
	assert.Equal(t, after, published(t, c, path("obj/c.c")))
	assert.Equal(t, [3]int{was[0], was[1] + 3, was[2]}, counts())
}

// Files and directories that come and go on disk, and the files a
// project is configured by: each change reaches the diagnostics.
func TestWatchedFiles(t *testing.T) {
	files := lib()
	files["dgdtools.yml"] = "root: .\ndriver_config: ../shared/mud.dgd\n"
	files["../shared/mud.dgd"] = files["mud.dgd"]
	dir := filepath.Join(tree(t, map[string]string{"lib/x": ""}), "lib")
	write(t, dir, files)
	path := func(rel string) string { return filepath.Join(dir, rel) }
	c := start(t)
	c.Initialize(lsptest.Capabilities{}, dir)
	c.Open(path("obj/c.c"), files["obj/c.c"])
	c.Open(path("obj/a.c"), files["obj/a.c"])
	check := func(rel, what string) []string {
		t.Helper()
		got := published(t, c, path(rel))
		assert.Equal(t, linted(t, path(rel), files[rel], c.Encoding), got, what)
		return got
	}
	require.Contains(t, rules(check("obj/c.c", "at first")), "target-object-missing")

	// A file is created, and deleted again.
	write(t, dir, map[string]string{"obj/missing.c": "void f() {}\n"})
	c.WatchedFiles(1, path("obj/missing.c"))
	assert.NotContains(t, rules(check("obj/c.c", "the missing object is there")), "target-object-missing")
	require.NoError(t, os.Remove(path("obj/missing.c")))
	c.WatchedFiles(3, path("obj/missing.c"))
	assert.Contains(t, rules(check("obj/c.c", "and gone")), "target-object-missing")

	// A directory goes, and comes back: one event, for the directory.
	require.NoError(t, os.RemoveAll(path("std")))
	c.WatchedFiles(3, path("std"))
	assert.Contains(t, fmt.Sprint(check("obj/a.c", "std is gone")), "/std/base does not exist")
	write(t, dir, map[string]string{"std/base.c": files["std/base.c"], "std/mid.c": files["std/mid.c"], "std/mixin.c": files["std/mixin.c"]})
	c.WatchedFiles(1, path("std"))
	assert.NotContains(t, fmt.Sprint(check("obj/a.c", "std is back")), "does not exist")

	// What the project is configured by is watched too, where the
	// patterns do not reach: the driver's config, outside the folder.
	drv := filepath.Join(filepath.Dir(dir), "shared", "mud.dgd")
	assert.Contains(t, c.Watchers(), lsptest.URI(filepath.Dir(drv))+" mud.dgd")
	assert.Len(t, c.Watchers(), 7, "the patterns, and that")
	sev := func() int {
		for _, f := range check("obj/a.c", "") {
			if strings.Contains(f, " callback-arity: ") {
				return int(strings.Fields(f)[1][0] - '0')
			}
		}
		return 0
	}
	require.Equal(t, lsp.SeverityWarning, sev())
	before := check("obj/a.c", "the driver's config as it was")
	c.Idle()
	require.NoError(t, os.WriteFile(drv, []byte("include_dirs = ({ \"/nowhere\" });\n"), 0o644))
	c.WatchedFiles(2, drv)
	assert.NotEqual(t, before, check("obj/a.c", "the driver's config changed"))
	c.Idle()
	require.NoError(t, os.WriteFile(drv, []byte(files["mud.dgd"]), 0o644))
	c.WatchedFiles(2, drv)
	assert.Equal(t, before, check("obj/a.c", "and changed back"))

	// The project's own configuration: a rule is turned off, for every
	// open document; a configuration that is not valid is said to be,
	// and nothing is claimed while it is.
	c.Idle()
	whole := files["dgdtools.yml"]
	files["dgdtools.yml"] = lib()["dgdtools.yml"] + "lint:\n  rules:\n    target-object-missing: off\n    callback-arity: off\n"
	write(t, dir, files)
	c.WatchedFiles(2, path("dgdtools.yml"))
	assert.NotContains(t, rules(check("obj/c.c", "a rule off")), "target-object-missing")
	assert.Equal(t, 0, sev())
	assert.NotContains(t, c.Watchers(), lsptest.URI(filepath.Dir(drv))+" mud.dgd", "the driver's config outside is no longer one of the project's files")
	assert.Contains(t, c.Watchers(), lsptest.URI(dir)+" mud.dgd", "the one in the folder is")
	files["dgdtools.yml"] = whole

	c.Idle()
	write(t, dir, map[string]string{"dgdtools.yml": "root: .\nlint:\n  rules:\n    no-such-rule: error\n"})
	c.WatchedFiles(2, path("dgdtools.yml"))
	assert.Empty(t, published(t, c, path("obj/c.c")))
	shown := c.Received("window/showMessage")
	require.Len(t, shown, 1, "said once, for all the documents it is true of")
	assert.Contains(t, string(shown[0].Params), "no-such-rule")
	write(t, dir, map[string]string{"dgdtools.yml": files["dgdtools.yml"]})
	c.WatchedFiles(2, path("dgdtools.yml"))
	assert.Contains(t, rules(check("obj/c.c", "mended")), "target-object-missing")

	// A baseline appears: what it records is no longer reported.
	c.Idle()
	baseline, err := lint.NewBaseline(path(lint.BaselineFileName), fresh(t, path("obj/c.c"), files["obj/c.c"])).Marshal()
	require.NoError(t, err)
	write(t, dir, map[string]string{lint.BaselineFileName: string(baseline)})
	c.WatchedFiles(1, path(lint.BaselineFileName))
	assert.Empty(t, check("obj/c.c", "baselined"))
	assert.NotEmpty(t, check("obj/a.c", "not in the baseline"))
}

// However fast the changes come, what is published is of the text it
// says it is of, and the last word is of the last text; while they come
// faster than the server waits, nothing is linted at all.
func TestBurst(t *testing.T) {
	dir := tree(t, lib())
	path := filepath.Join(dir, "obj/a.c")
	const debounce = 400 * time.Millisecond
	c := lsptest.Start(t, lsp.Options{Debounce: debounce, DependentDebounce: time.Second})
	c.Initialize(lsptest.Capabilities{}, dir)
	c.Open(path, lib()["obj/a.c"])
	texts := map[int]string{1: c.Doc(path).Text}
	began := time.Now()
	for i := range 200 {
		c.Change(path, lsptest.Edit{Text: fmt.Sprintf("int v%d; x == %d;\n", i, i)})
		texts[c.Doc(path).Version] = c.Doc(path).Text
	}
	took := time.Since(began)
	final := published(t, c, path)
	assert.Equal(t, linted(t, path, c.Doc(path).Text, c.Encoding), final)
	versions := []int{}
	for _, m := range c.Received("textDocument/publishDiagnostics") {
		var p lsp.PublishDiagnosticsParams
		require.NoError(t, json.Unmarshal(m.Params, &p))
		versions = append(versions, *p.Version)
		got := []string{}
		for _, d := range p.Diagnostics {
			got = append(got, ruleOf(t, d.Code))
		}
		assert.Equal(t, rules(linted(t, path, texts[*p.Version], c.Encoding)), got, "version %d", *p.Version)
	}
	assert.True(t, slices.IsSorted(versions), "%v", versions)
	if took < debounce/2 {
		assert.Equal(t, []int{201}, versions, "one lint, of the last text")
	} else {
		t.Logf("the burst took %s: not held to one lint", took)
	}
}

// A request the editor no longer wants is answered as cancelled.
func TestCancel(t *testing.T) {
	dir := tree(t, lib())
	c := lsptest.Start(t, lsp.Options{Debounce: time.Hour})
	c.Initialize(lsptest.Capabilities{}, dir)
	c.Open(filepath.Join(dir, "obj/a.c"), lib()["obj/a.c"])
	// The lint is an hour away, and so is the answer to this:
	err := c.CallCancelled(lsp.MethodIdle, nil)
	assert.True(t, lsptest.IsCode(err, -32800), "%v", err)
	// Cancelling what was answered long ago, or never asked, is nothing.
	c.Notify("$/cancelRequest", map[string]any{"id": 1})
	c.Notify("$/cancelRequest", map[string]any{"id": "never"})
	var edits []lsp.TextEdit
	require.NoError(t, c.Call("textDocument/formatting", map[string]any{"textDocument": map[string]any{"uri": lsptest.URI(filepath.Join(dir, "obj/a.c"))}}, &edits))
	assert.NotEmpty(t, edits)
	assert.NoError(t, c.Shutdown(), "a lint that is scheduled does not hold up the end")
}

// faulty is a file system that panics when a file whose name has "boom"
// in it is read.
type faulty struct{ vfs.FS }

func (f faulty) ReadFile(path string) ([]byte, error) {
	if strings.Contains(path, "boom") {
		panic("boom")
	}
	return f.FS.ReadFile(path)
}

func (f faulty) WalkDir(root string, fn fs.WalkDirFunc) error { return f.FS.WalkDir(root, fn) }

// A panic costs the message it happened in, and is logged: a request is
// answered with an internal error, and the server goes on.
func TestPanics(t *testing.T) {
	dir := tree(t, map[string]string{"boom/dgdtools.yml": "root: .\n", "boom/a.c": "int x;\n", "fine/a.c": "void f() { a == 1; }\n"})
	var log safeBuffer
	c := lsptest.Start(t, lsp.Options{FS: faulty{vfs.OS{}}, Debounce: time.Millisecond,
		Log: slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	c.Initialize(lsptest.Capabilities{}, dir)
	boom, fine := filepath.Join(dir, "boom/a.c"), filepath.Join(dir, "fine/a.c")
	format := func(path string) error {
		return c.Call("textDocument/formatting", map[string]any{"textDocument": map[string]any{"uri": lsptest.URI(path)}}, nil)
	}
	// In a lint (the project's configuration is read), in a request, in
	// a notification (the file is read to see what encoding it has).
	c.Open(boom, "int x;\n")
	assert.Nil(t, c.Diagnostics(boom))
	assert.True(t, lsptest.IsCode(format(boom), -32603), "%v", format(boom))
	c.Open(filepath.Join(dir, "boom/b.c"), "/* � */\n")
	c.Idle()
	assert.GreaterOrEqual(t, strings.Count(log.String(), "level=ERROR msg=panic"), 4)
	assert.Contains(t, log.String(), "stack=")

	c.Open(fine, "void f() { a == 1; }\n")
	assert.Equal(t, []string{"no-effect-statement"}, rules(published(t, c, fine)))
	assert.NoError(t, format(fine))
	assert.NoError(t, c.Shutdown())
}

// safeBuffer is a buffer a server logs to while a test reads it.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// The log has what the server did, and with Trace every message; the
// client's own trace setting gets the time each request took.
func TestLogAndTrace(t *testing.T) {
	dir := tree(t, lib())
	var log safeBuffer
	c := lsptest.Start(t, lsp.Options{Debounce: time.Millisecond, Trace: true,
		Log: slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	c.Initialize(lsptest.Capabilities{}, dir)
	path := filepath.Join(dir, "obj/a.c")
	c.Open(path, lib()["obj/a.c"])
	c.Idle()
	assert.Empty(t, c.Received("$/logTrace"))
	c.Notify("$/setTrace", map[string]any{"value": "messages"})
	require.NoError(t, c.Call("textDocument/formatting", map[string]any{"textDocument": map[string]any{"uri": lsptest.URI(path)}}, nil))
	c.Idle()
	require.NotEmpty(t, c.Received("$/logTrace"))
	assert.Contains(t, string(c.Received("$/logTrace")[0].Params), "textDocument/formatting: ")
	require.NoError(t, c.Shutdown())
	for _, want := range []string{"msg=initialized", "msg=lint", "msg=request method=textDocument/formatting", "textDocument/didOpen", "textDocument/publishDiagnostics"} {
		assert.Contains(t, log.String(), want)
	}
}

// What the editor's settings say of a folder that has no dgdtools.yml:
// read from the client, and read again when they change; a client that
// cannot be asked says them itself.
func TestSettings(t *testing.T) {
	files := map[string]string{
		"ann/a.c":           "void f() { \"/players/ann/b\"->nofn(); \"/players/ann/b\"->fn(); }\n",
		"ann/b.c":           "void fn() {}\n",
		"bob/a.c":           "void f() { \"/players/bob/b\"->nofn(); }\n",
		"bob/b.c":           "void fn() {}\n",
		"lib/dgdtools.yml":  "root: .\n",
		"lib/players/c.c":   "void f() { \"/players/b\"->nofn(); }\n",
		"lib/players/b.c":   "void fn() {}\n",
		"elsewhere/alone.c": "void f() { \"/players/ann/b\"->nofn(); a == 1; }\n",
	}
	dir := tree(t, files)
	path := func(rel string) string { return filepath.Join(dir, rel) }
	open := func(c *lsptest.Client, rels ...string) {
		for _, rel := range rels {
			c.Open(path(rel), files[rel])
		}
	}
	all := []string{"ann/a.c", "bob/a.c", "lib/players/c.c", "elsewhere/alone.c"}
	got := func(c *lsptest.Client) string {
		var b strings.Builder
		for _, rel := range all {
			fmt.Fprintln(&b, rel, rules(published(t, c, path(rel))))
		}
		return b.String()
	}
	const (
		none = "ann/a.c []\nbob/a.c []\nlib/players/c.c [callable-not-found]\nelsewhere/alone.c [no-effect-statement]\n"
		ann  = "ann/a.c [callable-not-found]\nbob/a.c []\nlib/players/c.c [callable-not-found]\nelsewhere/alone.c [no-effect-statement]\n"
		both = "ann/a.c [callable-not-found]\nbob/a.c [callable-not-found]\nlib/players/c.c [callable-not-found]\nelsewhere/alone.c [no-effect-statement]\n"
	)

	c := start(t)
	// The client answers the server's requests on a goroutine of its own.
	var mu sync.Mutex
	settings := map[string]any{lsptest.URI(path("ann")): map[string]any{"libPath": "/players/ann"}}
	set := func(uri string, v any) {
		mu.Lock()
		defer mu.Unlock()
		if v == nil {
			delete(settings, uri)
		} else {
			settings[uri] = v
		}
	}
	c.Settings = func(uri string) any {
		mu.Lock()
		defer mu.Unlock()
		return settings[uri]
	}
	c.Initialize(lsptest.Capabilities{}, path("ann"), path("bob"), path("lib"))
	open(c, all...)
	// ann is /players/ann: its files are checked against each other. Of
	// bob nothing is known, a config file needs no settings, and a file
	// in no folder has no lib.
	assert.Equal(t, ann, got(c))
	assert.Len(t, c.Received("workspace/configuration"), 1)
	// The messages' text, read as text: how fmt prints a json.RawMessage
	// differs between Go releases.
	var logged strings.Builder
	for _, m := range c.Received("window/logMessage") {
		logged.Write(m.Params)
	}
	assert.Contains(t, logged.String(), path("ann")+" is /players/ann")

	// The settings change: the editor says so and is asked again.
	set(lsptest.URI(path("bob")), map[string]any{"libPath": "/players/bob"})
	c.Notify("workspace/didChangeConfiguration", map[string]any{"settings": nil})
	assert.Equal(t, both, got(c))
	set(lsptest.URI(path("ann")), nil)
	set(lsptest.URI(path("bob")), nil)
	c.Notify("workspace/didChangeConfiguration", map[string]any{"settings": nil})
	assert.Equal(t, none, got(c))
	// Nothing changed: nothing is linted again.
	n := len(c.Received("textDocument/publishDiagnostics"))
	c.Notify("workspace/didChangeConfiguration", map[string]any{"settings": nil})
	c.Idle()
	assert.Len(t, c.Received("textDocument/publishDiagnostics"), n)

	// A folder leaves the workspace and comes back with settings.
	folder := []lsp.WorkspaceFolder{{URI: lsptest.URI(path("ann")), Name: "ann"}}
	c.Notify("workspace/didChangeWorkspaceFolders", map[string]any{"event": map[string]any{"removed": folder}})
	set(lsptest.URI(path("ann")), map[string]any{"libPath": "/players/ann"})
	c.Idle()
	assert.Equal(t, none, got(c))
	c.Notify("workspace/didChangeWorkspaceFolders", map[string]any{"event": map[string]any{"added": folder}})
	assert.Equal(t, ann, got(c))
	require.NoError(t, c.Shutdown())

	// A client that is never asked: what it says at the start holds for
	// its folders, and what it says later replaces that.
	c = start(t)
	c.Initialize(lsptest.Capabilities{Plain: true, Options: map[string]any{"libPath": "/players/ann"}}, path("ann"))
	open(c, all...)
	assert.Equal(t, ann, got(c))
	assert.Empty(t, c.Received("workspace/configuration"))
	c.Notify("workspace/didChangeConfiguration", map[string]any{"settings": map[string]any{"dgdtools": map[string]any{}}})
	assert.Equal(t, none, got(c))
}

// A declaration that comes from a profile is located in a stub, which is
// no file: the server names it by a URI of its own and gives the text of
// it when asked, so that an editor can show the place.
func TestStubContent(t *testing.T) {
	// A stub is a file of the lib without its function bodies.
	const room = "inherit \"/std/object\";\n\nstatic void room_static() {}\nvoid room_public() {}\n"
	p := &profile.Profile{
		Manifest: profile.Manifest{Name: "Mini Lib", Dialect: profile.Dialect{Driver: "dgd"}, Covered: []string{"/std/"}},
		Files:    map[string]string{"/std/room.c": room, "/std/object.c": "void create() {}\n"},
	}
	data, err := p.Marshal()
	require.NoError(t, err)
	files := map[string]string{
		"ann/mini.profile":  string(data),
		"ann/x.c":           "void f() { \"/std/room\"->room_static(); }\n",
		"bob/b.c":           "void f() { }\n",
		"elsewhere/alone.c": "void f() { }\n",
	}
	dir := tree(t, files)
	path := func(rel string) string { return filepath.Join(dir, rel) }
	content := func(c *lsptest.Client, uri string) (string, error) {
		var res lsp.TextDocumentContentResult
		err := c.Call(lsp.MethodTextDocumentContent, lsp.TextDocumentContentParams{URI: uri}, &res)
		return res.Text, err
	}

	c := start(t)
	// The profile is the folder's by the editor's settings, as for a
	// wizard who has no config file.
	c.Settings = func(uri string) any {
		if uri == lsptest.URI(path("ann")) {
			return map[string]any{"profile": path("ann/mini.profile"), "libPath": "/players/ann"}
		}
		return nil
	}
	var res lsp.InitializeResult
	require.NoError(t, json.Unmarshal(c.Initialize(lsptest.Capabilities{}, path("ann"), path("bob")), &res))
	assert.Equal(t, []string{profile.Scheme}, res.Capabilities.Workspace.TextDocumentContent.Schemes)

	c.Open(path("ann/x.c"), files["ann/x.c"])
	c.Open(path("bob/b.c"), files["bob/b.c"])
	ds := c.Diagnostics(path("ann/x.c")).Diagnostics
	require.Len(t, ds, 1)
	assert.Equal(t, "static-crossobj", ruleOf(t, ds[0].Code))
	require.Len(t, ds[0].RelatedInformation, 1)
	loc := ds[0].RelatedInformation[0].Location
	assert.Equal(t, "dgdtools-profile://Mini%20Lib/std/room.c", loc.URI)

	// What the location names is there to read, and the range is of it.
	text, err := content(c, loc.URI)
	require.NoError(t, err)
	assert.Equal(t, room, text)
	start, end := lsptest.OffsetOf(text, loc.Range.Start, c.Encoding), lsptest.OffsetOf(text, loc.Range.End, c.Encoding)
	assert.Equal(t, "room_static", text[start:end])

	// An editor writes a URI its own way: the host in lower case.
	text, err = content(c, "dgdtools-profile://mini%20lib/std/object.c")
	require.NoError(t, err)
	assert.Equal(t, "void create() {}\n", text)

	// With the document closed the stub is still the folder's profile's.
	c.Close(path("ann/x.c"))
	text, err = content(c, loc.URI)
	require.NoError(t, err)
	assert.Equal(t, room, text)

	for _, uri := range []string{
		"dgdtools-profile://Mini%20Lib/std/nothing.c", // no such stub
		"dgdtools-profile://Mini%20Lib/std",           // a directory of stubs
		"dgdtools-profile://other/std/room.c",         // a profile nobody uses
		lsptest.URI(path("ann/x.c")),                  // a file: the editor has those itself
		"untitled:Untitled-1",
	} {
		_, err := content(c, uri)
		assert.True(t, lsptest.IsCode(err, -32602), "%s: %v", uri, err)
	}
	require.NoError(t, c.Shutdown())
}

// A file that is Latin-1 on disk, opened as what it is and opened as
// UTF-8, which it is not: the same findings in the same places of the
// text the editor has, and a warning for the second.
func TestLatin1(t *testing.T) {
	const (
		disk    = "/* gr\xf8t */ void f() { \"bl\xe5b\xe6r\"->x(); a == 1; \"\xe5\" \"\xf8\"; }\nvoid g() { \"/obj/t\"->nofn(\"\xe6\"); }\n"
		decoded = "/* grøt */ void f() { \"blåbær\"->x(); a == 1; \"å\" \"ø\"; }\nvoid g() { \"/obj/t\"->nofn(\"æ\"); }\n"
		garbled = "/* gr�t */ void f() { \"bl�b�r\"->x(); a == 1; \"�\" \"�\"; }\nvoid g() { \"/obj/t\"->nofn(\"�\"); }\n"
		// Another file refers into the one on disk, in a line that has
		// bytes before the place.
		user = "void f() { \"/obj/t\"->h(1, 2); }\n"
	)
	dir := tree(t, map[string]string{"dgdtools.yml": "root: .\n", "obj/t.c": disk + "/* \xe5\xe5\xe5 */ void h(int a) {}\n", "obj/u.c": user})
	path := filepath.Join(dir, "obj/t.c")
	for _, enc := range [][]string{nil, {lsp.UTF8}} {
		var found [][]string
		for _, text := range []string{decoded, garbled} {
			c := start(t)
			c.Initialize(lsptest.Capabilities{Encodings: enc}, dir)
			c.Open(path, text)
			got := published(t, c, path)
			assert.Equal(t, linted(t, path, text, c.Encoding), got)
			assert.Equal(t, []string{"target-object-missing", "no-effect-statement", "adjacent-strings", "callable-not-found"}, rules(got))
			found = append(found, rules(got))
			assert.Len(t, c.Received("window/showMessage"), map[string]int{decoded: 0, garbled: 1}[text])
			// Opened again, it is not said again.
			c.Close(path)
			c.Open(path, text)
			c.Idle()
			assert.Len(t, c.Received("window/showMessage"), map[string]int{decoded: 0, garbled: 1}[text])
			require.NoError(t, c.Shutdown())
		}
		assert.Equal(t, found[0], found[1])

		// A related location in the file, which is not open: its bytes
		// are the disk's, and each that is no UTF-8 is one column, which
		// is what an editor that reads the file as Latin-1 shows.
		c := start(t)
		c.Initialize(lsptest.Capabilities{Encodings: enc}, dir)
		c.Open(filepath.Join(dir, "obj/u.c"), user)
		p := c.Diagnostics(filepath.Join(dir, "obj/u.c"))
		require.Len(t, p.Diagnostics, 1)
		require.Len(t, p.Diagnostics[0].RelatedInformation, 1)
		loc := p.Diagnostics[0].RelatedInformation[0].Location
		assert.Equal(t, lsptest.URI(path), loc.URI)
		assert.Equal(t, lsp.Range{Start: lsp.Position{Line: 2, Character: 15}, End: lsp.Position{Line: 2, Character: 16}}, loc.Range)
	}
}

var seeds = flag.Int("seeds", 8, "how many random sessions TestRandomSession plays")

// A random session: edits of every kind, sent as ranges, in both
// encodings; documents closed and opened; files changing on disk. After
// each step what is published for every open document is what a fresh
// lint of its text finds, in range of the text.
func TestRandomSession(t *testing.T) {
	for seed := range int64(*seeds) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			r := rand.New(rand.NewSource(seed))
			files := lib()
			files["obj/e.c"] = "/* blåbær \U0001F600 */ void f() { \"grøt \U0001F600\"->x(); a == 1; \"€\" \"å\"; }\r\nvoid g() { \"/obj/b\"->nofn(); }\n"
			dir := tree(t, files)
			path := func(rel string) string { return filepath.Join(dir, rel) }
			c := start(t)
			c.Initialize(lsptest.Capabilities{Encodings: [][]string{nil, {lsp.UTF8}}[seed%2]}, dir)
			names := []string{"obj/a.c", "obj/b.c", "obj/c.c", "obj/e.c", "std/base.c", "obj/local.h"}
			open := map[string]bool{}
			for step := range 60 {
				rel := names[r.Intn(len(names))]
				switch n := r.Intn(10); {
				case !open[rel]:
					c.Open(path(rel), files[rel])
					open[rel] = true
				case n == 0:
					c.Close(path(rel))
					delete(open, rel)
				case n == 1:
					// The disk changes under a file that is not open.
					other := names[r.Intn(len(names))]
					if !open[other] {
						c.Idle()
						files[other] += "int more;\n"
						write(t, dir, map[string]string{other: files[other]})
						c.WatchedFiles(2, path(other))
					}
				default:
					var edits []lsptest.Edit
					text := c.Doc(path(rel)).Text
					for range 1 + r.Intn(3) {
						e := edit(r, text)
						edits = append(edits, e)
						text = text[:e.Start] + e.Text + text[e.End:]
					}
					c.Change(path(rel), edits...)
				}
				if r.Intn(3) > 0 {
					continue // several changes before anybody looks
				}
				buffers = map[string]string{}
				for rel := range open {
					buffers[path(rel)] = c.Doc(path(rel)).Text
				}
				for rel := range open {
					text := c.Doc(path(rel)).Text
					got := published(t, c, path(rel))
					require.Equal(t, linted(t, path(rel), text, c.Encoding), got, "seed %d step %d: %s\n%q", seed, step, rel, text)
					end := lsptest.PositionOf(text, len(text), c.Encoding)
					for _, d := range c.Diagnostics(path(rel)).Diagnostics {
						require.LessOrEqual(t, d.Range.End.Line, end.Line)
					}
				}
			}
			require.NoError(t, c.Shutdown())
			buffers = nil
		})
	}
}

// A client that watches no files for the server is told once, in the
// log, that changes made outside the editor are not seen.
func TestPlainClientIsToldOfWatching(t *testing.T) {
	dir := tree(t, lib())
	for _, plain := range []bool{false, true} {
		c := start(t)
		c.Initialize(lsptest.Capabilities{Plain: plain}, dir)
		c.Idle()
		said := 0
		for _, m := range c.Received("window/logMessage") {
			if strings.Contains(string(m.Params), "the client does not watch files for the server") {
				said++
			}
		}
		if plain {
			assert.Equal(t, 1, said)
		} else {
			assert.Zero(t, said)
		}
	}
}
