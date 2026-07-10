package lint_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/lint"
	_ "github.com/mmcdole/dgdtools/pkg/lint/rules"
)

func minilibConfig() *config.Config {
	cfg := config.Default()
	cfg.Dir = "testdata"
	cfg.Root = "minilib"
	cfg.Lint.IncludeDirs = []string{"/include", "/dgd/include"}
	cfg.Lint.IncludeFile = "/dgd/include/Std.h"
	cfg.Lint.AutoObjects = []string{"/dgd/lib/object"}
	cfg.Lint.CallRegistry = map[string]int{"register_handler": 0}
	return cfg
}

// runLint executes the given rules over the fixture lib and returns
// findings as "rule path:line" plus messages for detail assertions.
func runLint(t *testing.T, cfg *config.Config, enable, disable []string) []diag.Diagnostic {
	t.Helper()
	analyzers, err := lint.Enabled(cfg, enable, disable)
	require.NoError(t, err)
	runner := &lint.Runner{Config: cfg, Analyzers: analyzers}
	ds, err := runner.Run([]string{cfg.AbsRoot()})
	require.NoError(t, err)
	return ds
}

func keys(ds []diag.Diagnostic) []string {
	var out []string
	for _, d := range ds {
		out = append(out, fmt.Sprintf("%s %s:%d", d.Rule, shortPath(d.Path), d.Line))
	}
	return out
}

func shortPath(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

func TestTier2SeededBugs(t *testing.T) {
	ds := runLint(t, minilibConfig(), []string{"static-autosave-var"}, nil)
	got := keys(ds)

	// Every seeded bug is found.
	assert.Contains(t, got, "callable-not-found user.c:10", "call_out to missing function")
	assert.Contains(t, got, "callable-not-found user.c:11", "config registrar to missing function")
	assert.Contains(t, got, "callable-not-found user.c:19", "this_object()->missing_fn()")
	assert.Contains(t, got, "static-crossobj user.c:27", "static cross-object")
	assert.Contains(t, got, "static-crossobj user.c:21", "private via ->")
	assert.Contains(t, got, "callable-not-found user.c:24", "call_other missing")
	assert.Contains(t, got, "callable-not-found user.c:26", "literal-path -> missing")
	assert.Contains(t, got, "static-autosave-var saver.c:4")
	assert.Contains(t, got, "lifecycle-chain messy.c:6")

	// Static via same-object call_other is legal in DGD: no finding.
	for _, k := range got {
		assert.NotContains(t, k, "user.c:20", "static + same object is legal")
	}

	// The auto object's functions and real functions never fire.
	for _, k := range got {
		assert.NotContains(t, k, "user.c:18", "visible_fn is real")
		assert.NotContains(t, k, "user.c:22", "object_name comes from the auto object")
		assert.NotContains(t, k, "user.c:23", "call_other to visible_fn is fine")
		assert.NotContains(t, k, "user.c:25", "literal-path -> visible_fn is fine")
	}

	// Partial chains are skipped, not guessed at.
	for _, k := range got {
		assert.NotContains(t, k, "broken.c:10", "partial chain must not produce findings")
	}

	// register_handler("on_event") resolves: on_event exists.
	for _, d := range ds {
		if d.Rule == "callable-not-found" {
			assert.NotContains(t, d.Message, "on_event")
		}
	}
}

func TestTier1RulesOptIn(t *testing.T) {
	ds := runLint(t, minilibConfig(), []string{"missing-visibility", "raw-inherit-path", "unresolved-inherit"}, nil)
	got := keys(ds)

	assert.Contains(t, got, "missing-visibility messy.c:13")
	assert.Contains(t, got, "raw-inherit-path messy.c:1")
	assert.Contains(t, got, "raw-inherit-path saver.c:1")
	assert.Contains(t, got, "unresolved-inherit broken.c:4")

	// Suppression comment works.
	assert.NotContains(t, got, "missing-visibility messy.c:21", "disable-next-line suppresses")
	// public counts as a specifier: thing.c's functions are never flagged.
	for _, k := range got {
		assert.NotContains(t, k, "missing-visibility thing.c")
	}
}

func TestDisableAndPathRules(t *testing.T) {
	cfg := minilibConfig()
	ds := runLint(t, cfg, nil, []string{"static-crossobj"})
	for _, d := range ds {
		assert.NotEqual(t, "static-crossobj", d.Rule)
	}

	cfg = minilibConfig()
	cfg.Lint.PathRules = []config.PathRule{{Paths: []string{"daemons/**"}, Disable: []string{"static-autosave-var"}}}
	ds = runLint(t, cfg, []string{"static-autosave-var"}, nil)
	for _, d := range ds {
		if d.Rule == "static-autosave-var" {
			assert.NotContains(t, d.Path, "saver.c")
		}
	}
}

func TestSeverityConfigAndThreshold(t *testing.T) {
	cfg := minilibConfig()
	cfg.Lint.Rules = map[string]config.RuleSettings{
		"callable-not-found": {Severity: "warning"},
	}
	ds := runLint(t, cfg, nil, nil)
	for _, d := range ds {
		if d.Rule == "callable-not-found" {
			assert.Equal(t, diag.Warning, d.Severity)
		}
	}
	assert.Equal(t, diag.Error, lint.Threshold(cfg))
	cfg.Lint.FailOn = "warning"
	assert.Equal(t, diag.Warning, lint.Threshold(cfg))
}

func TestUnknownRuleRejected(t *testing.T) {
	_, err := lint.Enabled(minilibConfig(), []string{"no-such-rule"}, nil)
	assert.Error(t, err)
}
