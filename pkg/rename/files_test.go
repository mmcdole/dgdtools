package rename_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mmcdole/dgdtools/pkg/profile"
	"github.com/mmcdole/dgdtools/pkg/rename"
	"github.com/mmcdole/dgdtools/pkg/vfs"
)

// filesLib is a lib whose files name each other every way there is: an
// inherit statement by its path, with and without ".c", and by a macro;
// object paths handed to functions; #include "..." beside the file and
// from the lib's root, #include <...> from an include directory.
func filesLib() vfs.Mem {
	return vfs.Mem{
		"/lib/dgdtools.yml":           "root: .\ndriver_config: mud.dgd\n",
		"/lib/mud.dgd":                "include_dirs = ({ \"/include\" });\n",
		"/lib/include/std.h":          "#define ROOM \"/std/room\"\n",
		"/lib/include/sub/deep.h":     "#define DEEP 1\n",
		"/lib/std/room.h":             "#define LIGHT 1\n",
		"/lib/std/room.c":             "#include \"room.h\"\nint light;\n",
		"/lib/obj/a.c":                "inherit \"/std/room\";\n",
		"/lib/obj/b.c":                "#include <std.h>\ninherit ROOM;\n",
		"/lib/obj/c.c":                "void f() { clone_object(\"/std/room\"); clone_object(\"/std/room.c\"); }\n",
		"/lib/obj/d.c":                "#include \"/std/room.h\"\n#include <sub/deep.h>\nint x;\n",
		"/lib/obj/e.c":                "inherit \"/obj/a\";\n#include \"../std/room.h\"\n",
		"/lib/obj/f.c":                "#include <std.h>\nvoid f() { \"/std/room\"->g(); call_other(\"/std/room.c\", \"g\"); ROOM->g(); }\n",
		"/lib/obj/g.c":                "void f() { add_exit(\"/std/room\", \"north\"); add_exit(\"std/room.c\", \"up\"); write(\"/std/room is a room\"); }\n",
		"/lib/players/ann/workroom.c": "inherit \"std/room\";\n",
	}
}

// moved plans moving the files (FROM=TO pairs of lib paths) and returns
// the files after the edits, each as "== FILE" and its text, in path
// order; then the possible places and the warnings. Or "refused: REASON".
func (w *world) moved(pairs ...string) string {
	w.t.Helper()
	var moves []rename.Move
	for _, p := range pairs {
		from, to, ok := strings.Cut(p, "=")
		require.True(w.t, ok)
		moves = append(moves, rename.Move{From: "/lib" + from, To: "/lib" + to})
	}
	plan, err := rename.Files(context.Background(), w.l.Index().Snapshot(), moves)
	var no *rename.Refusal
	if errors.As(err, &no) {
		return "refused: " + no.Reason
	}
	require.NoError(w.t, err)
	if plan == nil {
		return "nil"
	}
	byPath := map[string][]rename.Edit{}
	for _, e := range plan.Edits {
		byPath[e.Path] = append(byPath[e.Path], e)
	}
	var b strings.Builder
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		src := w.fs[p]
		es := byPath[p]
		for i := len(es) - 1; i >= 0; i-- {
			e := es[i]
			src = src[:e.Start] + e.NewText + src[e.End:]
		}
		fmt.Fprintf(&b, "== %s\n%s", p, src)
	}
	for _, o := range plan.Occurrences {
		if o.Class != rename.Certain {
			fmt.Fprintf(&b, "%s %s:%d:%d %s\n", o.Class, o.Path, o.Line, o.Col, o.Reason)
		}
	}
	for _, warn := range plan.Warnings {
		fmt.Fprintf(&b, "! %s\n", warn)
	}
	return b.String()
}

