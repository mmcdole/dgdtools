//go:build corpus

package tokcmp

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mmcdole/dgdtools/pkg/lexer"
)

// TestCorpusSelfCompare verifies Compare(x, x) over every corpus file.
func TestCorpusSelfCompare(t *testing.T) {
	root := os.Getenv("LPC_CORPUS")
	if root == "" {
		t.Skip("LPC_CORPUS not set")
	}

	paths := make(chan string, 1024)
	var files, failures atomic.Int64
	var mu sync.Mutex
	var sample []string

	var wg sync.WaitGroup
	for range runtime.NumCPU() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range paths {
				src, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				files.Add(1)
				a := lexer.Lex(path, src, lexer.Default)
				b := lexer.Lex(path, src, lexer.Default)
				if eq, div := Compare(a, b); !eq {
					failures.Add(1)
					mu.Lock()
					if len(sample) < 20 {
						sample = append(sample, path+": "+div.APos.String())
					}
					mu.Unlock()
				}
			}
		}()
	}

	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if ext := filepath.Ext(path); ext == ".c" || ext == ".h" {
			paths <- path
		}
		return nil
	})
	close(paths)
	wg.Wait()

	t.Logf("corpus: %d files self-compared, %d failures", files.Load(), failures.Load())
	for _, s := range sample {
		t.Errorf("  FAIL: %s", s)
	}
}
