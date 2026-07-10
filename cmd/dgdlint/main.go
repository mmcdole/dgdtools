// dgdlint is a linter for DGD LPC source. Rules, configuration, and the
// cross-file index are under construction; this stub reserves the CLI.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-version" {
		fmt.Println("dgdlint", version)
		return
	}
	fmt.Fprintln(os.Stderr, "dgdlint: not implemented yet (see dgdfmt and dgdcmp)")
	os.Exit(2)
}