func TestFiles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		moves []string
		want  string
	}{{
		name:  "object in its directory",
		moves: []string{"/std/room.c=/std/place.c"},
		want: `== /lib/obj/a.c
inherit "/std/place";
== /lib/obj/c.c
void f() { clone_object("/std/place"); clone_object("/std/place.c"); }
== /lib/obj/f.c
#include <std.h>
void f() { "/std/place"->g(); call_other("/std/place.c", "g"); ROOM->g(); }
== /lib/players/ann/workroom.c
inherit "std/place";
possible /lib/include/std.h:1:15 a string in a directive equal to the old path, not known to be an object path
possible /lib/obj/b.c:2:9 names /std/room.c by the macro ROOM, which other files may use too: not rewritten
possible /lib/obj/f.c:2:62 names /std/room.c by the macro ROOM, which other files may use too: not rewritten
possible /lib/obj/g.c:1:22 a string equal to the old path, not known to be an object path
possible /lib/obj/g.c:1:54 a string equal to the old path, not known to be an object path
! moved base files: /std/room.c (4 objects inherit it): an object keeps the program it was compiled with, so the objects that inherit them must be recompiled for the move to take effect
`,
	}, {
		name:  "object to another directory: its own relative #include",
		moves: []string{"/std/room.c=/domain/room.c"},
		want: `== /lib/obj/a.c
inherit "/domain/room";
== /lib/obj/c.c
void f() { clone_object("/domain/room"); clone_object("/domain/room.c"); }
== /lib/obj/f.c
#include <std.h>
void f() { "/domain/room"->g(); call_other("/domain/room.c", "g"); ROOM->g(); }
== /lib/players/ann/workroom.c
inherit "domain/room";
== /lib/std/room.c
#include "/std/room.h"
int light;
possible /lib/include/std.h:1:15 a string in a directive equal to the old path, not known to be an object path
possible /lib/obj/b.c:2:9 names /std/room.c by the macro ROOM, which other files may use too: not rewritten
possible /lib/obj/f.c:2:62 names /std/room.c by the macro ROOM, which other files may use too: not rewritten
possible /lib/obj/g.c:1:22 a string equal to the old path, not known to be an object path
possible /lib/obj/g.c:1:54 a string equal to the old path, not known to be an object path
! moved base files: /std/room.c (4 objects inherit it): an object keeps the program it was compiled with, so the objects that inherit them must be recompiled for the move to take effect
`,
	}, {
		name:  "header to an include directory",
		moves: []string{"/std/room.h=/include/room.h"},
		want: `== /lib/obj/d.c
#include "/include/room.h"
#include <sub/deep.h>
int x;
== /lib/obj/e.c
inherit "/obj/a";
#include "room.h"
`,
	}, {
		name:  "header in an include directory",
		moves: []string{"/include/sub/deep.h=/include/other/deep.h"},
		want: `== /lib/obj/d.c
#include "/std/room.h"
#include <other/deep.h>
int x;
`,
	}, {
		name:  "a directory",
		moves: []string{"/std=/base"},
		want: `== /lib/obj/a.c
inherit "/base/room";
== /lib/obj/c.c
void f() { clone_object("/base/room"); clone_object("/base/room.c"); }
== /lib/obj/d.c
#include "/base/room.h"
#include <sub/deep.h>
int x;
== /lib/obj/e.c
inherit "/obj/a";
#include "../base/room.h"
== /lib/obj/f.c
#include <std.h>
void f() { "/base/room"->g(); call_other("/base/room.c", "g"); ROOM->g(); }
== /lib/players/ann/workroom.c
inherit "base/room";
possible /lib/include/std.h:1:15 a string in a directive equal to the old path, not known to be an object path
possible /lib/obj/b.c:2:9 names /std/room.c by the macro ROOM, which other files may use too: not rewritten
possible /lib/obj/f.c:2:62 names /std/room.c by the macro ROOM, which other files may use too: not rewritten
possible /lib/obj/g.c:1:22 a string equal to the old path, not known to be an object path
possible /lib/obj/g.c:1:54 a string equal to the old path, not known to be an object path
! moved base files: /std/room.c (4 objects inherit it): an object keeps the program it was compiled with, so the objects that inherit them must be recompiled for the move to take effect
`,
	}, {
		name:  "out of the lib",
		moves: []string{"/obj/a.c=/../a.c"},
		want: `possible /lib/obj/e.c:1:9 names /obj/a.c, which moves out of the mudlib
! moved base files: /obj/a.c (1 object inherits it): an object keeps the program it was compiled with, so the objects that inherit them must be recompiled for the move to take effect
`,
	}, {
		// What a moved file names by a path that does not depend on where
		// it is written (a macro's, a path from the root) names the same
		// file from its new place: nothing to say of it.
		name:  "a file that inherits by a macro",
		moves: []string{"/obj/b.c=/obj/sub/b.c"},
		want:  "",
	}, {
		name:  "not of the lib",
		moves: []string{"/../x.c=/../y.c"},
		want:  "nil",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, filesLib(), "/lib")
			assert.Equal(t, tc.want, w.moved(tc.moves...))
		})
	}
}

