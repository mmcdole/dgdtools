// Package config loads dgdtools.yml, the one configuration file of the
// dgdtools, discovers the project a path belongs to, and resolves the
// settings each file is formatted and linted by. Every field is optional;
// the zero config gives working defaults. Everything mudlib-specific
// (include dirs, the auto object, call registries, conventions) lives
// here or in a profile, never in the tools.
//
// A file is one of two things (docs/design/config.md). A root config
// defines a project: its top-level keys say what the project is, and its
// lint: and format: sections are the project's settings. A settings scope
// (`root: false`) holds only lint: and format:, and adjusts them for the
// files under its directory.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mmcdole/dgdtools/internal/fuzzy"

	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/fileset"
	"github.com/mmcdole/dgdtools/pkg/kfun"
	"github.com/mmcdole/dgdtools/pkg/pp"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/token"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// FileName is the canonical config file name; AltFileName is also found.
const (
	FileName    = "dgdtools.yml"
	AltFileName = ".dgdtools.yml"
)

type Config struct {
	// Project keys: what the project is. A settings scope has none of
	// them.

	// Root is the filesystem directory that is the lib's "/" (default:
	// the config file's directory). Relative paths are resolved against
	// the config file's directory. In YAML the key also takes the value
	// false, which makes the file a settings scope; see Scope.
	Root string `yaml:"root"`
	// Profile names the mudlib profile to work against: a name, looked
	// up as NAME.profile next to this file, in $DGDTOOLS_PROFILE_PATH and
	// in the user config directory (profile.UserDirs), or a file path. A
	// profile supplies what the checkout does not contain: the driver's
	// settings, the shared headers, and the signatures of the shared
	// objects.
	Profile string `yaml:"profile"`
	// LibPath says where this checkout sits in the lib when it is not
	// the whole lib: the lib path of Root, e.g. "/users/name".
	LibPath string `yaml:"lib_path"`
	// Exclude are globs (lib-relative) of files directory walks skip.
	Exclude []string `yaml:"exclude"`
	// DriverConfig is the DGD driver's config file, relative to the
	// config file's directory: where the driver settings (include_dirs,
	// include_file, auto_object, driver_object, typechecking) are read
	// from, for a whole-lib lint and for a profile build. A partial
	// checkout gets them from its profile instead.
	DriverConfig string `yaml:"driver_config"`
	// Driver states driver settings in the file itself, under the driver
	// config's names. Each one it sets wins over what the driver config
	// or the profile says; see Config.applyDriver.
	Driver DriverConfig `yaml:"driver"`

	Dialect DialectCfg `yaml:"dialect"`
	// RelativeObjectPaths says what an object path without a leading
	// slash is relative to. The driver leaves that to the lib (its driver
	// object resolves every inherit and every call_other by path):
	// ObjectPathsFromRoot, the default and what the driver's own
	// functions do, or ObjectPathsFromFile, the directory of the file
	// that says it.
	RelativeObjectPaths string `yaml:"relative_object_paths"`
	// Conventions are the lib's own habits: macro classes, registries.
	Conventions profile.Conventions `yaml:"conventions"`
	// ProfileBuild are the inputs of `dgdtools profile build`, for
	// whoever has the whole lib. They also serve a whole-lib lint
	// directly.
	ProfileBuild ProfileBuild `yaml:"profile_build"`

	// Settings: how files are formatted and linted. A settings scope may
	// set them for its directory; see Project.Settings.

	Format Format `yaml:"format"`
	Lint   Lint   `yaml:"lint"`

	// Driver settings in effect: what the driver: block, the driver
	// config (DriverConfig) or the profile says.

	// IncludeDirs are lib-absolute search paths for #include <...> and
	// "..." resolution (the driver config's include_dirs).
	IncludeDirs []string `yaml:"-"`
	// IncludeFile is force-included into every compile by the driver.
	IncludeFile string `yaml:"-"`
	// AutoObjects are lib paths implicitly inherited by every object.
	AutoObjects []string `yaml:"-"`
	// DriverObject is the lib path of the driver object (the driver
	// config's driver_object), whose functions the driver calls by name.
	DriverObject string `yaml:"-"`

	// Scope is set by `root: false`: the file is a settings scope, not a
	// project's root config.
	Scope bool `yaml:"-"`
	// Dir is the directory the config was loaded from, and Path the config
	// file; both are empty for a project without one.
	Dir  string `yaml:"-"`
	Path string `yaml:"-"`
}

