package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/mmcdole/dgdtools/internal/fuzzy"

	"github.com/mmcdole/dgdtools/pkg/editorconfig"
	"github.com/mmcdole/dgdtools/pkg/fileset"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// Format are the formatter's settings (docs/design/formatter.md). A zero
// field is not set: the value comes from an outer configuration, from
// .editorconfig, or is the default.
type Format struct {
	// IndentStyle is what a level of indentation is written with.
	IndentStyle IndentStyle `yaml:"indent_style"`
	// IndentWidth is the number of spaces per level, 1 to MaxIndentWidth.
	IndentWidth int `yaml:"indent_width"`
	// LineEndings is the newline policy.
	LineEndings LineEndings `yaml:"line_endings"`
}

// IndentStyle is "space" or "tab".
type IndentStyle string

const (
	IndentSpace IndentStyle = "space"
	IndentTab   IndentStyle = "tab"
)

// LineEndings is "preserve" (each line break keeps its bytes), "lf" or
// "crlf".
type LineEndings string

const (
	LineEndingsPreserve LineEndings = "preserve"
	LineEndingsLF       LineEndings = "lf"
	LineEndingsCRLF     LineEndings = "crlf"
)

// The defaults, and the widest indentation accepted.
const (
	DefaultIndentStyle = IndentSpace
	DefaultIndentWidth = 4
	DefaultLineEndings = LineEndingsPreserve
	MaxIndentWidth     = 16
)

// ParseIndentStyle maps a configuration or command-line spelling.
func ParseIndentStyle(s string) (IndentStyle, error) {
	switch v := IndentStyle(s); v {
	case IndentSpace, IndentTab:
		return v, nil
	}
	return "", fmt.Errorf("unknown indent style %q (want space or tab)", s)
}

// ParseLineEndings maps a configuration or command-line spelling.
func ParseLineEndings(s string) (LineEndings, error) {
	switch v := LineEndings(s); v {
	case LineEndingsPreserve, LineEndingsLF, LineEndingsCRLF:
		return v, nil
	}
	return "", fmt.Errorf("unknown line endings %q (want preserve, lf or crlf)", s)
}

// CheckIndentWidth reports whether n is an indentation width.
func CheckIndentWidth(n int) error {
	if n < 1 || n > MaxIndentWidth {
		return fmt.Errorf("indent width %d is not between 1 and %d", n, MaxIndentWidth)
	}
	return nil
}

