package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// scoped is a lib with a root config above it, two settings scopes, one
// nested in the other, and a wizard's own root config.
func scoped() vfs.Mem {
	return vfs.Mem{
		"/mud/dgdtools.yml": "root: lib\nexclude: [\"attic/**\"]\nformat: {indent_width: 8, line_endings: lf}\n" +
			"lint:\n  rules: {missing-visibility: warning}\n  overrides:\n    - paths: [\"players/**\"]\n      rules: {unreachable-code: info}\n",
		"/mud/lib/std/room.c":           "",
		"/mud/lib/players/dgdtools.yml": "root: false\nformat: {indent_width: 2}\nlint:\n  rules: {missing-visibility: off}\n",
		"/mud/lib/players/ann/a.c":      "",
		"/mud/lib/players/bob/.dgdtools.yml": "root: false\nformat: {indent_style: tab}\n" +
			"lint:\n  rules: {missing-visibility: error}\n  overrides:\n    - paths: [\"old/**\"]\n      rules: {all: off}\n",
		"/mud/lib/players/bob/old/b.c":      "",
		"/mud/lib/players/cyd/dgdtools.yml": "profile: mymud\nlib_path: /players/cyd\nformat: {line_endings: crlf}\nlint:\n  baseline: b.json\n",
		"/mud/lib/players/cyd/c.c":          "",
	}
}

func TestScopesBelongToTheProjectAbove(t *testing.T) {
	m := scoped()
	// A settings scope is passed over when the project is looked for.
	root, err := FindRoot(m, "/mud/lib/players/bob/old")
	require.NoError(t, err)
	assert.Equal(t, "/mud/dgdtools.yml", root)
	// Files under different scopes are one project.
	prj, err := Discover(m, []string{"/mud/lib/players/ann/a.c", "/mud/lib/players/bob/old/b.c", "/mud/lib/std/room.c"}, DiscoverOptions{})
	require.NoError(t, err)
	assert.Equal(t, "/mud/dgdtools.yml", prj.ConfigPath)
	assert.Equal(t, "/mud/lib", prj.Root)
	// A nested root config is still the project of the paths under it.
	root, err = FindRoot(m, "/mud/lib/players/cyd")
	require.NoError(t, err)
	assert.Equal(t, "/mud/lib/players/cyd/dgdtools.yml", root)
	_, err = Discover(m, []string{"/mud/lib/players/ann/a.c", "/mud/lib/players/cyd/c.c"}, DiscoverOptions{})
	assert.ErrorContains(t, err, "different projects")
}

