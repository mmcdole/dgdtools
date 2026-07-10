//go:build corpus

package format

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mmcdole/dgdtools/pkg/lexer"
)

// TestCorpusFormatGate formats every cleanly-lexable corpus file and relies
// on Format's internal gate: output token stream must equal the input's and
// the engine must be idempotent. Any gate trip is an engine bug.
func TestCorpusFormatGate(t *testing.T) {
	root := os.Getenv("LPC_CORPUS")
	if root == "" {
		t.Skip("LPC_CORPUS not set")
	}

	paths := make(chan string, 1024)
	var formatted, refused, failures atomic.Int64
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
				func() {
					defer func() {
						if r := recover(); r != nil {
							failures.Add(1)
							mu.Lock()
							if len(sample) < 30 {
								sample = append(sample, fmt.Sprintf("%s: PANIC: %v", path, r))
							}
							mu.Unlock()
						}
					}()
					f := lexer.Lex(path, src, lexer.Default)
					_, err := Format(f, Options{})
					switch {
					case err == nil:
						formatted.Add(1)
					case errors.Is(err, ErrIllegal):
						refused.Add(1)
					default:
						failures.Add(1)
						mu.Lock()
						if len(sample) < 30 {
							sample = append(sample, err.Error())
						}
						mu.Unlock()
					}
				}()
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

	t.Logf("corpus: %d formatted, %d refused (illegal), %d gate failures",
		formatted.Load(), refused.Load(), failures.Load())
	for _, s := range sample {
		t.Errorf("  FAIL: %s", s)
	}
}
