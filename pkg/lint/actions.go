package lint

import (
	"context"
	"path"
	"strings"

	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/kfun"
	"github.com/mmcdole/dgdtools/pkg/pp"
	"github.com/mmcdole/dgdtools/pkg/resolve"
	"github.com/mmcdole/dgdtools/pkg/syntax"
	"github.com/mmcdole/dgdtools/pkg/token"
)

// Refactorings of a document: edits offered at a place that no finding
// carries, or that need more than the lint knew. Each edit is one that
// keeps the code compiling as it does and meaning what it means, or, for
// a fix, one that makes it compile as the finding asks (Unsafe: it may
// not be what was meant).

// Prototype returns the edit that declares the function called at byte
// offset off of the document before its first call: when the file
// defines the function after the calls, and nothing declares it before
// them (an inherited object, the auto object, a header, a kfun). DGD
// compiles a call to a function it knows nothing of yet as a call to an
// undefined one, which typechecking refuses ("undefined function"); with
// the prototype the call is to the function. The prototype is the
// definition's declaration as written (syntax.File.FuncDeclText), put
// above the declaration the first call is in and the comment above it.
// It is nil where there is no such call, and for a function whose
// declaration cannot be written above it as it is: one without a return
// type or with a parameter without one, a macro it names that is defined
// after that place, a directive in it.
func (d *Document) Prototype(off uint32) *diag.Fix {
	if d.resolver == nil || d.Tree == nil || d.Tree.Sel == nil || d.Unit == nil || d.Snapshot == nil {
		return nil
	}
	ref := d.resolver.At(off)
	if ref == nil || ref.Kind != resolve.Function || ref.Call != nil || ref.Decl || len(ref.Defs) != 1 {
		return nil
	}
	def := ref.Defs[0]
	if def.Path != d.Path || def.Func == nil || def.Func.PrototypeOnly {
		return nil
	}
	name := ref.Name
	tree, toks := d.Tree, d.File.Tokens

	// The file's declarations of the name, all after the calls; the
	// definition to declare.
	var fn *syntax.FuncDecl
	first := uint32(len(d.File.Src))
	tree.Funcs(func(f *syntax.FuncDecl) bool {
		if f.Name != nil && f.Name.Name == name {
			first = min(first, tree.Off(f))
			if f.Body != nil && fn == nil {
				fn = f
			}
		}
		return true
	})
	if fn == nil || off >= first {
		return nil
	}
	// The first call, and the declaration it is in.
	var call uint32
	found := false
	syntax.Inspect(tree, func(n syntax.Node) bool {
		c, ok := n.(*syntax.CallExpr)
		if !ok || c.Scoped || c.Fun == nil || c.Fun.Name != name {
			return true
		}
		at := tree.Off(c.Fun)
		if ti, _ := c.Fun.Span(); at < first && !tree.Sel.Guessed(ti) && (!found || at < call) {
			call, found = at, true
		}
		return true
	})
	if !found {
		return nil
	}
	var in syntax.Decl
	for _, decl := range tree.Decls {
		if a, b := decl.Span(); toks[a].Off <= call && call < toks[b].End {
			in = decl
			break
		}
	}
	if in == nil {
		return nil
	}

	// Nothing else declares it before the call.
	if !d.undeclaredElsewhere(name) {
		return nil
	}

	// The declaration as it can be written above.
	if fn.Type == nil {
		return nil
	}
	for _, p := range fn.Params {
		if p.Type == nil {
			return nil
		}
	}
	a, _ := fn.Span()
	at := d.insertionPoint(in)
	includesAfter := false
	if own := d.Env.Own(); own != nil {
		for _, inc := range own.Includes {
			includesAfter = includesAfter || inc.Off >= at
		}
	}
	for ti := a; ti <= fn.RParen; ti++ {
		switch t := toks[ti]; t.Kind {
		case token.Directive:
			return nil
		case token.Ident:
			if m := d.Env.Definition(string(d.File.Text(t))); m != nil && !macroBefore(m, d.Path, at, includesAfter) {
				return nil
			}
		}
	}
	return &diag.Fix{
		Title:         "Add the prototype of '" + name + "' before its first call",
		Applicability: diag.Safe,
		Edits:         []diag.TextEdit{{Start: at, End: at, NewText: tree.FuncDeclText(fn) + ";\n\n"}},
	}
}