func TestSettingsPerFile(t *testing.T) {
	m := scoped()
	prj, err := Discover(m, []string{"/mud/lib"}, DiscoverOptions{})
	require.NoError(t, err)
	settings := func(path string) *Settings {
		t.Helper()
		s, err := prj.Settings(path)
		require.NoError(t, err)
		return s
	}

	// No scope: the project's own settings, and the defaults.
	s := settings("/mud/lib/std/room.c")
	assert.Equal(t, Format{IndentSpace, 8, LineEndingsLF}, s.Format)
	assert.Equal(t, FormatSources{FromDefault, "/mud/dgdtools.yml", "/mud/dgdtools.yml"}, s.From)
	assert.Empty(t, s.Scopes)

	// One scope: its keys win, the rest comes from above.
	s = settings("/mud/lib/players/ann/a.c")
	assert.Equal(t, Format{IndentSpace, 2, LineEndingsLF}, s.Format)
	assert.Equal(t, "/mud/lib/players/dgdtools.yml", s.From.IndentWidth)
	require.Len(t, s.Scopes, 1)
	assert.Equal(t, "off", s.Scopes[0].Rules["missing-visibility"].Severity)

	// Two: per key the nearest that sets it; outermost first in Scopes.
	s = settings("/mud/lib/players/bob/old/b.c")
	assert.Equal(t, Format{IndentTab, 2, LineEndingsLF}, s.Format)
	assert.Equal(t, FormatSources{"/mud/lib/players/bob/.dgdtools.yml", "/mud/lib/players/dgdtools.yml", "/mud/dgdtools.yml"}, s.From)
	require.Len(t, s.Scopes, 2)
	assert.Equal(t, "/mud/lib/players", s.Scopes[0].Dir)
	assert.Equal(t, "/mud/lib/players/bob", s.Scopes[1].Dir)
	assert.Equal(t, []string{"old/**"}, s.Scopes[1].Overrides[0].Paths)
	// A file that does not exist yet has settings too (an editor buffer).
	assert.Equal(t, s.Format, settings("/mud/lib/players/bob/new/unsaved.c").Format)

	// A root config below the project is another project (TestNestedProject).
	_, err = prj.Settings("/mud/lib/players/cyd/c.c")
	assert.ErrorContains(t, err, "belongs to the project of /mud/lib/players/cyd/dgdtools.yml")

	// The caller's settings win over every file.
	prj, err = Discover(m, []string{"/mud/lib"}, DiscoverOptions{Format: Format{IndentWidth: 3}})
	require.NoError(t, err)
	s = settings("/mud/lib/players/bob/old/b.c")
	assert.Equal(t, Format{IndentTab, 3, LineEndingsLF}, s.Format)
	assert.Equal(t, FromFlag, s.From.IndentWidth)
	_, err = Discover(m, []string{"/mud/lib"}, DiscoverOptions{Format: Format{IndentWidth: 99}})
	assert.ErrorContains(t, err, "indent width 99")

	// A config named by the caller is the whole configuration: no nested
	// file is looked for. Without configuration nothing is read at all.
	prj, err = Discover(m, []string{"/mud/lib"}, DiscoverOptions{ConfigPath: "/mud/dgdtools.yml"})
	require.NoError(t, err)
	s = settings("/mud/lib/players/bob/old/b.c")
	assert.Equal(t, Format{IndentSpace, 8, LineEndingsLF}, s.Format)
	assert.Empty(t, s.Scopes)
	prj, err = Discover(m, []string{"/mud/lib"}, DiscoverOptions{NoConfig: true})
	require.NoError(t, err)
	s = settings("/mud/lib/players/bob/old/b.c")
	assert.Equal(t, Format{DefaultIndentStyle, DefaultIndentWidth, DefaultLineEndings}, s.Format)
	assert.Equal(t, FormatSources{FromDefault, FromDefault, FromDefault}, s.From)
	assert.Empty(t, s.Scopes)

	// The lib somewhere else than the config: a scope at the lib root
	// applies, and nothing above the lib is looked at.
	m = vfs.Mem{
		"/proj/dgdtools.yml": "root: ../lib\n",
		"/lib/dgdtools.yml":  "root: false\nformat: {indent_width: 3}\n",
		"/dgdtools.yml":      "root: false\nformat: {indent_style: tab}\n",
		"/lib/std/a.c":       "",
		"/elsewhere/b.c":     "",
	}
	prj, err = Discover(m, nil, DiscoverOptions{WorkDir: "/proj"})
	require.NoError(t, err)
	assert.Equal(t, "/lib", prj.Root)
	assert.Equal(t, Format{IndentSpace, 3, LineEndingsPreserve}, settings("/lib/std/a.c").Format)
	// A file that is not the project's has the project's settings only.
	assert.Equal(t, Format{IndentSpace, 4, LineEndingsPreserve}, settings("/elsewhere/b.c").Format)
}

