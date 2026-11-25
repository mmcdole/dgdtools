package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

const driverCfg = `telnet_port	= 2001;			/* telnet port number */
directory	= "/usr/local/mud/lib";
typechecking	= 1;			/* global typechecking */
include_file	= "/dgd/include/Std.h";	/* standard include file */
include_dirs	= ({ "/dgd/include",
		     "/std/include" }); /* directories to search */
auto_object	= "/dgd/lib/object";	/* auto inherited object */
create		= "_F_create";		/* name of create function */
modules = ([ "/x/ext.so" : "" ]);
`

func TestDiscoverWalksUpFromThePath(t *testing.T) {
	m := vfs.Mem{
		"/mud/dgdtools.yml":         "root: lib\ndriver_config: etc/mud.dgd\n",
		"/mud/etc/mud.dgd":          driverCfg,
		"/mud/lib/d/Foo/castle.c":   "",
		"/other/.dgdtools.yml":      "root: .\nexclude: [\"old/**\"]\n",
		"/noroot/dgdtools.yml":      "exclude: [\"old/**\"]\n",
		"/noroot/lib/w.c":           "",
		"/other/src/x.c":            "",
		"/nowhere/deep/down/y.c":    "",
		"/mud/lib/sub/dgdtools.yml": "root: .\n",
		"/mud/lib/sub/z.c":          "",
	}

	// From a file deep in the lib, from any working directory.
	prj, err := Discover(m, []string{"/mud/lib/d/Foo/castle.c"}, DiscoverOptions{})
	require.NoError(t, err)
	assert.Equal(t, "/mud/dgdtools.yml", prj.ConfigPath)
	assert.Equal(t, "/mud/lib", prj.Root)
	// The driver config is the compile database.
	assert.Equal(t, []string{"/dgd/include", "/std/include"}, prj.Config.IncludeDirs)
	assert.Equal(t, "/dgd/include/Std.h", prj.Config.IncludeFile)
	assert.Equal(t, []string{"/dgd/lib/object"}, prj.Config.AutoObjects)
	// The driver's create (_F_create) is what the auto object receives,
	// not what the lib's objects define: that is conventions.create.
	assert.Equal(t, "create", prj.Config.Create())
	require.NotNil(t, prj.Config.Dialect.Typechecking)
	assert.Equal(t, 1, *prj.Config.Dialect.Typechecking)

	// The alternate file name; a directory argument.
	prj, err = Discover(m, []string{"/other/src"}, DiscoverOptions{})
	require.NoError(t, err)
	assert.Equal(t, "/other/.dgdtools.yml", prj.ConfigPath)
	assert.Equal(t, "/other", prj.Root)
	assert.Nil(t, prj.Config.Dialect.Typechecking)

	// A config that does not say where the lib is: no root is guessed.
	prj, err = Discover(m, []string{"/noroot/lib/w.c"}, DiscoverOptions{})
	require.NoError(t, err)
	assert.Equal(t, "/noroot/dgdtools.yml", prj.ConfigPath)
	assert.Empty(t, prj.Root)
	assert.False(t, prj.HasLib())

	// No config anywhere: no root is invented.
	prj, err = Discover(m, []string{"/nowhere/deep/down/y.c"}, DiscoverOptions{})
	require.NoError(t, err)
	assert.Empty(t, prj.ConfigPath)
	assert.Empty(t, prj.Root)

	// An explicit root without a config.
	prj, err = Discover(m, []string{"/nowhere/deep/down/y.c"}, DiscoverOptions{Root: "/nowhere"})
	require.NoError(t, err)
	assert.Equal(t, "/nowhere", prj.Root)

	// --no-config ignores a config that exists; --config forces one.
	prj, err = Discover(m, []string{"/mud/lib/d/Foo/castle.c"}, DiscoverOptions{NoConfig: true})
	require.NoError(t, err)
	assert.Empty(t, prj.Root)
	prj, err = Discover(m, []string{"/nowhere/deep/down/y.c"}, DiscoverOptions{ConfigPath: "/mud/dgdtools.yml"})
	require.NoError(t, err)
	assert.Equal(t, "/mud/lib", prj.Root)

	// The nearest root config wins; paths from two projects are refused.
	prj, err = Discover(m, []string{"/mud/lib/sub/z.c"}, DiscoverOptions{})
	require.NoError(t, err)
	assert.Equal(t, "/mud/lib/sub/dgdtools.yml", prj.ConfigPath)
	_, err = Discover(m, []string{"/mud/lib/sub/z.c", "/other/src/x.c"}, DiscoverOptions{})
	assert.ErrorContains(t, err, "different projects")

	// A root that is not a directory is an error, not a silent no-op.
	_, err = Discover(m, nil, DiscoverOptions{NoConfig: true, Root: "/does/not/exist"})
	assert.ErrorContains(t, err, "not a directory")
}

