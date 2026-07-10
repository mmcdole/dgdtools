//go:build corpus

package lexer

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mmcdole/dgdtools/pkg/token"
)

// TestCorpusRoundTrip lexes every .c/.h file under $LPC_CORPUS and verifies
// the lossless invariant: contiguous coverage and byte-exact reassembly.
// Illegal tokens are expected in a real corpus (dead files, non-LPC content)
// and are reported, not failed on; a round-trip violation or panic fails.
func TestCorpusRoundTrip(t *testing.T) {
	root := os.Getenv("LPC_CORPUS")
	if root == "" {
		t.Skip("LPC_CORPUS not set")
	}

	paths := make(chan string, 1024)
	var files, illegalFiles, failures atomic.Int64
	var mu sync.Mutex
	var illegalSample []string
	var failureSample []string

	var wg sync.WaitGroup
	for range runtime.NumCPU() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range paths {
				lexOne(t, path, &files, &illegalFiles, &failures, &mu, &illegalSample, &failureSample)
			}
		}()
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("corpus: %d files lexed, %d with Illegal tokens, %d round-trip failures",
		files.Load(), illegalFiles.Load(), failures.Load())
	for _, s := range illegalSample {
		t.Logf("  illegal: %s", s)
	}
	for _, s := range failureSample {
		t.Errorf("  FAIL: %s", s)
	}
	if failures.Load() > 0 {
		t.Fatalf("%d files failed round-trip", failures.Load())
	}
}

func lexOne(t *testing.T, path string, files, illegalFiles, failures *atomic.Int64,
	mu *sync.Mutex, illegalSample, failureSample *[]string) {

	defer func() {
		if r := recover(); r != nil {
			failures.Add(1)
			mu.Lock()
			*failureSample = append(*failureSample, fmt.Sprintf("%s: PANIC: %v", path, r))
			mu.Unlock()
		}
	}()

	src, err := os.ReadFile(path)
	if err != nil {
		return
	}
	files.Add(1)
	f := Lex(path, src, Default)

	if err := f.CheckRoundTrip(); err != nil {
		failures.Add(1)
		mu.Lock()
		if len(*failureSample) < 50 {
			*failureSample = append(*failureSample, err.Error())
		}
		mu.Unlock()
		return
	}
	// Reassemble and compare bytes — belt and braces over CheckRoundTrip.
	var buf bytes.Buffer
	buf.Grow(len(src))
	for _, tok := range f.Tokens {
		buf.Write(f.Text(tok))
	}
	if !bytes.Equal(buf.Bytes(), src) {
		failures.Add(1)
		mu.Lock()
		if len(*failureSample) < 50 {
			*failureSample = append(*failureSample, path+": reassembly differs")
		}
		mu.Unlock()
		return
	}
	if f.HasIllegal() {
		illegalFiles.Add(1)
		mu.Lock()
		if len(*illegalSample) < 40 {
			var first string
			for _, tok := range f.Tokens {
				if tok.Kind == token.Illegal {
					p := f.Pos(tok.Off)
					txt := string(f.Text(tok))
					if len(txt) > 20 {
						txt = txt[:20] + "..."
					}
					first = fmt.Sprintf("%s:%s %q", path, p, txt)
					break
				}
			}
			if !strings.Contains(strings.Join(*illegalSample, "\n"), filepath.Dir(path)) || len(*illegalSample) < 15 {
				*illegalSample = append(*illegalSample, first)
			}
		}
		mu.Unlock()
	}
}