func TestScopeValidation(t *testing.T) {
	for name, c := range map[string]struct{ yml, want string }{
		"root path":      {"root: false\nexclude: [\"old/**\"]\n", "exclude belongs in the project's root config"},
		"profile":        {"root: false\nprofile: mymud\n", "profile belongs in the project's root config"},
		"dialect":        {"root: false\ndialect: {closures: true}\n", "dialect belongs"},
		"driver config":  {"root: false\ndriver_config: mud.dgd\n", "driver_config belongs"},
		"driver block":   {"root: false\ndriver: {typechecking: 1}\n", "driver belongs"},
		"conventions":    {"root: false\nconventions: {defines: [X]}\n", "conventions belongs"},
		"build":          {"root: false\nprofile_build: {name: x}\n", "profile_build belongs"},
		"baseline":       {"root: false\nlint: {baseline: b.json}\n", "lint.baseline belongs"},
		"root true":      {"root: true\n", "root: true is not a setting"},
		"width":          {"format: {indent_width: 40}\n", "format.indent_width: indent width 40 is not between 1 and 16"},
		"style":          {"format:\n  indent_style: tabs\n", "line 2: unknown indent style \"tabs\""},
		"endings":        {"format: {line_endings: dos}\n", "unknown line endings \"dos\""},
		"format key":     {"format: {line_width: 80}\n", "line_width"},
		"object paths":   {"relative_object_paths: cwd\n", `relative_object_paths: unknown value "cwd" (want root or file)`},
		"macro class":    {"conventions:\n  macros: {FOREACH: statment}\n", `conventions.macros: FOREACH: unknown class "statment" (want expression, empty, specifier, type, string, statement, opaque)`},
		"typechecking":   {"dialect: {typechecking: 2}\n", "dialect.typechecking moved: set driver.typechecking, or let dgdtools read it from driver_config"},
		"kfun":           {"dialect:\n  kfuns: [\"int *rusage()\", \"rusage\"]\n", `dialect.kfuns: kfun prototype "rusage": want TYPE NAME(PARAMETERS)`},
		"scope kfuns":    {"root: false\ndialect: {kfuns: [\"int f()\"]}\n", "dialect belongs"},
		"registry index": {"conventions:\n  object_registry: {load: -1}\n", "conventions.object_registry: load: -1 is not an argument index"},
		"owners":         {"profile_build: {share_min_owners: -1}\n", "profile_build.share_min_owners"},
		"severity":       {"lint:\n  rules:\n    syntax: loud\n", `line 3: lint.rules: syntax: unknown severity "loud"`},
		"scope severity": {"root: false\nlint:\n  overrides:\n    - paths: [\"a/**\"]\n      rules: {all: none}\n", `lint.overrides[0].rules: all: unknown severity "none"`},
		"override paths": {"lint:\n  overrides:\n    - rules: {all: off}\n", "lint.overrides[0]: no paths"},
		"width zero":     {"format: {indent_width: 0}\n", "line 1: format.indent_width: indent width 0 is not between 1 and 16"},
		"width fraction": {"format: {indent_width: 1.5}\n", "line 1: format.indent_width: 1.5 is not a whole number"},
		"width quoted":   {"format: {indent_width: \"4\"}\n", "format.indent_width: 4 is not a whole number"},
		"key typo":       {"fromat: {indent_width: 2}\n", `line 1: unknown key "fromat" (did you mean "format"?)`},
		"nested typo":    {"format: {indent_widht: 2}\n", `unknown key "indent_widht" in format (did you mean "indent_width"?)`},
		"kind":           {"root: [a]\n", "line 1: want a string here, not a list"},
		"bad glob":       {"exclude: [\"[bad\"]\n", `exclude: "[bad": "[" is not part of a glob here`},
		"override glob":  {"lint:\n  overrides:\n    - paths: [\"{a,b}/**\"]\n      rules: {all: off}\n", "lint.overrides[0].paths:"},
	} {
		m := vfs.Mem{"/p/sub/dgdtools.yml": c.yml, "/p/sub/a.c": ""}
		_, err := Load(m, "/p/sub/dgdtools.yml")
		assert.ErrorContains(t, err, c.want, name)
		assert.ErrorContains(t, err, "/p/sub/dgdtools.yml", name)
	}

	// What a scope may hold.
	m := vfs.Mem{
		"/p/dgdtools.yml":        "root: .\nformat: {indent_width: 8}\n",
		"/p/sub/dgdtools.yml":    "root: false\nformat: {indent_style: tab}\nlint:\n  rules: {syntax: error}\n  overrides:\n    - paths: [\"x/**\"]\n      rules: {all: off}\n",
		"/p/sub/a.c":             "",
		"/p/bad/dgdtools.yml":    "root: false\nlib_path: /x\n",
		"/p/bad/deep/b.c":        "",
		"/p/quoted/dgdtools.yml": "root: \"false\"\n",
	}
	c, err := Load(m, "/p/sub/dgdtools.yml")
	require.NoError(t, err)
	assert.True(t, c.Scope)
	assert.Equal(t, IndentTab, c.Format.IndentStyle)
	assert.Len(t, c.Lint.Overrides, 1)
	// A quoted false is a directory name.
	c, err = Load(m, "/p/quoted/dgdtools.yml")
	require.NoError(t, err)
	assert.False(t, c.Scope)
	assert.Equal(t, "/p/quoted/false", c.AbsRoot())

	// A scope that is not valid fails the settings of the files under
	// it, and only those.
	prj, err := Discover(m, []string{"/p"}, DiscoverOptions{})
	require.NoError(t, err)
	_, err = prj.Settings("/p/bad/deep/b.c")
	assert.ErrorContains(t, err, "/p/bad/dgdtools.yml: lib_path belongs")
	s, err := prj.Settings("/p/sub/a.c")
	require.NoError(t, err)
	assert.Equal(t, Format{IndentTab, 8, LineEndingsPreserve}, s.Format)

	// A scope cannot be the project.
	_, err = Discover(m, []string{"/p"}, DiscoverOptions{ConfigPath: "/p/sub/dgdtools.yml"})
	assert.ErrorContains(t, err, "is a settings scope")
}

