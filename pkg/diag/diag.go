// Package diag defines the diagnostics of the linter and the text edits
// the linter and the formatter express changes in.
package diag

import (
	"fmt"
	"unicode/utf8"
)

type Severity uint8

const (
	Info Severity = iota
	Warning
	Error
)

func (s Severity) String() string {
	switch s {
	case Info:
		return "info"
	case Warning:
		return "warning"
	case Error:
		return "error"
	}
	return "unknown"
}

// ParseSeverity maps a severity's spelling (info, warning, error) to the
// Severity.
func ParseSeverity(s string) (Severity, bool) {
	switch s {
	case "info":
		return Info, true
	case "warning":
		return Warning, true
	case "error":
		return Error, true
	}
	return 0, false
}

// Diagnostic is one finding of a lint rule, tied to a source range.
//
// Line and Col are 1-based; Col counts bytes. Start and End are byte
// offsets into the file (End exclusive); use UTF16Col to derive the
// columns an LSP client expects.
type Diagnostic struct {
	Path     string
	Line     int
	Col      int
	EndLine  int
	EndCol   int
	Start    uint32
	End      uint32
	Severity Severity
	Message  string
	Rule     string // stable rule name, e.g. "syntax-error"
	Code     string // stable short code, e.g. "E001"
	// Tags say what kind of thing the finding is about, for a client
	// that shows such code differently.
	Tags    []Tag
	Related []Related
	Fixes   []Fix
	// Fingerprint identifies the finding by the text of the source line
	// it is on rather than by its position, so that it survives edits
	// elsewhere in the file. Baselines match on it.
	Fingerprint string
}

// Related is a secondary location that explains a diagnostic: a range in
// Path, which may be another file than the diagnostic's. Positions are as
// in Diagnostic: 1-based lines and byte columns, the end exclusive, and
// the same range as byte offsets.
type Related struct {
	Path            string
	Line, Col       int
	EndLine, EndCol int
	Start, End      uint32
	Message         string
}

// Tag classifies what a finding is about. The values are those an editor
// renders specially (LSP DiagnosticTag).
type Tag uint8

const (
	// Unnecessary: code or a comment that does nothing and can go
	// (unreachable code, a comment that suppresses nothing). Editors
	// fade it.
	Unnecessary Tag = iota + 1
	// Deprecated: a use of something that should no longer be used.
	// Editors strike it through.
	Deprecated
)

func (t Tag) String() string {
	switch t {
	case Unnecessary:
		return "unnecessary"
	case Deprecated:
		return "deprecated"
	}
	return "unknown"
}

// Fix is a suggested change, expressed as text edits against the file the
// diagnostic is in. The linter never applies fixes itself.
type Fix struct {
	Title string
	// Applicability says whether the fix can be applied without
	// somebody looking at it.
	Applicability Applicability
	Edits         []TextEdit
}

// Applicability says how far a fix can be trusted. The zero value is the
// careful one.
type Applicability uint8

const (
	// Unsafe: the fix may change what the code means, or may not be what
	// was meant. It is offered, and applied only when asked for by name.
	Unsafe Applicability = iota
	// Safe: the fix keeps what the code means. A tool may apply all safe
	// fixes at once.
	Safe
)

func (a Applicability) String() string {
	if a == Safe {
		return "safe"
	}
	return "unsafe"
}

// TextEdit replaces the bytes [Start, End) with NewText.
type TextEdit struct {
	Start   uint32
	End     uint32
	NewText string
}

// Note is a remark about a run as a whole rather than about a place in a
// file: how the project was resolved, what could not be checked and why.
// Kind identifies the remark to programs; Message is for people.
type Note struct {
	Kind    NoteKind
	Message string
	// Path is the file or directory the note is about, when there is one.
	Path string
}

// NoteKind is the stable identifier of a kind of Note.
type NoteKind string

const (
	// NoteNoLib: no lib is known, so only file-local rules ran.
	NoteNoLib NoteKind = "no-lib"
	// NoteIncompleteLib: the lib root lacks things the configuration
	// names; missing-reference findings are off.
	NoteIncompleteLib NoteKind = "incomplete-lib"
	// NoteOutsideLib: some linted files are not part of the lib; only
	// file-local rules ran on them.
	NoteOutsideLib NoteKind = "outside-lib"
	// NoteProfile: which mudlib profile is in effect.
	NoteProfile NoteKind = "profile"
	// NoteUnplacedCheckout: a partial checkout whose place in the lib is
	// not known; references between its own files are not checked.
	NoteUnplacedCheckout NoteKind = "unplaced-checkout"
	// NoteUnread: a file the project is configured by (a profile, the
	// driver's configuration) could not be read, and a caller that needs
	// the settings only went without it.
	NoteUnread NoteKind = "unread"
	// NoteNestedProject: a directory below the project has a root config
	// of its own and is another project; its files were passed over.
	NoteNestedProject NoteKind = "nested-project"
	// NoteDriverUnset: driver settings that nothing gives; what depends
	// on them is unknown.
	NoteDriverUnset NoteKind = "driver-unset"
	// NoteDriverConfigIgnored: a profile named by the caller gives the
	// driver settings, and the config's driver_config is not read.
	NoteDriverConfigIgnored NoteKind = "driver-config-ignored"
)

// String renders the diagnostic on one line, compiler style.
func (d Diagnostic) String() string {
	return fmt.Sprintf("%s:%d:%d: %s: %s [%s %s]", d.Path, d.Line, d.Col, d.Severity, d.Message, d.Code, d.Rule)
}

// UTF16Col converts a 1-based byte column within line (the bytes of one
// source line) to a 0-based UTF-16 code-unit offset, the unit LSP
// positions use by default. Bytes that are not valid UTF-8 (the corpus
// has Latin-1 files) count as one unit each, which is how editors that
// decode them as single-byte text see them.
func UTF16Col(line []byte, byteCol int) int {
	n := byteCol - 1
	if n > len(line) {
		n = len(line)
	}
	units := 0
	for i := 0; i < n; {
		r, size := utf8.DecodeRune(line[i:])
		if r == utf8.RuneError && size <= 1 {
			units++
			i++
			continue
		}
		if i+size > n { // column points into the middle of a rune
			units++
			break
		}
		if r >= 0x10000 {
			units += 2
		} else {
			units++
		}
		i += size
	}
	return units
}

// ByteCol is the inverse of UTF16Col: it converts a 0-based UTF-16
// code-unit offset within line, as an LSP position gives it, to a 1-based
// byte column. An offset past the end of the line is the end of the line,
// and one inside a character (between the halves of a surrogate pair) is
// the character's start.
func ByteCol(line []byte, utf16Col int) int {
	i := 0
	for units := 0; i < len(line); {
		r, size := utf8.DecodeRune(line[i:])
		n := 1
		if r == utf8.RuneError && size <= 1 {
			size = 1
		} else if r >= 0x10000 {
			n = 2
		}
		if units+n > utf16Col {
			break
		}
		units += n
		i += size
	}
	return i + 1
}
