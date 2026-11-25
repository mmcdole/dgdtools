package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/editorconfig"
	"github.com/mmcdole/dgdtools/pkg/fileset"
	"github.com/mmcdole/dgdtools/pkg/lexer"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/token"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// Project is a resolved project: a config (possibly the empty one),
// optionally a profile, and where the lib is.
//
// Three shapes exist:
//
//   - whole lib: Root is the lib on disk.
//   - partial checkout: the wizard has one subtree. Partial is set, Root
//     is empty, and CheckoutDir on disk is the lib directory CheckoutLib
//     (both empty when the checkout's place in the lib is not known). The
//     rest of the lib is known as far as the profile describes it.
//   - no lib: neither; only file-local analysis is possible.
//
// A Project is made by Discover or by a Workspace and is safe for
// concurrent use.
type Project struct {
	Config *Config
	// ConfigPath is the config file used, "" when none.
	ConfigPath string
	// UserConfig says ConfigPath is the user-level configuration, used
	// because no project config was found: it defines the project, but
	// its directory is not the project's.
	UserConfig bool
	// Root is the absolute path of the lib on disk, or "" when the whole
	// lib is not there.
	Root string
	// Profile is the mudlib profile in effect, or nil.
	Profile *profile.Profile
	// Partial, CheckoutDir and CheckoutLib describe a partial checkout.
	Partial                  bool
	CheckoutDir, CheckoutLib string
	// Base is the directory the project is anchored at: the config's
	// root, or the first path's directory. It is what a run with no path
	// arguments works on when there is no whole lib.
	Base string
	// inputs are the files the project was resolved from; see Inputs.
	inputs []string
	// Notes are remarks about how the project was resolved.
	Notes []diag.Note

	// What Settings reads the per-directory files with, and how.
	fs       vfs.FS
	format   Format // set by the caller: wins over every file
	explicit bool   // the config was named by the caller: nested files are not looked for
	noFiles  bool   // no configuration file of any kind is read
	settingsCache
}

// HasLib reports whether cross-file analysis is possible: the lib is on
// disk, or part of it is and a profile or a lib path places it.
func (prj *Project) HasLib() bool { return prj.Root != "" || prj.Partial }

// NoLib says why there is no mudlib to analyze across files, and how to
// give one; "" when there is (HasLib).
func (prj *Project) NoLib() string {
	switch {
	case prj.HasLib():
		return ""
	case prj.ConfigPath == "":
		return "no dgdtools.yml, and no --root (or --profile and --lib-path for a checkout)"
	}
	return fmt.Sprintf("%s does not say where the mudlib is (root:, profile: or lib_path:; `root: .` makes its directory the mudlib's /)", prj.ConfigPath)
}

// Inputs returns the files the project was resolved from: its config
// file, the profile, the driver's configuration. When one of them
// changes the project is another one, and is resolved again
// (Workspace.Invalidate); a long-lived caller watches them.
func (prj *Project) Inputs() []string { return slices.Clone(prj.inputs) }

// Tree returns where the project's lib is on disk.
func (prj *Project) Tree() fileset.Tree {
	return fileset.Tree{Root: prj.Root, CheckoutDir: prj.CheckoutDir, CheckoutLib: prj.CheckoutLib}
}

// DiscoverOptions are what the caller knows that the file system does
// not say: explicit overrides (CLI flags, editor settings) and the parts
// of the process environment discovery depends on. Discover itself reads
// neither the working directory nor the environment.
type DiscoverOptions struct {
	ConfigPath string // use this config file, and no nested one
	Root       string // use this lib root
	NoConfig   bool   // read no configuration file of any kind
	Profile    string // use this profile (name or file)
	// LibPath is the lib path of the checkout, for a project whose
	// config does not say: the lib path of WorkDir.
	LibPath string
	// Format are format settings that win over every configuration file
	// (command-line flags).
	Format Format
	// SettingsOnly is for a caller that needs a file's settings and what
	// the configuration file itself says, not the mudlib (the formatter):
	// a profile or a driver configuration that cannot be read is a note
	// of the project's, not an error, and the project goes without it.
	SettingsOnly bool

	// WorkDir is the absolute directory that relative paths (the paths
	// to discover from, ConfigPath, Root, a Profile file) are relative
	// to, and where discovery starts when no paths are given: a command's
	// working directory. Without it every path must be absolute.
	WorkDir string
	// ProfileDirs are the directories searched for a profile given by
	// name, after the directory of the config that names it; see
	// profile.UserDirs.
	ProfileDirs []string
	// UserConfig is the user-level configuration file (UserConfigPath):
	// the root config of a path that has no project config. It need not
	// exist.
	UserConfig string
	// Folders are directories somebody has spoken for without writing a
	// config file: an editor's workspace folders, with the profile and
	// the lib path their user set for them. They speak only for the
	// paths under them that have no project config; a config file
	// always wins. Such paths are a project of their folder's, anchored
	// there, instead of sharing the one of all paths without a config.
	Folders []Folder
}