// A root config below a project's own is another project: its directory
// is not the outer project's, whichever way it is come by.
func TestNestedProject(t *testing.T) {
	m := vfs.Mem{
		"/p/dgdtools.yml":               "root: .\nformat: {indent_width: 8}\n",
		"/p/a.c":                        "",
		"/p/sub/dgdtools.yml":           "root: false\nformat: {indent_width: 2}\n",
		"/p/sub/own/dgdtools.yml":       "root: .\nformat: {indent_style: tab}\nconventions: {macros: {public: specifier}}\n",
		"/p/sub/own/deep/b.c":           "",
		"/p/sub/own/deep/.dgdtools.yml": "root: false\nformat: {indent_width: 3}\n",
	}
	outer, err := Discover(m, []string{"/p"}, DiscoverOptions{})
	require.NoError(t, err)
	assert.Empty(t, outer.Nested("/p/a.c"))
	assert.Empty(t, outer.Nested("/p/sub/x.c"))
	assert.Equal(t, "/p/sub/own/dgdtools.yml", outer.Nested("/p/sub/own/deep/b.c"))
	assert.Equal(t, "/p/sub/own/dgdtools.yml", outer.Nested("/p/sub/own"))
	_, err = outer.Settings("/p/sub/own/deep/b.c")
	var nested *NestedProjectError
	require.ErrorAs(t, err, &nested)
	assert.Equal(t, "/p/sub/own/dgdtools.yml", nested.Config)
	assert.EqualError(t, err, "/p/sub/own/deep/b.c belongs to the project of /p/sub/own/dgdtools.yml")

	// The same file, asked about by itself or through a workspace.
	for _, inner := range []func() (*Project, error){
		func() (*Project, error) { return Discover(m, []string{"/p/sub/own/deep/b.c"}, DiscoverOptions{}) },
		func() (*Project, error) { return NewWorkspace(m, DiscoverOptions{}).Project("/p/sub/own/deep/b.c") },
	} {
		prj, err := inner()
		require.NoError(t, err)
		assert.Equal(t, "/p/sub/own/dgdtools.yml", prj.ConfigPath)
		assert.Equal(t, map[string]string{"public": "specifier"}, prj.Config.Conventions.Macros)
		s, err := prj.Settings("/p/sub/own/deep/b.c")
		require.NoError(t, err)
		assert.Equal(t, Format{IndentTab, 3, LineEndingsPreserve}, s.Format, "its own scopes, and none of the outer project's")
	}
	// A config named by hand is the whole configuration.
	only, err := Discover(m, []string{"/p"}, DiscoverOptions{ConfigPath: "/p/dgdtools.yml"})
	require.NoError(t, err)
	assert.Empty(t, only.Nested("/p/sub/own/deep/b.c"))
}

