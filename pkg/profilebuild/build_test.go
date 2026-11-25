package profilebuild_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/index"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/profilebuild"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// wholeLib is a small lib with shared code, a domain, and two wizards.
func wholeLib() vfs.Mem {
	return vfs.Mem{
		"/mud/dgdtools.yml": `root: lib
driver_config: etc/mud.dgd
profile_build:
  name: mini
  share: ["std/**", "secure/**"]
  omit: ["secure/private/**"]
  checkouts: ["players/*", "d/*"]
conventions:
  call_registry: {add_action: {function: 0}}
`,
		"/mud/etc/mud.dgd":                "include_file = \"/sys/forced.h\";\ninclude_dirs = ({ \"/std/include\" });\nauto_object = \"/sys/auto\";\ndriver_object = \"/sys/driver\";\ncreate = \"create\";\ntypechecking = 1;\n",
		"/mud/lib/sys/forced.h":           "#define TRUE 1\n#define public\n",
		"/mud/lib/sys/auto.c":             "static string object_name() { return \"x\"; }\nvoid secret_impl() { int body; body = 42; }\n",
		"/mud/lib/std/include/std.h":      "#ifndef STD_H\n#define STD_H\n#include <paths.h>\n#define status int\n#define MAX(a, b) ((a) > (b) ? (a) : (b))\n#endif\n",
		"/mud/lib/std/include/paths.h":    "#define I_ROOM \"/std/room\"\n#define D_TIME \"/secure/timed\"\n#define SECRETD \"/secure/private/vault\"\n",
		"/mud/lib/std/base.c":             "#include <std.h>\nprivate int hidden_state;\nint shared_state;\nstatic string *names, title;\nstatic void create() { hidden_state = 1; }\nvarargs int query(int a, int b) { return a; }\nprivate void internal() { }\nlegacy(x, y) { }\nnomask status *flags(object ob, varargs mixed rest...) { return 0; }\n",
		"/mud/lib/std/room.c":             "#include <std.h>\n#define LOCAL_ONLY 99\ninherit \"/std/base\";\nstatic void create() { ::create(); }\nvoid add_exit(string to, string dir) { /* body with LOCAL_ONLY */ }\nstatic int room_static() { return LOCAL_ONLY; }\n",
		"/mud/lib/secure/timed.c":         "int query_season() { return 1; }\n",
		"/mud/lib/secure/private/vault.c": "string password() { return \"hunter2\"; }\n",
		"/mud/lib/d/Wild/std/mob.c":       "inherit \"/std/base\";\nvoid roar() { }\n",
		"/mud/lib/players/ann/tools.h":    "#define ANN \"/players/ann/\"\n",
		"/mud/lib/players/ann/base.c":     "#include <std.h>\ninherit I_ROOM;\nstatic void create() { ::create(); }\nvoid helper() { }\n",
		"/mud/lib/players/ann/hall.c": `#include <std.h>
#include "tools.h"
inherit ANN + "base";
static void create() {
    status ok;
    ::create();
    add_exit("x", "north", 3);
    this_object()->add_exit("y", "south", 3);
    this_object()->nope();
    D_TIME->query_season();
    D_TIME->no_such();
    I_ROOM->room_static();
    (ANN + "base")->helper();
    (ANN + "gone")->x();
    "/std/missing"->x();
    "/d/Wild/std/mob"->roar();
    "/d/Wild/std/mob"->not_there();
    "/players/bob/thing"->whatever();
    SECRETD->password();
    add_action("typo_fn", "go");
    ok = MAX(1, 2)
    ok = 3;
}
`,
		"/mud/lib/players/bob/thing.c": "void f() { }\n",
	}
}

