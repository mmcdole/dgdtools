// Package config loads dgdtools.yml, the shared configuration for dgdfmt
// and dgdlint. Every field is optional; the zero config gives working
// defaults. Everything mudlib-specific — include dirs, the auto object,
// call registries, lint conventions — lives here, never in the tools.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/mmcdole/dgdtools/pkg/token"
)

const FileName = "dgdtools.yml"

type Config struct {
	Dialect DialectCfg `yaml:"dialect"`
	// Root is the filesystem directory that is the lib's "/". Relative
	// paths are resolved against the config file's directory.
	Root    string   `yaml:"root"`
	Exclude []string `yaml:"exclude"`
	Format  Format   `yaml:"format"`
	Lint    Lint     `yaml:"lint"`

	// Dir is the directory the config was loaded from ("." for defaults).
	Dir string `yaml:"-"`
}

type DialectCfg struct {
	SlashSlash *bool `yaml:"slash_slash"` // default true
	Closures   bool  `yaml:"closures"`    // default false
}

type Format struct {
	Indent        int    `yaml:"indent"`
	LineEndings   string `yaml:"line_endings"` // preserve | lf | crlf
	MaxBlankLines int    `yaml:"max_blank_lines"`
}

type Lint struct {
	// IncludeDirs are lib-absolute search paths for #include <...> and
	// "..." resolution (mirror the driver config's include_dirs).
	IncludeDirs []string `yaml:"include_dirs"`
	// IncludeFile is force-included into every compile by the driver
	// (its #defines are visible everywhere).
	IncludeFile string `yaml:"include_file"`
	// AutoObjects are lib paths implicitly inherited by every object
	// (the driver's auto_object); their functions exist on all chains.
	AutoObjects []string `yaml:"auto_objects"`
	// SpecifierMacros are empty-macro visibility markers ("public").
	SpecifierMacros []string `yaml:"specifier_macros"`

	Enable  []string `yaml:"enable"`
	Disable []string `yaml:"disable"`

	// CallRegistry maps function names to the argument index (0-based)
	// holding a callback function name targeting this_object(). Built-in,
	// always present: call_other (cross-object, arg 1), call_out (arg 0),
	// and the obj->name() call form.
	CallRegistry map[string]int `yaml:"call_registry"`
	// AutosaveMarkers are function or macro names whose presence marks an
	// object as persisting via save_object/restore_object.
	AutosaveMarkers []string `yaml:"autosave_markers"`

	// ObjectRegistry maps path-taking functions to the argument index
	// (0-based) holding an object path. Built-in: clone_object,
	// compile_object, find_object (all arg 0).
	ObjectRegistry map[string]int `yaml:"object_registry"`
	// VirtualPaths are lib-path globs served by virtual-object daemons —
	// objects that exist without a backing .c file.
	VirtualPaths []string `yaml:"virtual_paths"`

	Rules     map[string]RuleSettings `yaml:"rules"`
	PathRules []PathRule              `yaml:"path_rules"`
	FailOn    string                  `yaml:"fail_on"` // info | warning | error
}

type RuleSettings struct {
	Severity string   `yaml:"severity"`
	Deny     []string `yaml:"deny"`  // raw-inherit-path
	Names    []string `yaml:"names"` // lifecycle-chain
}

type PathRule struct {
	Paths   []string `yaml:"paths"`
	Disable []string `yaml:"disable"`
}

// Default returns the zero configuration with Dir set.
func Default() *Config {
	return &Config{Dir: ".", Root: "."}
}

// Load reads a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Dir = filepath.Dir(path)
	if c.Root == "" {
		c.Root = "."
	}
	return c, nil
}

// Find searches dir and its parents for dgdtools.yml; returns Default()
// when none exists.
func Find(dir string) (*Config, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for {
		p := filepath.Join(dir, FileName)
		if _, err := os.Stat(p); err == nil {
			return Load(p)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return Default(), nil
		}
		dir = parent
	}
}

// AbsRoot returns the lib root as an absolute filesystem path.
func (c *Config) AbsRoot() string {
	if filepath.IsAbs(c.Root) {
		return filepath.Clean(c.Root)
	}
	p, err := filepath.Abs(filepath.Join(c.Dir, c.Root))
	if err != nil {
		return filepath.Clean(filepath.Join(c.Dir, c.Root))
	}
	return p
}

// TokenDialect converts to the lexer dialect.
func (c *Config) TokenDialect() token.Dialect {
	d := token.Dialect{SlashSlash: true, Closures: c.Dialect.Closures}
	if c.Dialect.SlashSlash != nil {
		d.SlashSlash = *c.Dialect.SlashSlash
	}
	return d
}

// SpecifierMacroSet returns the specifier-macro set (default: public).
func (c *Config) SpecifierMacroSet() map[string]bool {
	if len(c.Lint.SpecifierMacros) == 0 {
		return map[string]bool{"public": true}
	}
	m := map[string]bool{}
	for _, s := range c.Lint.SpecifierMacros {
		m[s] = true
	}
	return m
}

// RuleSettingsFor returns the per-rule settings (zero value if unset).
func (c *Config) RuleSettingsFor(rule string) RuleSettings {
	if c.Lint.Rules == nil {
		return RuleSettings{}
	}
	return c.Lint.Rules[rule]
}