// macroBefore reports whether the macro is defined at byte offset at of
// the file at path: by the file before it, or by another file when the
// file includes nothing after it.
func macroBefore(m *pp.Macro, path string, at uint32, includesAfter bool) bool {
	if m.Path == path {
		return m.Off < at && !m.Undefined
	}
	return !includesAfter
}

// undeclaredElsewhere reports whether no object the document's inherits,
// no auto object, no file it includes and no kfun has a function of the
// name: a call of it before the file's own declaration is a call to
// nothing the compiler knows.
func (d *Document) undeclaredElsewhere(name string) bool {
	s := d.Snapshot
	for _, inh := range d.Unit.Inherits {
		if !inh.Resolved {
			return false
		}
		if _, ok := s.ChainFunc(inh.Path, name); ok || s.ChainUnseen(inh.Path) {
			return false
		}
	}
	if !s.AutoMember(d.Unit.LibPath) {
		if _, ok := s.AutoFunc(name); ok || s.AutoUnseen() || s.AutoUnset() {
			return false
		}
	}
	for _, dep := range d.Unit.Deps[min(1, len(d.Unit.Deps)):] {
		if u := s.UnitAt(dep); u == nil || u.Partial {
			return false
		} else if _, ok := u.Func(name); ok {
			return false
		}
	}
	kfuns := d.resolver.Kfuns
	if kfuns == nil {
		kfuns = kfun.DGD()
	}
	return kfuns.Lookup(name) == nil
}

// insertionPoint returns where a declaration goes above the top-level
// declaration decl: the start of its line, above the comments right
// over it (no blank line between).
func (d *Document) insertionPoint(decl syntax.Decl) uint32 {
	toks, src := d.File.Tokens, d.File.Src
	a, _ := decl.Span()
	start := toks[a].Off
	newlines := 0
above:
	for ti := a - 1; ti >= 0; ti-- {
		switch t := toks[ti]; t.Kind {
		case token.Newline:
			if newlines++; newlines > 1 {
				break above // a blank line
			}
		case token.Space:
		case token.LineComment, token.BlockComment:
			if bol := lineStart(src, t.Off); strings.TrimSpace(string(src[bol:t.Off])) != "" {
				break above // the end of a line of code
			}
			start, newlines = t.Off, 0
		default:
			break above
		}
	}
	return lineStart(src, start)
}

// lineStart returns the offset of the start of the line off is on.
func lineStart(src []byte, start uint32) uint32 {
	for start > 0 && src[start-1] != '\n' && src[start-1] != '\r' {
		start--
	}
	return start
}