// Folder is a directory and what is known of it without a config file.
type Folder struct {
	// Dir is the directory, an absolute path.
	Dir string
	// Profile and LibPath are to the paths under Dir what the options
	// of the same names are to every path: the profile to work against
	// and the lib path of Dir. Either may be empty.
	Profile, LibPath string
}

// inFolder returns the options for a path in dir that has no project
// config, and where its project is anchored: when dir lies in one of
// opt.Folders, the innermost such folder says both.
func (opt DiscoverOptions) inFolder(dir string) (DiscoverOptions, string, bool) {
	var in *Folder
	for i := range opt.Folders {
		f := &opt.Folders[i]
		if _, under := fileset.Under(f.Dir, dir); under && (in == nil || len(f.Dir) > len(in.Dir)) {
			in = f
		}
	}
	if in == nil {
		return opt, dir, false
	}
	// What was said of every path (a flag) is not unsaid by a folder.
	if opt.Profile == "" {
		opt.Profile = in.Profile
	}
	if opt.LibPath == "" {
		opt.LibPath = in.LibPath
	}
	opt.WorkDir = filepath.Clean(in.Dir)
	return opt, opt.WorkDir, true
}

// UserConfigPath returns where the user-level configuration is looked
// for, given os.UserConfigDir() and the home directory (either may be
// empty): the first of userConfigDir/dgdtools/dgdtools.yml and
// home/.config/dgdtools/dgdtools.yml that exists, or "".
func UserConfigPath(fs vfs.FS, userConfigDir, home string) string {
	var dirs []string
	if userConfigDir != "" {
		dirs = append(dirs, userConfigDir)
	}
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".config"))
	}
	for _, d := range dirs {
		if p := filepath.Join(d, "dgdtools", FileName); fs.Exists(p) && !fs.IsDir(p) {
			return p
		}
	}
	return ""
}

func (opt *DiscoverOptions) abs(p string) (string, error) {
	switch {
	case filepath.IsAbs(p):
		return filepath.Clean(p), nil
	case opt.WorkDir == "":
		return "", fmt.Errorf("%s: a relative path needs a working directory", p)
	}
	return filepath.Join(opt.WorkDir, p), nil
}

// Discover resolves the project for the given paths. The nearest root
// config at or above each path is used (WorkDir when there are no
// paths); settings scopes (`root: false`) are passed over. Paths that
// resolve to different root configs are an error, because a lint run has
// exactly one lib. Without a root config the user-level configuration is
// the project's, when there is one. A root is never guessed, and a
// profile is never assumed: without a config file or explicit options
// the project has neither.
func Discover(fs vfs.FS, paths []string, opt DiscoverOptions) (*Project, error) {
	var starts []string // the directories discovery starts from
	if len(paths) == 0 && opt.WorkDir != "" {
		paths = []string{opt.WorkDir}
	}
	for _, p := range paths {
		p, err := opt.abs(p)
		if err != nil {
			return nil, err
		}
		if !fs.IsDir(p) {
			p = filepath.Dir(p)
		}
		starts = append(starts, p)
	}
	base := "" // where a project without a config of its own is anchored
	if len(starts) > 0 {
		base = starts[0]
	}
	found := ""
	if opt.ConfigPath == "" && !opt.NoConfig {
		for _, dir := range starts {
			p, err := FindRoot(fs, dir)
			if err != nil {
				return nil, err
			}
			if p == "" {
				continue
			}
			if found != "" && found != p {
				return nil, fmt.Errorf("paths belong to different projects (%s and %s); run them separately or pass --config",
					found, p)
			}
			found = p
		}
	}
	if found == "" && base != "" {
		opt, base, _ = opt.inFolder(base)
	}
	return resolve(fs, opt, found, base)
}

