VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build test corpus clean

build:
	go build $(LDFLAGS) -o bin/dgdfmt ./cmd/dgdfmt
	go build $(LDFLAGS) -o bin/dgdlint ./cmd/dgdlint
	go build $(LDFLAGS) -o bin/dgdcmp ./cmd/dgdcmp

test:
	go test ./...

# Full-corpus invariants: lex round-trip, comparator self-compare, and the
# formatter gate over every .c/.h under LPC_CORPUS.
corpus:
	@test -n "$(LPC_CORPUS)" || (echo "set LPC_CORPUS=/path/to/lpc/tree" && exit 2)
	LPC_CORPUS=$(LPC_CORPUS) go test -tags corpus -v -run 'TestCorpus' ./...

clean:
	rm -rf bin
