// Package profile is a mudlib profile: everything the tools need to know
// about a lib that a wizard's own checkout does not contain.
//
// A profile is to a mudlib what lib.d.ts is to a JavaScript runtime or
// typeshed is to Python's standard library: declarations without
// implementations. It holds
//
//   - a manifest: the dialect (driver, build flags, typechecking), the
//     driver's include dirs / force-included header / auto object, the
//     lib's conventions (registries, macro classes, per-wizard layout),
//     and which lib directories the profile captures completely;
//   - stub files: for each shared header its #includes and #defines, and
//     for each shared object its inherit statements and the signatures of
//     its functions and variables, each with the comment hover shows for
//     it. No function bodies, no other comments.
//
// Stubs are ordinary source in the profile's dialect, so the same lexer,
// parser and index that read real files read them; a real file on disk
// always wins over a stub.
//
// The file format is deterministic (same lib, same bytes) and compact: a
// gzip stream of a header line, the manifest as JSON, and the stubs in
// path order.
package profile

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// Format is the profile file format version. Format 2 (dgdtools 0.4)
// moved create into the conventions and dropped specifier_macros and
// format_registry from them.
const Format = 2

// FileName is the default name of a locally built profile.
const FileName = "dgdtools.profile"

const magic = "dgdtools-profile"

// Dialect describes the language a lib is written in. Only driver "dgd"
// is implemented; the field exists so a profile says what it is, and so
// other LPC drivers can be added without changing the format.
type Dialect struct {
	Language     string   `json:"language"`               // "lpc"
	Driver       string   `json:"driver"`                 // "dgd"
	Version      string   `json:"version,omitempty"`      // informational, e.g. "1.7"
	SlashSlash   bool     `json:"slash_slash"`            // // comments
	Closures     bool     `json:"closures"`               // function-pointer syntax
	Typechecking *int     `json:"typechecking,omitempty"` // driver typechecking level
	Predefined   []string `json:"predefined,omitempty"`   // macros the driver defines
	// Kfuns are the kernel functions the lib's driver has beyond the
	// stock driver's (its extension modules'), as prototypes.
	Kfuns []string `json:"kfuns,omitempty"`
}

// Conventions are the lib's own habits that the tools need to be told.
// The same keys are the `conventions:` section of dgdtools.yml. Nothing
// here has a default: a lib that does not say has none.
type Conventions struct {
	// Create is the function the lib's objects are initialized by: the
	// one the driver calls or, in a lib whose auto object receives that
	// call and passes it on under another name (the kernel library's
	// _F_create), the one the lib's objects define. Empty is "create".
	Create string `json:"create,omitempty" yaml:"create"`
	// Macros overrides the class the parser derives for a macro, by the
	// class's name (pp.ClassNames). A lib's `#define public`, for files
	// read without the header that defines it, is {public: specifier}.
	Macros map[string]string `json:"macros,omitempty" yaml:"macros"`
	// Defines are names treated as defined when choosing #ifdef branches.
	Defines []string `json:"defines,omitempty" yaml:"defines"`
	// CallRegistry lists the lib's functions that take the name of a
	// function to call later, with where their arguments say which.
	CallRegistry map[string]CallForms `json:"call_registry,omitempty" yaml:"call_registry"`
	// ObjectRegistry maps path-taking functions to the argument index.
	ObjectRegistry map[string]int `json:"object_registry,omitempty" yaml:"object_registry"`
	// AutosaveMarkers are function or macro names whose mention marks an
	// object as one that saves itself with save_object.
	AutosaveMarkers []string `json:"autosave_markers,omitempty" yaml:"autosave_markers"`
	// VirtualPaths are lib-path globs served by virtual-object daemons.
	VirtualPaths []string `json:"virtual_paths,omitempty" yaml:"virtual_paths"`
}

