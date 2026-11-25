package profile_test

import (
	"bytes"
	"compress/gzip"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// sample is a profile with every part of the manifest set.
func sample() *profile.Profile {
	tc, zero, two := 1, 0, 2
	p := &profile.Profile{Files: map[string]string{
		"/std/room.c":        "inherit \"/std/base\";\nstatic void create() {}\nvoid add_exit(string to, string dir) {}\n",
		"/std/base.c":        "int query(int a) {}\n",
		"/include/std.h":     "#define status int\n#define I_ROOM \"/std/room\"",
		"/sys/auto.c":        "static string object_name() {}\n",
		"/std/unreadable.c":  "#pragma dgdtools partial\n",
		"/std/sep/=== odd.c": "void f() {}\n",
	}}
	p.Name = "mylib"
	p.Dialect = profile.Dialect{
		Language: "lpc", Driver: "dgd", Version: "1.7", SlashSlash: true,
		Typechecking: &tc, Predefined: []string{"__DGD__"},
	}
	p.IncludeDirs, p.IncludeFile, p.AutoObjects = []string{"/include"}, "/include/std.h", []string{"/sys/auto"}
	p.DriverObject = "/sys/driver"
	p.RelativeObjectPaths = "file"
	p.Covered, p.Holes, p.Checkouts = []string{"/include/", "/std/"}, []string{"std/secret/**"}, []string{"usr/*"}
	p.Conventions = profile.Conventions{
		Create:  "setup",
		Macros:  map[string]string{"FOREACH": "statement", "public": "specifier"},
		Defines: []string{"__FEATURE__"},
		CallRegistry: map[string]profile.CallForms{
			"add_action": {{Function: 0}},
			"bind":       {{Object: &zero, Function: 1, Args: &two}, {Function: 0}},
		},
		ObjectRegistry:  map[string]int{"load": 0},
		AutosaveMarkers: []string{"set_save"},
		VirtualPaths:    []string{"virtual/**"},
	}
	return p
}

// The file format: a gzip stream of a header line, the manifest and the
// stubs in path order. The same profile gives the same bytes.
func TestMarshal(t *testing.T) {
	p := sample()
	data, err := p.Marshal()
	require.NoError(t, err)
	again, err := sample().Marshal()
	require.NoError(t, err)
	assert.Equal(t, data, again, "deterministic")

	zr, err := gzip.NewReader(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Empty(t, zr.Name, "no file name in the stream")
	assert.True(t, zr.ModTime.IsZero(), "and no time")
	plain, err := io.ReadAll(zr)
	require.NoError(t, err)
	path := filepath.Join("testdata", "sample.profile.txt")
	if *update {
		require.NoError(t, os.WriteFile(path, plain, 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(plain), "go test ./pkg/profile -update rewrites the golden file")

	// It reads back as what was written, compressed or not.
	for _, form := range [][]byte{data, plain} {
		back, err := profile.Parse(form)
		require.NoError(t, err)
		want := p.Manifest
		want.Format = profile.Format
		assert.Equal(t, want, back.Manifest)
		assert.Equal(t, len(p.Files), len(back.Files))
		assert.Equal(t, p.Files["/std/room.c"], back.Files["/std/room.c"])
		assert.Equal(t, p.Files["/include/std.h"]+"\n", back.Files["/include/std.h"], "a stub ends with a newline")
		assert.Empty(t, back.Path)
	}
}

func TestParseErrors(t *testing.T) {
	for src, want := range map[string]string{
		"":                          "not a dgdtools profile",
		"some-other-file 1\n{}\n":   "not a dgdtools profile",
		"dgdtools-profile 99\n{}\n": "profile format 99 is from a newer dgdtools (this one reads format 2): update dgdtools",
		// A profile of dgdtools 0.3 or older has conventions this one
		// does not read (specifier_macros, format_registry, a top-level
		// create): it is refused, not half understood.
		"dgdtools-profile 1\n{\"name\":\"old\",\"dialect\":{\"driver\":\"dgd\"},\"conventions\":{\"specifier_macros\":[\"public\"]}}\n": "profile format 1 is from an older dgdtools (this one reads format 2): rebuild the profile with dgdtools profile build",
		"dgdtools-profile 2\nnonsense": "profile manifest:",
		"dgdtools-profile 2\n{\"name\":\"x\",\"dialect\":{\"driver\":\"fluffos\"}}\n":                            `profile "x" is for driver "fluffos"; only the dgd dialect is implemented`,
		"dgdtools-profile 2\n{\"dialect\":{\"driver\":\"dgd\"},\"conventions\":{\"call_registry\":{\"f\":0}}}\n": "profile manifest:",
		"\x1f\x8bnot gzip": "gzip",
	} {
		_, err := profile.Parse([]byte(src))
		assert.ErrorContains(t, err, want, "%q", src)
	}
	p, err := profile.Parse([]byte("dgdtools-profile 2\n{\"name\":\"bare\",\"dialect\":{\"driver\":\"dgd\"}}\n"))
	require.NoError(t, err)
	assert.Equal(t, "bare", p.Name)
	assert.Empty(t, p.Files)
}

// A profile is found by name next to the config that names it, then in
// the directories the front end lists; or by path.
func TestResolveAndLoad(t *testing.T) {
	dirs := profile.UserDirs("/opt/a"+string(os.PathListSeparator)+"/opt/b", "/home/zed/Library/Application Support", "/home/zed")
	assert.Equal(t, []string{"/opt/a", "/opt/b", "/home/zed/Library/Application Support/dgdtools/profiles", "/home/zed/.config/dgdtools/profiles"}, dirs)
	assert.Equal(t, []string{"/home/zed/.config/dgdtools/profiles"}, profile.UserDirs("", "/home/zed/.config", "/home/zed"), "one directory named twice is searched once")
	assert.Empty(t, profile.UserDirs("", "", ""))

	data, err := sample().Marshal()
	require.NoError(t, err)
	m := vfs.Mem{
		"/home/zed/.config/dgdtools/profiles/mylib.profile": string(data),
		"/opt/b/mylib.profile":                              string(data),
		"/proj/mylib.profile":                               string(data),
		"/proj/sub/other.profile":                           "dgdtools-profile 7\n{}\n",
		"/proj/mylib.profile.d/x":                           "",
	}
	for _, c := range []struct{ ref, dir, want string }{
		{"mylib", "/proj", "/proj/mylib.profile"},         // next to the config first
		{"mylib", "/elsewhere", "/opt/b/mylib.profile"},   // then the listed directories, in order
		{"mylib.profile", "/proj", "/proj/mylib.profile"}, // a file name is a path
		{"sub/other.profile", "/proj", "/proj/sub/other.profile"},
		{"../proj/mylib.profile", "/opt", "/proj/mylib.profile"},
		{"/opt/b/mylib.profile", "/proj", "/opt/b/mylib.profile"},
	} {
		got, err := profile.Resolve(m, c.ref, c.dir, dirs)
		require.NoError(t, err, c.ref)
		assert.Equal(t, c.want, got, "%s from %s", c.ref, c.dir)
	}
	_, err = profile.Resolve(m, "other", "/proj", dirs)
	require.Error(t, err)
	for _, want := range []string{`profile "other" not found: no other.profile in`, "/proj", "/opt/a", "/opt/b", "/home/zed/.config/dgdtools/profiles", "$DGDTOOLS_PROFILE_PATH"} {
		assert.Contains(t, err.Error(), want)
	}
	_, err = profile.Resolve(m, "/nowhere/x.profile", "/", nil)
	assert.EqualError(t, err, "profile file /nowhere/x.profile does not exist")

	p, err := profile.Load(m, "mylib", "/proj", dirs)
	require.NoError(t, err)
	assert.Equal(t, "/proj/mylib.profile", p.Path)
	assert.Equal(t, "mylib", p.Name)
	_, err = profile.Load(m, "sub/other.profile", "/proj", nil)
	assert.ErrorContains(t, err, "/proj/sub/other.profile: profile format 7 is from a newer dgdtools")
}

// A stub is not a file on disk and is not named like one.
func TestLocations(t *testing.T) {
	assert.Equal(t, "dgdtools-profile://mylib", profile.Location("mylib"))
	for loc, want := range map[string][2]string{
		"dgdtools-profile://mylib/std/room.c": {"mylib", "/std/room.c"},
		"dgdtools-profile://mylib":            {"mylib", "/"},
		"dgdtools-profile://mylib/":           {"mylib", "/"},
	} {
		name, lib, ok := profile.SplitLocation(loc)
		assert.True(t, ok, loc)
		assert.Equal(t, want, [2]string{name, lib}, loc)
	}
	for _, not := range []string{"/std/room.c", "file:///std/room.c", "dgdtools-profile:/x", ""} {
		_, _, ok := profile.SplitLocation(not)
		assert.False(t, ok, not)
	}
}

// FS answers for a profile's stubs under the profile's location and
// leaves every other path to the file system below it.
func TestFS(t *testing.T) {
	base := vfs.Mem{"/home/me/x.c": "void f() { }\n", "/std/room.c": "on disk"}
	f := profile.NewFS(base, sample())
	loc := f.Location()
	assert.Equal(t, "dgdtools-profile://mylib", loc)

	src, err := f.ReadFile(loc + "/std/room.c")
	require.NoError(t, err)
	assert.Equal(t, sample().Files["/std/room.c"], string(src))
	_, err = f.ReadFile(loc + "/std/none.c")
	assert.ErrorIs(t, err, fs.ErrNotExist)
	_, err = f.ReadFile(loc + "/std")
	assert.ErrorIs(t, err, fs.ErrNotExist, "a directory is not a file")

	for p, want := range map[string][2]bool{ // exists, is a directory
		loc + "/std/room.c":                   {true, false},
		loc + "/std":                          {true, true},
		loc + "/std/sep":                      {true, true},
		loc + "/":                             {true, true},
		loc:                                   {true, true},
		loc + "/std/none.c":                   {false, false},
		loc + "/st":                           {false, false},
		loc + "x/std/room.c":                  {false, false}, // another profile's name
		"dgdtools-profile://other/std/room.c": {false, false},
		"/std/room.c":                         {true, false}, // the base's
		"/home/me/x.c":                        {true, false},
		"/home/me":                            {true, true},
		"/home/me/none.c":                     {false, false},
	} {
		assert.Equal(t, want[0], f.Exists(p), "Exists(%s)", p)
		assert.Equal(t, want[1], f.IsDir(p), "IsDir(%s)", p)
	}
	disk, err := f.ReadFile("/std/room.c")
	require.NoError(t, err)
	assert.Equal(t, "on disk", string(disk), "a path on disk is the disk's, whatever the profile has")

	// A walk is over what is on disk: stubs are consulted, not linted.
	var walked []string
	require.NoError(t, f.WalkDir("/", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			walked = append(walked, p)
		}
		return nil
	}))
	assert.ElementsMatch(t, []string{"/home/me/x.c", "/std/room.c"}, walked)
}