// The driver settings come from the driver config alone; what the file
// says of the dialect and the conventions is its own.
func TestDriverConfigIsTheSource(t *testing.T) {
	m := vfs.Mem{
		"/mud/dgdtools.yml": "root: .\ndriver_config: mud.dgd\nconventions: {create: init}\n",
		"/mud/mud.dgd":      driverCfg,
		"/mud/a.c":          "",
	}
	prj, err := Discover(m, []string{"/mud/a.c"}, DiscoverOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"/dgd/include", "/std/include"}, prj.Config.IncludeDirs)
	assert.Equal(t, 1, *prj.Config.Dialect.Typechecking)
	assert.Equal(t, "init", prj.Config.Create())
	assert.Equal(t, "/dgd/include/Std.h", prj.Config.IncludeFile)
	assert.Equal(t, []string{"/mud/dgdtools.yml", "/mud/mud.dgd"}, prj.Inputs())

	// A driver config that cannot be read is an error that names the key.
	m["/mud/dgdtools.yml"] = "root: .\ndriver_config: nope.dgd\n"
	_, err = Discover(m, []string{"/mud/a.c"}, DiscoverOptions{})
	assert.ErrorContains(t, err, "driver_config: ")
}

// A driver: block sets driver settings in the file. Each key it sets
// wins over the driver config and the profile; the rest come from them.
func TestDriverBlock(t *testing.T) {
	m := vfs.Mem{
		"/mud/dgdtools.yml": "root: .\ndriver_config: mud.dgd\ndriver:\n  include_dirs: [/inc, sub]\n  typechecking: 2\n",
		"/mud/mud.dgd":      driverCfg,
		"/mud/a.c":          "",
	}
	prj, err := Discover(m, []string{"/mud/a.c"}, DiscoverOptions{})
	require.NoError(t, err)
	cfg := prj.Config
	assert.Equal(t, []string{"/inc", "sub"}, cfg.IncludeDirs)
	assert.Equal(t, 2, *cfg.Dialect.Typechecking)
	assert.Equal(t, "/dgd/include/Std.h", cfg.IncludeFile, "from driver_config")
	assert.Equal(t, []string{"/dgd/lib/object"}, cfg.AutoObjects, "from driver_config")

	// Without a driver config the block is all there is; include_dirs: []
	// says there are none.
	m["/mud/dgdtools.yml"] = "root: .\ndriver:\n  include_dirs: []\n  include_file: /include/std.h\n  auto_object: /kernel/auto\n  driver_object: /kernel/driver\n"
	prj, err = Discover(m, []string{"/mud/a.c"}, DiscoverOptions{})
	require.NoError(t, err)
	cfg = prj.Config
	assert.NotNil(t, cfg.IncludeDirs)
	assert.Empty(t, cfg.IncludeDirs)
	assert.Equal(t, "/include/std.h", cfg.IncludeFile)
	assert.Equal(t, []string{"/kernel/auto"}, cfg.AutoObjects)
	assert.Equal(t, "/kernel/driver", cfg.DriverObject)
	assert.Nil(t, cfg.Dialect.Typechecking)
	assert.Equal(t, []string{"/mud/dgdtools.yml"}, prj.Inputs())

	// Over a profile, in a partial checkout.
	m = vfs.Mem{
		"/w/dgdtools.yml": "profile: mud.profile\nlib_path: /usr/ann\ndriver:\n  typechecking: 1\n  auto_object: /sys/auto\n",
		"/w/mud.profile": "dgdtools-profile 2\n" + `{"name":"mud","dialect":{"driver":"dgd","typechecking":0},` +
			`"include_dirs":["/include"],"include_file":"/include/std.h","auto_objects":["/kernel/auto"],"conventions":{}}` + "\n",
		"/w/a.c": "",
	}
	prj, err = Discover(m, []string{"/w/a.c"}, DiscoverOptions{})
	require.NoError(t, err)
	cfg = prj.Config
	assert.Equal(t, 1, *cfg.Dialect.Typechecking)
	assert.Equal(t, []string{"/sys/auto"}, cfg.AutoObjects)
	assert.Equal(t, []string{"/include"}, cfg.IncludeDirs, "from the profile")
	assert.Equal(t, "/include/std.h", cfg.IncludeFile, "from the profile")

	// What is wrong with the block names the file and the line.
	for text, want := range map[string]string{
		"driver:\n  typechecking: 3\n":          "line 2: driver.typechecking: 3 is not a typechecking level (0, 1 or 2)",
		"driver:\n  auto_objects: [/a]\n":       `line 2: unknown key "auto_objects" in driver (did you mean "auto_object"?)`,
		"driver:\n  include_file: \"\"\n":       "line 2: driver.include_file is empty: give a value, or leave the key out",
		"driver:\n  include_dirs: /inc\n":       "line 2: driver.include_dirs: want a list of directories",
		"driver:\n  include_dirs: [/a, \"\"]\n": "line 2: driver.include_dirs: an empty directory",
		"driver:\n  auto_object: [/a]\n":        "line 2: want a string here, not a list",
		"driver:\n  typechecking: high\n":       "line 2: want a number here, not a string (high)",
		"driver: /etc/mud.dgd\n":                "line 1: driver: want a mapping with any of include_dirs",
	} {
		_, err := Load(vfs.Mem{"/p/dgdtools.yml": text}, "/p/dgdtools.yml")
		require.Error(t, err, text)
		assert.Contains(t, err.Error(), "/p/dgdtools.yml: ", text)
		assert.Contains(t, err.Error(), want, text)
	}
}