func findings(t *testing.T, prj *config.Project, fsys vfs.FS, paths ...string) []string {
	t.Helper()
	l, err := lint.New(prj, fsys)
	require.NoError(t, err)
	res, err := l.Run(context.Background(), lint.Request{Paths: paths})
	require.NoError(t, err)
	var out []string
	for _, d := range res.Diagnostics {
		msg := strings.Replace(d.Message, " (nor in this checkout; inheritors elsewhere in the mudlib cannot be seen)", "", 1)
		msg = strings.Replace(msg, " or is private there (a profile lists public functions only)", "", 1)
		out = append(out, fmt.Sprintf("%s:%d %s %s", d.Path[strings.LastIndex(d.Path, "/")+1:], d.Line, d.Rule, msg))
	}
	sort.Strings(out)
	return out
}

func TestProfileBuildAndPartialCheckout(t *testing.T) {
	m := wholeLib()
	full, err := config.Discover(m, []string{"/mud/lib/players/ann"}, config.DiscoverOptions{})
	require.NoError(t, err)
	p, err := profilebuild.Build(context.Background(), full, m)
	require.NoError(t, err)
	l, err := lint.New(full, m)
	require.NoError(t, err)

	// --- what a profile contains
	assert.Equal(t, "mini", p.Name)
	assert.Equal(t, "dgd", p.Dialect.Driver)
	require.NotNil(t, p.Dialect.Typechecking)
	assert.Equal(t, 1, *p.Dialect.Typechecking)
	assert.Equal(t, []string{"/std/include"}, p.IncludeDirs)
	assert.Equal(t, "/sys/forced.h", p.IncludeFile)
	assert.Empty(t, p.Conventions.Create, "the default, create")
	assert.Equal(t, "/sys/driver", p.DriverObject)
	assert.Equal(t, map[string]profile.CallForms{"add_action": {{Function: 0}}}, p.Conventions.CallRegistry)
	assert.Equal(t, []string{"/secure/", "/std/", "/std/include/"}, p.Covered)
	var files []string
	for f := range p.Files {
		files = append(files, f)
	}
	sort.Strings(files)
	assert.Equal(t, []string{
		"/secure/timed.c", "/std/base.c", "/std/include/paths.h", "/std/include/std.h", "/std/room.c",
		"/sys/auto.c", "/sys/forced.h", // not shared, but everything needs them
	}, files, "shared files plus the driver's includes and auto object; no wizard or domain code, nothing omitted")

	room := p.Files["/std/room.c"]
	assert.Equal(t, "#include <std.h>\ninherit \"/std/base\";\nstatic void create() {}\nvoid add_exit(string to, string dir) {}\nstatic int room_static() {}\n", room)
	// Declarations exactly as written, minus bodies: real types,
	// specifiers, parameter names; a typeless function stays typeless; a
	// type macro stays the token that was written.
	assert.Equal(t, "#include <std.h>\nint shared_state;\nstatic string *names, title;\nstatic void create() {}\nvarargs int query(int a, int b) {}\nlegacy(x, y) {}\nnomask status *flags(object ob, varargs mixed rest...) {}\n",
		p.Files["/std/base.c"], "no private functions or variables")
	all := ""
	for _, s := range p.Files {
		all += s
	}
	for _, leak := range []string{"hunter2", "42", "LOCAL_ONLY", "body", "vault.c"} {
		assert.NotContains(t, all, leak, "no bodies, no object-local macros, nothing omitted")
	}
	assert.Contains(t, p.Files["/std/include/std.h"], "#define MAX(a, b) ((a) > (b) ? (a) : (b))")

	// --- declared types are index facts, from real files and stubs alike
	fn := l.Index().Snapshot().Chain("/std/room").Funcs["flags"]
	assert.Equal(t, "nomask status *flags(object ob, varargs mixed rest...)", fn.Decl)
	assert.Equal(t, "status *", fn.Type)
	assert.Equal(t, "/std/base", fn.DefinedIn)
	assert.Equal(t, []string{"object ob", "varargs mixed rest..."}, index.Params(fn.Decl))
	assert.Equal(t, "", l.Index().Snapshot().Chain("/std/base").Funcs["legacy"].Type)
	v, _ := l.Index().Snapshot().Unit("/std/base").Var("names")
	assert.Equal(t, "string *", v.Type)
	v, _ = l.Index().Snapshot().Unit("/std/base").Var("title")
	assert.Equal(t, "string", v.Type)

	// --- the file format: deterministic, round-trips
	data, err := p.Marshal()
	require.NoError(t, err)
	again, err := p.Marshal()
	require.NoError(t, err)
	assert.Equal(t, data, again)
	loaded, err := profile.Parse(data)
	require.NoError(t, err)
	assert.Equal(t, p.Manifest, loaded.Manifest)
	assert.Equal(t, p.Files, loaded.Files)

	// --- whole lib vs. the wizard's folder alone with the profile
	want := findings(t, full, m, "/mud/lib/players/ann")

	alone := vfs.Mem{
		"/home/ann/players/ann/dgdtools.yml": "profile: ../../mini.profile\n",
		"/home/ann/mini.profile":             string(data),
	}
	for path, src := range m {
		if rest, ok := strings.CutPrefix(path, "/mud/lib/players/ann/"); ok {
			alone["/home/ann/players/ann/"+rest] = src
		}
	}
	part, err := config.Discover(alone, []string{"/home/ann/players/ann/hall.c"}, config.DiscoverOptions{})
	require.NoError(t, err)
	assert.True(t, part.Partial)
	assert.Equal(t, "/players/ann", part.CheckoutLib, "read off the folder's path")
	assert.Equal(t, "/home/ann/players/ann", part.CheckoutDir)
	got := findings(t, part, alone) // no paths: the checkout
	pl, err := lint.New(part, alone)
	require.NoError(t, err)
	assert.Equal(t, fn.Decl, pl.Index().Snapshot().Chain("/players/ann/hall").Funcs["flags"].Decl, "the same declaration, read from the stub")
	assert.Equal(t, "status *", pl.Index().Snapshot().Chain("/players/ann/hall").Funcs["flags"].Type)

	// Everything that is about the wizard's own files or shared code is
	// found identically; what points elsewhere in the lib is unknown.
	var expect []string
	for _, w := range want {
		if strings.Contains(w, "/d/Wild/") || strings.Contains(w, "/players/bob/") {
			continue // trees the wizard does not have: unknown, silent
		}
		expect = append(expect, w)
	}
	assert.Equal(t, expect, got)
	assert.Len(t, want, len(got)+2)
	joined := strings.Join(got, "\n")
	for _, must := range []string{
		"hall.c:8 callback-arity",                     // signature from the profile
		"hall.c:9 callable-not-found function 'nope'", // own chain through profile stubs
		"hall.c:11 callable-not-found function 'no_such' is not defined in /secure/timed",
		"hall.c:12 static-crossobj",
		"hall.c:14 target-object-missing object /players/ann/gone", // own tree: covered
		"hall.c:15 target-object-missing object /std/missing",      // profile-covered dir
		"hall.c:20 callable-not-found function 'typo_fn'",          // registry from the profile
		"hall.c:21 syntax-error missing ';'",                       // macros from profile headers
	} {
		assert.Contains(t, joined, must)
	}
	for _, never := range []string{"bob", "vault", "hall.c:18", "hall.c:19", "include-not-found", "hall.c:5"} {
		assert.NotContains(t, joined, never)
	}

	// --- explicit lib_path when the folder's path says nothing
	loose := vfs.Mem{"/work/stuff/dgdtools.yml": "profile: /work/mini.profile\nlib_path: /players/ann\n", "/work/mini.profile": string(data)}
	nolib := vfs.Mem{"/work/stuff/dgdtools.yml": "profile: /work/mini.profile\n", "/work/mini.profile": string(data)}
	for path, src := range alone {
		if rest, ok := strings.CutPrefix(path, "/home/ann/players/ann/"); ok && rest != "dgdtools.yml" {
			loose["/work/stuff/"+rest] = src
			nolib["/work/stuff/"+rest] = src
		}
	}
	prj, err := config.Discover(loose, []string{"/work/stuff"}, config.DiscoverOptions{})
	require.NoError(t, err)
	assert.Equal(t, got, findings(t, prj, loose))

	// Without it: no claims between the wizard's own files, but the
	// profile's macros still make the syntax check exact.
	prj, err = config.Discover(nolib, []string{"/work/stuff"}, config.DiscoverOptions{})
	require.NoError(t, err)
	assert.Empty(t, prj.CheckoutLib)
	require.NotEmpty(t, prj.Notes)
	assert.Equal(t, diag.NoteUnplacedCheckout, prj.Notes[0].Kind)
	assert.Contains(t, prj.Notes[0].Message, "lib_path")
	assert.Equal(t, []string{"hall.c:21 syntax-error missing ';' before 'ok'"}, findings(t, prj, nolib))

	// --- real files win over stubs; a whole lib with a profile is whole
	m["/mud/mini.profile"] = string(data)
	m["/mud/dgdtools.yml"] = "root: lib\nprofile: mini.profile\n"
	m["/mud/lib/secure/timed.c"] = "int query_season() { return 1; }\nint no_such() { return 2; }\n"
	prj, err = config.Discover(m, []string{"/mud/lib/players/ann"}, config.DiscoverOptions{})
	require.NoError(t, err)
	assert.False(t, prj.Partial)
	assert.Equal(t, "/mud/lib", prj.Root)
	now := strings.Join(findings(t, prj, m, "/mud/lib/players/ann"), "\n")
	assert.NotContains(t, now, "no_such", "the file on disk defines it; the stale stub does not matter")
	assert.Contains(t, now, "hall.c:17 callable-not-found", "and the rest of the lib is visible again")
}

