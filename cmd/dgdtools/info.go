package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/fileset"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// rulesCmd is `dgdtools rules`: the rule table.
func rulesCmd(_ context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags("rules", "usage: dgdtools rules\n\nLists the lint rules: code, name, on or off by default, default severity,\nwhether the rule needs the mudlib (mudlib) or one file is enough (file).\n", stderr)
	pos, code, done := parse(fs, args)
	if done {
		return code
	}
	if len(pos) > 0 {
		return usageError(fs, "takes no arguments")
	}
	group := ""
	for _, r := range lint.Rules() {
		if r.Group != group {
			group = r.Group
			fmt.Fprintf(stdout, "%s:\n", group)
		}
		def := "off"
		if r.Default {
			def = "on "
		}
		scope := "file  "
		if r.CrossFile {
			scope = "mudlib"
		}
		fmt.Fprintf(stdout, "  %s %-28s %s %-7s %s %s\n", r.Code, r.Name, def, r.Severity, scope, r.Doc)
	}
	return 0
}

// explainCmd is `dgdtools explain RULE`.
func explainCmd(_ context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags("explain", "usage: dgdtools explain RULE\n\nDescribes one lint rule, given by name or code.\n", stderr)
	pos, code, done := parse(fs, args)
	if done {
		return code
	}
	if len(pos) != 1 {
		return usageError(fs, "want one RULE")
	}
	r := lint.RuleByName(pos[0])
	if r == nil {
		return failure(stderr, "explain", fmt.Errorf("unknown rule %q (see `dgdtools rules`)", pos[0]))
	}
	def := "off by default"
	if r.Default {
		def = "on by default"
	}
	scope := "file-local"
	if r.CrossFile {
		scope = "needs the mudlib index"
	}
	fmt.Fprintf(stdout, "%s  %s\n%s group, %s, %s, %s\n\n%s\n\n%s\n", r.Code, r.Name, r.Group, r.Severity, def, scope, r.Doc, r.Explain)
	if opts := r.OptionList(); len(opts) > 0 {
		fmt.Fprintf(stdout, "\nOptions, in lint.rules under %s:\n", r.Name)
		for _, o := range opts {
			fmt.Fprintf(stdout, "    %s: %s; %s\n", o.Key, o.Takes, o.Doc)
		}
	}
	if !r.Late {
		fmt.Fprintf(stdout, "\nSuppress one occurrence with a comment on its line, or on the line above:\n    /* dgdtools-ignore lint/%s */\n", r.Name)
	}
	fmt.Fprintf(stdout, "\n%s\n", r.URL(version))
	return 0
}

const configUsage = `usage: dgdtools config [flags] [path]

Checks the configuration of a path (default: the working directory) and
shows what it comes to: the project, and the settings a file there is
formatted and linted by, with where each comes from. For a directory the
settings are those of a .c file in it, and every settings scope below the
directory is checked as well; a root config below it is another project,
checked when asked about by itself.

Exit codes: 0 the configuration is valid; 2 it is not (an unknown key,
value, rule or rule option, a project key in a settings scope, a profile
that is not found), with what is wrong and where.
`