// A move in a wizard's checkout: what the moved file names in the
// profile (a stub, by a macro) is said nothing of, and the checkout's
// own files that name it are rewritten.
func TestFilesInCheckout(t *testing.T) {
	p := &profile.Profile{
		Manifest: profile.Manifest{
			Name: "mini", Dialect: profile.Dialect{Driver: "dgd"},
			IncludeDirs: []string{"/include"}, AutoObjects: []string{"/sys/auto"},
			Covered: []string{"/std/", "/include/", "/sys/"},
		},
		Files: map[string]string{
			"/include/std.h": "#define ROOM \"/std/room\"\n",
			"/sys/auto.c":    "void autof();\n",
			"/std/room.c":    "int room_var;\n",
		},
	}
	data, err := p.Marshal()
	require.NoError(t, err)
	fs := vfs.Mem{
		"/home/ann/mini.profile": string(data),
		"/home/ann/dgdtools.yml": "profile: mini.profile\nlib_path: /players/ann\n",
		"/home/ann/obj/a.c":      "#include <std.h>\ninherit ROOM;\ninherit \"/std/room\";\nvoid f() { \"/std/nowhere\"->g(); }\n",
		"/home/ann/key.c":        "void f() { \"/players/ann/obj/a\"->g(); }\n",
	}
	w := newWorld(t, fs, "/home/ann")
	require.True(t, w.l.Scan(context.Background()))
	plan, err := rename.Files(context.Background(), w.l.Index().Snapshot(), []rename.Move{{From: "/home/ann/obj/a.c", To: "/home/ann/obj/b.c"}})
	require.NoError(t, err)
	assert.Equal(t, []rename.Edit{{Path: "/home/ann/key.c", Start: 12, End: 30, NewText: "/players/ann/obj/b"}}, plan.Edits)
	assert.Zero(t, plan.Count(rename.Possible), "%+v", plan.Occurrences)
	assert.Equal(t, []string{"this checkout is part of the mudlib: the places outside it that name the moved files are not changed, and cannot be seen from here"}, plan.Warnings)
}

// A string that is a moved object's path, in a call the index does not
// know takes one, is a possible place, with the edit that names the new
// place in the same form.
func TestFilesStrings(t *testing.T) {
	w := newWorld(t, filesLib(), "/lib")
	require.True(t, w.l.Scan(context.Background()))
	plan, err := rename.Files(context.Background(), w.l.Index().Snapshot(), []rename.Move{{From: "/lib/std/room.c", To: "/lib/std/place.c"}})
	require.NoError(t, err)
	var got []string
	for _, e := range plan.PossibleEdits {
		got = append(got, fmt.Sprintf("%s:%d %s", e.Path, e.Start, e.NewText))
	}
	assert.Equal(t, []string{"/lib/include/std.h:14 /std/place", "/lib/obj/g.c:21 /std/place", "/lib/obj/g.c:53 std/place.c"}, got)
	assert.Equal(t, 5, plan.Count(rename.Possible))
}
