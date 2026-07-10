// dgdcmp compares the significant token streams of two DGD LPC files,
// proving whether a diff is formatting-only. It is the verification gate
// behind dgdfmt, exposed standalone.
//
//	dgdcmp a.c b.c     exit 0: token-identical; 1: divergent; 2: error
//	dgdcmp -dump a.c   print the token stream (lexer debugging)
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/mmcdole/dgdtools/pkg/lexer"
	"github.com/mmcdole/dgdtools/pkg/token"
	"github.com/mmcdole/dgdtools/pkg/tokcmp"
)

var version = "dev"

func main() {
	dump := flag.Bool("dump", false, "dump the token stream of one file")
	trivia := flag.Bool("trivia", false, "with -dump: include whitespace/comment tokens")
	noSlashSlash := flag.Bool("no-slash-slash", false, "disable // line comments (DGD without SLASHSLASH)")
	closures := flag.Bool("closures", false, "reserve 'function' as a keyword (DGD with CLOSURES)")
	showVersion := flag.Bool("version", false, "print version")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: dgdcmp [flags] a.c b.c\n       dgdcmp -dump [flags] file.c\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("dgdcmp", version)
		return
	}

	dialect := lexer.Dialect{SlashSlash: !*noSlashSlash, Closures: *closures}

	if *dump {
		if flag.NArg() != 1 {
			flag.Usage()
			os.Exit(2)
		}
		os.Exit(runDump(flag.Arg(0), *trivia, dialect))
	}

	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	os.Exit(runCompare(flag.Arg(0), flag.Arg(1), dialect))
}

func lexFile(path string, dialect lexer.Dialect) (*token.File, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f := lexer.Lex(path, src, dialect)
	if err := f.CheckRoundTrip(); err != nil {
		return nil, err
	}
	return f, nil
}

func runDump(path string, trivia bool, dialect lexer.Dialect) int {
	f, err := lexFile(path, dialect)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dgdcmp:", err)
		return 2
	}
	for _, t := range f.Tokens {
		if !trivia && (t.Kind.IsTrivia() || t.Kind == token.EOF) {
			continue
		}
		pos := f.Pos(t.Off)
		text := string(f.Text(t))
		if len(text) > 60 {
			text = text[:57] + "..."
		}
		fmt.Printf("%s\t%-12s\t%q\n", pos, t.Kind, text)
	}
	for _, d := range f.Errs {
		fmt.Fprintln(os.Stderr, d)
	}
	return 0
}

func runCompare(pathA, pathB string, dialect lexer.Dialect) int {
	a, err := lexFile(pathA, dialect)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dgdcmp:", err)
		return 2
	}
	b, err := lexFile(pathB, dialect)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dgdcmp:", err)
		return 2
	}

	eq, div := tokcmp.Compare(a, b)
	if eq {
		fmt.Printf("token streams identical: %s == %s\n", pathA, pathB)
		return 0
	}

	fmt.Printf("token streams DIVERGE:\n")
	printSide(a, pathA, div.AIndex, div.A, div.APos, div.AMissing)
	printSide(b, pathB, div.BIndex, div.B, div.BPos, div.BMissing)
	return 1
}

func printSide(f *token.File, path string, idx int, t token.Token, pos token.Pos, missing bool) {
	if missing {
		fmt.Printf("  %s:%s: stream ends here\n", path, pos)
		return
	}
	before, after := tokcmp.Context(f, idx, 3)
	fmt.Printf("  %s:%s: %s %q\n", path, pos, t.Kind, string(f.Text(t)))
	fmt.Printf("    context: %s >>>%s<<< %s\n",
		strings.Join(before, " "), string(f.Text(t)), strings.Join(after, " "))
}
