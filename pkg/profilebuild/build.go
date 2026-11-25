// Package profilebuild makes a profile (pkg/profile) out of a whole lib:
// it selects the lib's shared files and renders each as a stub.
package profilebuild

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/mmcdole/dgdtools/internal/par"
	"github.com/mmcdole/dgdtools/pkg/calls"
	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/fileset"
	"github.com/mmcdole/dgdtools/pkg/index"
	"github.com/mmcdole/dgdtools/pkg/pp"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/resolve"
	"github.com/mmcdole/dgdtools/pkg/syntax"
	"github.com/mmcdole/dgdtools/pkg/token"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// Build captures the project's lib as a profile: the driver facts and
// conventions from the configuration, and a stub for every shared file.
// Shared files are those referenced from at least
// profile_build.share_min_owners owners, those matching
// profile_build.share, everything under the include dirs, the
// force-included header, the auto objects, and whatever those inherit or
// include, minus profile_build.omit.
//
// It needs the whole lib: a profile cannot be built from a partial
// checkout.
func Build(ctx context.Context, prj *config.Project, fsys vfs.FS) (*profile.Profile, error) {
	cfg, ix := prj.Config, index.Open(prj, fsys).Snapshot()
	if prj.Root == "" {
		return nil, fmt.Errorf("profile build needs the whole mudlib (a dgdtools.yml with root: at the mudlib)")
	}
	if !ix.Complete() {
		var why []string
		for _, n := range ix.Notes() {
			why = append(why, n.Message)
		}
		return nil, fmt.Errorf("profile build: %s", strings.Join(why, "; "))
	}
	pb := cfg.ProfileBuild
	share := pb.Share
	shared := fileset.NewMatcher(share)
	minOwners := pb.MinOwners()
	omit := fileset.NewMatcher(pb.Omit)
	incDirs := map[string]bool{}
	for _, d := range cfg.IncludeDirs {
		incDirs[strings.Trim(d, "/")] = true
	}
	underInc := func(rel string) bool {
		for d := path.Dir(rel); d != "." && d != "/"; d = path.Dir(d) {
			if incDirs[d] {
				return true
			}
		}
		return false
	}

	p := &profile.Profile{Files: map[string]string{}}
	p.Format = profile.Format
	p.Holes = append(append([]string{}, pb.Omit...), cfg.Exclude...)
	p.Name = pb.Name
	if p.Name == "" {
		p.Name = filepath.Base(prj.Root)
	}
	d := cfg.TokenDialect()
	p.Dialect = profile.Dialect{
		Language: "lpc", Driver: "dgd", Version: "1.7",
		SlashSlash: d.SlashSlash, Closures: d.Closures,
		Typechecking: cfg.Dialect.Typechecking, Predefined: pp.Predefined,
		Kfuns: cfg.Dialect.Kfuns,
	}
	p.IncludeDirs, p.IncludeFile, p.AutoObjects = cfg.IncludeDirs, cfg.IncludeFile, cfg.AutoObjects
	p.DriverObject = cfg.DriverObject
	p.RelativeObjectPaths = cfg.RelativeObjectPaths
	p.Checkouts = pb.Checkouts
	p.Conventions = cfg.Conventions

	// Seeds: shared files and include dirs, by walking the lib once.
	var queue []string
	queued := map[string]bool{}
	add := func(fsPath string) {
		fsPath = filepath.Clean(fsPath)
		rel, ok := fileset.Under(prj.Root, fsPath)
		if !ok {
			return // outside the lib: nothing to say about it
		}
		rel = filepath.ToSlash(rel)
		if queued[fsPath] || omit.Match(rel) || !fsys.Exists(fsPath) || fsys.IsDir(fsPath) {
			return
		}
		queued[fsPath] = true
		queue = append(queue, fsPath)
	}
	covered := map[string]bool{}
	// The walk the linter makes, less what is never to be in a profile.
	skip := fileset.NewMatcher(slices.Concat(cfg.Exclude, pb.Omit))
	err := prj.Tree().Walk(ctx, fsys, prj.Root, []string{".c", ".h"}, skip, func(fp string) {
		if rel, ok := prj.Tree().Rel(fp); ok && (shared.Match(rel) || underInc(rel)) {
			add(fp)
		}
	})
	if err != nil {
		return nil, err
	}
	if minOwners > 0 {
		for _, fp := range sharedByUse(ctx, ix, calls.NewRegistry(cfg.Conventions), prj.Root, minOwners, pb.Checkouts) {
			add(fp)
		}
	}
	// A directory is covered when a share glob takes all of it.
	for _, g := range share {
		if dir, ok := strings.CutSuffix(strings.Trim(g, "/"), "/**"); ok && !strings.ContainsAny(dir, "*?") {
			if fsys.IsDir(filepath.Join(prj.Root, filepath.FromSlash(dir))) && !omit.MatchDir(dir) {
				covered["/"+dir+"/"] = true
			}
		}
	}
	for d := range incDirs {
		covered["/"+d+"/"] = true
	}
	if f := cfg.IncludeFile; f != "" {
		add(filepath.Join(prj.Root, filepath.FromSlash(f)))
	}
	for _, a := range cfg.AutoObjects {
		add(ix.ObjectFile(index.NormalizeLibPath(a, "/")))
	}

	// Closure: everything a shared file includes or inherits is needed to
	// read it. Files that are #included by another file are headers in
	// effect, whatever their extension: their macros are part of what
	// they give their includers.
	included := map[string]bool{}
	for i := 0; i < len(queue); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		parsed, err := ix.Parse(queue[i])
		if err != nil {
			continue
		}
		for _, dep := range parsed.Unit.Deps[1:] {
			included[filepath.Clean(dep)] = true
			add(dep)
		}
		for _, inh := range parsed.Unit.Inherits {
			if inh.Resolved {
				add(ix.ObjectFile(inh.Path))
			}
		}
	}
	for _, fsPath := range queue {
		parsed, err := ix.Parse(fsPath)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(prj.Root, fsPath)
		p.Files["/"+filepath.ToSlash(rel)] = Stub(parsed, included[fsPath] || path.Ext(fsPath) != ".c")
	}
	for c := range covered {
		p.Covered = append(p.Covered, c)
	}
	sort.Strings(p.Covered)
	// A file the tools crashed on is missing from the profile, or from
	// what was counted as shared: not a profile to publish.
	if crashed := ix.Crashed(); len(crashed) > 0 {
		return nil, fmt.Errorf("profile build: %d file(s) could not be analyzed; the first: %w", len(crashed), crashed[0])
	}
	return p, nil
}

