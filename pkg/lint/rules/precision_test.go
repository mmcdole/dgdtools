package rules

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/lint"
)

func runRuleFiles(t *testing.T, analyzer *lint.Analyzer, files map[string]string, names []string) []diag.Diagnostic {
	return runRulesFiles(t, []*lint.Analyzer{analyzer}, files, names)
}

func runRulesFiles(t *testing.T, analyzers []*lint.Analyzer, files map[string]string, names []string) []diag.Diagnostic {
	t.Helper()
	root := t.TempDir()
	for rel, src := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(src), 0o644))
	}
	cfg := config.Default()
	cfg.Root = root
	if len(names) > 0 {
		cfg.Lint.Rules = map[string]config.RuleSettings{
			"lifecycle-chain": {Names: names},
		}
	}
	runner := &lint.Runner{Config: cfg, Analyzers: analyzers}
	ds, err := runner.Run([]string{root})
	require.NoError(t, err)
	return ds
}

func TestLifecycleChainIndependentOfOtherTier2Rules(t *testing.T) {
	files := map[string]string{
		"std/base.c": `static void create() {}`,
		"obj/test.c": `inherit "/std/base"; static void create() {}`,
	}
	alone := runRulesFiles(t, []*lint.Analyzer{lifecycleChain}, files, nil)
	withOthers := runRulesFiles(t, []*lint.Analyzer{lifecycleChain, callableNotFound}, files, nil)
	require.Len(t, alone, 1)
	require.Len(t, withOthers, 1)
	assert.Equal(t, alone[0].Rule, withOthers[0].Rule)
	assert.Equal(t, alone[0].Line, withOthers[0].Line)
	assert.Equal(t, alone[0].Message, withOthers[0].Message)
}

func TestAssignmentInConditionPrecision(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    int
		message string
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
		{"assign and test", `if (x = sizeof(values)) x = 2;`, 0, ""},
		{"while assign and test", `while (value = next()) x++;`, 0, ""},
		{"parenthesized constant", `if ((x = 1)) x = 2;`, 0, ""},
		{"parenthesized assignment before logical", `if ((x = next()) && ready) x = 2;`, 0, ""},
		{"parenthesized assignment after logical", `if (ready && (x = next())) x = 2;`, 0, ""},
		{"parenthesized logical RHS", `if (x = (next() && ready)) x = 2;`, 0, ""},
		{"compiler owns unary LHS", `if (!x = next() || ready) x = 2;`, 0, ""},
		{"compiler owns logical LHS", `if (ready && x = 0) x = 2;`, 0, ""},
		{"compiler owns chained logical LHS", `if (x = ready && y = next()) x = 2;`, 0, ""},
		{"for middle clause", `for (x = 0; x = next() && ready; x++) y++;`, 1, "stores the result of &&/||"},
		{"for init and update", `for (x = 0; ready; x = 1) y++;`, 0, ""},
		{"comma segments", `if (x = 1, y = 2) x = 3;`, 2, "assignment of a constant"},
		{"root ternary is conservatively skipped", `if (ready ? x = 0 : y) x = 2;`, 0, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "static void test()\n{\n" +
				"    int x, y, ready;\n" +
				"    string s;\n" +
				"    mixed value, values;\n" +
				"    " + tt.body + "\n}\n"
			ds := runRuleFiles(t, assignInCondition, map[string]string{"test.c": src}, nil)
			require.Len(t, ds, tt.want)
			for _, d := range ds {
				assert.Equal(t, "assignment-in-condition", d.Rule)
				assert.Contains(t, d.Message, tt.message)
			}
		})
	}
}