// A workspace is told what changed, and resolves again what was resolved
// from it.
func TestWorkspaceInvalidate(t *testing.T) {
	m := vfs.Mem{
		"/ws/lib/dgdtools.yml":   "profile: /ws/x.profile\nformat:\n  indent_width: 2\n",
		"/ws/x.profile":          "dgdtools-profile 2\n{\"name\":\"x\",\"dialect\":{\"driver\":\"dgd\"},\"conventions\":{}}\n",
		"/ws/lib/a.c":            "",
		"/ws/lib/sub/b.c":        "",
		"/ws/other/dgdtools.yml": "root: .\n",
		"/ws/other/c.c":          "",
		"/ws/.editorconfig":      "[*]\nend_of_line = lf\n",
	}
	w := NewWorkspace(m, DiscoverOptions{})
	project := func(path string) *Project {
		t.Helper()
		prj, err := w.Project(path)
		require.NoError(t, err)
		return prj
	}
	width := func(path string) int {
		t.Helper()
		s, err := project(path).Settings(path)
		require.NoError(t, err)
		return s.Format.IndentWidth
	}
	lib, other := project("/ws/lib/sub/b.c"), project("/ws/other/c.c")
	assert.Equal(t, []string{"/ws/lib/dgdtools.yml", "/ws/x.profile"}, lib.Inputs())
	require.Equal(t, 2, width("/ws/lib/sub/b.c"))

	// A source file is nothing to it.
	dropped, changed := w.Invalidate("/ws/lib/a.c")
	assert.Empty(t, dropped)
	assert.False(t, changed)
	assert.Same(t, lib, project("/ws/lib/a.c"))

	// The root config; the profile.
	m["/ws/lib/dgdtools.yml"] = "profile: /ws/x.profile\nformat:\n  indent_width: 8\n"
	dropped, changed = w.Invalidate("/ws/lib/dgdtools.yml")
	assert.Equal(t, []*Project{lib}, dropped)
	assert.True(t, changed)
	assert.Equal(t, 8, width("/ws/lib/sub/b.c"))
	assert.Same(t, other, project("/ws/other/c.c"), "another project is left alone")
	for _, input := range []string{"/ws/x.profile"} {
		lib = project("/ws/lib/a.c")
		dropped, _ = w.Invalidate(input)
		assert.Equal(t, []*Project{lib}, dropped, input)
		assert.NotSame(t, lib, project("/ws/lib/a.c"), input)
	}

	// A settings scope appears, then becomes a project of its own.
	lib = project("/ws/lib/a.c")
	// One with nothing but settings is a scope without saying root: false.
	m["/ws/lib/sub/dgdtools.yml"] = "format:\n  indent_width: 3\n"
	dropped, _ = w.Invalidate("/ws/lib/sub/dgdtools.yml")
	assert.Equal(t, []*Project{lib}, dropped)
	assert.Equal(t, 3, width("/ws/lib/sub/b.c"))
	assert.Equal(t, "/ws/lib/dgdtools.yml", project("/ws/lib/sub/b.c").ConfigPath)
	m["/ws/lib/sub/dgdtools.yml"] = "root: .\nformat:\n  indent_width: 5\n"
	w.Invalidate("/ws/lib/sub/dgdtools.yml")
	assert.Equal(t, 5, width("/ws/lib/sub/b.c"))
	assert.Equal(t, "/ws/lib/sub/dgdtools.yml", project("/ws/lib/sub/b.c").ConfigPath)
	assert.Equal(t, 8, width("/ws/lib/a.c"))

	// An .editorconfig is read again; no project is another one for it.
	s, err := other.Settings("/ws/other/c.c")
	require.NoError(t, err)
	require.Equal(t, LineEndingsLF, s.Format.LineEndings)
	m["/ws/.editorconfig"] = "[*]\nend_of_line = crlf\n"
	dropped, changed = w.Invalidate("/ws/.editorconfig")
	assert.Empty(t, dropped)
	assert.True(t, changed)
	s, err = other.Settings("/ws/other/c.c")
	require.NoError(t, err)
	assert.Equal(t, LineEndingsCRLF, s.Format.LineEndings)

	// A project that could not be resolved is tried again.
	m["/ws/other/dgdtools.yml"] = "no_such_key: 1\n"
	w.Invalidate("/ws/other/dgdtools.yml")
	_, err = w.Project("/ws/other/c.c")
	require.Error(t, err)
	m["/ws/other/dgdtools.yml"] = ""
	w.Invalidate("/ws/other/dgdtools.yml")
	project("/ws/other/c.c")

	// A directory that holds a project's config goes away.
	gone := project("/ws/other/c.c")
	delete(m, "/ws/other/dgdtools.yml")
	delete(m, "/ws/other/c.c")
	dropped, _ = w.Invalidate("/ws/other")
	assert.Equal(t, []*Project{gone}, dropped)
}

