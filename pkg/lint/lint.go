// Package lint is the dgdlint analyzer framework, modeled on go/analysis:
// each rule is a named Analyzer that can be enabled, disabled, and
// configured independently, runs over a lexed+recognized file (tier 1) or
// with a cross-file index (tier 2), and reports typed diagnostics.
package lint

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/fileset"
	"github.com/mmcdole/dgdtools/pkg/index"
	"github.com/mmcdole/dgdtools/pkg/lexer"
	"github.com/mmcdole/dgdtools/pkg/structure"
	"github.com/mmcdole/dgdtools/pkg/token"
)

// Analyzer is one lint rule.
type Analyzer struct {
	Name            string
	Doc             string
	Tier            int // 1 = per-file; 2 = needs the cross-file index
	Default         bool
	DefaultSeverity diag.Severity
	Run             func(*Pass)
}

// Pass is the per-file context handed to an Analyzer.
type Pass struct {
	File      *token.File
	Structure *structure.FileStructure
	Index     *index.Index // nil for tier-1-only runs
	Object    *index.ObjectInfo
	LibPath   string
	Config    *config.Config
	Settings  config.RuleSettings

	analyzer *Analyzer
	severity diag.Severity
	report   func(diag.Diagnostic)
}

// Reportf records a finding at the given byte offset.
func (p *Pass) Reportf(off uint32, format string, args ...any) {
	pos := p.File.Pos(off)
	p.report(diag.Diagnostic{
		Path: p.File.Path, Line: pos.Line, Col: pos.Col,
		Severity: p.severity, Rule: p.analyzer.Name,
		Message: fmt.Sprintf(format, args...),
	})
}

var (
	regMu    sync.Mutex
	registry = map[string]*Analyzer{}
)

// Register adds an analyzer; rules self-register in init().
func Register(a *Analyzer) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[a.Name]; dup {
		panic("lint: duplicate analyzer " + a.Name)
	}
	registry[a.Name] = a
}

// Analyzers returns all registered analyzers, sorted by name.
func Analyzers() []*Analyzer {
	regMu.Lock()
	defer regMu.Unlock()
	var out []*Analyzer
	for _, a := range registry {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Enabled resolves the active analyzer set from config + CLI overrides.
func Enabled(cfg *config.Config, enable, disable []string) ([]*Analyzer, error) {
	on := map[string]bool{}
	for _, a := range Analyzers() {
		on[a.Name] = a.Default
	}
	setAll := func(names []string, v bool) error {
		for _, n := range names {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			if _, ok := registry[n]; !ok {
				return fmt.Errorf("unknown rule %q", n)
			}
			on[n] = v
		}
		return nil
	}
	if err := setAll(cfg.Lint.Enable, true); err != nil {
		return nil, err
	}
	if err := setAll(cfg.Lint.Disable, false); err != nil {
		return nil, err
	}
	if err := setAll(enable, true); err != nil {
		return nil, err
	}
	if err := setAll(disable, false); err != nil {
		return nil, err
	}
	var out []*Analyzer
	for _, a := range Analyzers() {
		if on[a.Name] {
			out = append(out, a)
		}
	}
	return out, nil
}

func severityOf(a *Analyzer, cfg *config.Config) diag.Severity {
	if s := cfg.RuleSettingsFor(a.Name).Severity; s != "" {
		switch strings.ToLower(s) {
		case "info":
			return diag.Info
		case "warning", "warn":
			return diag.Warning
		case "error":
			return diag.Error
		}
	}
	return a.DefaultSeverity
}

// Runner executes analyzers over a file set.
type Runner struct {
	Config    *config.Config
	Analyzers []*Analyzer
}

// Run lints every .c file under the given paths (directories are walked;
// the config's excludes apply). It returns sorted diagnostics.
func (r *Runner) Run(paths []string) ([]diag.Diagnostic, error) {
	needIndex := false
	for _, a := range r.Analyzers {
		if a.Tier >= 2 {
			needIndex = true
		}
	}
	var ix *index.Index
	if needIndex {
		var err error
		ix, err = index.Build(r.Config)
		if err != nil {
			return nil, fmt.Errorf("building index: %w", err)
		}
	}

	type job struct{ path, rel string }
	var jobs []job
	root := r.Config.AbsRoot()
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return nil, err // a mistyped path must fail loudly, not pass
		}
		err := fileset.Walk(p, r.Config.Exclude, func(path, rel string) {
			if strings.HasSuffix(path, ".c") {
				jobs = append(jobs, job{path, rel})
			}
		})
		if err != nil {
			return nil, err
		}
	}

	var mu sync.Mutex
	var all []diag.Diagnostic
	jobCh := make(chan job, 256)
	var wg sync.WaitGroup
	for range runtime.NumCPU() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				ds := r.lintFile(j.path, root, ix)
				if len(ds) > 0 {
					mu.Lock()
					all = append(all, ds...)
					mu.Unlock()
				}
			}
		}()
	}
	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)
	wg.Wait()

	sort.Slice(all, func(i, j int) bool {
		if all[i].Path != all[j].Path {
			return all[i].Path < all[j].Path
		}
		if all[i].Line != all[j].Line {
			return all[i].Line < all[j].Line
		}
		return all[i].Col < all[j].Col
	})
	return all, nil
}

