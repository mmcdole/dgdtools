package lint_test

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/lint"
)

var update = flag.Bool("update", false, "rewrite docs/rules.md from the rule table")

// docs/rules.md is the rule table as a document: what Rule.URL points
// into. It is written from the table, so the two cannot disagree:
//
//	go test ./pkg/lint -run TestRulesDoc -update
func TestRulesDoc(t *testing.T) {
	const path = "../../docs/rules.md"
	want := rulesDoc()
	if *update {
		require.NoError(t, os.WriteFile(path, []byte(want), 0o644))
	}
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	if want != string(got) {
		t.Error("docs/rules.md is not what the rule table says; run: go test ./pkg/lint -run TestRulesDoc -update")
	}
	for _, r := range lint.Rules() {
		anchor := strings.TrimPrefix(r.URL("dev"), lint.DocsURL("dev"))
		assert.Contains(t, want, "\n### "+strings.TrimPrefix(anchor, "#")+"\n", "the section %s points to", r.URL("dev"))
	}
}

func rulesDoc() string {
	var b strings.Builder
	b.WriteString("# Lint rules\n\n")
	b.WriteString("<!-- Written from the rule table by `go test ./pkg/lint -run TestRulesDoc -update`; do not edit. -->\n\n")
	b.WriteString("The rules of `dgdtools lint`, by group. `dgdtools rules` lists them and\n")
	b.WriteString("`dgdtools explain RULE` prints one. A rule is set in `lint.rules` of\n")
	b.WriteString("`dgdtools.yml` under its name or its code:\n\n")
	b.WriteString("```yaml\nlint:\n  rules:\n    unreachable-code: warning                    # off | info | warning | error\n")
	b.WriteString("    lifecycle-chain: { names: [create, reset] }  # with options; severity is optional\n```\n\n")
	b.WriteString("\"Needs the lib\" says the rule reads other files than the one it reports\n")
	b.WriteString("in: it runs when the lib root, or a profile of the lib, is known.\n")
	groups := map[string]string{
		lint.GroupSyntax:     "The driver refuses to compile it: an object with one of these does not load.",
		lint.GroupRuntime:    "It compiles, and fails or silently returns nil when it runs.",
		lint.GroupSuspicious: "Legal, and almost certainly not what was meant.",
		lint.GroupConvention: "The lib's own policy.",
	}
	group := ""
	for _, r := range lint.Rules() {
		if r.Group != group {
			group = r.Group
			fmt.Fprintf(&b, "\n## %s\n\n%s\n", group, groups[group])
		}
		def, scope := "off by default", "one file is enough"
		if r.Default {
			def = "on by default"
		}
		if r.CrossFile {
			scope = "needs the lib"
		}
		fmt.Fprintf(&b, "\n### %s\n\n`%s` · %s · %s · %s\n\n%s.\n\n%s\n", r.Name, r.Code, r.Severity, def, scope, upperFirst(r.Doc), r.Explain)
		if opts := r.OptionList(); len(opts) > 0 {
			b.WriteString("\nOptions:\n\n")
			for _, o := range opts {
				fmt.Fprintf(&b, "- `%s`: %s; %s\n", o.Key, o.Takes, o.Doc)
			}
		}
	}
	return b.String()
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
