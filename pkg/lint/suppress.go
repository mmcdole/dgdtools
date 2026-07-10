package lint

import (
	"strings"

	"github.com/mmcdole/dgdtools/pkg/token"
)

// suppressions holds the dgdlint:disable directives found in one file.
//
// Supported forms, inside any comment:
//
//	dgdlint:disable rule-a,rule-b     — from this line to EOF (or enable)
//	dgdlint:enable [rule-a,...]       — re-enable (all rules if none named)
//	dgdlint:disable-line [rules]      — this line only (all rules if bare)
//	dgdlint:disable-next-line [rules] — the following line
//
// A bare file-scoped disable (no rule list) is ignored: suppress-all is a
// footgun.
type suppressions struct {
	lines  map[int]map[string]bool // line -> rules ("" = all)
	ranges []supRange
}

type supRange struct {
	rule       string
	start, end int // lines, inclusive; end 0 = EOF
}

func scanSuppressions(f *token.File) *suppressions {
	s := &suppressions{lines: map[int]map[string]bool{}}
	open := map[string]int{} // rule -> start line of an open disable

	for _, t := range f.Tokens {
		if t.Kind != token.LineComment && t.Kind != token.BlockComment {
			continue
		}
		text := string(f.Text(t))
		i := strings.Index(text, "dgdlint:")
		if i < 0 {
			continue
		}
		line := f.Pos(t.Off).Line
		directive := strings.TrimPrefix(text[i:], "dgdlint:")
		directive = strings.TrimSuffix(directive, "*/")
		fields := strings.Fields(directive)
		if len(fields) == 0 {
			continue
		}
		var rules []string
		if len(fields) > 1 {
			for _, r := range strings.Split(fields[1], ",") {
				if r = strings.TrimSpace(r); r != "" {
					rules = append(rules, r)
				}
			}
		}
		switch fields[0] {
		case "disable":
			for _, r := range rules { // bare disable ignored deliberately
				if _, dup := open[r]; !dup {
					open[r] = line
				}
			}
		case "enable":
			if len(rules) == 0 {
				for r, start := range open {
					s.ranges = append(s.ranges, supRange{r, start, line})
					delete(open, r)
				}
			} else {
				for _, r := range rules {
					if start, ok := open[r]; ok {
						s.ranges = append(s.ranges, supRange{r, start, line})
						delete(open, r)
					}
				}
			}
		case "disable-line":
			s.addLine(line, rules)
		case "disable-next-line":
			s.addLine(line+1, rules)
		}
	}
	for r, start := range open {
		s.ranges = append(s.ranges, supRange{r, start, 0})
	}
	return s
}

func (s *suppressions) addLine(line int, rules []string) {
	m := s.lines[line]
	if m == nil {
		m = map[string]bool{}
		s.lines[line] = m
	}
	if len(rules) == 0 {
		m[""] = true
		return
	}
	for _, r := range rules {
		m[r] = true
	}
}

func (s *suppressions) suppressed(rule string, line int) bool {
	if m := s.lines[line]; m != nil && (m[""] || m[rule]) {
		return true
	}
	for _, r := range s.ranges {
		if r.rule == rule && line >= r.start && (r.end == 0 || line <= r.end) {
			return true
		}
	}
	return false
}
