package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/rename"
	"github.com/mmcdole/dgdtools/pkg/report"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

const lintUsage = `usage: dgdtools lint [flags] [path ...]
       dgdtools lint [flags] -

Lints the named files and the .c files under the named directories: the
objects, with the headers they include read as part of them. A header
named by itself is linted on its own. With no path, the working
directory. Either way the project is the nearest dgdtools.yml at or
above the paths, and the files are checked against its whole mudlib.
Without a dgdtools.yml only file-local rules run. A wizard
with only their own folder puts "profile: NAME" in a dgdtools.yml there
(or passes --profile).

"-" lints standard input. With --stdin-filename it is linted as that
file, in that file's project, in place of what is on disk (an editor's
unsaved buffer); without, it has no place in a mudlib and file-local rules
run.

--fix applies the safe fixes (those that keep what the code means, as an
editor's "fix all" does), linting again until none is left, writes the
files and reports what is left; --fix --diff prints the fixes as diffs
and writes nothing (exit 1 when there are some). With "-", --fix prints
the text fixed and reports on standard error.

Exit codes: 0 no findings at or above the fail-on severity, 1 findings,
2 usage or configuration error, or an internal error on some file. A file
that cannot be read is a warning.
`

// lintCmd is `dgdtools lint`: pkg/lint run over the paths, pkg/report
// for the output.
func lintCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var (
		project                    projectFlags
		format, output, stdinName  string
		sel, ignore, enable        string
		failOn, baseline           string
		writeBaseline, noBaseline  bool
		local, stats, quiet, exit0 bool
		fix, diff                  bool
		maxPerFile                 int
	)
	fs := newFlags("lint", lintUsage, stderr)
	project.register(fs)
	fs.StringVar(&format, "output-format", "human", "write findings in this `format`: "+strings.Join(report.Formats, ", "))
	fs.StringVar(&output, "output", "", "write findings to `file` instead of standard output")
	fs.StringVar(&output, "o", "", "short for --output")
	fs.StringVar(&stdinName, "stdin-filename", "", "lint standard input as the file `path` (which need not exist): its project, its settings, its name in the output")
	fs.StringVar(&sel, "select", "", "run only these `rules` (names, codes, groups or a group's letter, comma-separated)")
	fs.StringVar(&enable, "enable", "", "also run these `rules`")
	fs.StringVar(&ignore, "ignore", "", "do not run these `rules`")
	fs.StringVar(&failOn, "fail-on", "", "exit 1 at this `severity` or above: info, warning, error (default error)")
	fs.StringVar(&baseline, "baseline", "", "baseline `file` (default "+lint.BaselineFileName+" next to the config)")
	fs.BoolVar(&writeBaseline, "write-baseline", false, "record the current findings as the baseline and exit 0")
	fs.BoolVar(&noBaseline, "no-baseline", false, "report findings that are in the baseline too")
	fs.BoolVar(&local, "local", false, "file-local rules only; do not use the mudlib index")
	fs.BoolVar(&stats, "stats", false, "print per-rule counts and what could not be verified")
	fs.IntVar(&maxPerFile, "max-per-file", 0, "report at most `n` findings per rule per file (0: no limit)")
	fs.BoolVar(&quiet, "quiet", false, "no summary on standard error")
	fs.BoolVar(&quiet, "q", false, "short for --quiet")
	fs.BoolVar(&exit0, "exit-zero", false, "exit 0 even when there are findings")
	fs.BoolVar(&fix, "fix", false, "apply the safe fixes, and report what is left")
	fs.BoolVar(&diff, "diff", false, "with --fix: print the fixes as unified diffs and write nothing")
	paths, code, done := parse(fs, args)
	if done {
		return code
	}
	fail := func(err error) int { return failure(stderr, "lint", err) }

	p, err := environment()
	if err != nil {
		return fail(err)
	}
	stdinPath, fromStdin, err := p.standardInput(paths, stdinName)
	switch {
	case err != nil:
		return fail(err)
	case diff && !fix:
		return fail(errors.New("--diff shows what --fix would do: give both"))
	case fix && writeBaseline:
		return fail(errors.New("--fix and --write-baseline cannot be combined"))
	case fromStdin && writeBaseline:
		return fail(errors.New("--write-baseline records the findings of files, not of standard input"))
	case fromStdin:
		// The text takes the file's place: the linter reads it wherever
		// it would have read the file.
		src, err := io.ReadAll(stdin)
		if err != nil {
			return fail(err)
		}
		local = local || stdinName == "" // nameless, it is no object of any lib
		buffer := vfs.NewOverlay(p.fsys)
		buffer.Set(stdinPath, src)
		p.fsys, paths = buffer, []string{stdinPath}
	case len(paths) == 0:
		// The working directory, as ruff and biome have it; the project
		// is still found upward, and its whole lib read for what files
		// say of each other.
		paths = []string{p.wd}
	default:
		for i, path := range paths {
			paths[i] = p.abs(path)
		}
	}
	// Fixes are made in an overlay, which each pass lints again; the
	// files are written, or their diffs printed, at the end.
	var fixed *vfs.Overlay
	if fix {
		fixed = vfs.NewOverlay(p.fsys)
		p.fsys = fixed
	}
	l, err := newLinter(p, &project, paths, lint.LintOnly())
	if err != nil {
		return fail(err)
	}
	// The severity that fails the run is the command line's to say.
	threshold := diag.Error
	if failOn != "" {
		var ok bool
		if threshold, ok = diag.ParseSeverity(failOn); !ok {
			return fail(fmt.Errorf("--fail-on: unknown severity %q (want info, warning or error)", failOn))
		}
	}
	if baseline != "" {
		baseline = p.abs(baseline)
	}
	if writeBaseline {
		maxPerFile = 0 // a baseline holds every finding
	}
	req := lint.Request{Paths: paths, Options: lint.Options{
		Select:     split(sel),
		Enable:     split(enable),
		Disable:    split(ignore),
		Local:      local,
		Baseline:   baseline,
		NoBaseline: noBaseline || writeBaseline,
		MaxPerFile: maxPerFile,
	}}
	res, err := l.Run(ctx, req)
	if err != nil {
		return fail(err)
	}
	var fixes fixRun
	if fix {
		if fixes, res, err = fixAll(ctx, l, fixed, req, res); err != nil {
			return fail(err)
		}
	}
	if fromStdin && stdinName == "" {
		// The note below says it of standard input, and how to say where
		// it belongs.
		res.Notes = slices.DeleteFunc(res.Notes, func(n diag.Note) bool { return n.Kind == diag.NoteOutsideLib })
	}

	if writeBaseline {
		if crash := internalError(res); crash != nil {
			return fail(fmt.Errorf("%w; no baseline written", crash))
		}
		path := baseline
		if path == "" {
			path = l.BaselinePath()
		}
		if path == "" {
			return fail(fmt.Errorf("--write-baseline needs a dgdtools.yml (or --baseline FILE) to anchor the baseline"))
		}
		data, err := lint.NewBaseline(path, res.Diagnostics).Marshal()
		if err != nil {
			return fail(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fail(err)
		}
		if !quiet {
			fmt.Fprintf(stderr, "wrote %s to %s\n", rename.Plural(len(res.Diagnostics), "finding"), path)
		}
		return 0
	}

	if fix && (diff || fromStdin) {
		// The fixes are the output: the diffs, or the text fixed.
		if err := fixes.show(stdout, fixed, p, diff, stdinPath); err != nil {
			return fail(err)
		}
		if diff {
			if !quiet {
				fmt.Fprintf(stderr, "%s would be made in %s (--fix without --diff makes them)\n",
					fixCount(fixes.n), rename.Plural(len(fixes.changed), "file"))
			}
			if len(fixes.changed) > 0 && !exit0 {
				return 1
			}
			return 0
		}
		stdout = stderr // what is left of standard input's findings
	} else if fix {
		if err := fixes.write(fixed); err != nil {
			return fail(err)
		}
	}
	out := stdout
	if output != "" {
		f, err := os.Create(output)
		if err != nil {
			return fail(err)
		}
		defer f.Close()
		out = f
	}
	// Paths are written as the user would write them: from the working
	// directory.
	ropt := report.Options{BaseDir: p.wd, Version: version}
	if err := report.Write(out, format, res, l.Rules(), ropt); err != nil {
		return fail(err)
	}
	if !quiet {
		if fromStdin && stdinName == "" && l.Project().HasLib() {
			fmt.Fprintln(stderr, "note: standard input has no place in the mudlib: file-local rules only (--stdin-filename says where it belongs)")
		}
		report.Summary(stderr, res, ropt, stats)
		if fix && !diff && fixes.n > 0 {
			fmt.Fprintf(stderr, "%s made in %s\n", fixCount(fixes.n), rename.Plural(len(fixes.changed), "file"))
		}
	}
	if crash := internalError(res); crash != nil && quiet {
		fail(crash) // the summary would have said it
	}
	return exitCode(res, threshold, exit0)
}

