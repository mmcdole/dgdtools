// dgdfmt formats DGD LPC source, gofmt-style.
//
// Without -l/-d/-w it prints the formatted source of each named file (or
// stdin) to stdout. Directories are walked for .c and .h files.
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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/mmcdole/dgdtools/pkg/format"
	"github.com/mmcdole/dgdtools/pkg/lexer"
)

var version = "dev"

var (
	list        = flag.Bool("l", false, "list files whose formatting differs")
	diff        = flag.Bool("d", false, "display diffs instead of rewriting files")
	write       = flag.Bool("w", false, "write result to (source) file instead of stdout")
	indent      = flag.Int("indent", 4, "spaces per indentation level")
	lineEndings = flag.String("line-endings", "preserve", "newline policy: preserve, lf, or crlf")
	maxBlank    = flag.Int("max-blank-lines", 2, "maximum consecutive blank lines")
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

	opts, dialect, err := options()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dgdfmt:", err)
		os.Exit(2)
	}

	if flag.NArg() == 0 {
		if *write {
			fmt.Fprintln(os.Stderr, "dgdfmt: cannot use -w with standard input")
			os.Exit(2)
		}
		src, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dgdfmt:", err)
			os.Exit(2)
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
			filepath.WalkDir(arg, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					report(err)
					return nil
				}
				if d.IsDir() {
					return nil
				}
				if ext := filepath.Ext(path); ext == ".c" || ext == ".h" {
					processFile(path, opts, dialect)
				}
				return nil
			})
		default:
			processFile(arg, opts, dialect)
		}
	}
	os.Exit(exitCode)
}

func options() (format.Options, lexer.Dialect, error) {
	o := format.Options{Indent: *indent, MaxBlankLines: *maxBlank}
	switch *lineEndings {
	case "preserve":
		o.LineEndings = format.Preserve
	case "lf":
		o.LineEndings = format.LF
	case "crlf":
		o.LineEndings = format.CRLF
	default:
		return o, lexer.Dialect{}, fmt.Errorf("invalid -line-endings %q", *lineEndings)
	}
	return o, lexer.Dialect{SlashSlash: !*noSlash, Closures: *closures}, nil
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
