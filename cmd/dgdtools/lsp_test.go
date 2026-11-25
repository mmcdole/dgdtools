package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/lsp"
	"github.com/mmcdole/dgdtools/pkg/lsp/lsptest"
)

// pipes are the standard streams of a command, seen from outside.
type pipes struct {
	io.Reader
	io.WriteCloser
}

// server runs `dgdtools lsp` with the given flags, its standard streams
// pipes, and returns a client at their other ends. done receives the
// command's exit code as an error (nil for 0).
func server(t *testing.T, args ...string) (*lsptest.Client, *strings.Builder) {
	t.Helper()
	dgdtools(t, "", "version") // the test's own home directory
	in, toServer := io.Pipe()
	fromServer, out := io.Pipe()
	done := make(chan error, 1)
	stderr := &strings.Builder{}
	go func() {
		code := run(context.Background(), append([]string{"lsp"}, args...), in, out, stderr)
		out.Close()
		if code != 0 {
			done <- fmt.Errorf("exit code %d", code)
		} else {
			done <- nil
		}
	}()
	return lsptest.Connect(t, pipes{fromServer, toServer}, done), stderr
}

// lspLib is a lib with findings of several rules, related locations
// in other files, and text that is not ASCII.
var lspLib = map[string]string{
	"dgdtools.yml":   "root: .\ndriver_config: mud.dgd\nlint:\n  rules:\n    unused-suppression: warning\n",
	"mud.dgd":        "include_dirs = ({ \"/include\" });\nauto_object = \"/sys/auto\";\n",
	"include/std.h":  "#define STD \"/std/base\"\n",
	"sys/auto.c":     "void autof() {}\n",
	"std/base.c":     "void create() {}\nint foo(int a) { return a; }\nstatic void sfn() {}\n",
	"std/mixin.c":    "void go() { this_object()->provided(); call_out(\"later\", 1); }\n",
	"obj/a.c":        "#include <std.h>\ninherit STD;\nvoid create() { \"/obj/b\"->nofn(); \"/obj/b\"->sfn(); this_object()->foo(1,2); call_out(\"cb\", 1); }\n",
	"obj/b.c":        "inherit \"/std/base\";\nvoid create() { /* blåbær \U0001F600 */ \"/obj/b\"->sfn(); \"€\" \"å\"; a == 1; }\n",
	"obj/c.c":        "void x() { \"/obj/missing\"->f(); \"/sys/auto\"->autof(1); } /* dgdtools-ignore lint/sscanf-format */\n",
	"obj/d.c":        "inherit \"/std/mixin\";\nvoid provided() {}\n",
	"obj/clean.c":    "void f() {}\n",
	"obj/unparsed.c": "void f() { if (x { }\n",
}

