package lint

import (
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/diag"
	"github.com/mmcdole/dgdtools/pkg/fileset"
)

// Which rules run on a file, and how, is said in layers: the rule table's
// defaults, the project's lint.rules, its lint.overrides, then the rules
// and overrides of each nested configuration file above the file, from
// the outermost in; the command line's --select, --enable and --ignore
// around them. This file checks the layers and applies them.

// setting is one entry of a rules mapping, checked against the rule
// table.
type setting struct {
	rules []*Rule
	// rank orders the entries of one mapping, least specific first: 0
	// for all, 1 for a group, 2 for one rule.
	rank     int
	off      bool
	severity *diag.Severity // nil: the entry gives none
	options  map[string]any // of the one rule; checked
}

// layer is a rules mapping and the files it is for.
type layer struct {
	// id tells the layers of one linter apart.
	id int
	// match selects the files: by lib-relative name for the project's
	// overrides, by name relative to its directory for a nested
	// configuration file's. nil is every file.
	match    *fileset.Matcher
	settings []setting
}

// newLayer checks a rules mapping: every key names a rule, a rule is
// named once, and options are options of the rule they are given to.
// file and where say where the mapping is, for errors: the configuration
// file ("" for none) and the key in it.
func (l *Linter) newLayer(file, where string, match *fileset.Matcher, m map[string]config.RuleSettings) (*layer, error) {
	lay := &layer{id: int(l.layerIDs.Add(1)), match: match}
	named := map[*Rule]string{}
	// In the order of the file, and of the keys where there is no file.
	keys := slices.Sorted(maps.Keys(m))
	sort.SliceStable(keys, func(i, j int) bool { return m[keys[i]].Line < m[keys[j]].Line })
	for _, key := range keys {
		rs := m[key]
		fail := func(format string, args ...any) error {
			at := where
			if rs.Line > 0 {
				at = "line " + strconv.Itoa(rs.Line) + ": " + where
			}
			if file != "" {
				at = file + ": " + at
			}
			return fmt.Errorf("%s: %s", at, fmt.Sprintf(format, args...))
		}
		rules := l.lookup(key)
		if rules == nil {
			return nil, fail("unknown rule %q (see `dgdtools rules`)", key)
		}
		s := setting{rules: rules, rank: 2}
		switch {
		case key == "all":
			s.rank = 0
		case rules[0].Group == key || len(key) == 1:
			s.rank = 1
		}
		if rs.Severity == config.SeverityOff {
			s.off = true
		} else if sev, ok := diag.ParseSeverity(rs.Severity); ok {
			s.severity = &sev
		}
		switch r := rules[0]; {
		case s.rank == 2 && named[r] != "":
			// A code and a name are two spellings of one key.
			return nil, fail("rule %s is set twice, as %q and as %q", r.Name, named[r], key)
		case s.rank == 2:
			named[r] = key
			if err := r.setOptions(r.newOptions(), rs.Options); err != nil {
				return nil, fail("%v", err)
			}
			s.options = rs.Options
		case len(rs.Options) > 0:
			return nil, fail("%q is a group of rules, and %q is not a setting of a group: an option belongs to the rule that has it",
				key, slices.Sorted(maps.Keys(rs.Options))[0])
		}
		lay.settings = append(lay.settings, s)
	}
	sort.SliceStable(lay.settings, func(i, j int) bool { return lay.settings[i].rank < lay.settings[j].rank })
	return lay, nil
}

// newOptions returns the rule's options at their defaults, nil when the
// rule has none.
func (r *Rule) newOptions() any {
	if r.Options == nil {
		return nil
	}
	return r.Options()
}

// setOptions sets the options given by key in dst, which newOptions
// returned. It fails on a key the rule does not have and on a value the
// option does not take.
func (r *Rule) setOptions(dst any, opts map[string]any) error {
	if len(opts) == 0 {
		return nil
	}
	fields := map[string]reflect.Value{}
	if dst != nil {
		v := reflect.ValueOf(dst).Elem()
		for i := range v.NumField() {
			fields[v.Type().Field(i).Tag.Get("yaml")] = v.Field(i)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(opts)) {
		f, ok := fields[key]
		if !ok {
			has := "it has no options"
			if len(fields) > 0 {
				has = "its options: " + strings.Join(slices.Sorted(maps.Keys(fields)), ", ")
			}
			return fmt.Errorf("rule %s has no option %q (%s)", r.Name, key, has)
		}
		// The value has the shape YAML gave it; YAML puts it into the
		// option's type, or says why not.
		text, err := yaml.Marshal(opts[key])
		if err == nil {
			fresh := reflect.New(f.Type())
			if err = yaml.Unmarshal(text, fresh.Interface()); err == nil {
				f.Set(fresh.Elem())
			}
		}
		if err != nil {
			return fmt.Errorf("rule %s: option %s: want %s", r.Name, key, typeName(f.Type()))
		}
	}
	return nil
}

// Option describes one option of a rule.
type Option struct {
	// Key is the option's key in the rule's mapping.
	Key string
	// Takes says what values it takes ("a list of strings").
	Takes string
	// Doc says what it is for.
	Doc string
}

// OptionList returns the rule's options, in the order they are declared.
func (r *Rule) OptionList() []Option {
	var out []Option
	if o := r.newOptions(); o != nil {
		t := reflect.TypeOf(o).Elem()
		for i := range t.NumField() {
			f := t.Field(i)
			out = append(out, Option{Key: f.Tag.Get("yaml"), Takes: typeName(f.Type), Doc: f.Tag.Get("doc")})
		}
	}
	return out
}

// typeName says what values an option of type t takes.
func typeName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "true or false"
	case reflect.Int:
		return "a number"
	case reflect.Slice:
		if t.Elem().Kind() == reflect.String {
			return "a list of strings"
		}
	}
	return "a " + t.String()
}

