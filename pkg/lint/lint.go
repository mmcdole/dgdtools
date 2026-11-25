// Package lint is the linter as a library: give it a project and a file
// system, ask it to lint, get diagnostics back.
//
//	fs := vfs.NewOverlay(vfs.OS{})
//	prj, err := config.Discover(fs, paths, config.DiscoverOptions{WorkDir: cwd})
//	l, err := lint.New(prj, fs)
//	res, err := l.Run(ctx, lint.Request{Paths: paths})   // many files
//	doc, err := l.Lint(ctx, path, lint.Options{})        // one document
//
// The package never prints, never exits, reads no environment or working
// directory (paths are absolute), and keeps no package-level mutable
// state. A Linter can live as long as its project's configuration: it
// is told about changed files with Invalidate, which starts a new
// generation of its index, and every Run or Lint reads one generation
// from start to end, so linting and editing never wait for each other.
// An editor server holds its linters, one per project, through
// session.Session, which also keeps the unsaved buffers and replaces a
// linter when its project's configuration changes. Fixes are returned as
// text edits on diagnostics and are never applied here.
//
// It is also the front ends' one door to the analysis: a Linter owns the
// project's index, and the Document a lint returns carries what the
// packages below answer of that file (Resolve, Hover, References,
// Complete, Tokens, PrepareRename and Rename, over pkg/resolve and
// pkg/rename), so that a command and the editor server ask the same
// questions of the same generation. There is no separate "analysis"
// package; this is it.
package lint

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mmcdole/dgdtools/internal/par"
	"github.com/mmcdole/dgdtools/pkg/calls"
	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/fileset"
	"github.com/mmcdole/dgdtools/pkg/format"
	"github.com/mmcdole/dgdtools/pkg/index"
	"github.com/mmcdole/dgdtools/pkg/kfun"
	"github.com/mmcdole/dgdtools/pkg/pp"
	"github.com/mmcdole/dgdtools/pkg/rename"
	"github.com/mmcdole/dgdtools/pkg/resolve"
	"github.com/mmcdole/dgdtools/pkg/syntax"
	"github.com/mmcdole/dgdtools/pkg/token"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// Rule is one lint rule. Name and Code are stable identifiers: either may
// be used in configuration, on the command line and in suppression
// comments.
type Rule struct {
	Code  string // "E001"
	Name  string // "syntax-error"
	Group string // syntax | runtime | suspicious | convention
	// Doc is a one-line summary; Explain is the long form shown by
	// `dgdtools explain`.
	Doc     string
	Explain string
	Default bool
	// Severity is the default severity.
	Severity diag.Severity
	// CrossFile rules need the lib index and do not run in file-local
	// mode.
	CrossFile bool
	// CompiledOnly says the rule's findings are claims about code the
	// driver compiles: this does not compile, this fails when it runs.
	// In a conditional branch that was chosen by guess, where it is not
	// known whether the code is compiled at all, they are withheld. A
	// rule about the text as it stands says false.
	CompiledOnly bool
	// Tags are put on every finding of the rule.
	Tags []diag.Tag
	// Late rules are about the lint itself, not the code: they run after
	// the other rules of the file, and see its suppression comments and
	// which of them found something to suppress. What they report is not
	// itself subject to suppression comments.
	Late bool
	// Options returns the rule's options at their defaults: a pointer to
	// a struct whose fields carry the option's key as yaml tag and what
	// it is for as doc tag. Those are the keys the rule's mapping in
	// lint.rules may have besides severity; a rule
	// without Options has none. Run finds the file's options in
	// Pass.Options.
	Options func() any
	Run     func(*Pass)
}