// CallForm is one way of calling a function that takes the name of a
// function to call later: which argument (0-based) is what. The driver's
// own two are call_other, {object: 0, function: 1, args: 2}, and
// call_out, {function: 0, args: 2}.
type CallForm struct {
	// Function is the argument that names the function, as a string.
	Function int `json:"function" yaml:"function"`
	// Object is the argument that says which object the function is
	// called in. With one, the call is a call_other: static and private
	// functions are out of reach. Without, the function is called in
	// this_object(), where static functions can be reached.
	Object *int `json:"object,omitempty" yaml:"object"`
	// Args is the first of the arguments that are passed on to the
	// function, the rest of the call's arguments being the function's.
	// Without it, how many arguments the function gets is not known.
	Args *int `json:"args,omitempty" yaml:"args"`
}

// CallForms are the forms a function can be called in. A call is read by
// the first form it fits: one whose function argument is a string
// constant that names a function. In YAML one form may be written without
// the list.
type CallForms []CallForm

func (f *CallForms) UnmarshalYAML(n *yaml.Node) error {
	nodes := []*yaml.Node{n}
	if n.Kind == yaml.SequenceNode {
		nodes = n.Content
		if len(nodes) == 0 {
			return fmt.Errorf("line %d: a call registry entry needs at least one form: {function: N}", n.Line)
		}
	}
	*f = nil
	for _, m := range nodes {
		if m.Kind != yaml.MappingNode {
			return fmt.Errorf("line %d: a call registry entry is {function: N} with optional object: and args:, or a list of those; N is a 0-based argument index", m.Line)
		}
		form, hasFunction := CallForm{}, false
		for i := 0; i+1 < len(m.Content); i += 2 {
			key, val := m.Content[i], m.Content[i+1]
			var idx int
			if err := val.Decode(&idx); err != nil || val.Tag != "!!int" || idx < 0 {
				return fmt.Errorf("line %d: %s: want a 0-based argument index", val.Line, key.Value)
			}
			switch key.Value {
			case "function":
				form.Function, hasFunction = idx, true
			case "object":
				form.Object = &idx
			case "args":
				form.Args = &idx
			default:
				return fmt.Errorf("line %d: unknown key %q in a call registry entry (want function, object, args)", key.Line, key.Value)
			}
		}
		if !hasFunction {
			return fmt.Errorf("line %d: a call registry entry needs function: the argument that names the function", m.Line)
		}
		*f = append(*f, form)
	}
	return nil
}

// Manifest is the non-source part of a profile.
type Manifest struct {
	Format  int     `json:"format"`
	Name    string  `json:"name"`
	Dialect Dialect `json:"dialect"`

	IncludeDirs         []string `json:"include_dirs,omitempty"`
	IncludeFile         string   `json:"include_file,omitempty"`
	AutoObjects         []string `json:"auto_objects,omitempty"`
	DriverObject        string   `json:"driver_object,omitempty"` // whose functions the driver calls by name
	RelativeObjectPaths string   `json:"relative_object_paths,omitempty"`

	// Covered are lib directory prefixes ("/std/") the profile captures
	// completely: a path under one that has no stub does not exist.
	Covered []string `json:"covered,omitempty"`
	// Holes are globs (lib-relative) of files that exist in the lib but
	// were left out of the profile on purpose (the build's omit and
	// exclude patterns). Nothing is known about a path matching one, even
	// under a covered directory.
	Holes []string `json:"holes,omitempty"`
	// Checkouts are the lib's per-wizard trees as path patterns
	// ("players/*"), used to recognize where a partial checkout sits.
	Checkouts []string `json:"checkouts,omitempty"`

	Conventions Conventions `json:"conventions"`
}

// Profile is a loaded profile.
type Profile struct {
	Manifest
	// Files maps a lib path with extension ("/std/room.c") to its stub.
	Files map[string]string
	// Path is the file the profile was loaded from, "" when built in
	// memory.
	Path string
}