// The command over its standard streams: what the server publishes for
// each file of a lib is what `dgdtools lint --output-format json` says
// of it, finding for finding, in both position encodings.
func TestLSPMatchesLint(t *testing.T) {
	dir := tree(t, lspLib)
	type finding struct {
		Code, Message, Severity string
		Start, End              lsp.Position
		Tags                    []string
		Related                 []string
	}
	type report struct {
		Diagnostics []struct {
			Rule, Code, Message, Severity, Path string
			Tags                                []string
			Range                               struct {
				Offset    int
				EndOffset int `json:"end_offset"`
			}
			Related []struct {
				Path, Message string
				Line          int
			}
		}
	}
	for _, enc := range [][]string{nil, {lsp.UTF8}} {
		c, stderr := server(t)
		c.Initialize(lsptest.Capabilities{Encodings: enc}, dir)
		for rel, text := range lspLib {
			if strings.HasSuffix(rel, ".c") {
				c.Open(filepath.Join(dir, rel), text)
			}
		}
		for rel, text := range lspLib {
			if !strings.HasSuffix(rel, ".c") {
				continue
			}
			path := filepath.Join(dir, rel)
			code, out, errs := dgdtools(t, "", "lint", "--output-format", "json", "-q", path)
			require.Contains(t, []int{0, 1}, code, errs)
			var rep report
			require.NoError(t, json.Unmarshal([]byte(out), &rep))
			want := []finding{}
			for _, d := range rep.Diagnostics {
				f := finding{Code: d.Code, Message: d.Message, Severity: d.Severity, Tags: d.Tags,
					Start: lsptest.PositionOf(text, d.Range.Offset, c.Encoding), End: lsptest.PositionOf(text, d.Range.EndOffset, c.Encoding)}
				for _, r := range d.Related {
					f.Related = append(f.Related, fmt.Sprintf("%s:%d %s", filepath.Base(r.Path), r.Line, r.Message))
				}
				want = append(want, f)
			}
			got := []finding{}
			for _, d := range c.Diagnostics(path).Diagnostics {
				f := finding{Code: d.Code, Message: d.Message, Start: d.Range.Start, End: d.Range.End,
					Severity: map[int]string{1: "error", 2: "warning", 3: "info"}[d.Severity]}
				for _, tag := range d.Tags {
					f.Tags = append(f.Tags, map[int]string{1: "unnecessary", 2: "deprecated"}[tag])
				}
				for _, r := range d.RelatedInformation {
					f.Related = append(f.Related, fmt.Sprintf("%s:%d %s", filepath.Base(r.Location.URI), r.Location.Range.Start.Line+1, r.Message))
				}
				assert.Equal(t, lint.RuleByName(d.Code).URL("dev"), d.CodeDescription.Href)
				got = append(got, f)
			}
			assert.Equal(t, want, got, "%s (%s)", rel, c.Encoding)
		}
		// Every rule the lib is there to show was shown.
		var seen []string
		for _, m := range c.Received("textDocument/publishDiagnostics") {
			var p lsp.PublishDiagnosticsParams
			require.NoError(t, json.Unmarshal(m.Params, &p))
			for _, d := range p.Diagnostics {
				seen = append(seen, lint.RuleByName(d.Code).Name)
			}
		}
		sort.Strings(seen)
		assert.Subset(t, seen, []string{"adjacent-strings", "callable-not-found", "callback-arity", "no-effect-statement",
			"static-crossobj", "syntax-error", "target-object-missing", "unused-suppression"})
		assert.NoError(t, c.Shutdown(), "exit code 0")
		assert.Empty(t, stderr.String())
	}
}

func TestLSPCommand(t *testing.T) {
	// A usage error is said on standard error, before the server runs.
	code, out, errs := dgdtools(t, "", "lsp", "--trace")
	assert.Equal(t, 2, code)
	assert.Empty(t, out)
	assert.Contains(t, errs, "--trace needs --log")
	code, _, errs = dgdtools(t, "", "lsp", "extra")
	assert.Equal(t, 2, code)
	assert.Contains(t, errs, "unexpected argument")
	code, _, errs = dgdtools(t, "", "lsp", "-h")
	assert.Equal(t, 0, code)
	assert.Contains(t, errs, "usage: dgdtools lsp")

	// The editor goes away without a word: exit code 1, and nothing on
	// standard error or standard output.
	code, out, errs = dgdtools(t, "", "lsp")
	assert.Equal(t, 1, code)
	assert.Empty(t, out+errs)

	// --log has what the server did, and with --trace the messages;
	// --stdio, which some clients add, is what the server does anyway.
	dir := tree(t, lspLib)
	logFile := filepath.Join(t.TempDir(), "lsp.log")
	c, stderr := server(t, "--stdio", "--log", logFile, "--trace")
	c.Initialize(lsptest.Capabilities{}, dir)
	c.Open(filepath.Join(dir, "obj/a.c"), lspLib["obj/a.c"])
	require.NotEmpty(t, c.Diagnostics(filepath.Join(dir, "obj/a.c")).Diagnostics)
	assert.ErrorContains(t, c.Exit(), "exit code 1", "exit without shutdown")
	assert.Empty(t, stderr.String())
	log, err := os.ReadFile(logFile)
	require.NoError(t, err)
	for _, want := range []string{"msg=initialized", "msg=lint", "textDocument/didOpen", "textDocument/publishDiagnostics"} {
		assert.Contains(t, string(log), want)
	}
}