// resolve builds the project of the root config at found ("" when the
// search found none), anchored at base when it has no config of its own.
func resolve(fs vfs.FS, opt DiscoverOptions, found, base string) (*Project, error) {
	prj := &Project{fs: fs, format: opt.Format, noFiles: opt.NoConfig}
	if n := opt.Format.IndentWidth; n != 0 {
		if err := CheckIndentWidth(n); err != nil {
			return nil, err
		}
	}
	switch {
	case opt.ConfigPath != "":
		p, err := opt.abs(opt.ConfigPath)
		if err != nil {
			return nil, err
		}
		prj.ConfigPath, prj.explicit = p, true
	case opt.NoConfig:
	case found != "":
		prj.ConfigPath = found
	case opt.UserConfig != "" && fs.Exists(opt.UserConfig):
		prj.ConfigPath, prj.UserConfig = opt.UserConfig, true
	}

	prj.Config = &Config{}
	if prj.ConfigPath != "" {
		c, err := Load(fs, prj.ConfigPath)
		if err != nil {
			return nil, err
		}
		prj.Config = c
		prj.inputs = []string{prj.ConfigPath}
		switch {
		case c.Scope:
			return nil, fmt.Errorf("%s is a settings scope (root: false), not a project's config", prj.ConfigPath)
		case !prj.UserConfig:
			prj.Root = c.AbsRoot()
			base = prj.Root
		case filepath.IsAbs(c.Root):
			prj.Root = filepath.Clean(c.Root)
			base = prj.Root
		case c.Root != "":
			// The user-level file has no directory that could be a lib.
			return nil, fmt.Errorf("%s: root: %s: the user-level configuration needs an absolute root, or none", prj.ConfigPath, c.Root)
		}
	}
	cfg := prj.Config
	prj.Base = base

	// Profile: from the flag, else from the config.
	ref, rel := opt.Profile, opt.WorkDir
	if ref == "" {
		ref, rel = cfg.Profile, cfg.Dir
	}
	// A caller that needs the settings only goes without what it cannot
	// read of the mudlib.
	lenient := func(err error) error {
		if opt.SettingsOnly {
			prj.Notes = append(prj.Notes, diag.Note{Kind: diag.NoteUnread, Message: err.Error()})
			return nil
		}
		return err
	}
	var prof *profile.Profile
	if ref != "" {
		p, err := profile.Load(fs, ref, rel, opt.ProfileDirs)
		if err != nil {
			if err := lenient(err); err != nil {
				return nil, err
			}
		}
		prof = p
	}
	if prof != nil {
		prj.Profile = prof
		prj.inputs = append(prj.inputs, prof.Path)
		applyProfile(cfg, prof)
		// What the profile says is checked like what the file says.
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", prof.Path, err)
		}
	} else if dc := cfg.DriverConfig; dc != "" && ref == "" {
		// A whole lib with its build inputs at hand needs no profile.
		if !filepath.IsAbs(dc) {
			dc = filepath.Join(cfg.Dir, dc)
		}
		prj.inputs = append(prj.inputs, dc)
		switch d, err := ReadDriverConfig(fs, dc); {
		case err != nil:
			if err := lenient(fmt.Errorf("driver_config: %w", err)); err != nil {
				return nil, err
			}
		default:
			d.Apply(cfg)
			if err := cfg.Validate(); err != nil {
				return nil, fmt.Errorf("%s: %w", dc, err)
			}
		}
	}

	switch {
	case opt.Root != "":
		root, err := opt.abs(opt.Root)
		if err != nil {
			return nil, err
		}
		prj.Root = root
	case opt.LibPath != "" && prj.Root == "" && opt.WorkDir != "":
		// With no config to say which directory the lib path is of, it
		// is the working directory's, whatever paths were asked about.
		prj.mount(opt.WorkDir, opt.LibPath)
	case opt.LibPath != "":
		prj.mount(base, opt.LibPath)
	case cfg.LibPath != "":
		prj.mount(base, cfg.LibPath)
	case prj.Profile != nil && !prj.wholeLib(fs):
		// A profile and something that is not the whole lib: a partial
		// checkout. Where it sits is read off its path when one of the
		// profile's checkout patterns shows there.
		if dir, lib, ok := InferCheckout(base, prj.Profile.Checkouts); ok {
			prj.mount(dir, lib)
		} else {
			prj.Partial = true
			prj.Root = ""
			prj.Notes = append(prj.Notes, diag.Note{Kind: diag.NoteUnplacedCheckout, Path: base, Message: unplaced(base, prj.Profile.Checkouts)})
		}
	}
	if prj.Profile != nil {
		what := fmt.Sprintf("profile %s (%d stubs) from %s", prj.Profile.Name, len(prj.Profile.Files), prj.Profile.Path)
		if prj.CheckoutLib != "" {
			what += fmt.Sprintf("; %s is %s", prj.CheckoutDir, prj.CheckoutLib)
		}
		prj.Notes = append(prj.Notes, diag.Note{Kind: diag.NoteProfile, Path: prj.Profile.Path, Message: what})
	}
	if prj.Root != "" && !prj.Partial && !fs.IsDir(prj.Root) {
		return nil, fmt.Errorf("mudlib root %s is not a directory", prj.Root)
	}
	return prj, nil
}

