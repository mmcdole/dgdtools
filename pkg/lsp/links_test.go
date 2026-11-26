package lsp_test

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/lsp"
	"github.com/mmcdole/dgdtools/pkg/lsp/lsptest"
)

// linkedLib is a lib with a wizard's directory linked into it from
// outside: dir/mud/lib/players/knubo is dir/outside/knubo. It returns
// dir.
func linkedLib(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("making a link needs a privilege on Windows")
	}
	dir := tree(t, map[string]string{
		"mud/dgdtools.yml":         "root: lib\n",
		"mud/lib/std/item.c":       "void create() { }\nint weight() { return 1; }\n",
		"mud/lib/obj/user.c":       "void f() { \"/players/knubo/raven\"->caw(); }\n",
		"outside/knubo/raven.c":    "inherit \"/std/item\";\ninherit \"/players/knubo/base\";\n\nvoid create() { }\nint caw() { return weight(); }\n",
		"outside/knubo/sub/crow.c": "inherit \"/players/knubo/raven\";\nint again() { return caw(); }\n",
	})
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "mud/lib/players"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(dir, "outside/knubo"), filepath.Join(dir, "mud/lib/players/knubo")))
	return dir
}

// An editor that resolves links (Eglot) names a file of a linked
// directory by its real path. The server knows it by its path in the
// lib, as the driver does: its project is the lib's, and what it says of
// the file it says under the URI the editor used, while files that are
// not open are named by their paths in the lib.
func TestLinkedDirectoryByRealPath(t *testing.T) {
	dir := linkedLib(t)
	lib := filepath.Join(dir, "mud/lib")
	real := filepath.Join(dir, "outside/knubo/raven.c")
	c := start(t)
	c.Initialize(lsptest.Capabilities{}, filepath.Join(dir, "mud"))
	text, err := os.ReadFile(real)
	require.NoError(t, err)
	c.Open(real, string(text))

	// The lib's config: the inherits resolve, so the lib's rules run.
	found := published(t, c, real)
	assert.Equal(t, []string{"target-object-missing", "lifecycle-chain"}, rules(found), "%q", found)

	// Into /std.
	off := strings.Index(string(text), "weight()")
	links := definition(t, c, real, off)
	require.Len(t, links, 1)
	assert.Equal(t, lsptest.URI(filepath.Join(lib, "std/item.c")), links[0].TargetURI)
	assert.Contains(t, complete(t, c, real, off+len("wei"), ""), `"weight"`)
	assert.Contains(t, complete(t, c, real, strings.Index(string(text), "/players/knubo/")+len("/players/knubo/"), "/"), `"label":"sub"`,
		"the linked directory is a directory of the lib")

	// Its own definition is the open document's; the uses in files that
	// are not open are named by their paths in the lib, the linked
	// directory's included.
	off = strings.Index(string(text), "caw()")
	links = definition(t, c, real, off)
	require.Len(t, links, 1)
	assert.Equal(t, lsptest.URI(real), links[0].TargetURI)
	var uris []string
	for _, l := range references(t, c, real, off, true) {
		uris = append(uris, l.URI)
	}
	slices.Sort(uris)
	assert.Equal(t, []string{
		lsptest.URI(filepath.Join(lib, "obj/user.c")),
		lsptest.URI(filepath.Join(lib, "players/knubo/sub/crow.c")),
		lsptest.URI(real),
	}, slices.Compact(uris))

	var edit *lsp.WorkspaceEdit
	require.NoError(t, c.Call("textDocument/rename", lsp.RenameParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: lsptest.URI(real)},
		Position:     lsptest.PositionOf(string(text), off, c.Encoding), NewName: "croak"}, &edit))
	require.NotNil(t, edit)
	versions := map[string]*int{}
	for _, dc := range edit.DocumentChanges {
		versions[dc.TextDocument.URI] = dc.TextDocument.Version
	}
	require.Contains(t, versions, lsptest.URI(real))
	require.NotNil(t, versions[lsptest.URI(real)], "the open document's edits are of its version")
	assert.Contains(t, versions, lsptest.URI(filepath.Join(lib, "players/knubo/sub/crow.c")))
	assert.Contains(t, versions, lsptest.URI(filepath.Join(lib, "obj/user.c")))

	// A change on disk the editor reports by the real path reaches the
	// file of the lib.
	base := filepath.Join(dir, "outside/knubo/base.c")
	require.NoError(t, os.WriteFile(base, []byte("void land() { }\n"), 0o644))
	c.WatchedFiles(1, base)
	assert.Equal(t, []string{"lifecycle-chain"}, rules(published(t, c, real)))

	// A real path no lib links to is the file it is.
	other := filepath.Join(dir, "outside/stray.c")
	c.Open(other, "inherit \"/std/item\";\n")
	assert.Equal(t, []string{}, rules(published(t, c, other)))
}

// An editor that does not resolve links names the file by its path in
// the lib, and hears of it by that path.
func TestLinkedDirectoryByLibPath(t *testing.T) {
	dir := linkedLib(t)
	lib := filepath.Join(dir, "mud/lib")
	path := filepath.Join(lib, "players/knubo/raven.c")
	c := start(t)
	c.Initialize(lsptest.Capabilities{}, filepath.Join(dir, "mud"))
	text, err := os.ReadFile(path)
	require.NoError(t, err)
	c.Open(path, string(text))
	assert.Equal(t, []string{"target-object-missing", "lifecycle-chain"}, rules(published(t, c, path)))
	off := strings.Index(string(text), "caw()")
	var uris []string
	for _, l := range references(t, c, path, off, true) {
		uris = append(uris, l.URI)
	}
	assert.Contains(t, uris, lsptest.URI(path))
	assert.Contains(t, uris, lsptest.URI(filepath.Join(lib, "players/knubo/sub/crow.c")))
}
