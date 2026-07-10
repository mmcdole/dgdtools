package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// relIfUnder returns path relative to root, or an error if path is not
// under root.
func relIfUnder(root, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("%s not under %s", path, root)
	}
	return filepath.ToSlash(rel), nil
}