// A profile named in a config is found next to the config, then in
// $DGDTOOLS_PROFILE_PATH, then in the user config directory; nothing is
// built in.
func TestProfileResolution(t *testing.T) {
	m := wholeLib()
	full, err := config.Discover(m, []string{"/mud/lib"}, config.DiscoverOptions{})
	require.NoError(t, err)
	p, err := profilebuild.Build(context.Background(), full, m)
	require.NoError(t, err)
	data, err := p.Marshal()
	require.NoError(t, err)

	// What a front end reads from its environment and hands over.
	dirs := profile.UserDirs("/opt/a"+string(os.PathListSeparator)+"/opt/b", "/home/zed/Library/Application Support", "/home/zed")
	opt := config.DiscoverOptions{ProfileDirs: dirs}

	room := "#include <std.h>\ninherit I_ROOM;\nstatic void create() {\n ::create();\n this_object()->no_such_function_anywhere();\n}\n"
	wiz := vfs.Mem{
		"/home/zed/players/zed/dgdtools.yml": "profile: mini\n",
		"/home/zed/players/zed/room.c":       room,
	}
	// Not installed anywhere: a clear error naming every place searched.
	_, err = config.Discover(wiz, []string{"/home/zed/players/zed"}, opt)
	require.Error(t, err)
	for _, want := range []string{`profile "mini" not found`, "/home/zed/players/zed", "/opt/a", "/opt/b", "/home/zed/.config/dgdtools/profiles", profile.PathEnv} {
		assert.Contains(t, err.Error(), want)
	}

	// Each location works, in order of precedence.
	other := *p
	other.Name = "from-user-dir"
	otherData, err := other.Marshal()
	require.NoError(t, err)
	wiz["/home/zed/.config/dgdtools/profiles/mini.profile"] = string(otherData)
	prj, err := config.Discover(wiz, []string{"/home/zed/players/zed"}, opt)
	require.NoError(t, err)
	assert.Equal(t, "from-user-dir", prj.Profile.Name)
	assert.Equal(t, "/home/zed/.config/dgdtools/profiles/mini.profile", prj.Profile.Path)

	wiz["/opt/b/mini.profile"] = string(data)
	prj, err = config.Discover(wiz, []string{"/home/zed/players/zed"}, opt)
	require.NoError(t, err)
	assert.Equal(t, "/opt/b/mini.profile", prj.Profile.Path, "the environment path beats the user directory")

	wiz["/home/zed/players/zed/mini.profile"] = string(data)
	prj, err = config.Discover(wiz, []string{"/home/zed/players/zed/room.c"}, opt)
	require.NoError(t, err)
	assert.Equal(t, "/home/zed/players/zed/mini.profile", prj.Profile.Path, "next to the config beats everything")
	assert.Equal(t, "/players/zed", prj.CheckoutLib)
	got := findings(t, prj, wiz, "/home/zed/players/zed/room.c")
	require.Len(t, got, 1, "%v", got)
	assert.Contains(t, got[0], "room.c:5 callable-not-found function 'no_such_function_anywhere'")

	// By flag, with no config file at all.
	delete(wiz, "/home/zed/players/zed/dgdtools.yml")
	prj, err = config.Discover(wiz, []string{"/home/zed/players/zed/room.c"}, config.DiscoverOptions{Profile: "mini", WorkDir: "/home/zed", ProfileDirs: dirs})
	require.NoError(t, err)
	assert.Equal(t, "/players/zed", prj.CheckoutLib)
}