// Without a config to say which directory a lib path is of, it is the
// working directory's, in whatever order the paths come.
func TestLibPathOption(t *testing.T) {
	m := vfs.Mem{"/home/ann/a.c": "", "/home/ann/sub/b.c": ""}
	opt := DiscoverOptions{LibPath: "/players/ann", WorkDir: "/home/ann"}
	for _, paths := range [][]string{{"a.c", "sub/b.c"}, {"sub/b.c", "a.c"}, nil} {
		prj, err := Discover(m, paths, opt)
		require.NoError(t, err)
		assert.Equal(t, "/home/ann", prj.CheckoutDir, "%v", paths)
		assert.Equal(t, "/players/ann", prj.CheckoutLib)
	}
	w := NewWorkspace(m, opt)
	prj, err := w.Project("/home/ann/sub/b.c")
	require.NoError(t, err)
	assert.Equal(t, "/home/ann", prj.CheckoutDir)
	// A config's own lib_path is that of its root.
	m["/home/ann/sub/dgdtools.yml"] = "lib_path: /players/ann/sub\n"
	prj, err = Discover(m, []string{"sub/b.c"}, DiscoverOptions{WorkDir: "/home/ann"})
	require.NoError(t, err)
	assert.Equal(t, "/home/ann/sub", prj.CheckoutDir)
}

// What an editor's user said of a folder holds for the paths under it
// that no config file claims, and makes them a project of the folder's.
func TestFolders(t *testing.T) {
	m := vfs.Mem{
		"/home/ann/a.c":              "",
		"/home/ann/sub/b.c":          "",
		"/home/ann/lib/dgdtools.yml": "lib_path: /players/ann/lib\n",
		"/home/ann/lib/c.c":          "",
		"/home/bob/d.c":              "",
		"/elsewhere/e.c":             "",
	}
	opt := DiscoverOptions{Folders: []Folder{
		{Dir: "/home/ann", LibPath: "/players/ann"},
		{Dir: "/home/bob", LibPath: "/players/bob"},
	}}
	w := NewWorkspace(m, opt)
	project := func(path string) *Project {
		t.Helper()
		prj, err := w.Project(path)
		require.NoError(t, err)
		return prj
	}
	ann := project("/home/ann/sub/b.c")
	assert.Equal(t, "/home/ann", ann.CheckoutDir, "anchored at the folder, whichever file is asked about first")
	assert.Equal(t, "/players/ann", ann.CheckoutLib)
	assert.Same(t, ann, project("/home/ann/a.c"))
	bob := project("/home/bob/d.c")
	assert.Equal(t, "/home/bob", bob.CheckoutDir)
	assert.Equal(t, "/players/bob", bob.CheckoutLib)
	// A config file wins, and a path in no folder is as it ever was.
	assert.Equal(t, "/players/ann/lib", project("/home/ann/lib/c.c").CheckoutLib)
	assert.False(t, project("/elsewhere/e.c").HasLib())
	// A command's one project is found the same way.
	prj, err := Discover(m, []string{"/home/ann/sub/b.c"}, opt)
	require.NoError(t, err)
	assert.Equal(t, "/home/ann", prj.CheckoutDir)
	// A config file that appears in a folder takes its files from it.
	m["/home/bob/dgdtools.yml"] = "lib_path: /players/robert\n"
	dropped, changed := w.Invalidate("/home/bob/dgdtools.yml")
	assert.True(t, changed)
	assert.Contains(t, dropped, bob)
	assert.Equal(t, "/players/robert", project("/home/bob/d.c").CheckoutLib)
}

