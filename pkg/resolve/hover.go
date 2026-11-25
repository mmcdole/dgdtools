package resolve

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mmcdole/dgdtools/pkg/calls"
	"github.com/mmcdole/dgdtools/pkg/kfun"
	"github.com/mmcdole/dgdtools/pkg/lexer"
	"github.com/mmcdole/dgdtools/pkg/pp"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/syntax"
	"github.com/mmcdole/dgdtools/pkg/token"
)

// Hover is what there is to tell a reader about a place in a file: what
// is named there (Ref) and what is known of each thing the name may be
// (Infos, one for each declaration; one for a function of the driver's
// or a call whose receiver is not known, which have none).
type Hover struct {
	Ref   *Ref
	Infos []Info
}

// Info is what is known of one thing a name refers to.
type Info struct {
	Kind Kind
	Name string
	// Code is the declaration, as LPC: a function's with its specifiers,
	// types and parameters, a variable's with its type, a macro's
	// #define with its parameters and body, an inherit statement for a
	// label, an #include line. It is "" for an object named by its path
	// and for a call whose receiver is not known.
	Code string
	// Doc is the comment that stands directly above the declaration,
	// without its comment marks, or the line the kfun table has; "" when
	// there is none. A profile's stub keeps the comment of each
	// function and variable it declares, and no other.
	Doc string
	// Class is the class of a macro, what the parser takes its uses for
	// (pp.Class).
	Class string
	// Note says what is not known, where that is the answer: the
	// receiver of a call.
	Note string
	Origin
}

// Origin is where a declaration is.
type Origin struct {
	// Kfun says the function is a kernel function, which no file
	// declares; Driver is the driver whose table has it ("DGD 1.7.9"),
	// "" when the project's configuration declares it.
	Kfun   bool
	Driver string
	// Auto says the function is one the auto object gives every object.
	Auto bool
	// Configured says the macro is one the project's configuration
	// says what to take for (conventions), which no file read defines.
	Configured bool
	// File is the file the declaration is in, by its name in the lib
	// ("/std/room.c"), or by its path when it is outside the lib. Line
	// is the 1-based line of the declared name; 0 when the file is meant
	// as a whole, or is a stub, whose lines are nobody's.
	File string
	Line int
	// Profile is the name of the profile when the declaration is in one
	// of its stubs and not in a file of the checkout.
	Profile string
}

// String says where the declaration is, in words: "/std/room.c:12",
// "auto object, /sys/auto.c:3", "profile viking, /std/room.c", "kfun,
// DGD 1.7.9". It is "" when nothing is known.
func (o Origin) String() string {
	var parts []string
	switch {
	case o.Kfun && o.Driver != "":
		parts = append(parts, "kfun", o.Driver)
	case o.Kfun:
		parts = append(parts, "kfun", "declared by the project (dialect.kfuns)")
	case o.Configured:
		parts = append(parts, "the project's configuration (conventions)")
	}
	if o.Auto {
		parts = append(parts, "auto object")
	}
	if o.Profile != "" {
		parts = append(parts, "profile "+o.Profile)
	}
	if o.File != "" {
		file := o.File
		if o.Line > 0 {
			file += ":" + strconv.Itoa(o.Line)
		}
		parts = append(parts, file)
	}
	return strings.Join(parts, ", ")
}

// UnknownReceiver is the Note of a call whose receiver is not known.
const UnknownReceiver = "the receiver is not known: which object's function this calls cannot be told"

// maxBody is how much of a macro's body a hover shows.
const maxBody = 400