// release is the version of a release, which has a tag of that name: not
// a development build ("dev"), a snapshot, or a build past a tag.
var release = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta|rc)[.0-9]*)?$`)

// DocsURL returns where the rules are documented, one section each, for
// a build of the given version: a release links to its own tag, any
// other build to main.
func DocsURL(version string) string {
	ref := "main"
	if release.MatchString(version) {
		ref = version
	}
	return "https://github.com/mmcdole/dgdtools/blob/" + ref + "/docs/rules.md"
}

// URL returns the address of the rule's documentation for a build of
// the given version.
func (r *Rule) URL(version string) string { return DocsURL(version) + "#" + r.Name }

// Pass is the per-file, per-rule context handed to Rule.Run.
type Pass struct {
	Ctx  context.Context
	File *token.File
	Tree *syntax.File
	// Env is the file's #define environment.
	Env  *pp.Env
	Unit *index.Unit
	// Index answers cross-file questions on the file's behalf.
	Index   *index.Query
	LibPath string // "" when the file has no lib path
	Config  *config.Config
	// Options are the rule's options for this file: what Rule.Options
	// returned, with what the configuration says set.
	Options any
	// Format are the formatter's options for this file.
	Format format.Options

	shared   *fileShared
	registry *calls.Registry
	rule     *Rule
	severity diag.Severity
	out      *[]*diag.Diagnostic
}

// fileShared caches per-file derived facts across rules. One file is
// linted by one goroutine.
type fileShared struct {
	calls *callFacts
	stats fileStats
	// sup are the file's suppression comments, and ran the names of the
	// rules that have run on it: what a Late rule looks at.
	sup *suppressions
	ran map[string]bool
}

// Report records a finding covering the byte range [start, end) and
// returns it, for the rule to add related locations and fixes to.
func (p *Pass) Report(start, end uint32, format string, args ...any) *diag.Diagnostic {
	if end < start {
		end = start
	}
	a, b := p.File.Pos(start), p.File.Pos(end)
	d := &diag.Diagnostic{
		Path: p.File.Path, Line: a.Line, Col: a.Col, EndLine: b.Line, EndCol: b.Col,
		Start: start, End: end,
		Severity: p.severity, Rule: p.rule.Name, Code: p.rule.Code, Tags: p.rule.Tags,
		Message: fmt.Sprintf(format, args...),
	}
	*p.out = append(*p.out, d)
	return d
}

// guessed reports whether byte offset off lies in a conditional branch
// chosen by the first-branch rule.
func guessed(tree *syntax.File, off uint32) bool {
	toks := tree.Tok.Tokens
	i := sort.Search(len(toks), func(i int) bool { return toks[i].End > off })
	return i < len(toks) && tree.Sel.Guessed(i)
}

// ReportNode records a finding covering a syntax node.
func (p *Pass) ReportNode(n syntax.Node, format string, args ...any) *diag.Diagnostic {
	return p.Report(p.Tree.Off(n), p.Tree.End(n), format, args...)
}

// Related builds a related location in the file being linted.
func (p *Pass) Related(start, end uint32, msg string) diag.Related {
	a, b := p.File.Pos(start), p.File.Pos(end)
	return diag.Related{
		Path: p.File.Path, Line: a.Line, Col: a.Col, EndLine: b.Line, EndCol: b.Col,
		Start: start, End: end, Message: msg,
	}
}

// Options select what a lint does. The zero value runs the configured
// rules, applies the baseline, and reads whatever it needs.
type Options struct {
	// Select, when non-empty, replaces the configured rule set: only the
	// named rules (names, codes or groups) run.
	Select []string
	// Enable and Disable adjust the configured set.
	Enable, Disable []string
	// Local restricts the lint to file-local rules; the index is not used.
	Local bool
	// Baseline is the baseline file to subtract, an absolute path,
	// instead of the project's (Linter.BaselinePath).
	Baseline string
	// NoBaseline reports findings recorded in the baseline too.
	NoBaseline bool
	// MaxPerFile reports at most that many findings of one rule in one
	// file; 0 or a negative number is no limit. What the limit holds
	// back is counted (Stats.Capped).
	MaxPerFile int
	// NoScan keeps the lint from starting the whole-lib scan, which the
	// first question about who inherits a file needs and which takes
	// seconds on a big lib. While the scan has not completed such
	// questions are unknown and nothing is concluded from them; NeedsScan
	// on the result says there were some. A server sets this, runs
	// Linter.Scan in the background, and lints again when it is done.
	NoScan bool
}

// Request describes one lint run.
type Request struct {
	// Paths are the files and directories to lint, as absolute paths.
	// Empty means the whole lib (or the whole checkout).
	Paths []string
	Options
}

// Stats summarizes a run, including what could not be verified.
type Stats struct {
	Files      int `json:"files"`
	Suppressed int `json:"suppressed"` // by dgdtools-ignore comments
	Baselined  int `json:"baselined"`
	Capped     int `json:"capped"` // over the MaxPerFile limit; not reported

	// Unknowns: facts the linter could not establish and therefore drew
	// no conclusions from.
	UnresolvedInherits int `json:"unresolved_inherits"`
	UnknownIncludes    int `json:"unknown_includes"`
	PartialFiles       int `json:"partial_files"` // symbol table incomplete
	CallSites          int `json:"call_sites"`
	UnknownReceivers   int `json:"unknown_receivers"`
	TaintedSyntax      int `json:"tainted_syntax_errors"`   // possibly macro artifacts; not reported
	GuessedBranch      int `json:"guessed_branch_findings"` // in #if branches that could not be decided; not reported
}

// fileStats are one file's contribution to Stats.
type fileStats struct {
	suppressed, baselined, capped                int
	callSites, unknownReceivers                  int
	tainted, guessed                             int
	unresolvedInherits, unknownIncludes, partial int
}

func (st *Stats) add(f fileStats) {
	st.Files++
	st.Suppressed += f.suppressed
	st.Baselined += f.baselined
	st.Capped += f.capped
	st.CallSites += f.callSites
	st.UnknownReceivers += f.unknownReceivers
	st.TaintedSyntax += f.tainted
	st.GuessedBranch += f.guessed
	st.UnresolvedInherits += f.unresolvedInherits
	st.UnknownIncludes += f.unknownIncludes
	st.PartialFiles += f.partial
}

// Result is the outcome of a run.
type Result struct {
	Diagnostics []diag.Diagnostic
	Stats       Stats
	// Notes are remarks about the run as a whole (no lib, incomplete lib,
	// ...).
	Notes []diag.Note
	// Errors are the files that were not linted, the rules that crashed
	// on a file, and the files whose analysis crashed while another was
	// being linted, in path order.
	Errors []*FileError
	// Generation is the index generation the run read; see
	// Linter.Generation.
	Generation uint64
	// NeedsScan is set when Options.NoScan left questions unanswered.
	NeedsScan bool
}

// FileError says why a file was not linted.
type FileError struct {
	Path string
	Kind ErrorKind
	// Message says what happened, without the path.
	Message string
}

func (e *FileError) Error() string { return e.Path + ": " + e.Message }

// ErrorKind tells what went wrong with a file.
type ErrorKind uint8

const (
	// Unreadable: the file could not be read. The run is otherwise
	// good.
	Unreadable ErrorKind = iota
	// Internal: the tools crashed on the file. That is a bug in them;
	// what the run says about the rest of the lib may be missing
	// something.
	Internal
	// Stale: the file's text changed under the lint, which would have
	// had to mix what it knew of the old text with the new. Lint again
	// (after Invalidate, when the linter has not been told).
	Stale
)

func (k ErrorKind) String() string {
	switch k {
	case Internal:
		return "internal"
	case Stale:
		return "stale"
	}
	return "unreadable"
}

// fileError makes the FileError of an error met while linting file.
func fileError(file string, err error) *FileError {
	var crash *index.InternalError
	if errors.As(err, &crash) {
		return &FileError{Path: crash.Path, Kind: Internal, Message: "internal error (please report): " + crash.Panic}
	}
	if errors.Is(err, index.ErrChanged) {
		return &FileError{Path: file, Kind: Stale, Message: "changed while it was being linted"}
	}
	msg := err.Error()
	var pe *fs.PathError
	if errors.As(err, &pe) {
		msg = pe.Err.Error()
	}
	return &FileError{Path: file, Kind: Unreadable, Message: "cannot be read: " + msg}
}

// Document is one linted file: its diagnostics together with what they
// were computed from, so a caller that wants the tree (an editor server
// answering hover, symbols, semantic tokens) need not parse again.
type Document struct {
	Path string
	// Snapshot is the generation of the index the document was linted
	// against, for further questions about the lib as the diagnostics
	// saw it; Generation is its number. See Linter.Generation.
	Snapshot   *index.Snapshot
	Generation uint64
	File       *token.File
	Tree       *syntax.File
	// Env is the file's #define environment: the macros visible in it.
	Env *pp.Env
	// Unit is the file's entry in the index.
	Unit        *index.Unit
	Diagnostics []diag.Diagnostic
	// NeedsScan is set when Options.NoScan left questions unanswered:
	// lint again after Linter.Scan.
	NeedsScan bool
	// Errors are the rules that crashed on the file, one internal error
	// each. Their findings are missing; everything else stands.
	Errors []*FileError

	stats    fileStats
	resolver *resolve.Resolver
	// markers are the project's autosave markers, and create the function
	// the driver initializes an object by, for rename.
	markers []string
	create  string
	driver  string

	tokensOnce sync.Once
	tokens     []resolve.SemanticToken
}

// Tokens returns the semantic tokens of the document (the document's
// resolve.Resolver's Tokens): what analysis knows of its names that a
// grammar cannot. They are made once, on first need, and shared; the
// caller must not modify them.
func (d *Document) Tokens() []resolve.SemanticToken {
	d.tokensOnce.Do(func() {
		if d.resolver != nil {
			d.tokens = d.resolver.Tokens()
		}
	})
	return d.tokens
}

// Resolve says what the token at byte offset off of the document refers
// to, in the lib as the document's diagnostics saw it (the document's
// resolve.Resolver's At): what is named there, and where that is
// declared. It is nil when that is nothing the tools can name.
func (d *Document) Resolve(off uint32) *resolve.Ref {
	if d.resolver == nil {
		return nil
	}
	return d.resolver.At(off)
}

// Hover says what is known of what the token at byte offset off of the
// document names: the declaration, where it is, the comment above it
// (the document's resolve.Resolver's Hover). It is nil when that is
// nothing.
func (d *Document) Hover(off uint32) *resolve.Hover {
	if d.resolver == nil {
		return nil
	}
	return d.resolver.Hover(off)
}

// Signature says what the call takes whose argument list byte offset
// off of the document is in (the document's resolve.Resolver's
// Signature). It is nil when off is in none, or nothing is known of the
// function called.
func (d *Document) Signature(off uint32) *resolve.SignatureHelp {
	if d.resolver == nil {
		return nil
	}
	return d.resolver.Signature(off)
}

// References finds the places that name what the token at byte offset
// off of the document names (the document's resolve.Resolver's
// References): in the document for a local, a parameter or a label, in
// the whole lib for anything else, which needs the whole-lib scan
// (Linter.Scan). With decl the declarations are among them. It is nil
// where nothing declared is named.
func (d *Document) References(ctx context.Context, off uint32, decl bool) *resolve.References {
	if d.resolver == nil {
		return nil
	}
	return d.resolver.References(ctx, off, decl)
}

// Highlights returns the places of the document that name what the
// token at byte offset off names, each with how it uses a variable (the
// document's resolve.Resolver's Highlights).
func (d *Document) Highlights(off uint32) []resolve.Site {
	if d.resolver == nil {
		return nil
	}
	return d.resolver.Highlights(off)
}

// PrepareRename says what can be renamed at byte offset off of the
// document (rename.Prepare): the symbol and the place of its name, or a
// *rename.Refusal that says why it cannot be. A symbol other files could
// name needs the whole-lib scan, which it runs for as long as ctx
// allows.
func (d *Document) PrepareRename(ctx context.Context, off uint32) (*rename.Target, error) {
	if d.resolver == nil {
		return nil, nil
	}
	return rename.Prepare(ctx, d.RenameFile(false), off)
}

// Rename plans the renaming of what is named at byte offset off of the
// document to newName (rename.Rename): the occurrences and the edits, in
// the document and in every other file of the lib that names it, or a
// *rename.Refusal. With force, a refusal that a person may overrule (a
// variable save_object writes, rename.Refusal.Forcible) becomes a
// warning of the plan.
func (d *Document) Rename(ctx context.Context, off uint32, newName string, force bool) (*rename.Plan, error) {
	if d.resolver == nil {
		return nil, &rename.Refusal{Reason: "nothing here can be renamed"}
	}
	return rename.Rename(ctx, d.RenameFile(force), off, newName)
}

// RenameFile is the document as pkg/rename takes it (rename.File), with
// force for File.Force.
func (d *Document) RenameFile(force bool) rename.File {
	return rename.File{Resolver: d.resolver, AutosaveMarkers: d.markers, Create: d.create, DriverObject: d.driver, Force: force}
}

// Symbols lists the declarations of the document, for an outline of it
// (the document's resolve.Resolver's Symbols).
func (d *Document) Symbols() []resolve.Symbol {
	if d.resolver == nil {
		return nil
	}
	return d.resolver.Symbols()
}

// Complete says what can be written at byte offset off of the document
// (the document's resolve.Resolver's Complete); nil where that is
// nothing the tools can say.
func (d *Document) Complete(off uint32) *resolve.Completion {
	if d.resolver == nil {
		return nil
	}
	return d.resolver.Complete(off)
}

// Comment returns the comment above the declaration at byte offset off
// of the file at path, which is where an item of a completion says its
// declaration is (the document's resolve.Resolver's Comment).
func (d *Document) Comment(path string, off uint32) string {
	if d.resolver == nil {
		return ""
	}
	return d.resolver.Comment(path, off)
}

// Linter lints one project. It is safe for concurrent use.
type Linter struct {
	project *config.Project
	fs      vfs.FS
	index   *index.Index

	rules     []*Rule
	byKey     map[string][]*Rule // name, code, group, "all" -> rules
	base      *layer             // the project's lint.rules
	overrides []*layer           // the project's lint.overrides
	scopes    sync.Map           // *config.Scope -> *scopeLayers
	layerIDs  atomic.Int32
	registry  *calls.Registry
	kfuns     *kfun.Table

	baselines sync.Map // path -> *baselineFile
}

// LinterOption is an option of New.
type LinterOption func(*linterOptions)

type linterOptions struct{ lintOnly bool }

// LintOnly makes a linter for linting only: its index keeps no Uses, the
// places each file names other files' functions, variables, macros and
// objects at, which a search for references (Document.References) and a
// rename across files read; those find nothing in other files. No rule
// needs them, and reading them is a third of what a run over a whole lib
// allocates (on the VikingMUD lib some 140 MB kept, 4 GB allocated and a
// fifth of the time). The command line's lint is such a run.
func LintOnly() LinterOption { return func(o *linterOptions) { o.lintOnly = true } }

// New creates a linter for the project. It fails on a configuration that
// is not valid: what Config.Validate reports, and rule settings that name
// unknown rules or options.
func New(prj *config.Project, fsys vfs.FS, opts ...LinterOption) (*Linter, error) {
	var o linterOptions
	for _, opt := range opts {
		opt(&o)
	}
	l := &Linter{project: prj, fs: fsys, rules: Rules(), byKey: map[string][]*Rule{}}
	for _, r := range l.rules {
		l.byKey[r.Name] = []*Rule{r}
		l.byKey[strings.ToUpper(r.Code)] = []*Rule{r}
		l.byKey[r.Group] = append(l.byKey[r.Group], r)
		l.byKey["all"] = append(l.byKey["all"], r)
	}
	// A group's letter, the first of its rules' codes ("S" for
	// suspicious), names the group too, where it is one group's only.
	letters := map[string]string{}
	for _, r := range l.rules {
		letter := strings.ToUpper(r.Code[:1])
		if g, ok := letters[letter]; ok && g != r.Group {
			letters[letter] = ""
		} else if !ok {
			letters[letter] = r.Group
		}
	}
	for letter, g := range letters {
		if g != "" && l.byKey[letter] == nil {
			l.byKey[letter] = l.byKey[g]
		}
	}
	cfg := prj.Config
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var err error
	if l.base, err = l.newLayer(cfg.Path, "lint.rules", nil, cfg.Lint.Rules); err != nil {
		return nil, err
	}
	for i, o := range cfg.Lint.Overrides {
		lay, err := l.newLayer(cfg.Path, fmt.Sprintf("lint.overrides[%d].rules", i), fileset.NewMatcher(o.Paths), o.Rules)
		if err != nil {
			return nil, err
		}
		l.overrides = append(l.overrides, lay)
	}
	l.registry = calls.NewRegistry(cfg.Conventions)
	if l.kfuns, err = cfg.Kfuns(); err != nil {
		return nil, fmt.Errorf("dialect.kfuns: %w", err)
	}
	// Unless LintOnly, every unit keeps the places its file names other
	// files' functions, variables, macros and objects at, for a search for
	// references.
	var uses []index.Option
	if !o.lintOnly {
		uses = append(uses, index.WithUses(resolve.Extractor(l.registry)))
	}
	l.index = index.Open(prj, fsys, uses...)
	return l, nil
}

// Project returns the project the linter was made for.
func (l *Linter) Project() *config.Project { return l.project }

// Index returns the linter's index of the lib.
func (l *Linter) Index() *index.Index { return l.index }

func (l *Linter) lookup(key string) []*Rule {
	if rs := l.byKey[key]; rs != nil {
		return rs
	}
	return l.byKey[strings.ToUpper(key)]
}

// Invalidate tells the linter that the files at the given absolute paths
// changed, were created or were deleted. It returns the files whose
// diagnostics may now differ; see index.Index.Invalidate for exactly
// which. Each call starts a new generation.
func (l *Linter) Invalidate(paths ...string) []string { return l.index.Invalidate(paths...) }

// Generation returns the current generation of the linter's index. A
// Result or Document carries the generation it was computed against: if
// that is no longer the current one, files changed while it was being
// computed, it describes the lib as it was before, and nothing watches
// the lib on its behalf (Invalidate reports dependents of the current
// generation's lints only). A server lints such a document again.
func (l *Linter) Generation() uint64 { return l.index.Generation() }

// Scan runs the whole-lib scan that questions about who inherits a file
// need, unless it is already complete. It reports false when the scan did
// not complete (ctx cancelled, files changed meanwhile, no lib); see
// Options.NoScan.
func (l *Linter) Scan(ctx context.Context) bool { return l.index.Snapshot().Scan(ctx) }

// Run lints the requested paths against one generation of the index.
// Cancelling ctx stops the run early; the partial result is returned with
// ctx.Err().
func (l *Linter) Run(ctx context.Context, req Request) (*Result, error) {
	prj := l.project
	s := l.index.Snapshot()
	res := &Result{Generation: s.Generation()}
	res.Notes = append(res.Notes, prj.Notes...)
	if !prj.HasLib() {
		res.Notes = append(res.Notes, diag.Note{Kind: diag.NoteNoLib, Message: prj.NoLib() + ": running file-local rules only (cross-file rules need a mudlib root)"})
	}
	// Rule names and the baseline are checked once, up front.
	sel, err := l.selection(&req.Options)
	if err != nil {
		return nil, err
	}
	if _, err := l.baselineFor(&req.Options); err != nil {
		return nil, err
	}

	paths := req.Paths
	if len(paths) == 0 {
		switch {
		case !prj.HasLib():
			return nil, fmt.Errorf("nothing to lint: give a path, or run inside a project with dgdtools.yml")
		case prj.Partial:
			paths = []string{prj.Base} // the checkout: the rest of the lib is not on disk
		default:
			paths = []string{prj.Root}
		}
	}
	var files []string
	seen, nested := map[string]bool{}, map[string]int{}
	add := func(f string) {
		if !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("%s: not an absolute path", p)
		}
		p = filepath.Clean(p)
		switch {
		case !l.fs.Exists(p):
			return nil, fmt.Errorf("%s: no such file or directory", p)
		case l.fs.IsDir(p):
			list, err := s.Files(ctx, p)
			if err != nil {
				return res, err
			}
			// A directory with a root config of its own is another
			// project, with a lib and settings of its own to be linted
			// by: its files are part of this lib, and not of this run.
			for _, f := range list {
				if other := prj.Nested(f); other != "" {
					nested[other]++
				} else {
					add(f)
				}
			}
		default:
			add(p) // a file named explicitly is linted even if excluded
		}
	}
	for _, other := range slices.Sorted(maps.Keys(nested)) {
		res.Notes = append(res.Notes, diag.Note{Kind: diag.NoteNestedProject, Path: other, Message: fmt.Sprintf(
			"%s under %s %s a project of their own (%s) and %s not linted: lint that directory by itself",
			plural(nested[other], "file"), filepath.Dir(other), are(nested[other]), filepath.Base(other), were(nested[other]))})
	}
	if place, what := prj.Root, "mudlib root"; prj.HasLib() && (place != "" || prj.CheckoutLib != "") {
		if place == "" {
			place, what = prj.CheckoutDir, "checkout"
		}
		outside := 0
		for _, f := range files {
			if l.index.LibPath(f) == "" {
				outside++
			}
		}
		if outside > 0 {
			res.Notes = append(res.Notes, diag.Note{Kind: diag.NoteOutsideLib, Path: place, Message: fmt.Sprintf(
				"%s %s outside the %s %s: file-local rules only for %s", plural(outside, "file"), are(outside), what, place, verb(outside, "it", "those"))})
		}
	}

	// A question about who inherits a file needs every file of the lib
	// indexed. A run over the lib is about to index every file anyway, so
	// the first pass declines those questions instead of starting the
	// scan, and only the files that had one are linted again once the
	// scan is complete: each file is parsed once, a few of them twice.
	//
	// What a file comes to is kept until the run is over, as its findings
	// and counts only: the files that wait for the scan are anywhere in
	// the lib, and the results are in the order of the files.
	results := make([]fileResult, len(files))
	order := make([]int, len(files))
	for i := range order {
		order[i] = i
	}
	// A file that cannot be linted is the file's error, and the run goes
	// on. A nested configuration file that is not valid fails the run:
	// its files would otherwise be linted by settings nobody chose.
	var failed atomic.Pointer[error]
	lintInto := func(i int, opt *Options) {
		d, err := l.lintFile(ctx, s, files[i], sel, opt)
		var fe *FileError
		switch {
		case errors.As(err, &fe):
			results[i] = fileResult{err: fe}
		case err != nil:
			failed.CompareAndSwap(nil, &err)
		default:
			results[i] = fileResult{linted: true, diags: d.Diagnostics, errs: d.Errors, stats: d.stats, needsScan: d.NeedsScan}
		}
	}
	first := req.Options
	first.NoScan = true
	par.Each(ctx, order, func(i int) { lintInto(i, &first) })
	var again []int
	for i, r := range results {
		if r.needsScan {
			again = append(again, i)
		}
	}
	if len(again) > 0 && !req.NoScan && s.Scan(ctx) {
		par.Each(ctx, again, func(i int) { lintInto(i, &req.Options) })
		again = nil
	}
	if err := failed.Load(); err != nil {
		return nil, *err
	}
	res.NeedsScan = len(again) > 0

	n := 0
	for _, r := range results {
		n += len(r.diags)
	}
	reported := map[string]bool{}
	res.Diagnostics = make([]diag.Diagnostic, 0, n)
	for i := range results {
		r := &results[i]
		if r.err != nil {
			res.Errors = append(res.Errors, r.err)
			reported[r.err.Path] = true
		}
		if r.linted {
			res.Diagnostics = append(res.Diagnostics, r.diags...)
			res.Stats.add(r.stats)
			res.Errors = append(res.Errors, r.errs...)
		}
		*r = fileResult{} // in res now
	}
	// A file that crashed while it was read on another's behalf (a
	// parent, a call target) made that other's answers unknown.
	for _, crash := range s.Crashed() {
		if !reported[crash.Path] {
			res.Errors = append(res.Errors, fileError(crash.Path, crash))
		}
	}
	sort.SliceStable(res.Errors, func(i, j int) bool { return res.Errors[i].Path < res.Errors[j].Path })
	sort.SliceStable(res.Diagnostics, func(i, j int) bool {
		a, b := res.Diagnostics[i], res.Diagnostics[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		return a.Rule < b.Rule
	})
	if prj.HasLib() && !req.Local {
		res.Notes = append(res.Notes, s.Notes()...)
	}
	return res, ctx.Err()
}

// fileResult is what a run keeps of one file: the file's error when it
// was not linted, else its findings, the rules that crashed on it and its
// counts.
type fileResult struct {
	err       *FileError
	linted    bool
	needsScan bool
	diags     []diag.Diagnostic
	errs      []*FileError
	stats     fileStats
}

// Lint lints the file at path (absolute) against the current generation
// of the index and returns its diagnostics together with its tree. It
// fails with a *FileError when the file cannot be read or its analysis
// crashes; a rule that crashes is in Document.Errors, and the document
// has what the other rules found. Cancelling ctx stops the lint early; what was found until then
// is returned with ctx.Err().
func (l *Linter) Lint(ctx context.Context, path string, opt Options) (*Document, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%s: not an absolute path", path)
	}
	sel, err := l.selection(&opt)
	if err != nil {
		return nil, err
	}
	if _, err := l.baselineFor(&opt); err != nil {
		return nil, err
	}
	d, err := l.lintFile(ctx, l.index.Snapshot(), filepath.Clean(path), sel, &opt)
	if err != nil {
		return nil, err
	}
	return d, ctx.Err()
}

// lintFile lints one file against s, with the settings of that file. It
// fails with a *FileError when the file itself cannot be linted, and with
// another error when the file's settings cannot be told: a configuration
// file above it is not valid.
func (l *Linter) lintFile(ctx context.Context, s *index.Snapshot, file string, sel *selection, opt *Options) (doc *Document, err error) {
	// A bug met on one file must not take down a run over a hundred
	// thousand.
	defer func() {
		if r := recover(); r != nil {
			doc, err = nil, &FileError{Path: file, Kind: Internal, Message: fmt.Sprintf("internal error (please report): %v", r)}
		}
	}()
	parsed, err := s.Parse(file)
	if err != nil {
		return nil, fileError(file, err)
	}
	set, err := l.project.Settings(file)
	if err != nil {
		return nil, err
	}
	u := parsed.Unit
	doc = &Document{
		Path: file, Snapshot: s, Generation: s.Generation(),
		File: parsed.Tok, Tree: parsed.Tree, Env: parsed.Env, Unit: u,
	}
	doc.resolver = resolve.New(resolve.File{
		Tok: parsed.Tok, Tree: parsed.Tree, Env: parsed.Env, Unit: u, Snapshot: s, Registry: l.registry, Kfuns: l.kfuns,
	})
	doc.markers = l.project.Config.Conventions.AutosaveMarkers
	doc.create = l.project.Config.Create()
	doc.driver = strings.TrimSuffix(l.project.Config.DriverObject, ".c")
	rel, _ := l.project.Tree().Rel(file) // the file's lib-relative name
	rules, err := sel.rules(file, rel, set)
	if err != nil {
		return nil, err
	}
	query := s.Query(u, opt.NoScan)
	formatting := format.OptionsFor(l.project.Config, set.Format)
	shared := &fileShared{}
	stats := &shared.stats
	if u.Partial {
		stats.partial++
	}
	stats.unknownIncludes += u.UnknownIncludes
	for _, r := range u.Inherits {
		if !r.Resolved {
			stats.unresolvedInherits++
		}
	}
	for _, e := range parsed.Tree.Errors {
		if e.Tainted {
			stats.tainted++
		}
	}

	// What a rule finds is not yet what is reported. In this order: a
	// finding about code that may not be compiled is withheld; one that a
	// comment suppresses is dropped; the rest is what the file has. From
	// that the baseline is subtracted, and only then is a rule's limit
	// per file applied, so that a limit counts what would be shown.
	local := opt.Local || !l.project.HasLib() || u.LibPath == ""
	shared.sup, shared.ran = l.suppressions(parsed.Tok), map[string]bool{}
	var ds []diag.Diagnostic
	for _, late := range []bool{false, true} {
		for i := range rules {
			a, r := &rules[i], rules[i].rule
			if r.Late != late || (r.CrossFile && local) {
				continue
			}
			if ctx.Err() != nil {
				break
			}
			var out []*diag.Diagnostic
			pass := &Pass{
				Ctx: ctx, File: parsed.Tok, Tree: parsed.Tree, Env: parsed.Env,
				Unit: u, Index: query, LibPath: u.LibPath,
				Config: l.project.Config, Options: a.options, Format: formatting,
				shared: shared, registry: l.registry, rule: r, severity: a.severity, out: &out,
			}
			// A rule that crashes costs its own findings and no more.
			if crash := runRule(r, pass); crash != nil {
				doc.Errors = append(doc.Errors, &FileError{Path: file, Kind: Internal,
					Message: fmt.Sprintf("internal error in rule %s (please report): %v", r.Name, crash)})
				continue
			}
			shared.ran[r.Name] = true
			for _, d := range out {
				switch {
				case late:
					ds = append(ds, *d)
				case r.CompiledOnly && guessed(parsed.Tree, d.Start):
					stats.guessed++
				case shared.sup.suppressed(d.Rule, d.Line):
					stats.suppressed++
				default:
					ds = append(ds, *d)
				}
			}
		}
	}
	for i := range ds {
		ds[i].Fingerprint = lineFingerprint(parsed.Tok.Src, ds[i].Start)
	}
	sort.SliceStable(ds, func(i, j int) bool {
		if ds[i].Start != ds[j].Start {
			return ds[i].Start < ds[j].Start
		}
		return ds[i].Rule < ds[j].Rule
	})
	if bl, _ := l.baselineFor(opt); bl != nil {
		ds, stats.baselined = bl.Filter(ds)
	}
	shown, kept := map[string]int{}, ds[:0]
	for _, d := range ds {
		if limit := opt.MaxPerFile; limit > 0 && shown[d.Rule] == limit {
			stats.capped++
			continue
		}
		shown[d.Rule]++
		kept = append(kept, d)
	}
	doc.Diagnostics, doc.NeedsScan, doc.stats = kept, query.NeedsScan(), *stats
	return doc, nil
}

// runRule runs r and returns what it panicked with, nil when it did not.
func runRule(r *Rule, p *Pass) (crash any) {
	defer func() { crash = recover() }()
	r.Run(p)
	return nil
}

// Rules returns the linter's rule table.
func (l *Linter) Rules() []*Rule { return l.rules }

// EffectiveRules returns the rules that would run on the file at path
// (absolute; "" for the project's own answer, before any path is
// considered), with their effective severity.
func (l *Linter) EffectiveRules(path string) ([]*Rule, error) {
	var set *config.Settings
	rel := ""
	if path != "" {
		var err error
		if set, err = l.project.Settings(path); err != nil {
			return nil, err
		}
		rel, _ = l.project.Tree().Rel(path)
	}
	sel, err := l.selection(&Options{})
	if err != nil {
		return nil, err
	}
	rules, err := sel.rules(path, rel, set)
	if err != nil {
		return nil, err
	}
	var out []*Rule
	for _, a := range rules {
		c := *a.rule
		c.Severity = a.severity
		out = append(out, &c)
	}
	return out, nil
}

// rule returns the rule with the given name or code, or nil.
func (l *Linter) rule(key string) *Rule {
	rs := l.lookup(key)
	if len(rs) == 1 && (rs[0].Name == key || strings.EqualFold(rs[0].Code, key)) {
		return rs[0]
	}
	return nil
}

// RuleByName returns the rule of the rule table with the given name or
// code, or nil.
func RuleByName(key string) *Rule {
	for _, r := range Rules() {
		if r.Name == key || strings.EqualFold(r.Code, key) {
			return r
		}
	}
	return nil
}

// plural says how many of a thing there are: "1 file", "2 files".
func plural(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return strconv.Itoa(n) + " " + thing + "s"
}

func verb(n int, one, more string) string {
	if n == 1 {
		return one
	}
	return more
}

func are(n int) string  { return verb(n, "is", "are") }
func were(n int) string { return verb(n, "was", "were") }