func TestEditorConfig(t *testing.T) {
	m := vfs.Mem{
		"/.editorconfig":         "[*]\nindent_size = 7\nend_of_line = crlf\n",
		"/mud/.editorconfig":     "root = true\n[*]\nindent_style = tab\nindent_size = tab\ntab_width = 8\nend_of_line = lf\n[*.h]\nindent_style = space\nindent_size = 2\n[legacy/**]\nend_of_line = cr\nindent_size = 99\n",
		"/mud/dgdtools.yml":      "root: .\nformat: {line_endings: preserve}\n",
		"/mud/std/a.c":           "",
		"/mud/sub/.editorconfig": "[*.c]\nindent_style = space\nindent_size = 3\n",
		"/mud/sub/dgdtools.yml":  "root: false\nformat: {indent_width: 5}\n",
		"/mud/sub/b.c":           "",
		"/bare/c.c":              "",
	}
	prj, err := Discover(m, []string{"/mud"}, DiscoverOptions{})
	require.NoError(t, err)
	settings := func(prj *Project, path string) *Settings {
		t.Helper()
		s, err := prj.Settings(path)
		require.NoError(t, err)
		return s
	}

	// What format: leaves unset comes from .editorconfig; what it sets
	// wins, however near the .editorconfig is.
	s := settings(prj, "/mud/std/a.c")
	assert.Equal(t, Format{IndentTab, 8, LineEndingsPreserve}, s.Format)
	assert.Equal(t, FormatSources{"/mud/.editorconfig", "/mud/.editorconfig", "/mud/dgdtools.yml"}, s.From)
	// Sections are matched against the file.
	assert.Equal(t, Format{IndentSpace, 2, LineEndingsPreserve}, settings(prj, "/mud/std/a.h").Format)
	// The nearer .editorconfig wins per property; a dgdtools.yml on the
	// way wins over both.
	s = settings(prj, "/mud/sub/b.c")
	assert.Equal(t, Format{IndentSpace, 5, LineEndingsPreserve}, s.Format)
	assert.Equal(t, FormatSources{"/mud/sub/.editorconfig", "/mud/sub/dgdtools.yml", "/mud/dgdtools.yml"}, s.From)
	// Values that mean nothing to the formatter are passed over.
	s = settings(prj, "/mud/legacy/old.c")
	assert.Equal(t, 4, s.Format.IndentWidth)
	assert.Equal(t, FromDefault, s.From.IndentWidth)

	// Without any dgdtools.yml, .editorconfig is all there is; the files
	// above one with root = true are not read.
	bare, err := Discover(m, []string{"/bare"}, DiscoverOptions{})
	require.NoError(t, err)
	assert.Equal(t, Format{IndentSpace, 7, LineEndingsCRLF}, settings(bare, "/bare/c.c").Format)
	delete(m, "/mud/dgdtools.yml")
	delete(m, "/mud/sub/dgdtools.yml")
	none, err := Discover(m, []string{"/mud"}, DiscoverOptions{})
	require.NoError(t, err)
	assert.Equal(t, Format{IndentTab, 8, LineEndingsLF}, settings(none, "/mud/std/a.c").Format)
	// A config named by the caller still leaves .editorconfig its part;
	// "no configuration" does not.
	m["/cfg.yml"] = "root: /mud\nformat: {indent_width: 6}\n"
	named, err := Discover(m, nil, DiscoverOptions{ConfigPath: "/cfg.yml"})
	require.NoError(t, err)
	assert.Equal(t, Format{IndentTab, 6, LineEndingsLF}, settings(named, "/mud/std/a.c").Format)
	isolated, err := Discover(m, []string{"/mud"}, DiscoverOptions{NoConfig: true})
	require.NoError(t, err)
	assert.Equal(t, Format{IndentSpace, 4, LineEndingsPreserve}, settings(isolated, "/mud/std/a.c").Format)
}

func TestUserConfigIsAFallback(t *testing.T) {
	m := vfs.Mem{
		"/home/u/.config/dgdtools/dgdtools.yml": "format: {indent_width: 2}\nconventions: {macros: {visible: specifier}}\nlint:\n  rules: {missing-visibility: warning}\n",
		"/work/mine/a.c":                        "",
		"/work/mine/sub/dgdtools.yml":           "root: false\nformat: {indent_style: tab}\n",
		"/work/mine/sub/b.c":                    "",
		"/proj/dgdtools.yml":                    "root: .\nformat: {line_endings: lf}\n",
		"/proj/c.c":                             "",
	}
	user := UserConfigPath(m, "", "/home/u")
	assert.Equal(t, "/home/u/.config/dgdtools/dgdtools.yml", user)
	assert.Empty(t, UserConfigPath(m, "/nowhere", "/home/other"))
	opt := DiscoverOptions{UserConfig: user}

	// No project config: the user's is the project's, but its directory
	// is not the project's, and no lib root comes of it.
	prj, err := Discover(m, []string{"/work/mine/a.c"}, opt)
	require.NoError(t, err)
	assert.True(t, prj.UserConfig)
	assert.Equal(t, user, prj.ConfigPath)
	assert.Empty(t, prj.Root)
	assert.Equal(t, "/work/mine", prj.Base)
	assert.Equal(t, map[string]string{"visible": "specifier"}, prj.Config.Conventions.Macros)
	s, err := prj.Settings("/work/mine/a.c")
	require.NoError(t, err)
	assert.Equal(t, 2, s.Format.IndentWidth)
	// A settings scope adjusts it like any project's.
	s, err = prj.Settings("/work/mine/sub/b.c")
	require.NoError(t, err)
	assert.Equal(t, Format{IndentTab, 2, LineEndingsPreserve}, s.Format)

	// A project config: the user's is not read, let alone merged.
	prj, err = Discover(m, []string{"/proj/c.c"}, opt)
	require.NoError(t, err)
	assert.False(t, prj.UserConfig)
	assert.Equal(t, "/proj/dgdtools.yml", prj.ConfigPath)
	assert.Empty(t, prj.Config.Conventions.Macros)
	s, err = prj.Settings("/proj/c.c")
	require.NoError(t, err)
	assert.Equal(t, Format{IndentSpace, 4, LineEndingsLF}, s.Format)
	// Nor with --no-config.
	prj, err = Discover(m, []string{"/work/mine/a.c"}, DiscoverOptions{UserConfig: user, NoConfig: true})
	require.NoError(t, err)
	assert.Empty(t, prj.ConfigPath)

	// It has no directory to be a lib: a relative root is refused, an
	// absolute one is the lib.
	m[user] = "root: lib\n"
	_, err = Discover(m, []string{"/work/mine/a.c"}, opt)
	assert.ErrorContains(t, err, "needs an absolute root")
	m[user] = "root: /proj\n"
	prj, err = Discover(m, []string{"/work/mine/a.c"}, opt)
	require.NoError(t, err)
	assert.Equal(t, "/proj", prj.Root)
}