// unplaced says that a checkout's place in the lib is not known, and how
// to tell.
func unplaced(dir string, checkouts []string) string {
	why := ""
	if len(checkouts) > 0 {
		why = " (no " + strings.Join(checkouts, " or ") + " in its path)"
	}
	return fmt.Sprintf("cannot tell where %s sits in the mudlib%s: set `lib_path:` in dgdtools.yml to the mudlib path of that directory to check references between its own files", dir, why)
}

// Workspace finds the project of each path it is asked about, for tools
// that take files from anywhere: the formatter, an editor server. Each
// project is resolved once, and again after Invalidate was told that
// what it was resolved from changed. It is safe for concurrent use.
type Workspace struct {
	fs  vfs.FS
	opt DiscoverOptions

	mu       sync.Mutex
	roots    map[string]string // directory -> its root config, "" for none
	projects map[string]*found // root config -> project
}

type found struct {
	prj *Project
	err error
}

// NewWorkspace returns a workspace that resolves projects with opt. With
// opt.ConfigPath or opt.NoConfig every path belongs to the one project
// those describe.
func NewWorkspace(fs vfs.FS, opt DiscoverOptions) *Workspace {
	return &Workspace{fs: fs, opt: opt, roots: map[string]string{}, projects: map[string]*found{}}
}