// DefaultCreate is the function a new object is initialized by when
// conventions.create does not say.
const DefaultCreate = "create"

// Create returns the function the lib's objects are initialized by:
// conventions.create, or DefaultCreate.
func (c *Config) Create() string {
	if c.Conventions.Create != "" {
		return c.Conventions.Create
	}
	return DefaultCreate
}

// ProfileBuild describes how to turn a whole lib into a profile.
type ProfileBuild struct {
	// Name of the profile (default: the lib root's directory name).
	Name string `yaml:"name"`
	// ShareMinOwners selects shared code by use: a file referenced (by
	// inherit, #include, or as a call/clone/find target) from at least
	// this many distinct owners goes into the profile. An owner is one
	// per-wizard tree (see Checkouts) or one other top-level directory.
	// nil is DefaultShareMinOwners; 0 turns inference off, so that only
	// what Share lists is shared.
	ShareMinOwners *int `yaml:"share_min_owners"`
	// Share are globs (lib-relative) added to what inference finds; a
	// glob taking a whole directory ("std/**") also marks it complete,
	// so that a path missing under it is known not to exist. The include
	// dirs, the force-included header, the auto object, and everything
	// shared files inherit or include are always added.
	Share []string `yaml:"share"`
	// Omit are globs never to put in a profile, even when referenced.
	Omit []string `yaml:"omit"`
	// Checkouts are the per-wizard trees as path patterns ("players/*").
	Checkouts []string `yaml:"checkouts"`
}

// DefaultShareMinOwners is the inference threshold when none is set.
const DefaultShareMinOwners = 5

// MinOwners returns the inference threshold: share_min_owners, or
// DefaultShareMinOwners when it is not set; 0 is no inference.
func (pb *ProfileBuild) MinOwners() int {
	if pb.ShareMinOwners == nil {
		return DefaultShareMinOwners
	}
	return *pb.ShareMinOwners
}

type DialectCfg struct {
	SlashSlash *bool `yaml:"slash_slash"` // default true
	Closures   bool  `yaml:"closures"`    // default false
	// Typechecking is the driver's typechecking level (0, 1, 2), from
	// driver.typechecking, driver_config or the profile; it is not a key
	// of dialect:.
	// Rules that only hold under typechecking stay silent when it is
	// unknown.
	Typechecking *int `yaml:"-"`
	// Kfuns are the kernel functions this driver has beyond stock DGD's,
	// those of the extension modules it loads, each as a prototype
	// ("int *rusage()"; kfun.Parse says how one is written). A kfun of
	// the driver's own that is listed is taken as written here.
	Kfuns []string `yaml:"kfuns"`
}

// Lint are the linter's settings.
type Lint struct {
	// Rules maps a rule name, code, or group to its settings.
	Rules map[string]RuleSettings `yaml:"rules"`
	// Overrides adjust rules for path globs: lib-relative in a root
	// config, relative to its directory in a settings scope.
	Overrides []Override `yaml:"overrides"`

	// Baseline is the baseline file (default lint.BaselineFileName next
	// to the config). It describes a run, not a file: root config only.
	Baseline string `yaml:"baseline"`
}

// RuleSettings is what a configuration says about one rule, or about a
// group of rules. In YAML it is a severity, or a mapping that may give a
// severity and the options of the rule.
type RuleSettings struct {
	// Severity is off, info, warning or error; a severity turns the rule
	// on. Empty: the entry says nothing about it.
	Severity string
	// Options are the rule's own settings, by key. Which keys a rule has,
	// and what values they take, is the rule's to say: the linter checks
	// them (lint.Rule.Options).
	Options map[string]any
	// Line is the line of the entry in its file, 0 when it is not from
	// one.
	Line int
}