func TestRuleSettings(t *testing.T) {
	m := vfs.Mem{
		"/proj/dgdtools.yml": `root: lib
exclude: ["**/old/**"]
conventions:
  call_registry: {add_action: {function: 0}, store_fp: [{object: 0, function: 1}, {function: 0}]}
  macros: { FOREACH: statement, public: specifier }
  defines: [__FEATURE__]
lint:
  rules:
    syntax: error
    raw-inherit-path: { severity: warning, deny: ["/std/"] }
    lifecycle-chain: { names: [create, reset] }
    E001: info
  overrides:
    - paths: ["players/**"]
      rules: { runtime: off, adjacent-strings: warning }
`,
	}
	c, err := Load(m, "/proj/dgdtools.yml")
	require.NoError(t, err)
	assert.Equal(t, "/proj/lib", c.AbsRoot())
	zero := 0
	assert.Equal(t, map[string]profile.CallForms{
		"add_action": {{Function: 0}},
		"store_fp":   {{Object: &zero, Function: 1}, {Function: 0}},
	}, c.Conventions.CallRegistry)
	// Scalar and mapping rule settings.
	assert.Equal(t, "error", c.Lint.Rules["syntax"].Severity)
	assert.Equal(t, "info", c.Lint.Rules["E001"].Severity)
	assert.Equal(t, map[string]any{"deny": []any{"/std/"}}, c.Lint.Rules["raw-inherit-path"].Options)
	assert.Equal(t, map[string]any{"names": []any{"create", "reset"}}, c.Lint.Rules["lifecycle-chain"].Options)
	assert.Equal(t, 11, c.Lint.Rules["lifecycle-chain"].Line, "the line in the file that says it")
	require.Len(t, c.Lint.Overrides, 1)
	assert.Equal(t, "off", c.Lint.Overrides[0].Rules["runtime"].Severity)
	assert.Equal(t, "statement", c.Conventions.Macros["FOREACH"])
	assert.Equal(t, "create", c.Create())

	// A misspelled key is an error, not a silently ignored setting.
	m["/typo/dgdtools.yml"] = "driver_conifg: x.dgd\n"
	_, err = Load(m, "/typo/dgdtools.yml")
	assert.ErrorContains(t, err, `unknown key "driver_conifg" (did you mean "driver_config"?)`)
	// So is a project key in the lint section, where it does not belong.
	m["/typo/dgdtools.yml"] = "lint:\n  exclude: [x]\n"
	_, err = Load(m, "/typo/dgdtools.yml")
	assert.ErrorContains(t, err, `unknown key "exclude"`)
}

