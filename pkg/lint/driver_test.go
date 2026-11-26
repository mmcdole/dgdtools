package lint_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mmcdole/dgdtools/pkg/config"
	"github.com/mmcdole/dgdtools/pkg/lint"
)

// lintUnset lints obj/t.c of memProject's lib with the driver settings
// unset leaves unset, and says what was found and how many includes
// were counted unknown.
func lintUnset(t *testing.T, src string, unset []string, extra map[string]string) ([]string, int) {
	t.Helper()
	files := map[string]string{"obj/t.c": src}
	for k, v := range extra {
		files[k] = v
	}
	prj, m := memProject(files)
	cfg := prj.Config
	cfg.DriverUnset = unset
	for _, k := range unset {
		switch k {
		case config.KeyIncludeDirs:
			cfg.IncludeDirs = nil
		case config.KeyIncludeFile:
			cfg.IncludeFile = ""
		case config.KeyAutoObject:
			cfg.AutoObjects = nil
		}
	}
	res := run(t, prj, m, lint.Request{Paths: []string{"/lib/obj/t.c"}})
	var out []string
	for _, d := range res.Diagnostics {
		out = append(out, fmt.Sprintf("%s %d", d.Code, d.Line))
	}
	return out, res.Stats.UnknownIncludes
}

// Unset include dirs are unknown: an include that is not found is not
// missing, nor is what it would define. A "..." include not found as
// written is looked for in the include dirs too, as DGD does.
func TestUnsetIncludeDirs(t *testing.T) {
	const src = "#include <nosuch.h>\n#include \"nosuch2.h\"\ninherit FROM_IT;\nFROM_IT_TYPE v;\nvoid f() { a == 1; }\n"
	got, _ := lintUnset(t, src, []string{config.KeyIncludeDirs}, nil)
	assert.Equal(t, []string{"S002 5"}, got)

	// Set, as before; include_dirs: [] says there are none.
	got, _ = lintUnset(t, src, nil, nil)
	assert.Equal(t, []string{"E006 1", "E006 2", "S002 5"}, got)
	prj, m := memProject(map[string]string{"obj/t.c": src})
	prj.Config.IncludeDirs = []string{}
	res := run(t, prj, m, lint.Request{Paths: []string{"/lib/obj/t.c"}})
	assert.Equal(t, []string{"include-not-found t.c:1", "include-not-found t.c:2", "no-effect-statement t.c:5"}, keys(res.Diagnostics))

	// An include that is found is found wherever it is.
	got, _ = lintUnset(t, "#include <std.h>\n#include \"/include/std.h\"\nvoid f() { a == 1; }\n", []string{config.KeyIncludeDirs}, nil)
	assert.Equal(t, []string{"S002 3"}, got)

	// With include dirs set, a "..." include not found as written is
	// found in them.
	got, _ = lintUnset(t, "#include \"std.h\"\nvoid f() { a == 1; }\n", nil, nil)
	assert.Equal(t, []string{"S002 2"}, got)
}

// An unset auto object may have any function: one called by name is not
// missing, nor is a callback by name sure to find a private function
// only. What one file says of itself is checked as before.
func TestUnsetAutoObject(t *testing.T) {
	const src = "private void tick() { }\nvoid f() {\n \"/std/base\"->nofn();\n call_other(\"/std/base\", \"nofn2\");\n call_out(\"nofn3\", 1);\n call_out(\"tick\", 1);\n a == 1;\n}\n"
	got, _ := lintUnset(t, src, []string{config.KeyAutoObject}, nil)
	assert.Equal(t, []string{"S002 7"}, got)

	got, _ = lintUnset(t, src, nil, nil)
	assert.Equal(t, []string{"R001 3", "R001 4", "R001 5", "R007 6", "S002 7"}, got)
}

// An unset force-included header may define any macro: a name written
// as a type or a specifier is not unknown for that alone. Errors no
// macro explains are still errors, and the file has no unknown include.
func TestUnsetIncludeFile(t *testing.T) {
	const src = "TYPE_MACRO y;\nstatic SPEC void g() { }\nvoid f() { int a; a = ; }\nvoid h() { x = 1 }\n"
	got, unknown := lintUnset(t, src, []string{config.KeyIncludeFile}, nil)
	assert.Equal(t, []string{"E001 3", "E001 4"}, got)
	assert.Zero(t, unknown)

	got, _ = lintUnset(t, src, nil, nil)
	assert.Contains(t, got, "E001 1")
}