// Marshal renders the profile. The output depends only on the profile's
// content.
func (p *Profile) Marshal() ([]byte, error) {
	var raw bytes.Buffer
	fmt.Fprintf(&raw, "%s %d\n", magic, Format)
	m := p.Manifest
	m.Format = Format
	js, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return nil, err
	}
	raw.Write(js)
	raw.WriteByte('\n')
	paths := make([]string, 0, len(p.Files))
	for f := range p.Files {
		paths = append(paths, f)
	}
	sort.Strings(paths)
	for _, f := range paths {
		fmt.Fprintf(&raw, "=== %s\n%s", f, p.Files[f])
		if !strings.HasSuffix(p.Files[f], "\n") {
			raw.WriteByte('\n')
		}
	}
	var out bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if _, err := zw.Write(raw.Bytes()); err != nil { // no name, no mtime: deterministic
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Parse reads a profile (gzip-compressed or plain).
func Parse(data []byte) (*Profile, error) {
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if data, err = io.ReadAll(zr); err != nil {
			return nil, err
		}
	}
	head, rest, _ := bytes.Cut(data, []byte("\n"))
	var format int
	if _, err := fmt.Sscanf(string(head), magic+" %d", &format); err != nil {
		return nil, fmt.Errorf("not a dgdtools profile")
	}
	switch {
	case format < Format:
		return nil, fmt.Errorf("profile format %d is from an older dgdtools (this one reads format %d): rebuild the profile with dgdtools profile build", format, Format)
	case format > Format:
		return nil, fmt.Errorf("profile format %d is from a newer dgdtools (this one reads format %d): update dgdtools", format, Format)
	}
	p := &Profile{Files: map[string]string{}}
	body := rest
	sep := []byte("\n=== ")
	if i := bytes.Index(rest, sep); i >= 0 {
		body, rest = rest[:i+1], rest[i+1:]
	} else {
		rest = nil
	}
	if err := json.Unmarshal(body, &p.Manifest); err != nil {
		return nil, fmt.Errorf("profile manifest: %w", err)
	}
	if d := p.Dialect.Driver; d != "dgd" {
		return nil, fmt.Errorf("profile %q is for driver %q; only the dgd dialect is implemented", p.Name, d)
	}
	for len(rest) > 0 {
		rest = bytes.TrimPrefix(rest, []byte("=== "))
		name, after, _ := bytes.Cut(rest, []byte("\n"))
		content := after
		if i := bytes.Index(after, sep); i >= 0 {
			content, rest = after[:i+1], after[i+1:]
		} else {
			rest = nil
		}
		p.Files[string(name)] = string(content)
	}
	return p, nil
}

// PathEnv names the environment variable that lists extra directories to
// search for profiles by name (separated like PATH). A front end reads it
// and passes its value to UserDirs.
const PathEnv = "DGDTOOLS_PROFILE_PATH"