// A key that an older dgdtools had says what took its place, naming the
// file and the line.
func TestRemovedKeys(t *testing.T) {
	for text, want := range map[string]string{
		"extends: base.yml\n":                                    "line 1: extends removed: put per-directory settings in a nested dgdtools.yml with root: false",
		"root: .\ninclude_dirs: [/include]\n":                    "line 2: include_dirs moved: set driver.include_dirs, or let dgdtools read it from driver_config",
		"include_file: /i.h\n":                                   "include_file moved: set driver.include_file, or let dgdtools read it from driver_config",
		"auto_objects: [/sys/auto]\n":                            "auto_objects moved: set driver.auto_object (one object, as in the driver config), or let dgdtools read it from driver_config",
		"driver_object: /sys/driver\n":                           "driver_object moved: set driver.driver_object, or let dgdtools read it from driver_config",
		"dialect:\n  typechecking: 2\n":                          "line 2: dialect.typechecking moved: set driver.typechecking, or let dgdtools read it from driver_config",
		"create: setup\n":                                        "create moved: use conventions.create",
		"profile_build:\n  name: x\n  driver_config: mud.dgd\n":  "line 3: profile_build.driver_config moved: use top-level driver_config",
		"conventions:\n  specifier_macros: [public]\n":           "conventions.specifier_macros removed: use conventions.macros: { public: specifier }",
		"conventions:\n  specifier_macros: [public, visible]\n":  "use conventions.macros: { public: specifier, visible: specifier }",
		"conventions:\n  format_registry: {scan: 1}\n":           "conventions.format_registry removed: sscanf is the scanf-style function dgdtools checks",
		"lint:\n  fail_on: warning\n":                            "lint.fail_on removed: pass --fail-on to dgdtools lint",
		"lint:\n  rules:\n    syntax-error: {max_per_file: 5}\n": "line 3: lint.rules.syntax-error.max_per_file removed: pass --max-per-file to dgdtools lint",
		"lint:\n  overrides:\n    - paths: [a]\n      rules: {E: {severity: info, max_per_file: 1}}\n": "lint.overrides[0].rules.E.max_per_file removed",
	} {
		_, err := Load(vfs.Mem{"/p/dgdtools.yml": text}, "/p/dgdtools.yml")
		require.Error(t, err, text)
		assert.Contains(t, err.Error(), "/p/dgdtools.yml: ", text)
		assert.Contains(t, err.Error(), want, text)
	}
	// In a settings scope too.
	m := vfs.Mem{"/p/dgdtools.yml": "root: .\n", "/p/w/dgdtools.yml": "root: false\nlint: {fail_on: info}\n", "/p/w/a.c": ""}
	prj, err := Discover(m, []string{"/p/w/a.c"}, DiscoverOptions{})
	require.NoError(t, err)
	_, err = prj.Settings("/p/w/a.c")
	assert.ErrorContains(t, err, "/p/w/dgdtools.yml: line 2: lint.fail_on removed")
}

func TestShareMinOwners(t *testing.T) {
	load := func(text string) (*Config, error) {
		return Load(vfs.Mem{"/p/dgdtools.yml": text}, "/p/dgdtools.yml")
	}
	c, err := load("profile_build: {name: x}\n")
	require.NoError(t, err)
	assert.Equal(t, DefaultShareMinOwners, c.ProfileBuild.MinOwners())
	c, err = load("profile_build: {share_min_owners: 0}\n")
	require.NoError(t, err)
	assert.Equal(t, 0, c.ProfileBuild.MinOwners(), "0: only what share: lists")
	c, err = load("profile_build: {share_min_owners: 3}\n")
	require.NoError(t, err)
	assert.Equal(t, 3, c.ProfileBuild.MinOwners())
	_, err = load("profile_build: {share_min_owners: -1}\n")
	assert.ErrorContains(t, err, "share_min_owners: -1: want a number of owners, or 0 to share only what share: lists")
}

