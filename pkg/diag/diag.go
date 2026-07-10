// Package diag defines diagnostics shared by the lexer, formatter, and linter.
package diag

import "fmt"

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

// Diagnostic is one finding tied to a source location. Rule is empty for
// diagnostics that don't come from a named lint rule (e.g. lex errors).
type Diagnostic struct {
	Path     string
	Line     int
	Col      int
	Severity Severity
	Message  string
	Rule     string
}

func (d Diagnostic) String() string {
	if d.Rule != "" {
		return fmt.Sprintf("%s:%d:%d: %s: %s [%s]", d.Path, d.Line, d.Col, d.Severity, d.Message, d.Rule)
	}
	return fmt.Sprintf("%s:%d:%d: %s: %s", d.Path, d.Line, d.Col, d.Severity, d.Message)
}