// Project returns the project of the file or directory at path
// (absolute): that of the nearest root config at or above it, as
// Discover finds it. Paths without one share a project, the user-level
// configuration's when there is one; those under one of the options'
// Folders are that folder's project.
func (w *Workspace) Project(path string) (*Project, error) {
	dir := filepath.Clean(path)
	if !w.fs.IsDir(dir) {
		dir = filepath.Dir(dir)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	root := ""
	if w.opt.ConfigPath == "" && !w.opt.NoConfig {
		var err error
		if root, err = w.root(dir); err != nil {
			return nil, err
		}
	}
	key, opt := root, w.opt
	if root == "" {
		var in bool
		if opt, dir, in = opt.inFolder(dir); in {
			key = "\x00" + dir // no config file's path
		}
	}
	f := w.projects[key]
	if f == nil {
		f = &found{}
		f.prj, f.err = resolve(w.fs, opt, root, dir)
		w.projects[key] = f
	}
	return f.prj, f.err
}

// root is FindRoot, remembering the answer for every directory on the
// way up.
func (w *Workspace) root(dir string) (string, error) {
	if r, ok := w.roots[dir]; ok {
		return r, nil
	}
	r, k, err := rootIn(w.fs, dir)
	if err != nil {
		return "", err
	}
	if parent := filepath.Dir(dir); (r == "" || k == kindSettings) && parent != dir {
		up, err := w.root(parent)
		if err != nil {
			return "", err
		}
		if up != "" {
			r = up // a project above, or an outer file of settings
		}
	}
	w.roots[dir] = r
	return r, nil
}

// Invalidate tells the workspace that the file or directory at path
// (absolute) changed, was created or was deleted. When it is something
// projects are configured by, the workspace forgets what it resolved from
// it and reports true: settings may be different now, for any file. The
// projects that are not what they were are returned too; Project resolves
// them again when next asked, as new values.
//
// A dgdtools.yml drops the project it is the root config of and the
// projects it is, or may now be, a settings scope of; one of a project's
// other Inputs drops that project; an .editorconfig is read again by
// all.
func (w *Workspace) Invalidate(path string) (dropped []*Project, changed bool) {
	path = filepath.Clean(path)
	name, dir := filepath.Base(path), filepath.Dir(path)
	config := name == FileName || name == AltFileName
	source := filepath.Ext(name) == ".c" || filepath.Ext(name) == ".h"
	w.mu.Lock()
	defer w.mu.Unlock()
	for root, f := range w.projects {
		prj := f.prj
		if prj == nil {
			// One that could not be resolved is tried again after any
			// change that may be to what was wrong with it.
			if !source {
				delete(w.projects, root)
				changed = true
			}
			continue
		}
		_, scoped := prj.top(dir)
		drop := config && scoped
		for _, in := range prj.inputs {
			if _, under := fileset.Under(path, in); under {
				drop = true
			}
		}
		switch {
		case drop:
			delete(w.projects, root)
			dropped = append(dropped, prj)
		case name == editorconfig.FileName:
			prj.forgetEditors()
			changed = true
		}
	}
	if config || len(dropped) > 0 {
		clear(w.roots)
		changed = true
	}
	return dropped, changed
}

// mount makes the project a partial checkout: dir on disk is lib path lib.
func (prj *Project) mount(dir, lib string) {
	prj.Partial = true
	prj.CheckoutDir = dir
	prj.CheckoutLib = "/" + strings.Trim(filepath.ToSlash(lib), "/")
	prj.Root = ""
}

// wholeLib reports whether the project root holds the lib itself: the
// profile's force-included header (or first include dir) exists there.
func (prj *Project) wholeLib(fs vfs.FS) bool {
	if prj.Root == "" {
		return false
	}
	p := prj.Profile
	probe := p.IncludeFile
	for _, d := range p.IncludeDirs {
		if probe == "" && strings.HasPrefix(d, "/") { // not one relative to each file
			probe = d
		}
	}
	if probe == "" && len(p.AutoObjects) > 0 {
		probe = p.AutoObjects[0] + ".c"
	}
	return probe != "" && fs.Exists(filepath.Join(prj.Root, filepath.FromSlash(probe)))
}

// InferCheckout recognizes a per-wizard tree in dir's path: with pattern
// "usr/*", /home/me/mud/usr/ann/rooms is the checkout
// /home/me/mud/usr/ann at lib path /usr/ann. Patterns are
// tried in order; within one, the outermost match wins.
func InferCheckout(dir string, patterns []string) (checkoutDir, lib string, ok bool) {
	segs := strings.Split(filepath.ToSlash(dir), "/")
	for _, pat := range patterns {
		want := strings.Split(strings.Trim(pat, "/"), "/")
		for i := 0; i+len(want) <= len(segs); i++ {
			match := true
			for j, w := range want {
				if segs[i+j] == "" || (w != "*" && w != segs[i+j]) {
					match = false
					break
				}
			}
			if match {
				return filepath.FromSlash(strings.Join(segs[:i+len(want)], "/")),
					"/" + strings.Join(segs[i:i+len(want)], "/"), true
			}
		}
	}
	return "", "", false
}

// applyProfile fills the config from a profile's manifest. The driver
// settings are the profile's; of the rest, anything the config sets
// itself wins, and registries are merged.
func applyProfile(cfg *Config, p *profile.Profile) {
	cfg.IncludeDirs = p.IncludeDirs
	cfg.IncludeFile = p.IncludeFile
	cfg.AutoObjects = p.AutoObjects
	cfg.DriverObject = p.DriverObject
	if cfg.RelativeObjectPaths == "" {
		cfg.RelativeObjectPaths = p.RelativeObjectPaths
	}
	if cfg.Dialect.Typechecking == nil {
		cfg.Dialect.Typechecking = p.Dialect.Typechecking
	}
	if cfg.Dialect.SlashSlash == nil {
		v := p.Dialect.SlashSlash
		cfg.Dialect.SlashSlash = &v
	}
	cfg.Dialect.Closures = cfg.Dialect.Closures || p.Dialect.Closures
	// The profile's kfuns, then the config's: of two with a name the
	// later is the one (kfun.Table.With).
	cfg.Dialect.Kfuns = append(slices.Clone(p.Dialect.Kfuns), cfg.Dialect.Kfuns...)
	own, c := &cfg.Conventions, p.Conventions
	if own.Create == "" {
		own.Create = c.Create
	}
	if len(own.AutosaveMarkers) == 0 {
		own.AutosaveMarkers = c.AutosaveMarkers
	}
	if len(own.VirtualPaths) == 0 {
		own.VirtualPaths = c.VirtualPaths
	}
	own.Defines = append(append([]string{}, c.Defines...), own.Defines...)
	own.Defines = append(own.Defines, p.Dialect.Predefined...)
	merge(&own.CallRegistry, c.CallRegistry)
	merge(&own.ObjectRegistry, c.ObjectRegistry)
	merge(&own.Macros, c.Macros)
}

// merge adds the entries of src that dst does not have.
func merge[V any](dst *map[string]V, src map[string]V) {
	for k, v := range src {
		if *dst == nil {
			*dst = map[string]V{}
		}
		if _, set := (*dst)[k]; !set {
			(*dst)[k] = v
		}
	}
}

// DriverConfig holds the values the tools need from a DGD driver config.
// Its create is not among them: what the lib's objects define is
// conventions.create, which in a lib whose auto object receives the
// driver's call is another function.
type DriverConfig struct {
	IncludeFile  string
	IncludeDirs  []string
	AutoObject   string
	DriverObject string
	Typechecking *int
}

// ReadDriverConfig parses a DGD driver configuration file
// (`name = value;` entries; values are numbers, strings, or ({ ... })).
func ReadDriverConfig(fs vfs.FS, path string) (*DriverConfig, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f := lexer.Lex(path, data, lexer.Default)
	var sig []token.Token
	for _, t := range f.Tokens {
		if !t.Kind.IsTrivia() && t.Kind != token.EOF {
			sig = append(sig, t)
		}
	}
	str := func(t token.Token) string {
		s := string(f.Text(t))
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
		return strings.Trim(s, `"`)
	}
	dc := &DriverConfig{}
	for i := 0; i+2 < len(sig); i++ {
		if sig[i].Kind != token.Ident || sig[i+1].Kind != token.Assign {
			continue
		}
		name := string(f.Text(sig[i]))
		var vals []token.Token
		j := i + 2
		for ; j < len(sig) && sig[j].Kind != token.Semicolon; j++ {
			vals = append(vals, sig[j])
		}
		i = j
		if len(vals) == 0 {
			continue
		}
		switch name {
		case "include_file":
			dc.IncludeFile = str(vals[0])
		case "auto_object":
			dc.AutoObject = str(vals[0])
		case "driver_object":
			dc.DriverObject = str(vals[0])
		case "typechecking":
			if n, err := strconv.Atoi(string(f.Text(vals[0]))); err == nil {
				dc.Typechecking = &n
			}
		case "include_dirs":
			for _, v := range vals {
				if v.Kind == token.StringLit {
					dc.IncludeDirs = append(dc.IncludeDirs, str(v))
				}
			}
		}
	}
	return dc, nil
}

// Apply sets the config's driver settings to the driver config's. A
// dialect.typechecking the file sets wins.
func (dc *DriverConfig) Apply(cfg *Config) {
	cfg.IncludeDirs = dc.IncludeDirs
	cfg.IncludeFile = dc.IncludeFile
	cfg.AutoObjects = nil
	if dc.AutoObject != "" {
		cfg.AutoObjects = []string{dc.AutoObject}
	}
	cfg.DriverObject = dc.DriverObject
	if cfg.Dialect.Typechecking == nil {
		cfg.Dialect.Typechecking = dc.Typechecking
	}
}