func (r *RuleSettings) UnmarshalYAML(n *yaml.Node) error {
	*r = RuleSettings{Line: n.Line}
	switch n.Kind {
	case yaml.ScalarNode:
		r.Severity = n.Value
		return nil
	case yaml.MappingNode:
	default:
		return fmt.Errorf("line %d: a rule is set to a severity (off, info, warning, error) or to a mapping", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		switch key.Value {
		case "severity":
			if val.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: severity: want off, info, warning or error", val.Line)
			}
			r.Severity = val.Value
		default:
			var v any
			if err := val.Decode(&v); err != nil {
				return at(val, err)
			}
			if r.Options == nil {
				r.Options = map[string]any{}
			}
			r.Options[key.Value] = v
		}
	}
	return nil
}

// Override is rule settings for the files that match some globs.
type Override struct {
	Paths []string                `yaml:"paths"`
	Rules map[string]RuleSettings `yaml:"rules"`
}

// Load reads and validates a config file.
func Load(fs vfs.FS, path string) (*Config, error) {
	c := &Config{}
	if err := loadInto(fs, path, c); err != nil {
		return nil, err
	}
	c.Dir = filepath.Dir(path)
	c.Path = path
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// header is what is read of a config file before the rest: whether it
// is a settings scope.
type header struct {
	Root yaml.Node `yaml:"root"`
}

// The kinds of configuration file, by what the file says.
const (
	// kindProject: the file has a project key (projectKey). It is a project's root config wherever it is.
	kindProject = iota
	// kindSettings: the file has settings only. Below a project's config
	// it is a settings scope of that project; with no project above, it
	// is the config of a project without a lib.
	kindSettings
	// kindScope: the file says root: false, a settings scope.
	kindScope
)

// kind classifies the configuration file at path.
func kind(fs vfs.FS, path string) (int, error) {
	head, _, err := readHeader(fs, path)
	if err != nil {
		return 0, err
	}
	if scope, err := head.scope(); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	} else if scope {
		return kindScope, nil
	}
	c := &Config{}
	if err := loadInto(fs, path, c); err != nil {
		return 0, err
	}
	if c.projectKey(false) != "" {
		return kindProject, nil
	}
	return kindSettings, nil
}

// scope reports whether the file says `root: false`. A boolean is told
// from a directory named like one by how it is written: `root: "false"`
// is a path.
func (h *header) scope() (bool, error) {
	if h.Root.Kind != yaml.ScalarNode || h.Root.Tag != "!!bool" {
		return false, nil
	}
	var v bool
	if err := h.Root.Decode(&v); err != nil {
		return false, err
	}
	if v {
		return false, fmt.Errorf("line %d: root: true is not a setting: a dgdtools.yml is a root config unless it says root: false; give the mudlib root's directory, or leave root out", h.Root.Line)
	}
	return true, nil
}

func readHeader(fs vfs.FS, path string) (*header, []byte, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	h := &header{}
	if err := yaml.Unmarshal(data, h); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, yamlError(err))
	}
	return h, data, nil
}