// Declarations that come from a profile are located by a stub URI, never
// by a path that looks like a file on disk.
func TestStubLocations(t *testing.T) {
	m := wholeLib()
	full, err := config.Discover(m, []string{"/mud/lib"}, config.DiscoverOptions{})
	require.NoError(t, err)
	p, err := profilebuild.Build(context.Background(), full, m)
	require.NoError(t, err)
	data, err := p.Marshal()
	require.NoError(t, err)

	alone := vfs.Mem{
		"/home/ann/players/ann/dgdtools.yml": "profile: ../../mini.profile\n",
		"/home/ann/mini.profile":             string(data),
		"/home/ann/players/ann/x.c":          "inherit \"/std/room\";\nvoid f() { \"/std/room\"->room_static(); }\n",
	}
	prj, err := config.Discover(alone, []string{"/home/ann/players/ann"}, config.DiscoverOptions{})
	require.NoError(t, err)
	assert.Empty(t, prj.Root, "a partial checkout has no lib root on disk")
	assert.True(t, prj.HasLib())
	l, err := lint.New(prj, alone)
	require.NoError(t, err)

	doc, err := l.Lint(context.Background(), "/home/ann/players/ann/x.c", lint.Options{})
	require.NoError(t, err)
	require.Len(t, doc.Diagnostics, 1)
	d := doc.Diagnostics[0]
	assert.Equal(t, "static-crossobj", d.Rule)
	require.Len(t, d.Related, 1)
	assert.Equal(t, "dgdtools-profile://mini/std/room.c", d.Related[0].Path)
	assert.Equal(t, 5, d.Related[0].Line)

	name, libFile, ok := profile.SplitLocation(d.Related[0].Path)
	require.True(t, ok)
	assert.Equal(t, "mini", name)
	assert.Equal(t, p.Files[libFile], prj.Profile.Files["/std/room.c"], "the location names the stub")
}