func TestLifecycleChainResolvedParents(t *testing.T) {
	baseCreate := "static void create() {}\n"
	baseReset := "void reset(int flag) {}\n"
	noCreate := "int helper() { return 1; }\n"
	tests := []struct {
		name    string
		files   map[string]string
		names   []string
		want    int
		message string
	}{
		{
			name:  "no inherit",
			files: map[string]string{"obj/test.c": `static void create() {}`},
		},
		{
			name:  "parent has no create",
			files: map[string]string{"std/base.c": noCreate, "obj/test.c": `inherit "/std/base"; static void create() {}`},
		},
		{
			name:  "prototype only parent",
			files: map[string]string{"std/base.c": `static void create();`, "obj/test.c": `inherit "/std/base"; static void create() {}`},
		},
		{
			name:  "private parent definition",
			files: map[string]string{"std/base.c": `private void create() {}`, "obj/test.c": `inherit "/std/base"; static void create() {}`},
		},
		{
			name:  "unresolved parent",
			files: map[string]string{"obj/test.c": `inherit UNKNOWN; static void create() {}`},
		},
		{
			name:  "single parent missing",
			files: map[string]string{"std/base.c": baseCreate, "obj/test.c": `inherit "/std/base"; static void create() {}`},
			want:  1, message: "call ::create()",
		},
		{
			name:  "partial parent with known create",
			files: map[string]string{"std/base.c": `inherit UNKNOWN; static void create() {}`, "obj/test.c": `inherit "/std/base"; static void create() {}`},
			want:  1, message: "call ::create()",
		},
		{
			name:  "single unqualified call",
			files: map[string]string{"std/base.c": baseCreate, "obj/test.c": `inherit "/std/base"; static void create() { ::create(); }`},
		},
		{
			name:  "duplicate direct program is one implementation",
			files: map[string]string{"std/base.c": baseCreate, "obj/test.c": `inherit "/std/base"; inherit "/std/base"; static void create() { ::create(); }`},
		},
		{
			name:  "single labeled call",
			files: map[string]string{"std/base.c": baseCreate, "obj/test.c": `inherit base "/std/base"; static void create() { base::create(); }`},
		},
		{
			name:  "portable branch label satisfies unlabeled parent",
			files: map[string]string{"std/base.c": baseCreate, "obj/test.c": `inherit "/std/base"; static void create() { base::create(); }`},
		},
		{
			name:  "wrong label is not a call",
			files: map[string]string{"std/base.c": baseCreate, "obj/test.c": `inherit base "/std/base"; static void create() { other::create(); }`},
			want:  1, message: "base::create()",
		},
		{
			name:  "multiple parents missing all",
			files: map[string]string{"std/left.c": baseCreate, "std/right.c": baseCreate, "obj/test.c": `inherit left "/std/left"; inherit right "/std/right"; static void create() {}`},
			want:  1, message: "missing left::create(), right::create()",
		},
		{
			name:  "multiple parents missing one",
			files: map[string]string{"std/left.c": baseCreate, "std/right.c": baseCreate, "obj/test.c": `inherit left "/std/left"; inherit right "/std/right"; static void create() { left::create(); }`},
			want:  1, message: "missing right::create()",
		},
		{
			name:  "multiple unqualified call is insufficient",
			files: map[string]string{"std/left.c": baseCreate, "std/right.c": baseCreate, "obj/test.c": `inherit left "/std/left"; inherit right "/std/right"; static void create() { ::create(); }`},
			want:  1, message: "missing left::create(), right::create()",
		},
		{
			name:  "multiple parents complete",
			files: map[string]string{"std/left.c": baseCreate, "std/right.c": baseCreate, "obj/test.c": `inherit left "/std/left"; inherit right "/std/right"; static void create() { left::create(); right::create(); }`},
		},
		{
			name:  "multiple parents require labels",
			files: map[string]string{"std/left.c": baseCreate, "std/right.c": baseCreate, "obj/test.c": `inherit "/std/left"; inherit "/std/right"; static void create() {}`},
			want:  1, message: "missing labeled create() call for /std/left, /std/right",
		},
		{
			name:  "portable branch labels satisfy unlabeled parents",
			files: map[string]string{"std/left.c": baseCreate, "std/right.c": baseCreate, "obj/test.c": `inherit "/std/left"; inherit "/std/right"; static void create() { left::create(); right::create(); }`},
		},
		{
			name:  "portable branch label leaves one parent missing",
			files: map[string]string{"std/left.c": baseCreate, "std/right.c": baseCreate, "obj/test.c": `inherit "/std/left"; inherit "/std/right"; static void create() { left::create(); }`},
			want:  1, message: "missing labeled create() call for /std/right",
		},
		{
			name:  "only defining parent matters",
			files: map[string]string{"std/left.c": baseCreate, "std/right.c": noCreate, "obj/test.c": `inherit "/std/left"; inherit "/std/right"; static void create() { ::create(); }`},
		},
		{
			name:  "alternate lifecycle name",
			files: map[string]string{"std/base.c": baseReset, "obj/test.c": `inherit "/std/base"; void reset(int flag) {}`},
			names: []string{"reset"}, want: 1, message: "call ::reset()",
		},
		{
			name:  "intentional diamond suppression",
			files: map[string]string{"std/left.c": baseCreate, "std/right.c": baseCreate, "obj/test.c": "inherit left \"/std/left\"; inherit right \"/std/right\";\n/* dgdlint:disable-next-line lifecycle-chain */\nstatic void create() { left::create(); }\n"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ds := runRuleFiles(t, lifecycleChain, tt.files, tt.names)
			require.Len(t, ds, tt.want)
			for _, d := range ds {
				assert.Equal(t, "lifecycle-chain", d.Rule)
				assert.Contains(t, d.Message, tt.message)
			}
		})
	}
}
