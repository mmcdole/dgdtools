package lsp

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/mmcdole/dgdtools/pkg/index"
	"github.com/mmcdole/dgdtools/pkg/lint"
	"github.com/mmcdole/dgdtools/pkg/rename"
)

// willRenameFiles answers workspace/willRenameFiles: before the editor
// renames or moves files and folders, the edits that keep the inherit
// statements, #includes and object paths of the lib naming them
// (rename.Files), as a WorkspaceEdit of the files as they are now. The
// strings that are a moved object's path, where nothing says they are
// one, are edits that need confirmation for a client that can confirm
// edits. What the plan has to tell is shown to the user or logged as for
// a rename (planMessage), and why nothing could be planned is shown; the
// rename itself goes ahead whatever is answered, so a refusal is null and
// a message, not an error.
func (s *server) willRenameFiles(ctx context.Context, p *RenameFilesParams) (*WorkspaceEdit, error) {
	s.mu.Lock()
	sess := s.session
	paths := make([][2]string, len(p.Files))
	for i, f := range p.Files {
		paths[i] = [2]string{s.pathOf(f.OldURI), s.pathOf(f.NewURI)}
	}
	s.mu.Unlock()
	// The moves of each lib.
	var libs []*lint.Linter
	moves := map[*lint.Linter][]rename.Move{}
	for _, fromTo := range paths {
		from, to := fromTo[0], fromTo[1]
		if from == "" || to == "" {
			continue
		}
		l, err := sess.Linter(from)
		if err != nil || l == nil || !l.Project().HasLib() {
			continue
		}
		if _, ok := moves[l]; !ok {
			libs = append(libs, l)
		}
		moves[l] = append(moves[l], rename.Move{From: from, To: to})
	}
	byPath := map[string][]edit{}
	var snaps []*index.Snapshot
	type told struct {
		plan *rename.Plan
		name func(string) string
	}
	var plans []told
	var refusals []string
	var all map[string]ChangeAnnotation
	for _, l := range libs {
		snap := l.Index().Snapshot()
		if !snap.Scanned() {
			scanning, cancel := context.WithTimeout(ctx, scanBudget)
			snap.Scan(scanning)
			cancel()
		}
		plan, err := rename.Files(ctx, snap, moves[l])
		var no *rename.Refusal
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.As(err, &no):
			refusals = append(refusals, "the inherit statements and #includes that name the moved files are not changed: "+no.Reason)
			continue
		case err != nil:
			return nil, err
		case plan == nil:
			continue
		}
		snaps = append(snaps, snap)
		perPlan := map[string][]edit{}
		for _, e := range plan.Edits {
			perPlan[e.Path] = append(perPlan[e.Path], edit{Edit: e})
		}
		annotations := s.annotate(plan, perPlan)
		for k, v := range perPlan {
			byPath[k] = append(byPath[k], v...)
		}
		for k, v := range annotations {
			if all == nil {
				all = map[string]ChangeAnnotation{}
			}
			all[k] = v
		}
		plans = append(plans, told{plan, func(p string) string { return cmp.Or(snap.LibFile(p), p) }})
	}
	var result *WorkspaceEdit
	var notUTF8 []string
	if len(byPath) > 0 {
		var err error
		result, notUTF8, err = s.workspaceEdit(byPath, all, func(path string) ([]byte, error) {
			return snaps[0].ReadFile(path) // every snapshot reads the session's files
		})
		if err != nil {
			return nil, err
		}
		for _, snap := range snaps {
			if !snap.Current() {
				return nil, errContentModified
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, msg := range refusals {
		s.tell(MessageWarning, msg)
	}
	for _, t := range plans {
		var own []string
		for _, p := range notUTF8 {
			if slices.ContainsFunc(t.plan.Edits, func(e rename.Edit) bool { return e.Path == p }) ||
				slices.ContainsFunc(t.plan.PossibleEdits, func(e rename.Edit) bool { return e.Path == p }) {
				own = append(own, p)
			}
		}
		s.planMessage(t.plan, all != nil, t.name, own)
	}
	return result, nil
}
