package vfs

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// walk returns what a walk of root visits ("dir/" for directories),
// skipping directories named in skip.
func walk(t *testing.T, fsys FS, root string, skip ...string) []string {
	t.Helper()
	var out []string
	require.NoError(t, fsys.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		rel := strings.TrimPrefix(filepath.ToSlash(p), filepath.ToSlash(root))
		if d.IsDir() {
			for _, s := range skip {
				if rel == s {
					return fs.SkipDir
				}
			}
			out = append(out, rel+"/")
			return nil
		}
		out = append(out, rel)
		return nil
	}))
	return out
}

func TestMem(t *testing.T) {
	m := Mem{"/lib/std/a.c": "a", "/lib/std/sub/b.c": "b", "/lib/x.h": "x"}
	b, err := m.ReadFile("/lib/std/a.c")
	require.NoError(t, err)
	assert.Equal(t, "a", string(b))
	_, err = m.ReadFile("/lib/std/none.c")
	assert.ErrorIs(t, err, fs.ErrNotExist)

	assert.True(t, m.Exists("/lib/x.h"))
	assert.True(t, m.Exists("/lib/std"), "a directory exists")
	assert.False(t, m.Exists("/lib/st"), "a name prefix is not a directory")
	assert.True(t, m.IsDir("/lib/std/"))
	assert.False(t, m.IsDir("/lib/x.h"))

	assert.Equal(t, []string{"/std/a.c", "/std/sub/b.c", "/x.h"}, walk(t, m, "/lib"))
	assert.Equal(t, []string{"/b.c"}, walk(t, m, "/lib/std/sub"))
}

func TestOS(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "std"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "std", "a.c"), []byte("a"), 0o644))
	fsys := OS{}
	b, err := fsys.ReadFile(filepath.Join(dir, "std", "a.c"))
	require.NoError(t, err)
	assert.Equal(t, "a", string(b))
	assert.True(t, fsys.Exists(filepath.Join(dir, "std")))
	assert.True(t, fsys.IsDir(filepath.Join(dir, "std")))
	assert.False(t, fsys.IsDir(filepath.Join(dir, "std", "a.c")))
	assert.False(t, fsys.Exists(filepath.Join(dir, "none")))
	assert.Equal(t, []string{"/", "/std/", "/std/a.c"}, walk(t, fsys, dir))
}

func TestOverlayShadowsTheBase(t *testing.T) {
	o := NewOverlay(Mem{"/lib/std/a.c": "disk"})
	read := func(p string) string {
		b, err := o.ReadFile(p)
		require.NoError(t, err)
		return string(b)
	}
	assert.Equal(t, "disk", read("/lib/std/a.c"))
	o.Set("/lib/std/a.c", []byte("buffer"))
	assert.Equal(t, "buffer", read("/lib/std/../std/a.c"), "paths are cleaned")
	o.Delete("/lib/std/a.c")
	assert.Equal(t, "disk", read("/lib/std/a.c"))

	o.Set("/lib/std/a.c", []byte{})
	assert.Equal(t, "", read("/lib/std/a.c"), "an empty buffer still shadows")
}