// Hover returns what is known of what the token at byte offset off of
// the file names, nil when that is nothing: no name (At), or a name of
// which nothing can be said, as an #include that leads nowhere or a
// function that is declared nowhere the tools see. A function of the
// driver's is described from the project's kfun table; a call whose
// receiver is not known is said to be one, and nothing is guessed about
// where it goes.
func (r *Resolver) Hover(off uint32) *Hover {
	ref := r.At(off)
	if ref == nil {
		return nil
	}
	h := &Hover{Ref: ref}
	texts := texts{r: r}
	for i := range ref.Defs {
		h.Infos = append(h.Infos, r.info(ref, &ref.Defs[i], &texts))
	}
	if len(ref.Defs) == 0 {
		switch ref.Kind {
		case Function:
			if ref.Call != nil {
				if ref.Call.Target == calls.TargetUnknown {
					h.Infos = []Info{{Kind: Function, Name: ref.Name, Note: UnknownReceiver}}
				}
			} else if kf := r.kfun(ref.Name); kf != nil {
				h.Infos = []Info{kfunInfo(kf)}
			}
		case Macro:
			info := Info{Kind: Macro, Name: ref.Name, Code: "#define " + ref.Name, Origin: Origin{Configured: true}}
			if m := r.Env.Lookup(ref.Name); m != nil {
				info.Class = m.Class.String()
			}
			h.Infos = []Info{info}
		}
	}
	if len(h.Infos) == 0 {
		return nil
	}
	return h
}

// kfuns returns the project's table of kernel functions.
func (r *Resolver) kfuns() *kfun.Table {
	if r.Kfuns != nil {
		return r.Kfuns
	}
	return kfun.DGD()
}

// kfun returns the kernel function that a call of name in the file's
// own code reaches when no function of the lib has the name, nil when
// the driver has none of that name or that cannot be told. It can be
// told when all of the object's program is known: an object of its
// chain that the index cannot see, the auto object among them, may have
// a function of the name, which would be the one called. The auto
// object's function of the name stands before the driver's wherever the
// name is written (label::name too).
func (r *Resolver) kfun(name string) *kfun.Func {
	if r.u.LibPath == "" || r.s.AutoUnseen() || r.s.Chain(r.u.LibPath).Unseen {
		return nil
	}
	if _, masked := r.s.AutoFunc(name); masked && !r.s.AutoMember(r.u.LibPath) {
		return nil
	}
	return r.kfuns().Lookup(name)
}

func kfunInfo(kf *kfun.Func) Info {
	return Info{Kind: Function, Name: kf.Name, Code: kf.Decl, Doc: kf.Doc, Origin: Origin{Kfun: true, Driver: kf.Origin}}
}

// info describes a declaration a reference resolves to.
func (r *Resolver) info(ref *Ref, d *Def, texts *texts) Info {
	info := Info{Kind: d.Kind, Name: d.Name, Origin: r.origin(d.Path)}
	info.Auto = d.Auto
	switch d.Kind {
	case Function:
		if d.Func != nil {
			info.Code = d.Func.Decl
		}
		info.Line, info.Doc = texts.declared(d.Path, d.Start, info.Profile != "")
	case Variable:
		info.Code = variableCode(d)
		info.Line, info.Doc = texts.declared(d.Path, d.Start, info.Profile != "")
	case Macro:
		if m := d.Macro; m != nil {
			info.Code = define(m)
			info.Class = m.Class.String()
			info.Line, info.Doc = texts.declared(d.Path, m.Off, info.Profile != "")
		} else {
			// The #define is being written, or was not read.
			info.Code = "#define " + d.Name
			info.Line, _ = texts.declared(d.Path, d.Start, info.Profile != "")
		}
		if m := r.Env.Lookup(d.Name); m != nil {
			info.Class = m.Class.String() // what the project says it is, before what its body says
		}
	case Local, Parameter:
		if d.Local != nil {
			info.Code = join(d.Local.Type, d.Name)
		}
		info.Line = r.Tok.Pos(d.Start).Line
	case Label:
		for _, decl := range r.Tree.Decls {
			in, ok := decl.(*syntax.InheritDecl)
			if !ok || in.Label == nil || r.Tree.Off(in.Label) != d.Start {
				continue
			}
			first, last := in.Span()
			info.Code = r.Tree.SourceText(first, last)
			// The label's meaning is the object it stands for.
			info.Origin = Origin{}
			for _, u := range r.u.Inherits {
				if u.Resolved && in.Path != nil && u.Off == r.Tree.Off(in.Path) && r.s.ObjectExists(u.Path) {
					info.Origin = r.origin(r.s.ObjectFile(u.Path))
				}
			}
		}
	case Include:
		info.Code = "#include " + string(r.Tok.Src[ref.Start:ref.End])
	case Object:
	}
	info.Code = utf8Text(info.Code)
	return info
}

