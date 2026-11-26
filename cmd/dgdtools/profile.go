package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/profilebuild"
)

const profileUsage = `usage: dgdtools profile build [flags]
       dgdtools profile show [NAME|FILE] [LIBPATH]

build captures the whole mudlib of the project (the working directory's, or
--config) as a profile: what somebody with only part of the mudlib needs to
know about the rest. show summarizes a profile, or prints its stub for
one mudlib file; without arguments it says where profiles are looked for.
`

// profileCmd is `dgdtools profile build|show`.
func profileCmd(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
	var (
		project projectFlags
		output  string
		quiet   bool
	)
	fs := newFlags("profile", profileUsage, stderr)
	project.register(fs)
	fs.StringVar(&output, "output", "", "build: write the profile to `file` (default "+profile.FileName+" next to the config)")
	fs.StringVar(&output, "o", "", "short for --output")
	fs.BoolVar(&quiet, "quiet", false, "build: do not say what was written")
	fs.BoolVar(&quiet, "q", false, "short for --quiet")
	pos, code, done := parse(fs, args)
	if done {
		return code
	}
	fail := func(err error) int { return failure(stderr, "profile", err) }
	p, err := environment()
	if err != nil {
		return fail(err)
	}
	switch {
	case len(pos) == 1 && pos[0] == "build":
		prj, err := config.Discover(p.fsys, nil, project.discover(p))
		if err != nil {
			return fail(err)
		}
		prof, err := profilebuild.Build(ctx, prj, p.fsys)
		if err != nil {
			return fail(err)
		}
		// Whoever uses the profile takes its driver settings as known.
		for _, n := range prj.Notes {
			if n.Kind == diag.NoteDriverUnset && !quiet {
				fmt.Fprintf(stderr, "note: %s; the profile gives them as none\n", n.Message)
			}
		}
		data, err := prof.Marshal()
		if err != nil {
			return fail(err)
		}
		if output == "" {
			output = filepath.Join(prj.Config.Dir, profile.FileName)
		}
		if err := os.WriteFile(output, data, 0o644); err != nil {
			return fail(err)
		}
		if !quiet {
			fmt.Fprintf(stderr, "wrote %s: profile %q, %d stubs, %d bytes\n", output, prof.Name, len(prof.Files), len(data))
		}
		return 0
	case len(pos) >= 1 && len(pos) <= 3 && pos[0] == "show":
		return profileShow(p, project.profile, pos[1:], stdout, stderr)
	}
	return usageError(fs, "want build, or show [NAME|FILE] [LIBPATH]")
}

// profileShow summarizes a profile, or prints one of its stubs.
func profileShow(p process, ref string, args []string, stdout, stderr io.Writer) int {
	if ref == "" && len(args) > 0 {
		ref, args = args[0], args[1:]
	}
	if ref == "" {
		fmt.Fprintf(stdout, "`profile: NAME` is looked up as NAME.profile in:\n  %s\n",
			strings.Join(append([]string{p.wd}, p.profileDirs...), "\n  "))
		return 0
	}
	prof, err := profile.Load(p.fsys, ref, p.wd, p.profileDirs)
	if err != nil {
		return failure(stderr, "profile", err)
	}
	if len(args) > 0 {
		stub, ok := prof.Files[args[0]]
		if !ok {
			return failure(stderr, "profile", fmt.Errorf("profile %s has no stub for %s", prof.Name, args[0]))
		}
		fmt.Fprint(stdout, stub)
		return 0
	}
	headers, objects, bytes := 0, 0, 0
	for name, s := range prof.Files {
		bytes += len(s)
		if strings.HasSuffix(name, ".c") {
			objects++
		} else {
			headers++
		}
	}
	tc := "unknown"
	if prof.Dialect.Typechecking != nil {
		tc = fmt.Sprint(*prof.Dialect.Typechecking)
	}
	fmt.Fprintf(stdout, "profile:      %s (format %d) %s\n", prof.Name, prof.Format, prof.Path)
	fmt.Fprintf(stdout, "dialect:      %s %s %s, typechecking %s, // comments %v, closures %v\n",
		prof.Dialect.Language, prof.Dialect.Driver, prof.Dialect.Version, tc, prof.Dialect.SlashSlash, prof.Dialect.Closures)
	fmt.Fprintf(stdout, "include dirs: %s\n", strings.Join(prof.IncludeDirs, ", "))
	fmt.Fprintf(stdout, "include file: %s\n", prof.IncludeFile)
	fmt.Fprintf(stdout, "auto objects: %s\n", strings.Join(prof.AutoObjects, ", "))
	fmt.Fprintf(stdout, "complete for: %s\n", strings.Join(prof.Covered, " "))
	fmt.Fprintf(stdout, "checkouts:    %s\n", strings.Join(prof.Checkouts, ", "))
	fmt.Fprintf(stdout, "stubs:        %d objects, %d headers, %d bytes of declarations\n", objects, headers, bytes)
	return 0
}