// internalError returns the first file the tools crashed on, nil when
// there is none.
func internalError(res *lint.Result) error {
	for _, e := range res.Errors {
		if e.Kind == lint.Internal {
			return e
		}
	}
	return nil
}

// exitCode is what a run comes to. A crash of the tools is an error of
// the run whatever was found and whatever was asked; a file that could
// not be read is a warning, and the rest of the run stands.
func exitCode(res *lint.Result, threshold diag.Severity, exitZero bool) int {
	if internalError(res) != nil {
		return 2
	}
	if exitZero {
		return 0
	}
	for _, d := range res.Diagnostics {
		if d.Severity >= threshold {
			return 1
		}
	}
	return 0
}

// newLinter discovers the one project of the paths and makes its linter.
func newLinter(p process, project *projectFlags, paths []string, opts ...lint.LinterOption) (*lint.Linter, error) {
	prj, err := config.Discover(p.fsys, paths, project.discover(p))
	if err != nil {
		return nil, err
	}
	return lint.New(prj, p.fsys, opts...)
}

func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// fixRun is what --fix made: how many fixes, and the files they changed
// with their texts before.
type fixRun struct {
	n       int
	changed map[string][]byte
}

// fixAll applies the safe fixes of a run's findings in the overlay and
// lints the files again, until no safe fix is left (a fix that would
// change what another changes waits for the next pass) or ten passes are
// made. It returns what was fixed and the last run.
func fixAll(ctx context.Context, l *lint.Linter, fs *vfs.Overlay, req lint.Request, res *lint.Result) (fixRun, *lint.Result, error) {
	run := fixRun{changed: map[string][]byte{}}
	for pass := 0; pass < 10; pass++ {
		byPath := map[string][]diag.Diagnostic{}
		for _, d := range res.Diagnostics {
			byPath[d.Path] = append(byPath[d.Path], d)
		}
		var touched []string
		for _, path := range slices.Sorted(maps.Keys(byPath)) {
			edits, n := diag.SafeEdits(byPath[path])
			if n == 0 {
				continue
			}
			src, err := fs.ReadFile(path)
			if err != nil {
				return run, nil, err
			}
			out, ok := diag.Apply(src, edits)
			if !ok {
				return run, nil, fmt.Errorf("%s: the fixes overlap (please report)", path)
			}
			if _, ok := run.changed[path]; !ok {
				run.changed[path] = src
			}
			fs.Set(path, out)
			touched = append(touched, path)
			run.n += n
		}
		if len(touched) == 0 {
			break
		}
		l.Invalidate(touched...)
		var err error
		if res, err = l.Run(ctx, req); err != nil {
			return run, nil, err
		}
	}
	return run, res, nil
}