func loadInto(fs vfs.FS, path string, c *Config) error {
	head, data, err := readHeader(fs, path)
	if err != nil {
		return err
	}
	// A key an older dgdtools had says what took its place. Any other
	// unknown key is an error too: a misspelled setting must not be
	// silently ignored (ruff, Biome and golangci-lint all reject them).
	if err := checkRemoved(data); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && err != io.EOF {
		return fmt.Errorf("%s: %w", path, yamlError(err))
	}
	if c.Scope, err = head.scope(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if c.Scope {
		c.Root = "" // root: false is no directory
	}
	return nil
}

// Validate reports what is wrong with the configuration, as far as the
// configuration itself can tell: values that are not among the ones a key
// takes, and keys that do not belong in a settings scope. Load calls it;
// so does whoever is handed a Config that was not loaded. Rule names and
// rule options are the linter's to check (lint.New).
func (c *Config) Validate() error {
	if n := c.Format.IndentWidth; n != 0 {
		if err := CheckIndentWidth(n); err != nil {
			return fmt.Errorf("format.indent_width: %w", err)
		}
	}
	if v := c.Format.IndentStyle; v != "" {
		if _, err := ParseIndentStyle(string(v)); err != nil {
			return fmt.Errorf("format.indent_style: %w", err)
		}
	}
	if v := c.Format.LineEndings; v != "" {
		if _, err := ParseLineEndings(string(v)); err != nil {
			return fmt.Errorf("format.line_endings: %w", err)
		}
	}
	if err := checkRules("lint.rules", c.Lint.Rules); err != nil {
		return err
	}
	for i, o := range c.Lint.Overrides {
		where := fmt.Sprintf("lint.overrides[%d]", i)
		if len(o.Paths) == 0 {
			return fmt.Errorf("%s: no paths: an override says which files it is for", where)
		}
		for _, p := range o.Paths {
			if err := fileset.CheckGlob(p); err != nil {
				return fmt.Errorf("%s.paths: %w", where, err)
			}
		}
		if err := checkRules(where+".rules", o.Rules); err != nil {
			return err
		}
	}
	if c.Scope {
		return c.validateScope()
	}

	for _, p := range c.Exclude {
		if err := fileset.CheckGlob(p); err != nil {
			return fmt.Errorf("exclude: %w", err)
		}
	}
	for _, key := range []struct {
		name  string
		globs []string
	}{{"profile_build.share", c.ProfileBuild.Share}, {"profile_build.omit", c.ProfileBuild.Omit}, {"conventions.virtual_paths", c.Conventions.VirtualPaths}} {
		for _, p := range key.globs {
			if err := fileset.CheckGlob(p); err != nil {
				return fmt.Errorf("%s: %w", key.name, err)
			}
		}
	}
	switch c.RelativeObjectPaths {
	case "", ObjectPathsFromRoot, ObjectPathsFromFile:
	default:
		return fmt.Errorf("relative_object_paths: unknown value %q (want %s or %s)", c.RelativeObjectPaths, ObjectPathsFromRoot, ObjectPathsFromFile)
	}
	if tc := c.Dialect.Typechecking; tc != nil && (*tc < 0 || *tc > 2) {
		return fmt.Errorf("dialect.typechecking: %d is not a typechecking level (0, 1 or 2)", *tc)
	}
	if _, err := c.Kfuns(); err != nil {
		return fmt.Errorf("dialect.kfuns: %w", err)
	}
	if n := c.ProfileBuild.ShareMinOwners; n != nil && *n < 0 {
		return fmt.Errorf("profile_build.share_min_owners: %d: want a number of owners, or 0 to share only what share: lists", *n)
	}
	conv := c.Conventions
	for _, name := range slices.Sorted(maps.Keys(conv.Macros)) {
		if _, ok := pp.ParseClass(conv.Macros[name]); !ok {
			return fmt.Errorf("conventions.macros: %s: unknown class %q (want %s)", name, conv.Macros[name], strings.Join(pp.ClassNames, ", "))
		}
	}
	for _, reg := range []struct {
		key string
		m   map[string]int
	}{{"object_registry", conv.ObjectRegistry}} {
		for _, name := range slices.Sorted(maps.Keys(reg.m)) {
			if reg.m[name] < 0 {
				return fmt.Errorf("conventions.%s: %s: %d is not an argument index (0 is the first argument)", reg.key, name, reg.m[name])
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(conv.CallRegistry)) {
		forms := conv.CallRegistry[name]
		if len(forms) == 0 {
			return fmt.Errorf("conventions.call_registry: %s: no form", name)
		}
		for _, f := range forms {
			if f.Function < 0 || (f.Object != nil && *f.Object < 0) || (f.Args != nil && *f.Args < 0) {
				return fmt.Errorf("conventions.call_registry: %s: an argument index is 0 or more", name)
			}
		}
	}
	return nil
}

// The values of relative_object_paths.
const (
	ObjectPathsFromRoot = "root"
	ObjectPathsFromFile = "file"
)

// checkRules checks the severities of rule settings.
func checkRules(where string, rules map[string]RuleSettings) error {
	for _, name := range slices.Sorted(maps.Keys(rules)) {
		rs := rules[name]
		line := ""
		if rs.Line > 0 {
			line = fmt.Sprintf("line %d: ", rs.Line)
		}
		if _, ok := diag.ParseSeverity(rs.Severity); !ok && rs.Severity != "" && rs.Severity != SeverityOff {
			return fmt.Errorf("%s%s: %s: unknown severity %q (want off, info, warning or error)", line, where, name, rs.Severity)
		}
	}
	return nil
}

// SeverityOff is the rule severity that turns a rule off.
const SeverityOff = "off"

// validateScope reports a key that a settings scope may not have. A scope
// holds settings; everything else describes the project, of which there
// is one, or a run.
func (c *Config) validateScope() error {
	if k := c.projectKey(true); k != "" {
		return fmt.Errorf("%s belongs in the project's root config; this file is a settings scope (root: false, or nothing but settings below a project's config) and holds only lint.rules, lint.overrides and format", k)
	}
	return nil
}

// projectKey returns the name of the first key set that says what the
// project is, or "" when the file has settings only. With run, the
// setting only a root config may have (lint.baseline) counts too.
func (c *Config) projectKey(run bool) string {
	for _, k := range []struct {
		name string
		set  bool
	}{
		{"root", c.Root != ""},
		{"profile", c.Profile != ""},
		{"lib_path", c.LibPath != ""},
		{"exclude", len(c.Exclude) > 0},
		{"driver_config", c.DriverConfig != ""},
		{"driver", !reflect.DeepEqual(c.Driver, DriverConfig{})},
		{"dialect", !reflect.DeepEqual(c.Dialect, DialectCfg{})},
		{"relative_object_paths", c.RelativeObjectPaths != ""},
		{"conventions", !reflect.DeepEqual(c.Conventions, profile.Conventions{})},
		{"profile_build", !reflect.DeepEqual(c.ProfileBuild, ProfileBuild{})},
		{"lint.baseline", run && c.Lint.Baseline != ""},
	} {
		if k.set {
			return k.name
		}
	}
	return ""
}

// FindRoot returns the root config of the project dir (an absolute path)
// belongs to, or "": the nearest configuration file at or above it that
// places the mudlib (root:, profile: or lib_path:). Settings scopes on the
// way are passed over: they belong to whatever project is above them. A
// file that says nothing of the mudlib is a settings scope of a project
// above it; when there is none, the outermost such file is the config of
// a project without a lib.
func FindRoot(fs vfs.FS, dir string) (string, error) {
	settings := ""
	for {
		p, k, err := rootIn(fs, dir)
		switch {
		case err != nil:
			return "", err
		case p != "" && k == kindProject:
			return p, nil
		case p != "":
			settings = p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return settings, nil
		}
		dir = parent
	}
}

// fileIn returns the config file in dir, or "".
func fileIn(fs vfs.FS, dir string) string {
	for _, name := range []string{FileName, AltFileName} {
		if p := filepath.Join(dir, name); fs.Exists(p) && !fs.IsDir(p) {
			return p
		}
	}
	return ""
}

// rootIn returns the config file in dir that may be a project's root
// config, and its kind (kindProject or kindSettings): "" when there is
// none or it says root: false.
func rootIn(fs vfs.FS, dir string) (string, int, error) {
	p := fileIn(fs, dir)
	if p == "" {
		return "", 0, nil
	}
	k, err := kind(fs, p)
	if err != nil || k == kindScope {
		return "", 0, err
	}
	return p, k, nil
}

// AbsRoot returns the lib root: Root as given when it is absolute, else
// relative to the config file's directory. Without root: it is the config
// file's directory when the file places the mudlib by profile: or
// lib_path:, and "" when the file says nothing of the mudlib: a root is
// never guessed.
func (c *Config) AbsRoot() string {
	switch {
	case filepath.IsAbs(c.Root):
		return filepath.Clean(c.Root)
	case c.Root == "" && c.Profile == "" && c.LibPath == "":
		return ""
	}
	return filepath.Join(c.Dir, c.Root)
}

// TokenDialect converts to the lexer dialect.
func (c *Config) TokenDialect() token.Dialect {
	d := token.Dialect{SlashSlash: true, Closures: c.Dialect.Closures}
	if c.Dialect.SlashSlash != nil {
		d.SlashSlash = *c.Dialect.SlashSlash
	}
	return d
}

// Kfuns returns the kernel functions of the project's driver: those of
// stock DGD and the ones dialect.kfuns declares.
func (c *Config) Kfuns() (*kfun.Table, error) {
	return kfun.DGD().With(c.Dialect.Kfuns)
}

// Predefined returns the names that are defined before the first line of
// every file: the preprocessor's own and conventions.defines.
func (c *Config) Predefined() map[string]bool {
	m := map[string]bool{}
	for _, d := range pp.Predefined {
		m[d] = true
	}
	for _, d := range c.Conventions.Defines {
		m[d] = true
	}
	return m
}

// MacroClasses returns the macro classes conventions.macros gives.
func (c *Config) MacroClasses() map[string]pp.Class {
	m := map[string]pp.Class{}
	for name, cls := range c.Conventions.Macros {
		m[name], _ = pp.ParseClass(cls) // Validate has seen to the spelling
	}
	return m
}

// yamlError says what is wrong with a configuration file in its own words,
// not in the decoder's: a key that is not one ("field fromat not found in
// type config.Config") with the key it was likely meant for, a value of
// the wrong kind ("cannot unmarshal !!seq into string").
func yamlError(err error) error {
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return errors.New(strings.TrimPrefix(err.Error(), "yaml: "))
	}
	msgs := make([]string, len(te.Errors))
	for i, m := range te.Errors {
		msgs[i] = yamlMessage(m)
	}
	return errors.New(strings.Join(msgs, "; "))
}

var (
	unknownField = regexp.MustCompile(`^(line \d+: )?field (\S+) not found in type (\S+)$`)
	wrongKind    = regexp.MustCompile("^(line \\d+: )?cannot unmarshal (!!\\w+)(?: `([^`]*)`)? into (\\S+)$")
)

// yamlMessage rewords one of the decoder's messages.
func yamlMessage(m string) string {
	if p := unknownField.FindStringSubmatch(m); p != nil {
		msg := fmt.Sprintf("%sunknown key %q", p[1], p[2])
		if near := fuzzy.Nearest(p[2], yamlKeys[p[3]]); near != "" {
			msg += fmt.Sprintf(" (did you mean %q?)", near)
		}
		return msg
	}
	if p := wrongKind.FindStringSubmatch(m); p != nil {
		got := map[string]string{"!!seq": "a list", "!!map": "a mapping", "!!str": "a string", "!!int": "a number",
			"!!float": "a number", "!!bool": "true or false", "!!null": "nothing"}[p[2]]
		if got == "" {
			got = p[2]
		}
		if p[3] != "" {
			got += " (" + p[3] + ")"
		}
		want := "a mapping"
		switch t := p[4]; {
		case t == "string":
			want = "a string"
		case strings.HasPrefix(t, "int") || strings.HasPrefix(t, "uint") || t == "float64":
			want = "a number"
		case t == "bool":
			want = "true or false"
		case strings.HasPrefix(t, "[]"):
			want = "a list"
		}
		return fmt.Sprintf("%swant %s here, not %s", p[1], want, got)
	}
	return m
}

// yamlKeys are the keys of each type a configuration file is decoded
// into, by the type's name as the decoder gives it.
var yamlKeys = func() map[string][]string {
	m := map[string][]string{}
	var add func(t reflect.Type)
	add = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || m[t.String()] != nil {
			return
		}
		m[t.String()] = []string{}
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name == "" || name == "-" {
				continue
			}
			m[t.String()] = append(m[t.String()], name)
			add(f.Type)
		}
	}
	add(reflect.TypeOf(Config{}))
	return m
}()