// The documented examples must stay loadable and name only real settings.
func TestExampleConfigsLoad(t *testing.T) {
	c, err := Load(vfs.OS{}, "../../dgdtools.example.yml")
	require.NoError(t, err)
	assert.Equal(t, "etc/mud.dgd", c.DriverConfig)
	assert.Equal(t, "warning", c.Lint.Rules["raw-inherit-path"].Severity)
	require.Len(t, c.Lint.Overrides, 1)
	assert.Equal(t, "off", c.Lint.Overrides[0].Rules["runtime"].Severity)

	c, err = Load(vfs.OS{}, "../../examples/viking/dgdtools.yml")
	require.NoError(t, err)
	assert.Equal(t, "etc/config.viking", c.DriverConfig)
	assert.Contains(t, c.ProfileBuild.Share, "std/**")
	assert.Equal(t, map[string]string{"public": "specifier"}, c.Conventions.Macros)
	zero := 0
	assert.Equal(t, map[string]profile.CallForms{
		"add_action": {{Function: 0}},
		"store_fp":   {{Object: &zero, Function: 1}, {Function: 0}},
	}, c.Conventions.CallRegistry)
	assert.Equal(t, "create", c.Create())
	assert.Equal(t, map[string]any{"deny": []any{"/std/", "/obj/"}}, c.Lint.Rules["raw-inherit-path"].Options)
	assert.Contains(t, c.Exclude, "attic/**")
	// The kfuns of the driver's extension module, beside the driver's.
	kfuns, err := c.Kfuns()
	require.NoError(t, err)
	assert.Equal(t, "string argon2_hash(string password, int ops_limit, int mem_limit)", kfuns.Lookup("argon2_hash").Decl)
	assert.NotNil(t, kfuns.Lookup("strlen"))
}

// What a call registry entry may say.
func TestCallRegistry(t *testing.T) {
	load := func(entry string) (*Config, error) {
		return Load(vfs.Mem{"/p/dgdtools.yml": "conventions:\n  call_registry:\n    f: " + entry + "\n"}, "/p/dgdtools.yml")
	}
	one, two := 1, 2
	c, err := load("{function: 0}")
	require.NoError(t, err)
	assert.Equal(t, profile.CallForms{{Function: 0}}, c.Conventions.CallRegistry["f"])
	c, err = load("[{object: 1, function: 0, args: 2}, {function: 1}]")
	require.NoError(t, err)
	assert.Equal(t, profile.CallForms{{Object: &one, Function: 0, Args: &two}, {Function: 1}}, c.Conventions.CallRegistry["f"])

	for entry, want := range map[string]string{
		"0":                         "{function: N}",
		"{object: 0}":               "needs function",
		"{function: 0, obj: 1}":     `unknown key "obj"`,
		"{function: -1}":            "0-based argument index",
		"{function: first}":         "0-based argument index",
		"[]":                        "at least one form",
		"[{function: 0}, 1]":        "{function: N}",
		"{function: 0, args: 1.5}":  "0-based argument index",
		"{function: 0, object: no}": "0-based argument index",
	} {
		_, err := load(entry)
		assert.ErrorContains(t, err, want, entry)
		assert.ErrorContains(t, err, "line 3", entry)
	}
}

func TestInferCheckout(t *testing.T) {
	pats := []string{"players/*", "d/*"}
	dir, lib, ok := InferCheckout("/home/me/mud/players/drake/rooms/east", pats)
	assert.True(t, ok)
	assert.Equal(t, "/home/me/mud/players/drake", dir)
	assert.Equal(t, "/players/drake", lib)
	_, lib, ok = InferCheckout("/srv/d/Wild", pats)
	assert.True(t, ok)
	assert.Equal(t, "/d/Wild", lib)
	// players/* is tried first, so a stray /d/ higher up does not win.
	_, lib, _ = InferCheckout("/d/home/players/drake", pats)
	assert.Equal(t, "/players/drake", lib)
	_, _, ok = InferCheckout("/home/me/drake", pats)
	assert.False(t, ok)
	_, _, ok = InferCheckout("/home/me/players", pats)
	assert.False(t, ok)
}