// UserDirs returns the directories, besides the one of the dgdtools.yml
// that names it, in which `profile: NAME` is looked up as NAME.profile:
//
//  1. each entry of pathList, the value of $DGDTOOLS_PROFILE_PATH;
//  2. userConfigDir/dgdtools/profiles, userConfigDir being
//     os.UserConfigDir() (~/.config on Linux,
//     ~/Library/Application Support on macOS, %AppData% on Windows);
//  3. home/.config/dgdtools/profiles, where that is a different
//     directory.
//
// The caller supplies all three (an empty one is skipped): this package
// reads no environment. No profile is built into the tools: a profile
// describes one particular mud and is published by that mud.
func UserDirs(pathList, userConfigDir, home string) []string {
	var dirs []string
	add := func(d string) {
		if d != "" && !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	for _, d := range filepath.SplitList(pathList) {
		add(d)
	}
	if userConfigDir != "" {
		add(filepath.Join(userConfigDir, "dgdtools", "profiles"))
	}
	if home != "" {
		add(filepath.Join(home, ".config", "dgdtools", "profiles"))
	}
	return dirs
}

// Resolve turns a profile reference into a file path. A reference with a
// path separator or ending in ".profile" is a path (relative to
// configDir); anything else is a name, looked up as NAME.profile in
// configDir and then in dirs (see UserDirs). configDir is absolute.
func Resolve(fsys vfs.FS, ref, configDir string, dirs []string) (string, error) {
	if strings.ContainsAny(ref, `/\\`) || strings.HasSuffix(ref, ".profile") {
		if !filepath.IsAbs(ref) {
			ref = filepath.Join(configDir, ref)
		}
		if !fsys.Exists(ref) {
			return "", fmt.Errorf("profile file %s does not exist", ref)
		}
		return ref, nil
	}
	search := []string{configDir}
	for _, d := range dirs {
		if !slices.Contains(search, d) {
			search = append(search, d)
		}
	}
	for _, d := range search {
		if p := filepath.Join(d, ref+".profile"); fsys.Exists(p) && !fsys.IsDir(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("profile %q not found: no %s.profile in\n  %s\n(get the profile from your mud and put it in one of these, list its directory in $%s, or give its path)",
		ref, ref, strings.Join(search, "\n  "), PathEnv)
}

// Load resolves a profile reference (see Resolve) and reads the profile.
func Load(fsys vfs.FS, ref, configDir string, dirs []string) (*Profile, error) {
	file, err := Resolve(fsys, ref, configDir, dirs)
	if err != nil {
		return nil, err
	}
	data, err := fsys.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	p.Path = file
	return p, nil
}

// Scheme is the URI scheme of stub locations.
const Scheme = "dgdtools-profile"

// Location returns the location prefix of a profile's stubs. A stub is
// not a file on disk and is not named like one: the stub for lib file
// "/std/room.c" of profile "mylib" is at
// "dgdtools-profile://mylib/std/room.c". That is the path FS answers
// for, the path diagnostics name when a declaration comes from a stub,
// and the URI of the read-only document an editor shows for it.
func Location(name string) string { return Scheme + "://" + name }

// SplitLocation splits a stub location into the profile's name and the
// lib file; false when p is not a stub location.
func SplitLocation(p string) (name, libFile string, ok bool) {
	rest, ok := strings.CutPrefix(p, Scheme+"://")
	if !ok {
		return "", "", false
	}
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return rest, "/", true
	}
	return rest[:i], rest[i:], true
}

// FS adds a profile's stubs to a file system: paths under the profile's
// Location are answered from the profile, everything else by the base.
type FS struct {
	base   vfs.FS
	prefix string
	p      *Profile
	dirs   map[string]bool
}

// NewFS returns base extended with the profile's stubs.
func NewFS(base vfs.FS, p *Profile) *FS {
	f := &FS{base: base, prefix: Location(p.Name), p: p, dirs: map[string]bool{"/": true}}
	for file := range p.Files {
		for d := path.Dir(file); d != "/" && !f.dirs[d]; d = path.Dir(d) {
			f.dirs[d] = true
		}
	}
	return f
}

// Location returns the location prefix the stubs are answered under.
func (f *FS) Location() string { return f.prefix }

// stub returns the lib file a stub location names.
func (f *FS) stub(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, f.prefix)
	switch {
	case !ok:
		return "", false
	case rest == "":
		return "/", true
	case rest[0] == '/':
		return rest, true
	}
	return "", false
}

func (f *FS) ReadFile(p string) ([]byte, error) {
	lib, ok := f.stub(p)
	if !ok {
		return f.base.ReadFile(p)
	}
	if s, ok := f.p.Files[lib]; ok {
		return []byte(s), nil
	}
	return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
}

func (f *FS) Exists(p string) bool {
	lib, ok := f.stub(p)
	if !ok {
		return f.base.Exists(p)
	}
	_, isFile := f.p.Files[lib]
	return isFile || f.dirs[lib]
}

func (f *FS) IsDir(p string) bool {
	lib, ok := f.stub(p)
	if !ok {
		return f.base.IsDir(p)
	}
	return f.dirs[lib]
}

// WalkDir walks the base: stubs are consulted, never linted.
func (f *FS) WalkDir(root string, fn fs.WalkDirFunc) error { return f.base.WalkDir(root, fn) }