func TestOverlayOnlyFiles(t *testing.T) {
	o := NewOverlay(Mem{"/lib/std/a.c": "a", "/lib/attic/old.c": "o"})
	o.Set("/lib/std/new.c", []byte("n"))
	o.Set("/lib/fresh/deep/x.c", []byte("x"))
	o.Set("/lib/attic/unsaved.c", []byte("u"))
	o.Set("/lib/std/a.c", []byte("shadow"))

	assert.True(t, o.Exists("/lib/std/new.c"))
	assert.True(t, o.Exists("/lib/fresh"), "a directory only the overlay has")
	assert.True(t, o.IsDir("/lib/fresh/deep"))
	assert.False(t, o.IsDir("/lib/fresh/deep/x.c"))
	assert.False(t, o.IsDir("/lib/fre"))

	got := walk(t, o, "/lib")
	assert.ElementsMatch(t, []string{
		"/attic/old.c", "/attic/unsaved.c", "/std/a.c", "/std/new.c",
		"/fresh/", "/fresh/deep/", "/fresh/deep/x.c",
	}, got)
	assert.Less(t, indexOf(got, "/fresh/"), indexOf(got, "/fresh/deep/x.c"), "a directory before what it holds")

	got = walk(t, o, "/lib", "/fresh/deep")
	assert.NotContains(t, got, "/fresh/deep/x.c", "a skipped directory is skipped for shadowed files too")
	assert.Contains(t, got, "/fresh/")

	assert.Equal(t, []string{"/", "/deep/", "/deep/x.c"}, walk(t, o, "/lib/fresh"), "a root only the overlay has")

	o.Delete("/lib/fresh/deep/x.c")
	assert.False(t, o.Exists("/lib/fresh"))
	assert.NotContains(t, walk(t, o, "/lib"), "/fresh/")
}

func TestOverlayWalkOverRealDirectories(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"std/a.c", "attic/old.c"} {
		p := filepath.Join(dir, filepath.FromSlash(f))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, nil, 0o644))
	}
	o := NewOverlay(OS{})
	o.Set(filepath.Join(dir, "std", "new.c"), nil)
	o.Set(filepath.Join(dir, "attic", "unsaved.c"), nil)
	o.Set(filepath.Join(dir, "std", "a.c"), nil)

	got := walk(t, o, dir, "/attic")
	assert.ElementsMatch(t, []string{"/", "/std/", "/std/a.c", "/std/new.c"}, got,
		"a real directory the walk skipped hides its shadowed files; a shadow of a real file is visited once")

	stopped := 0
	require.NoError(t, o.WalkDir(dir, func(string, fs.DirEntry, error) error {
		stopped++
		return fs.SkipAll
	}))
	assert.Equal(t, 1, stopped, "SkipAll ends the walk, shadowed files included")
}

func TestOSWalksLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("making a link needs a privilege on Windows")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for _, f := range []string{"lib/std/a.c", "out/b.c"} {
		p := filepath.Join(dir, filepath.FromSlash(f))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, nil, 0o644))
	}
	for link, target := range map[string]string{"lib/out": "out", "out/loop": "out", "out/up": "", "lib/gone": "none", "lib/f.c": "out/b.c"} {
		require.NoError(t, os.Symlink(filepath.Join(dir, target), filepath.Join(dir, filepath.FromSlash(link))))
	}
	lib := filepath.Join(dir, "lib")
	assert.Equal(t, []string{"/", "/f.c", "/gone", "/out/", "/out/b.c", "/std/", "/std/a.c"}, walk(t, OS{}, lib),
		"a link to a directory is entered, a loop is passed over, a link to a file or to nothing is as it was")
	assert.Equal(t, []string{"/", "/b.c"}, walk(t, OS{}, filepath.Join(lib, "out")), "a root that is a link is entered")
	assert.Equal(t, []string{"/", "/f.c", "/gone", "/std/", "/std/a.c"}, walk(t, OS{}, lib, "/out"), "SkipDir skips a link")
	var mode fs.FileMode
	require.NoError(t, OS{}.WalkDir(lib, func(p string, d fs.DirEntry, err error) error {
		if d.Name() == "out" {
			mode = d.Type()
		}
		return nil
	}))
	assert.Equal(t, fs.ModeDir|fs.ModeSymlink, mode)

	real, err := OS{}.RealPath(filepath.Join(lib, "out", "b.c"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "out", "b.c"), real)
	o := NewOverlay(OS{})
	o.Set(filepath.Join(lib, "out", "new.c"), nil)
	real, err = o.RealPath(filepath.Join(lib, "out", "new.c"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "out", "new.c"), real, "a file only the overlay has is resolved as far as the disk goes")
	assert.Contains(t, walk(t, o, lib), "/out/new.c")
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}