// variableCode writes a global variable as it is declared: "private
// static int weight".
func variableCode(d *Def) string {
	if d.Var == nil {
		return ""
	}
	code := join(d.Var.Type, d.Name)
	if d.Var.Static {
		code = "static " + code
	}
	if d.Var.Private {
		code = "private " + code
	}
	return code
}

// origin says where the file at path is.
func (r *Resolver) origin(path string) Origin {
	var o Origin
	if name, file, stub := profile.SplitLocation(path); stub {
		o.Profile, o.File = name, file
	} else if o.File = r.s.LibFile(path); o.File == "" {
		o.File = path
	}
	return o
}

// join writes a type before a name: "int x", "string *x".
func join(typ, name string) string {
	switch {
	case typ == "":
		return name
	case strings.HasSuffix(typ, "*"):
		return typ + name
	}
	return typ + " " + name
}

// define writes a macro as its #define, the body cut where it is long.
func define(m *pp.Macro) string {
	s := "#define " + m.Name
	if m.FuncLike {
		s += "(" + strings.Join(m.Params, ", ") + ")"
	}
	body := oneLine(m.Body)
	if len(body) > maxBody {
		cut := maxBody
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		body = body[:cut] + " ..."
	}
	if body != "" {
		s += " " + body
	}
	return s
}

// oneLine writes a macro's body with each run of space outside a string
// or character constant as one space: the body of a #define that runs
// over several lines has their indentation in it.
func oneLine(body string) string {
	var b strings.Builder
	quote := byte(0)
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case quote != 0:
			if c == '\\' && i+1 < len(body) {
				b.WriteByte(c)
				i++
				c = body[i]
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			for i+1 < len(body) && (body[i+1] == ' ' || body[i+1] == '\t' || body[i+1] == '\n' || body[i+1] == '\r') {
				i++
			}
			c = ' '
		}
		b.WriteByte(c)
	}
	return b.String()
}

// texts are the files one answer reads, each lexed once.
type texts struct {
	r     *Resolver
	files map[string]*token.File
}

// file returns the tokens of the file at path, in the text the
// resolver's generation of the index reads (Snapshot.ReadFile: an
// editor's buffer for a file that is open); nil when it cannot be read.
func (t *texts) file(path string) *token.File {
	if path == t.r.Tok.Path {
		return t.r.Tok
	}
	if f, ok := t.files[path]; ok {
		return f
	}
	var f *token.File
	if src, err := t.r.s.ReadFile(path); err == nil {
		f = lexer.Lex(path, src, t.r.Tok.Dialect)
	}
	if t.files == nil {
		t.files = map[string]*token.File{}
	}
	t.files[path] = f
	return f
}

// declared returns the line of the byte offset off of the file at path
// and the comment directly above the declaration that the token at off
// is of (Comment). A stub keeps the comment of each declaration in it
// (CommentSource), but its lines are not those of the file it stands
// for: the line is 0.
func (t *texts) declared(path string, off uint32, stub bool) (line int, doc string) {
	f := t.file(path)
	if f == nil || off > uint32(len(f.Src)) {
		return 0, ""
	}
	if stub {
		return 0, Comment(f, off)
	}
	return f.Pos(off).Line, Comment(f, off)
}

// Comment returns the comment that documents the declaration which the
// token at byte offset off of f belongs to: the one directly above it,
// which is a block comment, or a run of // lines, that ends on the line
// before the one the declaration begins on (or on that line, before
// it), with no code before it on the lines it stands on. The
// declaration begins after the last ';' or '}' or directive before off;
// a directive is its own beginning.
//
// The comment is returned as its text: without the marks, the '*' that
// may lead its lines and the indentation they share, and as UTF-8 (a
// text that is not valid UTF-8 is read as Latin-1). It is "" when there
// is none.
func Comment(f *token.File, off uint32) string {
	first, last := docComment(f, off)
	if first < 0 {
		return ""
	}
	toks := f.Tokens
	if toks[first].Kind == token.BlockComment {
		return utf8Text(strings.Join(trimLines(blockLines(f.Text(toks[first])), true), "\n"))
	}
	var lines []string
	for _, t := range toks[first : last+1] {
		if t.Kind == token.LineComment {
			lines = append(lines, strings.TrimPrefix(string(f.Text(t)), "//"))
		}
	}
	return utf8Text(strings.Join(trimLines(lines, false), "\n"))
}

