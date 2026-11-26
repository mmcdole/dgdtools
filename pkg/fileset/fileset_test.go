package fileset

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/vfs"
)

func TestMatcher(t *testing.T) {
	m := NewMatcher([]string{"attic/**", "**/old/**", "**/*.pre*", "d/*/tmp.c"})
	for rel, want := range map[string]bool{
		"attic/a.c":        true,
		"d/attic/a.c":      false,
		"d/x/old/a.c":      true,
		"old/a.c":          true,
		"d/x/a.preBob.c":   true,
		"d/x/tmp.c":        true,
		"d/x/y/tmp.c":      false,
		"std/room.c":       false,
		"/std/old/room.c/": true,
	} {
		assert.Equal(t, want, m.Match(rel), rel)
	}
	assert.True(t, m.MatchDir("attic"))
	assert.True(t, m.MatchDir("d/x/old"))
	assert.False(t, m.MatchDir("d/x"))
	assert.False(t, NewMatcher(nil).Match("anything"))
}

func TestTreePaths(t *testing.T) {
	whole := Tree{Root: "/mud/lib"}
	rel, ok := whole.Rel("/mud/lib/std/room.c")
	assert.True(t, ok)
	assert.Equal(t, "std/room.c", rel)
	_, ok = whole.Rel("/mud/other/x.c")
	assert.False(t, ok)
	p, ok := whole.Path("/std/room.c")
	assert.True(t, ok)
	assert.Equal(t, "/mud/lib/std/room.c", p)
	assert.False(t, whole.InCheckout("/std/room.c"))

	// A partial checkout: one lib directory, somewhere else on disk.
	part := Tree{CheckoutDir: "/home/ann", CheckoutLib: "/players/ann"}
	rel, ok = part.Rel("/home/ann/rooms/hall.c")
	assert.True(t, ok)
	assert.Equal(t, "players/ann/rooms/hall.c", rel)
	_, ok = part.Rel("/home/bob/x.c")
	assert.False(t, ok)
	p, ok = part.Path("/players/ann/rooms/hall.c")
	assert.True(t, ok)
	assert.Equal(t, "/home/ann/rooms/hall.c", p)
	_, ok = part.Path("/std/room.c")
	assert.False(t, ok, "nothing on disk holds the rest of the lib")
	assert.True(t, part.InCheckout("/players/ann"))
	assert.False(t, part.InCheckout("/players/annabel/x.c"))

	_, ok = Tree{}.Rel("/anywhere/x.c")
	assert.False(t, ok)
}