// UnmarshalYAML reads format: as Format, with indent_width a whole
// number from 1 to MaxIndentWidth as it is written: 0, 1.5 or "4" is an
// error, not the default or 1.
func (f *Format) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return at(n, errors.New("format: want a mapping of indent_style, indent_width and line_endings"))
	}
	keys := []string{"indent_style", "indent_width", "line_endings"}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		var err error
		switch k.Value {
		case "indent_style":
			err = f.IndentStyle.UnmarshalYAML(v)
		case "line_endings":
			err = f.LineEndings.UnmarshalYAML(v)
		case "indent_width":
			w, aerr := strconv.Atoi(v.Value)
			switch {
			case v.Kind != yaml.ScalarNode || v.Tag != "!!int" || aerr != nil:
				err = at(v, fmt.Errorf("format.indent_width: %s is not a whole number", v.Value))
			default:
				if cerr := CheckIndentWidth(w); cerr != nil {
					err = at(v, fmt.Errorf("format.indent_width: %w", cerr))
				}
				f.IndentWidth = w
			}
		default:
			msg := fmt.Sprintf("unknown key %q in format", k.Value)
			if near := fuzzy.Nearest(k.Value, keys); near != "" {
				msg += fmt.Sprintf(" (did you mean %q?)", near)
			}
			err = at(k, errors.New(msg))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (v *IndentStyle) UnmarshalYAML(n *yaml.Node) (err error) {
	*v, err = ParseIndentStyle(n.Value)
	return at(n, err)
}

func (v *LineEndings) UnmarshalYAML(n *yaml.Node) (err error) {
	*v, err = ParseLineEndings(n.Value)
	return at(n, err)
}

// at puts the line of a YAML node in front of an error about it.
func at(n *yaml.Node, err error) error {
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	return nil
}

// Settings are what one file is formatted and linted by: the project's
// own settings as adjusted by the configuration files between the
// project and the file (docs/design/config.md).
type Settings struct {
	// Format has every key set. For each key the first of these that
	// sets it wins: the caller's overrides, the nearest settings scope
	// and then outer ones, the project's config, the file's
	// .editorconfig properties, the default.
	Format Format
	// From says where each key of Format came from: a file, or one of
	// FromFlag and FromDefault.
	From FormatSources
	// Scopes are the configuration files below the project's that apply
	// to the file, outermost first. The linter applies their rules in
	// that order after the project's own.
	Scopes []*Scope
}

// FormatSources names the source of each format key.
type FormatSources struct {
	IndentStyle, IndentWidth, LineEndings string
}

// Where a setting comes from when not from a file.
const (
	FromFlag    = "command line"
	FromDefault = "default"
)

// Scope is the settings one settings scope, a nested dgdtools.yml that
// says `root: false`, gives the files under its directory.
type Scope struct {
	// Path is the file and Dir its directory, which the globs of
	// Overrides are relative to.
	Path, Dir string
	Format    Format
	Rules     map[string]RuleSettings
	Overrides []Override
}

// settingsCache is what a project has read of the per-directory files.
type settingsCache struct {
	mu      sync.Mutex
	scopes  map[string]*scopeList // by directory
	editors map[string][]*editorconfig.File
}

type scopeList struct {
	list []*Scope
	err  error
	// nested is the root config, below the project's own, of the project
	// the directory belongs to instead; "" when it is this project's.
	nested string
}

// forgetEditors drops the .editorconfig files read so far.
func (prj *Project) forgetEditors() {
	prj.mu.Lock()
	defer prj.mu.Unlock()
	prj.editors = nil
}

// NestedProjectError says a file is not the project's: a root config
// between the project's own and the file makes it another project's.
type NestedProjectError struct {
	Path string
	// Config is the root config of the project the file belongs to.
	Config string
}

func (e *NestedProjectError) Error() string {
	return fmt.Sprintf("%s belongs to the project of %s", e.Path, e.Config)
}

// Nested returns the root config of the project the file or directory at
// path belongs to when that is not this project but one below it: a
// dgdtools.yml that is not a settings scope, between the project's own
// and the path. Every tool treats such a directory as the other
// project's: a walk of this project passes over it. It is "" for the
// project's own files.
func (prj *Project) Nested(path string) string {
	dir := filepath.Clean(path)
	if !prj.isDir(dir) {
		dir = filepath.Dir(dir)
	}
	prj.mu.Lock()
	defer prj.mu.Unlock()
	return prj.scopesAt(dir).nested
}

func (prj *Project) isDir(path string) bool { return prj.fs != nil && prj.fs.IsDir(path) }

// Settings returns the settings of the file at path (absolute; the file
// need not exist). It fails when a configuration file that applies to
// the file is not valid, and with a *NestedProjectError when the file
// belongs to a project below this one.
func (prj *Project) Settings(path string) (*Settings, error) {
	dir := filepath.Dir(path)
	prj.mu.Lock()
	scopes := prj.scopesAt(dir)
	var editors []*editorconfig.File
	if scopes.err == nil && scopes.nested == "" {
		editors = prj.editorsAt(dir)
	}
	prj.mu.Unlock()
	switch {
	case scopes.err != nil:
		return nil, scopes.err
	case scopes.nested != "":
		return nil, &NestedProjectError{Path: path, Config: scopes.nested}
	}

	s := &Settings{Scopes: scopes.list}
	fill := func(f Format, from string) {
		if s.Format.IndentStyle == "" && f.IndentStyle != "" {
			s.Format.IndentStyle, s.From.IndentStyle = f.IndentStyle, from
		}
		if s.Format.IndentWidth == 0 && f.IndentWidth != 0 {
			s.Format.IndentWidth, s.From.IndentWidth = f.IndentWidth, from
		}
		if s.Format.LineEndings == "" && f.LineEndings != "" {
			s.Format.LineEndings, s.From.LineEndings = f.LineEndings, from
		}
	}
	fill(prj.format, FromFlag)
	for i := len(s.Scopes) - 1; i >= 0; i-- {
		fill(s.Scopes[i].Format, s.Scopes[i].Path)
	}
	fill(prj.Config.Format, prj.ConfigPath)
	if set := s.Format; len(editors) > 0 && (set.IndentStyle == "" || set.IndentWidth == 0 || set.LineEndings == "") {
		// The properties are asked for per key: each may come from a
		// different file. Values that mean nothing here are skipped, as
		// the format asks.
		props := editorconfig.Properties(editors, path)
		if p, ok := props["indent_style"]; ok {
			if v, err := ParseIndentStyle(p.Value); err == nil {
				fill(Format{IndentStyle: v}, p.Source)
			}
		}
		size := props["indent_size"]
		if size.Value == "tab" {
			size = props["tab_width"]
		}
		if n, err := strconv.Atoi(size.Value); err == nil && CheckIndentWidth(n) == nil {
			fill(Format{IndentWidth: n}, size.Source)
		}
		if p, ok := props["end_of_line"]; ok && p.Value != string(LineEndingsPreserve) {
			if v, err := ParseLineEndings(p.Value); err == nil {
				fill(Format{LineEndings: v}, p.Source)
			}
		}
	}
	fill(Format{DefaultIndentStyle, DefaultIndentWidth, DefaultLineEndings}, FromDefault)
	return s, nil
}

// scopesAt returns the settings scopes that apply to the files in dir:
// those in dir and the directories above it, as far as the project
// reaches. A root config on the way makes dir another project's. Called
// with prj.mu held.
func (prj *Project) scopesAt(dir string) *scopeList {
	if l, ok := prj.scopes[dir]; ok {
		return l
	}
	l := &scopeList{}
	if top, ok := prj.top(dir); ok {
		parent := filepath.Dir(dir)
		if dir != top && parent != dir {
			up := prj.scopesAt(parent)
			l.list, l.err, l.nested = up.list, up.err, up.nested
		}
		if p := fileIn(prj.fs, dir); p != "" && p != prj.ConfigPath && l.err == nil && l.nested == "" {
			switch c, err := loadScope(prj.fs, p); {
			case err != nil:
				l.err = err
			case c == nil:
				l.nested = p
			default:
				l.list = append(l.list[:len(l.list):len(l.list)], &Scope{
					Path: p, Dir: dir, Format: c.Format, Rules: c.Lint.Rules, Overrides: c.Lint.Overrides,
				})
			}
		}
	}
	if prj.scopes == nil {
		prj.scopes = map[string]*scopeList{}
	}
	prj.scopes[dir] = l
	return l
}

// loadScope loads the settings scope at path; nil when the file is a
// root config, which is not read further: it is another project's. A
// file that says nothing of the mudlib is a settings scope here, below
// the project's config, as one that says root: false is.
func loadScope(fs vfs.FS, path string) (*Config, error) {
	k, err := kind(fs, path)
	switch {
	case err != nil:
		return nil, err
	case k == kindProject:
		return nil, nil
	case k == kindScope:
		return Load(fs, path)
	}
	c := &Config{}
	if err := loadInto(fs, path, c); err != nil {
		return nil, err
	}
	c.Dir, c.Path, c.Scope = filepath.Dir(path), path, true
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// top returns the highest directory whose configuration file can apply
// to the files in dir; false when none can. Nested files are looked for
// from the file's directory up to the project: below the directory of
// the project's config, or, when the lib is somewhere else, down from
// the lib root. A project without a config of its own has no top but the
// file system's, and one whose config was named by the caller has no
// nested files.
func (prj *Project) top(dir string) (string, bool) {
	if prj.fs == nil || prj.explicit || prj.noFiles {
		return "", false
	}
	if prj.ConfigPath == "" || prj.UserConfig {
		return "", true
	}
	// The project's own directory holds its config, not a nested one,
	// and scopesAt passes over that file by name.
	for _, t := range []string{prj.Config.Dir, prj.Root, prj.CheckoutDir} {
		if _, below := fileset.Under(t, dir); t != "" && below {
			return t, true
		}
	}
	return "", false
}

// editorsAt returns the .editorconfig files that apply to the files in
// dir, nearest first. Called with prj.mu held.
func (prj *Project) editorsAt(dir string) []*editorconfig.File {
	if prj.fs == nil || prj.noFiles {
		return nil
	}
	if l, ok := prj.editors[dir]; ok {
		return l
	}
	var own *editorconfig.File
	if p := filepath.Join(dir, editorconfig.FileName); prj.fs.Exists(p) && !prj.fs.IsDir(p) {
		if data, err := prj.fs.ReadFile(p); err == nil {
			own = editorconfig.Parse(p, data)
		}
	}
	var l []*editorconfig.File
	if parent := filepath.Dir(dir); parent != dir && (own == nil || !own.Root) {
		l = prj.editorsAt(parent)
	}
	if own != nil {
		l = append([]*editorconfig.File{own}, l...)
	}
	if prj.editors == nil {
		prj.editors = map[string][]*editorconfig.File{}
	}
	prj.editors[dir] = l
	return l
}
