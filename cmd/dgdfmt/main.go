// dgdfmt formats DGD LPC source, gofmt-style.
//
// Without -l/-d/-w it prints the formatted source of each named file (or
// stdin) to stdout. Directories are walked for .c and .h files, honoring
// the excludes from dgdtools.yml (found by searching upward from the
// working directory, or via -config). The config also supplies dialect
// and format defaults; explicitly passed flags win.
//
// Every write is protected by the formatter's internal gate: the output is
// re-lexed and its significant token stream must be identical to the
// input's, and formatting must be idempotent — otherwise the file is left
// untouched and an error is reported. Files that do not fully tokenize
// (dead or non-LPC content) are refused.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/fileset"
	"github.com/mmcdole/dgdtools/pkg/format"
	"github.com/mmcdole/dgdtools/pkg/lexer"
)

var version = "dev"

var (
	list        = flag.Bool("l", false, "list files whose formatting differs")
	diff        = flag.Bool("d", false, "display diffs instead of rewriting files")
	write       = flag.Bool("w", false, "write result to (source) file instead of stdout")
	configPath  = flag.String("config", "", "path to dgdtools.yml (default: search upward)")
	indent      = flag.Int("indent", 4, "spaces per indentation level")
	lineEndings = flag.String("line-endings", "preserve", "newline policy: preserve, lf, or crlf")
	maxBlank    = flag.Int("max-blank-lines", 2, "maximum consecutive blank lines")
	funcHeaders = flag.String("func-headers", "split", "function header layout: split (KNF, name at column 0) or joined (one line)")
	noSlash     = flag.Bool("no-slash-slash", false, "disable // line comments (DGD without SLASHSLASH)")
	closures    = flag.Bool("closures", false, "reserve 'function' as a keyword (DGD with CLOSURES)")
	showVersion = flag.Bool("version", false, "print version")
)

var exitCode = 0

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: dgdfmt [flags] [path ...]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("dgdfmt", version)
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		fatal(err)
	}
	opts, dialect, err := options(cfg)
	if err != nil {
		fatal(err)
	}

	if flag.NArg() == 0 {
		if *write {
			fatal(fmt.Errorf("cannot use -w with standard input"))
		}
		src, err := io.ReadAll(os.Stdin)
		if err != nil {
			fatal(err)
		}
		processBytes("<stdin>", src, opts, dialect)
		os.Exit(exitCode)
	}

	for _, arg := range flag.Args() {
		info, err := os.Stat(arg)
		switch {
		case err != nil:
			report(err)
		case info.IsDir():
			err := fileset.Walk(arg, cfg.Exclude, func(path, rel string) {
				processFile(path, opts, dialect)
			})
			if err != nil {
				report(err)
			}
		default:
			processFile(arg, opts, dialect)
		}
	}
	os.Exit(exitCode)
}

func loadConfig() (*config.Config, error) {
	if *configPath != "" {
		return config.Load(*configPath)
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return config.Find(wd)
}

// options merges config-file format settings with flags; flags that were
// explicitly passed on the command line win.
func options(cfg *config.Config) (format.Options, lexer.Dialect, error) {
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	o := format.Options{Indent: *indent, MaxBlankLines: *maxBlank}
	le := *lineEndings
	fh := *funcHeaders
	dialect := lexer.Dialect{SlashSlash: !*noSlash, Closures: *closures}

	if !set["indent"] && cfg.Format.Indent > 0 {
		o.Indent = cfg.Format.Indent
	}
	if !set["max-blank-lines"] && cfg.Format.MaxBlankLines > 0 {
		o.MaxBlankLines = cfg.Format.MaxBlankLines
	}
	if !set["line-endings"] && cfg.Format.LineEndings != "" {
		le = cfg.Format.LineEndings
	}
	if !set["func-headers"] && cfg.Format.FunctionHeaders != "" {
		fh = cfg.Format.FunctionHeaders
	}
	if !set["no-slash-slash"] && !set["closures"] {
		dialect = cfg.TokenDialect()
	}

	switch le {
	case "preserve":
		o.LineEndings = format.Preserve
	case "lf":
		o.LineEndings = format.LF
	case "crlf":
		o.LineEndings = format.CRLF
	default:
		return o, dialect, fmt.Errorf("invalid line-endings %q", le)
	}
	switch fh {
	case "split":
		o.FuncHeaders = format.HeadersSplit
	case "joined":
		o.FuncHeaders = format.HeadersJoined
	default:
		return o, dialect, fmt.Errorf("invalid func-headers %q (want split or joined)", fh)
	}
	return o, dialect, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "dgdfmt:", err)
	os.Exit(2)
}

func report(err error) {
	fmt.Fprintln(os.Stderr, "dgdfmt:", err)
	exitCode = 2
}

func processFile(path string, opts format.Options, dialect lexer.Dialect) {
	src, err := os.ReadFile(path)
	if err != nil {
		report(err)
		return
	}
	out, changed, ok := run(path, src, opts, dialect)
	if !ok {
		return
	}

	switch {
	case *list:
		if changed {
			fmt.Println(path)
		}
	case *diff:
		if changed {
			printDiff(path, src, out)
		}
	case *write:
		if changed {
			if err := writeFile(path, out); err != nil {
				report(err)
			}
		}
	default:
		os.Stdout.Write(out)
	}
}

func processBytes(name string, src []byte, opts format.Options, dialect lexer.Dialect) {
	out, changed, ok := run(name, src, opts, dialect)
	if !ok {
		return
	}
	switch {
	case *list:
		if changed {
			fmt.Println(name)
		}
	case *diff:
		if changed {
			printDiff(name, src, out)
		}
	default:
		os.Stdout.Write(out)
	}
}

func run(name string, src []byte, opts format.Options, dialect lexer.Dialect) (out []byte, changed, ok bool) {
	f := lexer.Lex(name, src, dialect)
	if err := f.CheckRoundTrip(); err != nil {
		report(err)
		return nil, false, false
	}
	out, err := format.Format(f, opts)
	if err != nil {
		report(err)
		return nil, false, false
	}
	return out, !bytes.Equal(src, out), true
}

// writeFile replaces path's contents, preserving its permission bits.
func writeFile(path string, out []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, info.Mode().Perm())
}

func printDiff(path string, before, after []byte) {
	dir := os.TempDir()
	a, err := os.CreateTemp(dir, "dgdfmt-a-*")
	if err != nil {
		report(err)
		return
	}
	defer os.Remove(a.Name())
	b, err := os.CreateTemp(dir, "dgdfmt-b-*")
	if err != nil {
		report(err)
		return
	}
	defer os.Remove(b.Name())
	a.Write(before)
	b.Write(after)
	a.Close()
	b.Close()

	cmd := exec.Command("diff", "-u",
		"--label", path+".orig", a.Name(),
		"--label", path, b.Name())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// diff exits 1 when files differ; that is the expected case.
	if err := cmd.Run(); err != nil {
		if ee, isExit := err.(*exec.ExitError); !isExit || ee.ExitCode() > 1 {
			report(fmt.Errorf("diff: %v", err))
		}
	}
}