// sharedByUse scans the whole lib and returns the files referenced from
// at least minOwners distinct owners. A reference is an inherit, an
// #include, or a constant call/clone/find target. An owner is a
// per-wizard tree matching one of the checkout patterns, or else a
// top-level directory.
func sharedByUse(ctx context.Context, ix *index.Snapshot, reg *calls.Registry, root string, minOwners int, checkouts []string) []string {
	owner := func(lib string) string {
		segs := strings.Split(strings.Trim(lib, "/"), "/")
		for _, pat := range checkouts {
			want := strings.Split(strings.Trim(pat, "/"), "/")
			if len(segs) > len(want) {
				ok := true
				for i, w := range want {
					if w != "*" && w != segs[i] {
						ok = false
					}
				}
				if ok {
					return strings.Join(segs[:len(want)], "/")
				}
			}
		}
		return segs[0]
	}
	files, _ := ix.Files(ctx, root)
	var mu sync.Mutex
	owners := map[string]map[string]bool{}
	par.Each(ctx, files, func(f string) {
		parsed, err := ix.Parse(f)
		if err != nil || parsed.Unit.LibPath == "" {
			return
		}
		u := parsed.Unit
		refs := append([]string{}, u.Deps[1:]...)
		for _, inh := range u.Inherits {
			if inh.Resolved {
				refs = append(refs, ix.ObjectFile(inh.Path))
			}
		}
		cf := calls.Extract(calls.File{
			Tree: parsed.Tree, Env: parsed.Env, LibPath: u.LibPath,
			ObjectPath: func(written string) string { return ix.ObjectPath(written, u.LibPath) },
		}, reg)
		for _, c := range cf.Named {
			if c.Target == calls.TargetPath {
				refs = append(refs, ix.ObjectFile(c.TargetPath))
			}
		}
		for _, r := range cf.Paths {
			refs = append(refs, ix.ObjectFile(r.Path))
		}
		me := owner(u.LibPath)
		mu.Lock()
		for _, r := range refs {
			if r == "" {
				continue
			}
			r = filepath.Clean(r)
			if owners[r] == nil {
				owners[r] = map[string]bool{}
			}
			owners[r][me] = true
		}
		mu.Unlock()
	})
	var out []string
	for f, o := range owners {
		if len(o) >= minOwners {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// Stub renders the public interface of a parsed file as source, the way
// a C header or a .d.ts file does: its #include directives, its inherit
// statements, and every non-private function and global variable
// declaration exactly as written (specifiers, types, parameter names,
// varargs and ellipsis), with function bodies replaced by `{}`. Each of
// those declarations keeps the comment hover shows for it
// (resolve.Comment); other comments, bodies, private functions and
// private variables are left out. A declaration written without a type
// stays without one: that matters to a driver with typechecking on.
//
// header says the file is #included by others: all of its macros are
// kept. A plain object keeps only the macros its declarations use (a type
// macro, a path macro in an inherit statement).
//
// A file that could not be fully read says so with
// `#pragma dgdtools partial`, which makes every lookup through it unknown
// rather than wrong.
func Stub(p *index.Parsed, header bool) string {
	f, tree := p.Tok, p.Tree
	partial := tree.Partial
	for _, e := range tree.Errors {
		if e.Severe {
			partial = true
		}
	}

	// A piece is the stub's text for one declaration of the file, and
	// the comment hover shows for it, which goes above that text.
	type piece struct {
		node syntax.Node
		doc  string
		text string
	}
	// doc returns the comment of a declaration that the stub keeps.
	doc := func(n syntax.Node) string {
		first, _ := n.Span()
		if c := resolve.CommentSource(f, f.Tokens[first].Off); c != "" {
			return c + "\n"
		}
		return ""
	}
	var decls []piece
	for _, d := range tree.Decls {
		switch d := d.(type) {
		case *syntax.InheritDecl:
			decls = append(decls, piece{d, "", tree.SourceText(d.First, d.Last) + "\n"})
		case *syntax.FuncDecl:
			if d.Name != nil && !d.Has(token.KwPrivate) {
				end := " {}\n"
				if d.Body == nil {
					end = ";\n"
				}
				decls = append(decls, piece{d, doc(d), tree.FuncDeclText(d) + end})
			}
		case *syntax.VarDecl:
			if d.Has(token.KwPrivate) {
				continue
			}
			// The variables are one declaration in the stub, and each
			// prototype is one; the comment goes above each.
			c := doc(d)
			if len(d.Vars) > 0 {
				var names []string
				for _, v := range d.Vars {
					names = append(names, strings.Repeat("*", v.Stars)+v.Name.Name)
				}
				decls = append(decls, piece{d, c, tree.DeclPrefix(d.Specs, d.Type) + strings.Join(names, ", ") + ";\n"})
			}
			for _, proto := range d.Protos {
				if proto.Name != nil {
					decls = append(decls, piece{d, c, tree.FuncDeclText(proto) + ";\n"})
				}
			}
		case *syntax.BadDecl:
			partial = true
		}
	}

	// Which of the file's own macros do the declarations need? Close
	// over macro bodies, since one macro may be written with another.
	needed := map[string]bool{}
	if !header {
		own := p.Env.Own().Macros
		var mark func(text string)
		mark = func(text string) {
			for _, w := range strings.FieldsFunc(text, func(r rune) bool {
				return !(r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
			}) {
				if m := own[w]; m != nil && !needed[w] {
					needed[w] = true
					mark(m.Body)
				}
			}
		}
		for _, d := range decls {
			mark(d.text)
		}
	}

	var b strings.Builder
	// Directives, in order. Conditionals were already resolved by branch
	// selection and are not reproduced.
	for _, i := range tree.Sel.Dirs {
		name, rest := pp.SplitDirective(f.Text(f.Tokens[i]))
		rest = strings.TrimSpace(rest)
		switch name {
		case "include":
			b.WriteString("#include " + rest + "\n")
		case "define", "undef":
			macro := rest
			if j := strings.IndexAny(rest, " \t("); j >= 0 {
				macro = rest[:j]
			}
			if header || needed[macro] {
				b.WriteString("#" + name + " " + rest + "\n")
			}
		}
	}
	for _, d := range decls {
		// A declaration in a conditional branch that was only guessed at
		// is kept under a condition the reader will also only guess at.
		if a, _ := d.node.Span(); tree.Sel.Guessed(a) {
			b.WriteString("#ifdef __DGDTOOLS_UNDECIDED__\n" + d.doc + d.text + "#endif\n")
		} else {
			b.WriteString(d.doc + d.text)
		}
	}
	if partial {
		b.WriteString("#pragma dgdtools partial\n")
	}
	return b.String()
}