// PathFixes returns the fixes of a finding that a file a path names is
// missing (include-not-found, and target-object-missing where the path
// is one string as written): the path of the one file of the lib that
// has the missing file's name, written as the path was (an #include
// beside the file, from an include directory or from the lib's root; an
// object from the lib's root or as the lib reads relative paths), and
// resolved to that file. There is none when no file has the name, or
// more than one does. The files looked at are the include directories,
// the file's own tree (its directory's parent, or its directory at the
// lib's top), and every file the whole-lib scan read. Each fix is
// Unsafe: the file found may not be the one meant.
func (d *Document) PathFixes(ctx context.Context, f diag.Diagnostic) []diag.Fix {
	s := d.Snapshot
	if s == nil || !s.HasLib() || d.Unit == nil || int(f.End) > len(d.File.Src) || f.Start >= f.End {
		return nil
	}
	lib := s.LibFile(d.Path)
	if lib == "" {
		return nil
	}
	text := string(d.File.Src[f.Start:f.End])
	dirs := ownTree(lib)
	switch f.Rule {
	case ruleIncludeNotFound.Name:
		// The target of the #include, with its delimiters.
		var inc pp.Include
		var start uint32
		for _, i := range d.Env.Own().Includes {
			if raw := i.Raw(); i.Off >= f.Start && i.Off < f.End {
				if k := strings.Index(text, raw); k >= 0 {
					inc, start = i, f.Start+uint32(k)
				}
			}
		}
		if inc.Name == "" {
			return nil
		}
		incDirs, _ := s.IncludeDirs(lib)
		dirs = append(dirs, incDirs...)
		found := s.FilesNamed(ctx, path.Base(inc.Name), dirs)
		if len(found) != 1 {
			return nil
		}
		target := found[0]
		for _, c := range includePaths(inc, lib, target, incDirs) {
			want, _ := s.ResolveInclude(d.Path, pp.Include{Name: c[1 : len(c)-1], Angle: c[0] == '<'})
			if want != "" && s.LibFile(want) == target {
				return []diag.Fix{{Title: "Include " + c, Applicability: diag.Unsafe,
					Edits: []diag.TextEdit{{Start: start, End: start + uint32(len(inc.Raw())), NewText: c}}}}
			}
		}
	case ruleTargetObjectMissing.Name:
		if len(text) < 2 || text[0] != '"' || text[len(text)-1] != '"' || strings.ContainsAny(text[1:len(text)-1], "\"\\") {
			return nil
		}
		written := text[1 : len(text)-1]
		ext := ""
		if strings.HasSuffix(written, ".c") {
			ext = ".c"
		}
		found := s.FilesNamed(ctx, path.Base(strings.TrimSuffix(written, ".c"))+".c", dirs)
		if len(found) != 1 {
			return nil
		}
		obj := strings.TrimSuffix(found[0], ".c")
		from := s.LibPath(d.Path)
		var cands []string
		if !strings.HasPrefix(written, "/") {
			cands = append(cands, strings.TrimPrefix(obj, "/")+ext)
			if rel, ok := strings.CutPrefix(obj, strings.TrimSuffix(path.Dir(from), "/")+"/"); ok {
				cands = append(cands, rel+ext)
			}
		}
		cands = append(cands, obj+ext)
		for _, c := range cands {
			if s.ObjectPath(c, from) == obj && s.ObjectExists(obj) {
				return []diag.Fix{{Title: "Name " + obj, Applicability: diag.Unsafe,
					Edits: []diag.TextEdit{{Start: f.Start + 1, End: f.End - 1, NewText: c}}}}
			}
		}
	}
	return nil
}

// ownTree returns the lib directory a file's missing file is looked for
// under, besides the include directories: its directory's parent, or its
// directory when that is at the lib's top.
func ownTree(lib string) []string {
	dir := path.Dir(lib)
	if parent := path.Dir(dir); parent != "/" {
		dir = parent
	}
	return []string{dir}
}

// includePaths returns the ways to write an #include of target (a lib
// file) in the file lib that keep the way inc was written, the most like
// it first, each with its delimiters.
func includePaths(inc pp.Include, lib, target string, includeDirs []string) []string {
	var out []string
	if !inc.Angle {
		if strings.HasPrefix(inc.Name, "/") {
			out = append(out, `"`+target+`"`)
		}
		if rel, ok := strings.CutPrefix(target, strings.TrimSuffix(path.Dir(lib), "/")+"/"); ok {
			out = append(out, `"`+rel+`"`)
		}
	}
	for _, d := range includeDirs {
		if rel, ok := strings.CutPrefix(target, strings.TrimSuffix(d, "/")+"/"); ok {
			if inc.Angle {
				out = append(out, "<"+rel+">")
			} else {
				out = append(out, `"`+rel+`"`)
			}
		}
	}
	return append(out, `"`+target+`"`)
}
