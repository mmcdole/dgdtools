// dgdlint lints DGD LPC source.
//
// Rules follow the go/analysis model: each is a named analyzer that can be
// enabled/disabled and configured in dgdtools.yml (found by searching
// upward from the working directory, or via -config). Tier-2 rules build a
// cross-file index of the whole lib to verify string-referenced calls —
// the class of silent runtime failure DGD's compiler cannot check.
//
// Exit codes: 0 clean, 1 findings at or above -fail-on, 2 tool error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/lint"
	_ "github.com/mmcdole/dgdtools/pkg/lint/rules"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to dgdtools.yml (default: search upward)")
	formatFlag := flag.String("f", "human", "output format: human or json")
	enable := flag.String("enable", "", "comma-separated rules to enable")
	disable := flag.String("disable", "", "comma-separated rules to disable")
	failOn := flag.String("fail-on", "", "exit 1 at this severity or above: info, warning, error")
	listRules := flag.Bool("rules", false, "list available rules and exit")
	showVersion := flag.Bool("version", false, "print version")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: dgdlint [flags] [path ...]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("dgdlint", version)
		return
	}
	if *listRules {
		for _, a := range lint.Analyzers() {
			def := " "
			if a.Default {
				def = "*"
			}
			fmt.Printf("%s %-20s (tier %d, %s) %s\n", def, a.Name, a.Tier, a.DefaultSeverity, a.Doc)
		}
		return
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fatal(err)
	}
	if *failOn != "" {
		cfg.Lint.FailOn = *failOn
	}

	analyzers, err := lint.Enabled(cfg, splitList(*enable), splitList(*disable))
	if err != nil {
		fatal(err)
	}

	paths := flag.Args()
	if len(paths) == 0 {
		paths = []string{cfg.AbsRoot()}
	}

	runner := &lint.Runner{Config: cfg, Analyzers: analyzers}
	ds, err := runner.Run(paths)
	if err != nil {
		fatal(err)
	}

	switch *formatFlag {
	case "human":
		for _, d := range ds {
			fmt.Println(d)
		}
	case "json":
		printJSON(ds)
	default:
		fatal(fmt.Errorf("unknown output format %q", *formatFlag))
	}

	threshold := lint.Threshold(cfg)
	for _, d := range ds {
		if d.Severity >= threshold {
			os.Exit(1)
		}
	}
}

func loadConfig(path string) (*config.Config, error) {
	if path != "" {
		return config.Load(path)
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return config.Find(wd)
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

type jsonDiag struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	Message  string `json:"message"`
}

func printJSON(ds []diag.Diagnostic) {
	out := make([]jsonDiag, 0, len(ds))
	for _, d := range ds {
		out = append(out, jsonDiag{
			Rule: d.Rule, Severity: d.Severity.String(),
			Path: d.Path, Line: d.Line, Col: d.Col, Message: d.Message,
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(out)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "dgdlint:", err)
	os.Exit(2)
}