// write writes the files the fixes changed.
func (r fixRun) write(fs *vfs.Overlay) error {
	for _, path := range slices.Sorted(maps.Keys(r.changed)) {
		out, err := fs.ReadFile(path)
		if err != nil {
			return err
		}
		if err := writeFile(path, out); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

// show prints the fixes: as unified diffs of the files they change, or,
// without diff, the text of standard input (stdin), fixed.
func (r fixRun) show(w io.Writer, fs *vfs.Overlay, p process, diff bool, stdin string) error {
	if !diff {
		out, err := fs.ReadFile(stdin)
		if err == nil {
			_, err = w.Write(out)
		}
		return err
	}
	for _, path := range slices.Sorted(maps.Keys(r.changed)) {
		out, err := fs.ReadFile(path)
		if err != nil {
			return err
		}
		name := path
		if rel, err := filepath.Rel(p.wd, path); err == nil && !strings.HasPrefix(rel, "..") {
			name = filepath.ToSlash(rel)
		}
		text, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A: diffLines(r.changed[path]), FromFile: name, B: diffLines(out), ToFile: name, Context: 3})
		if err != nil {
			return err
		}
		io.WriteString(w, text)
	}
	return nil
}

// fixCount says how many fixes there are: "1 fix", "2 fixes".
func fixCount(n int) string {
	if n == 1 {
		return "1 fix"
	}
	return fmt.Sprintf("%d fixes", n)
}