func TestTreeWalk(t *testing.T) {
	fsys := vfs.Mem{
		"/mud/lib/std/room.c":       "",
		"/mud/lib/std/room.h":       "",
		"/mud/lib/std/notes.txt":    "",
		"/mud/lib/d/x/a.c":          "",
		"/mud/lib/d/x/old/b.c":      "",
		"/mud/lib/d/old/c.c":        "",
		"/mud/lib/attic/d.c":        "",
		"/mud/elsewhere/old/e.c":    "",
		"/home/ann/rooms/hall.c":    "",
		"/home/ann/old/rooms/was.c": "",
	}
	walk := func(tr Tree, dir string, exts []string, exclude ...string) []string {
		var got []string
		require.NoError(t, tr.Walk(context.Background(), fsys, dir, exts, NewMatcher(exclude), func(p string) { got = append(got, p) }))
		return got
	}
	whole := Tree{Root: "/mud/lib"}
	assert.Equal(t, []string{"/mud/lib/d/x/a.c", "/mud/lib/std/room.c"},
		walk(whole, "/mud/lib", []string{".c"}, "attic/**", "**/old/**"))
	assert.Equal(t, []string{"/mud/lib/std/room.c", "/mud/lib/std/room.h"},
		walk(whole, "/mud/lib/std", []string{".c", ".h"}))
	// The globs are lib-relative wherever the walk starts.
	assert.Equal(t, []string{"/mud/lib/d/x/a.c", "/mud/lib/d/x/old/b.c"},
		walk(whole, "/mud/lib/d", []string{".c"}, "d/old/**"))
	assert.Empty(t, walk(whole, "/mud/lib/d/old", []string{".c"}, "d/old/**"),
		"the directory walked is itself excluded file by file")
	// A file outside the lib is never excluded.
	assert.Equal(t, []string{"/mud/elsewhere/old/e.c"}, walk(whole, "/mud/elsewhere", []string{".c"}, "**/old/**"))
	// In a partial checkout the globs see lib paths too.
	part := Tree{CheckoutDir: "/home/ann", CheckoutLib: "/players/ann"}
	assert.Equal(t, []string{"/home/ann/rooms/hall.c"}, walk(part, "/home/ann", []string{".c"}, "players/*/old/**"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, whole.Walk(ctx, fsys, "/mud/lib", []string{".c"}, NewMatcher(nil), func(string) {}), context.Canceled)
}

// linkedTree makes a lib on disk with links in it: to a directory outside
// it, to one inside it, and loops in the outside directory. It returns
// the directory that has mud/lib and outside.
func linkedTree(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("making a link needs a privilege on Windows")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for _, f := range []string{"mud/lib/std/room.c", "mud/lib/d/Viking/hall.c", "outside/knubo/raven.c", "outside/knubo/sub/nest.c"} {
		p := filepath.Join(dir, filepath.FromSlash(f))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, nil, 0o644))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "mud/lib/players"), 0o755))
	for link, target := range map[string]string{
		"mud/lib/players/knubo":   "outside/knubo",    // out of the lib
		"mud/lib/players/viking":  "mud/lib/d/Viking", // into it
		"outside/knubo/sub/back":  "outside/knubo",    // a loop
		"outside/knubo/sub/up":    "mud",              // a loop through the lib
		"mud/lib/players/knubo2":  "outside/knubo",    // the same directory again
		"mud/lib/players/broken":  "outside/nothing",
		"mud/lib/players/raven.c": "outside/knubo/raven.c", // a file
	} {
		require.NoError(t, os.Symlink(filepath.Join(dir, filepath.FromSlash(target)), filepath.Join(dir, filepath.FromSlash(link))))
	}
	return dir
}

func TestTreeWalkLinks(t *testing.T) {
	dir := linkedTree(t)
	lib := filepath.Join(dir, "mud/lib")
	tree := Tree{Root: lib}
	walk := func(from string, exclude ...string) []string {
		var got []string
		err := tree.Walk(context.Background(), vfs.OS{}, filepath.Join(lib, filepath.FromSlash(from)), []string{".c"}, NewMatcher(exclude), func(p string) {
			rel, _ := filepath.Rel(lib, p)
			got = append(got, filepath.ToSlash(rel))
		})
		require.NoError(t, err)
		return got
	}
	// The outside directory is part of the lib, by way of either link;
	// the one into the lib is not walked again, and the loops are not
	// entered.
	assert.Equal(t, []string{
		"d/Viking/hall.c",
		"players/knubo/raven.c",
		"players/knubo/sub/nest.c",
		"players/knubo2/raven.c",
		"players/knubo2/sub/nest.c",
		"players/raven.c",
		"std/room.c",
	}, walk("."))
	assert.Equal(t, []string{"players/raven.c", "std/room.c"}, walk(".", "players/knubo*/**", "d/**"), "exclude takes a linked directory")
	assert.Equal(t, []string{"players/knubo/raven.c", "players/knubo/sub/nest.c"}, walk("players/knubo"), "a link walked is entered")
	assert.Equal(t, []string{"players/viking/hall.c"}, walk("players/viking"), "even one into the lib")

	links, err := tree.Links(context.Background(), vfs.OS{}, NewMatcher(nil))
	require.NoError(t, err)
	knubo := filepath.Join(dir, "outside/knubo")
	assert.Equal(t, Links{{Target: knubo, Dir: filepath.Join(lib, "players/knubo")}}, links, "each directory by its first link")
	p, ok := links.Path(filepath.Join(knubo, "sub/nest.c"))
	assert.True(t, ok)
	assert.Equal(t, filepath.Join(lib, "players/knubo/sub/nest.c"), p)
	_, ok = links.Path(filepath.Join(dir, "outside/other.c"))
	assert.False(t, ok)

	links, err = tree.Links(context.Background(), vfs.OS{}, NewMatcher([]string{"players/knubo/**"}))
	require.NoError(t, err)
	assert.Equal(t, Links{{Target: knubo, Dir: filepath.Join(lib, "players/knubo2")}}, links, "nor by an excluded one")
}