// What a new object is initialized by is a convention of the lib: a lib
// whose objects define another function than create says so in its
// configuration, and its profile tells the checkouts. The driver config's
// create is what the auto object receives, and is not it.
func TestProfileCarriesTheCreateFunction(t *testing.T) {
	m := vfs.Mem{
		"/mud/dgdtools.yml":        "root: lib\ndriver_config: mud.dgd\nconventions: {create: setup}\ndialect:\n  kfuns: [\"int *rusage()\", \"int strlen(string s)\"]\nprofile_build:\n  name: m\n  share: [\"std/**\"]\n  checkouts: [\"usr/*\"]\n",
		"/mud/mud.dgd":             "auto_object = \"/sys/auto\";\ncreate = \"_F_create\";\n",
		"/mud/lib/sys/auto.c":      "void _F_create() { }\n",
		"/mud/lib/std/thing.c":     "void setup() { }\n",
		"/mud/lib/usr/ann/sword.c": "inherit \"/std/thing\";\nvoid setup() { }\n",
	}
	full, err := config.Discover(m, []string{"/mud/lib"}, config.DiscoverOptions{})
	require.NoError(t, err)
	assert.Equal(t, "setup", full.Config.Create())
	p, err := profilebuild.Build(context.Background(), full, m)
	require.NoError(t, err)
	assert.Equal(t, "setup", p.Conventions.Create)
	data, err := p.Marshal()
	require.NoError(t, err)

	alone := vfs.Mem{
		"/home/ann/usr/ann/dgdtools.yml": "profile: ../../m.profile\n",
		"/home/ann/m.profile":            string(data),
		"/home/ann/usr/ann/sword.c":      m["/mud/lib/usr/ann/sword.c"],
	}
	part, err := config.Discover(alone, []string{"/home/ann/usr/ann"}, config.DiscoverOptions{})
	require.NoError(t, err)
	// So it is with the kfuns of the driver's extension modules; what the
	// checkout's own configuration says of one stands before the
	// profile's word.
	assert.Equal(t, []string{"int *rusage()", "int strlen(string s)"}, p.Dialect.Kfuns)
	kfuns, err := part.Config.Kfuns()
	require.NoError(t, err)
	assert.Equal(t, "int *rusage()", kfuns.Lookup("rusage").Decl)
	assert.Equal(t, "int strlen(string s)", kfuns.Lookup("strlen").Decl)
	alone["/home/ann/usr/ann/dgdtools.yml"] += "dialect:\n  kfuns: [\"int *rusage(varargs int who)\"]\n"
	own, err := config.Discover(alone, []string{"/home/ann/usr/ann"}, config.DiscoverOptions{})
	require.NoError(t, err)
	kfuns, err = own.Config.Kfuns()
	require.NoError(t, err)
	assert.Equal(t, "int *rusage(varargs int who)", kfuns.Lookup("rusage").Decl)
	assert.Equal(t, "int strlen(string s)", kfuns.Lookup("strlen").Decl)

	want := []string{"sword.c:2 lifecycle-chain setup() overrides inherited /std/thing::setup() but does not chain it; call ::setup()"}
	assert.Equal(t, want, findings(t, full, m, "/mud/lib/usr/ann"))
	assert.Equal(t, want, findings(t, part, alone))
}

// share_min_owners: files used by that many owners are shared; 0 shares
// only what share: lists (and what every file needs).
func TestShareMinOwners(t *testing.T) {
	build := func(owners string) []string {
		t.Helper()
		m := wholeLib()
		m["/mud/dgdtools.yml"] = "root: lib\ndriver_config: etc/mud.dgd\nprofile_build:\n  name: mini\n  checkouts: [\"players/*\", \"d/*\"]\n" + owners
		prj, err := config.Discover(m, []string{"/mud/lib"}, config.DiscoverOptions{})
		require.NoError(t, err)
		p, err := profilebuild.Build(context.Background(), prj, m)
		require.NoError(t, err)
		var files []string
		for f := range p.Files {
			files = append(files, f)
		}
		sort.Strings(files)
		return files
	}
	needed := []string{"/std/include/paths.h", "/std/include/std.h", "/sys/auto.c", "/sys/forced.h"}
	assert.Equal(t, needed, build("  share_min_owners: 0\n"), "only what every file needs")
	assert.Equal(t, needed, build(""), "the default, 5: nothing here has as many owners")
	assert.Contains(t, build("  share_min_owners: 1\n"), "/std/base.c", "what an owner uses")
}
