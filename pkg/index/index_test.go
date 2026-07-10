package index

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/config"
)

func TestInheritRefPreservesLabels(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "std"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "obj"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "std", "base.c"), []byte("int helper() { return 1; }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "obj", "test.c"), []byte(
		"inherit base \"/std/base\";\ninherit missing UNKNOWN;\n"), 0o644))

	cfg := config.Default()
	cfg.Root = root
	ix, err := Build(cfg)
	require.NoError(t, err)
	obj := ix.Objects["/obj/test"]
	require.NotNil(t, obj)
	require.Len(t, obj.Inherits, 2)
	assert.Equal(t, "base", obj.Inherits[0].Label)
	assert.True(t, obj.Inherits[0].Resolved)
	assert.Equal(t, "missing", obj.Inherits[1].Label)
	assert.False(t, obj.Inherits[1].Resolved)
}