// configCmd is `dgdtools config [path]`: what discovery and settings
// resolution make of a path, or what is wrong with its configuration.
func configCmd(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
	var project projectFlags
	fs := newFlags("config", configUsage, stderr)
	project.register(fs)
	pos, code, done := parse(fs, args)
	if done {
		return code
	}
	fail := func(err error) int { return failure(stderr, "config", err) }
	if len(pos) > 1 {
		return usageError(fs, "want at most one path")
	}
	p, err := environment()
	if err != nil {
		return fail(err)
	}
	target := p.wd
	if len(pos) == 1 {
		target = p.abs(pos[0])
	}
	l, err := newLinter(p, &project, []string{target})
	if err != nil {
		return fail(err)
	}
	file, shown := target, target
	if p.fsys.IsDir(target) {
		file, shown = filepath.Join(target, "_.c"), filepath.Join(target, "*.c")
	}
	settings, err := l.Project().Settings(file)
	if err != nil {
		return fail(err)
	}
	rules, err := l.EffectiveRules(file)
	if err != nil {
		return fail(err)
	}
	// The configuration files further down are part of what is asked
	// about: each settings scope is checked as the files next to it would
	// meet it. A root config down there is another project's.
	var scopes, projects []string
	if p.fsys.IsDir(target) {
		nested, err := nestedConfigs(ctx, p.fsys, l.Project(), target)
		if err != nil {
			return fail(err)
		}
		for _, n := range nested {
			if l.Project().Nested(n) != "" {
				projects = append(projects, n)
				continue
			}
			if _, err := l.EffectiveRules(filepath.Join(filepath.Dir(n), "_.c")); err != nil {
				return fail(err)
			}
			scopes = append(scopes, n)
		}
	}

	w, prj := stdout, l.Project()
	cfg := prj.Config
	orNone := func(s string) string {
		if s == "" {
			return "(none)"
		}
		return s
	}
	switch {
	case prj.UserConfig:
		fmt.Fprintf(w, "config file:   %s (user-level: no project config found)\n", prj.ConfigPath)
	default:
		fmt.Fprintf(w, "config file:   %s\n", orNone(prj.ConfigPath))
	}
	fmt.Fprintf(w, "mudlib root:   %s\n", orNone(prj.Root))
	switch {
	case !prj.HasLib():
		fmt.Fprintf(w, "mode:          file-local (cross-file rules need a mudlib root)\n")
	case prj.Partial:
		fmt.Fprintf(w, "mode:          partial checkout (what is not in the checkout or the profile is unknown)\n")
	case l.Index().Snapshot().Complete():
		fmt.Fprintf(w, "mode:          whole mudlib\n")
	default:
		fmt.Fprintf(w, "mode:          whole mudlib, incomplete\n")
	}
	if prj.Profile != nil {
		fmt.Fprintf(w, "profile:       %s (%d stubs)\n", prj.Profile.Name, len(prj.Profile.Files))
	} else {
		fmt.Fprintf(w, "profile:       (none)\n")
	}
	if prj.CheckoutLib != "" {
		fmt.Fprintf(w, "checkout:      %s is %s\n", prj.CheckoutDir, prj.CheckoutLib)
	}
	fmt.Fprintf(w, "include dirs:  %s\n", orNone(strings.Join(cfg.IncludeDirs, ", ")))
	fmt.Fprintf(w, "include file:  %s\n", orNone(cfg.IncludeFile))
	fmt.Fprintf(w, "auto objects:  %s\n", orNone(strings.Join(cfg.AutoObjects, ", ")))
	tc := "(unknown: typechecking-dependent rules are silent)"
	if cfg.Dialect.Typechecking != nil {
		tc = fmt.Sprint(*cfg.Dialect.Typechecking)
	}
	fmt.Fprintf(w, "typechecking:  %s\n", tc)
	fmt.Fprintf(w, "baseline:      %s\n", orNone(l.BaselinePath()))
	for _, n := range slices.Concat(prj.Notes, l.Index().Snapshot().Notes()) {
		fmt.Fprintf(w, "note: %s\n", n.Message)
	}

	fmt.Fprintf(w, "\nsettings of %s\n", shown)
	for _, sc := range settings.Scopes {
		fmt.Fprintf(w, "scope:         %s\n", sc.Path)
	}
	for _, n := range scopes {
		fmt.Fprintf(w, "below:         %s (valid)\n", n)
	}
	for _, n := range projects {
		fmt.Fprintf(w, "below:         %s (a project of its own)\n", n)
	}
	f, from := settings.Format, settings.From
	fmt.Fprintf(w, "indent style:  %-9s (%s)\n", f.IndentStyle, from.IndentStyle)
	fmt.Fprintf(w, "indent width:  %-9d (%s)\n", f.IndentWidth, from.IndentWidth)
	fmt.Fprintf(w, "line endings:  %-9s (%s)\n", f.LineEndings, from.LineEndings)
	fmt.Fprintf(w, "rules:\n")
	for _, r := range rules {
		fmt.Fprintf(w, "  %s %-28s %s\n", r.Code, r.Name, r.Severity)
	}
	return 0
}

// nestedConfigs lists the configuration files below dir, the project's
// own aside, that a walk of dir meets: not those in directories the
// project excludes, and of a project below only its root config.
func nestedConfigs(ctx context.Context, fsys vfs.FS, prj *config.Project, dir string) ([]string, error) {
	var found []string
	err := prj.Tree().Walk(ctx, fsys, dir, []string{filepath.Ext(config.FileName)}, fileset.NewMatcher(prj.Config.Exclude), func(p string) {
		name := filepath.Base(p)
		if (name != config.FileName && name != config.AltFileName) || p == prj.ConfigPath {
			return
		}
		if other := prj.Nested(p); other == "" || other == p {
			found = append(found, p)
		}
	})
	return found, err
}