func (r *Runner) lintFile(path, root string, ix *index.Index) []diag.Diagnostic {
	f, fs, obj, libPath := r.parse(path, root, ix)
	if f == nil {
		return nil
	}

	pathDisabled := r.pathDisabled(path, root)
	sup := scanSuppressions(f)

	var out []diag.Diagnostic
	for _, a := range r.Analyzers {
		if a.Tier >= 2 && ix == nil {
			continue
		}
		if pathDisabled[a.Name] {
			continue
		}
		pass := &Pass{
			File: f, Structure: fs, Index: ix, Object: obj,
			LibPath: libPath, Config: r.Config,
			Settings: r.Config.RuleSettingsFor(a.Name),
			analyzer: a, severity: severityOf(a, r.Config),
			report: func(d diag.Diagnostic) {
				if !sup.suppressed(d.Rule, d.Line) {
					out = append(out, d)
				}
			},
		}
		a.Run(pass)
	}
	return out
}

func (r *Runner) parse(path, root string, ix *index.Index) (*token.File, *structure.FileStructure, *index.ObjectInfo, string) {
	libPath := ""
	if rel, err := relIfUnder(root, path); err == nil {
		libPath = fileset.LibPath(rel)
	}
	if ix != nil && libPath != "" {
		if obj := ix.Objects[libPath]; obj != nil {
			return obj.File, obj.Structure, obj, libPath
		}
	}
	src, err := readFile(path)
	if err != nil {
		return nil, nil, nil, ""
	}
	f := lexer.Lex(path, src, r.Config.TokenDialect())
	fs := structure.Analyze(f, structure.Config{SpecifierMacros: r.Config.SpecifierMacroSet()})
	return f, fs, nil, libPath
}

func (r *Runner) pathDisabled(path, root string) map[string]bool {
	out := map[string]bool{}
	rel, err := relIfUnder(root, path)
	if err != nil {
		return out
	}
	for _, pr := range r.Config.Lint.PathRules {
		m := fileset.NewMatcher(pr.Paths)
		if m.Match(rel) {
			for _, rule := range pr.Disable {
				out[rule] = true
			}
		}
	}
	return out
}

// Threshold returns the fail-on severity from config (default error).
func Threshold(cfg *config.Config) diag.Severity {
	switch strings.ToLower(cfg.Lint.FailOn) {
	case "info":
		return diag.Info
	case "warning", "warn":
		return diag.Warning
	}
	return diag.Error
}