// CommentSource returns the comment Comment reads for the token at byte
// offset off of f as it is written: from the start of the comment to its
// end, "" when there is none. Put on the line above a copy of the
// declaration, it gives the copy the same Comment.
func CommentSource(f *token.File, off uint32) string {
	if Comment(f, off) == "" {
		return ""
	}
	first, last := docComment(f, off)
	return string(f.Src[f.Tokens[first].Off:f.Tokens[last].End])
}

// docComment returns the indexes of the first and the last token of the
// comment Comment reads, or -1 and -1.
func docComment(f *token.File, off uint32) (first, last int) {
	toks := f.Tokens
	i := f.TokenAt(off)
	// Back to the first token of the declaration.
	if toks[i].Kind != token.Directive {
		for i > 0 {
			k := toks[i-1].Kind
			if k == token.Semicolon || k == token.RBrace || k == token.Directive || k == token.LBrace ||
				k == token.LineComment || k == token.BlockComment {
				break
			}
			i--
		}
		for i < len(toks) && (toks[i].Kind == token.Space || toks[i].Kind == token.Newline) {
			i++
		}
	}
	// Above it: space, at most one line break, and the comment.
	j, breaks := i-1, 0
	for ; j >= 0 && (toks[j].Kind == token.Space || toks[j].Kind == token.Newline); j-- {
		if toks[j].Kind == token.Newline {
			breaks++
		}
	}
	if j < 0 || breaks > 1 {
		return -1, -1
	}
	// alone reports whether nothing but space stands before the token
	// at index k on its line.
	alone := func(k int) bool {
		for k--; k >= 0 && toks[k].Kind == token.Space; k-- {
		}
		return k < 0 || toks[k].Kind == token.Newline
	}
	switch toks[j].Kind {
	case token.BlockComment:
		if alone(j) {
			return j, j
		}
	case token.LineComment:
		// The run of // lines that ends here, each alone on its line.
		if !alone(j) {
			return -1, -1
		}
		last = j
		for {
			// The line above, when it is such a comment too.
			k := j - 1
			for ; k >= 0 && toks[k].Kind == token.Space; k-- {
			}
			if k < 1 || toks[k].Kind != token.Newline || toks[k-1].Kind != token.LineComment || !alone(k-1) {
				break
			}
			j = k - 1
		}
		return j, last
	}
	return -1, -1
}

// blockLines returns the lines of a block comment without its marks and
// the '*' that leads a line.
func blockLines(text []byte) []string {
	s := strings.TrimSuffix(strings.TrimPrefix(string(text), "/*"), "*/")
	// "/**" and "**/" are marks too.
	s = strings.TrimRight(strings.TrimLeft(s, "*"), "*")
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, line := range lines {
		if i == 0 {
			continue
		}
		if t := strings.TrimLeft(line, " \t"); strings.HasPrefix(t, "*") {
			lines[i] = strings.TrimLeft(t, "*")
		}
	}
	return lines
}

// trimLines drops the empty lines at both ends, the space at the end of
// each line and the indentation all lines share. The first line of a
// block comment stands after the mark, and has no indentation to share.
func trimLines(lines []string, block bool) []string {
	for i, line := range lines {
		lines[i] = strings.TrimRight(strings.ReplaceAll(line, "\t", "        "), " \r")
		if i == 0 && block {
			lines[i] = strings.TrimLeft(lines[i], " ")
		}
	}
	first := ""
	if block && len(lines) > 1 && lines[0] != "" {
		first, lines = lines[0], lines[1:]
	}
	for first == "" && len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	indent := -1
	for _, line := range lines {
		if line == "" {
			continue
		}
		n := len(line) - len(strings.TrimLeft(line, " "))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	for i, line := range lines {
		if line != "" {
			lines[i] = line[indent:]
		}
	}
	if first != "" {
		lines = append([]string{first}, lines...)
	}
	return lines
}

// utf8Text returns s as UTF-8: as it is when it is that, and read as
// Latin-1 when not, which is what a lib's old files are written in.
func utf8Text(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b bytes.Buffer
	for i := 0; i < len(s); i++ {
		b.WriteRune(rune(s[i]))
	}
	return b.String()
}