// scopeLayers are the layers of one nested configuration file: its rules,
// then its overrides.
type scopeLayers struct {
	layers []*layer
	err    error
}

// layersOf returns the layers of a nested configuration file, checked
// once.
func (l *Linter) layersOf(sc *config.Scope) ([]*layer, error) {
	if v, ok := l.scopes.Load(sc); ok {
		sl := v.(*scopeLayers)
		return sl.layers, sl.err
	}
	sl := &scopeLayers{}
	lay, err := l.newLayer(sc.Path, "lint.rules", nil, sc.Rules)
	sl.layers, sl.err = append(sl.layers, lay), err
	for i, o := range sc.Overrides {
		if sl.err != nil {
			break
		}
		lay, sl.err = l.newLayer(sc.Path, fmt.Sprintf("lint.overrides[%d].rules", i), fileset.NewMatcher(o.Paths), o.Rules)
		sl.layers = append(sl.layers, lay)
	}
	v, _ := l.scopes.LoadOrStore(sc, sl)
	sl = v.(*scopeLayers)
	return sl.layers, sl.err
}

// active is one rule as it runs on a file.
type active struct {
	rule     *Rule
	severity diag.Severity
	options  any // what rule.Options returned, with the file's settings
}

// selection is the rules of one lint call: for each file, the configured
// rules as the call's Options adjust them. Files that the same layers
// apply to share one answer.
type selection struct {
	l   *Linter
	opt *Options
	// picked are the rules --select and --enable name.
	picked map[*Rule]bool

	mu   sync.Mutex
	sets map[string][]active
}

// selection checks the rule names of the options.
func (l *Linter) selection(opt *Options) (*selection, error) {
	s := &selection{l: l, opt: opt, picked: map[*Rule]bool{}, sets: map[string][]active{}}
	for _, names := range [][]string{opt.Select, opt.Enable, opt.Disable} {
		for _, n := range names {
			if n = strings.TrimSpace(n); n != "" && l.lookup(n) == nil {
				return nil, fmt.Errorf("unknown rule %q (see `dgdtools rules`)", n)
			}
		}
	}
	for _, names := range [][]string{opt.Select, opt.Enable} {
		for _, n := range names {
			for _, r := range l.lookup(strings.TrimSpace(n)) {
				s.picked[r] = true
			}
		}
	}
	return s, nil
}

// rules returns the rules that run on a file, in the order of the rule
// table. rel is the file's lib-relative name ("" when it is outside the
// lib) and set its settings (nil: the project's own). It fails when a
// nested configuration file that applies to the file is not valid.
func (s *selection) rules(file, rel string, set *config.Settings) ([]active, error) {
	l := s.l
	layers := []*layer{l.base}
	if rel != "" {
		for _, o := range l.overrides {
			if o.match.Match(rel) {
				layers = append(layers, o)
			}
		}
	}
	if set != nil {
		for _, sc := range set.Scopes {
			ls, err := l.layersOf(sc)
			if err != nil {
				return nil, err
			}
			in, _ := filepath.Rel(sc.Dir, file)
			for _, o := range ls {
				if o.match == nil || o.match.Match(filepath.ToSlash(in)) {
					layers = append(layers, o)
				}
			}
		}
	}
	key := make([]byte, 0, 4*len(layers))
	for _, lay := range layers {
		key = strconv.AppendInt(append(key, ' '), int64(lay.id), 10)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rules, ok := s.sets[string(key)]
	if !ok {
		rules = s.resolve(layers)
		s.sets[string(key)] = rules
	}
	return rules, nil
}

// resolve applies the layers, the first of which is the project's
// lint.rules. The command line's --select names the candidates and
// --enable adds to them; both are applied after the project's rules and
// before what is said about paths (overrides and nested files), so a path
// that turns a rule off keeps it off; --ignore has the last word.
func (s *selection) resolve(layers []*layer) []active {
	type state struct {
		on bool
		active
	}
	l := s.l
	st := make(map[*Rule]*state, len(l.rules))
	for _, r := range l.rules {
		st[r] = &state{on: r.Default, active: active{rule: r, severity: r.Severity, options: r.newOptions()}}
	}
	apply := func(lay *layer) {
		for _, e := range lay.settings {
			for _, r := range e.rules {
				a := st[r]
				switch {
				case e.off:
					a.on = false
				case e.severity != nil:
					a.on, a.severity = true, *e.severity
				}
				r.setOptions(a.options, e.options) // newLayer has checked them
			}
		}
	}
	apply(layers[0])
	for r := range s.picked {
		st[r].on = true
	}
	for _, lay := range layers[1:] {
		apply(lay)
	}
	if len(s.opt.Select) > 0 {
		for r, a := range st {
			if !s.picked[r] {
				a.on = false
			}
		}
	}
	for _, n := range s.opt.Disable {
		for _, r := range l.lookup(strings.TrimSpace(n)) {
			st[r].on = false
		}
	}
	var out []active
	for _, r := range l.rules {
		if a := st[r]; a.on {
			out = append(out, a.active)
		}
	}
	return out
}