func TestWorkspace(t *testing.T) {
	m := scoped()
	m["/other/dgdtools.yml"] = "format: {indent_width: 3}\n"
	m["/other/x.c"] = ""
	m["/loose/y.c"] = ""
	m["/broken/dgdtools.yml"] = "frmat: {}\n"
	m["/broken/z.c"] = ""
	ws := NewWorkspace(m, DiscoverOptions{})
	project := func(path string) *Project {
		t.Helper()
		prj, err := ws.Project(path)
		require.NoError(t, err)
		return prj
	}
	// Each path has the project of its nearest root config; a project is
	// resolved once.
	lib := project("/mud/lib/std/room.c")
	assert.Equal(t, "/mud/dgdtools.yml", lib.ConfigPath)
	assert.Same(t, lib, project("/mud/lib/players/bob/old/b.c"))
	assert.Same(t, lib, project("/mud/lib/players"))
	// A root config further down is the project of what is under it, and
	// resolved as one: this one names a profile that is not there.
	_, err := ws.Project("/mud/lib/players/cyd/c.c")
	assert.ErrorContains(t, err, `profile "mymud" not found`)
	assert.Equal(t, "/other/dgdtools.yml", project("/other/x.c").ConfigPath)
	// Paths without one share a project without a config.
	loose := project("/loose/y.c")
	assert.Empty(t, loose.ConfigPath)
	assert.Same(t, loose, project("/elsewhere/q.c"))
	// A config that is not valid is the error of its paths only.
	_, err = ws.Project("/broken/z.c")
	assert.ErrorContains(t, err, "frmat")

	// With a config named by the caller there is one project.
	ws = NewWorkspace(m, DiscoverOptions{ConfigPath: "/other/dgdtools.yml"})
	one := project("/mud/lib/std/room.c")
	assert.Equal(t, "/other/dgdtools.yml", one.ConfigPath)
	assert.Same(t, one, project("/loose/y.c"))
}

// What a profile or the driver's configuration contributes is checked
// like what the file says, and the error names where it came from.
func TestContributedValuesAreValidated(t *testing.T) {
	m := vfs.Mem{
		"/p/dgdtools.yml": "profile: bad.profile\n",
		"/p/bad.profile":  "dgdtools-profile 2\n{\"name\":\"bad\",\"dialect\":{\"driver\":\"dgd\"},\"relative_object_paths\":\"sideways\",\"conventions\":{}}\n",
		"/p/a.c":          "",
		"/q/dgdtools.yml": "root: .\ndriver_config: mud.dgd\n",
		"/q/mud.dgd":      "typechecking = 7;\n",
		"/q/a.c":          "",
	}
	_, err := Discover(m, []string{"/p/a.c"}, DiscoverOptions{})
	assert.ErrorContains(t, err, `/p/bad.profile: relative_object_paths: unknown value "sideways"`)
	_, err = Discover(m, []string{"/q/a.c"}, DiscoverOptions{})
	assert.ErrorContains(t, err, "/q/mud.dgd: dialect.typechecking: 7 is not a typechecking level")
}
