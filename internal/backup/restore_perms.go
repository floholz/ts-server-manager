package backup

import (
	"fmt"
	"sort"

	"github.com/floholz/ts-server-manager/internal/ts"
)

// permBatchSize caps the items per batched permission command.
const permBatchSize = 100

// replacePerms makes the permission set of a group or channel equal to
// want: permissions not in want are removed (new groups and channels come
// with server-assigned defaults), the rest are added or updated. Unknown
// permission names and items the server rejects are skipped and reported.
func (r *restorer) replacePerms(k groupKind, id, label string, want []Perm) error {
	current := map[string]Perm{}
	if !isPlaceholder(id) {
		have, err := readPerms(r.q, ts.NewCommand(k.permList).Arg(k.idKey, id).Option("-permsid"))
		if err != nil {
			return err
		}
		for _, p := range have {
			current[p.PermSID] = p
		}
	}

	wanted := map[string]bool{}
	var set []Perm
	for _, p := range want {
		if !r.t.knownPerms[p.PermSID] {
			r.report.SkippedPerms = append(r.report.SkippedPerms,
				Skipped{Target: label, Name: p.PermSID, Reason: "unknown permission on this server"})
			continue
		}
		wanted[p.PermSID] = true
		if !k.groupPerms {
			p.Negated, p.Skip = false, false
		}
		if cur, ok := current[p.PermSID]; ok && cur == p {
			continue
		}
		set = append(set, p)
	}
	var del []string
	for sid := range current {
		if !wanted[sid] && r.t.knownPerms[sid] {
			del = append(del, sid)
		}
	}
	sort.Strings(del)

	if len(del) > 0 {
		if err := r.permBatches(k, id, label, "remove", del, func(cmd *ts.Command, sid string) {
			cmd.Arg("permsid", sid)
		}); err != nil {
			return err
		}
	}
	if len(set) > 0 {
		sids := make([]string, len(set))
		byID := map[string]Perm{}
		for i, p := range set {
			sids[i] = p.PermSID
			byID[p.PermSID] = p
		}
		if err := r.permBatches(k, id, label, "set", sids, func(cmd *ts.Command, sid string) {
			p := byID[sid]
			cmd.Arg("permsid", p.PermSID).Arg("permvalue", p.Value)
			if k.groupPerms {
				cmd.Arg("permnegated", p.Negated).Arg("permskip", p.Skip)
			}
		}); err != nil {
			return err
		}
	}
	return nil
}

// permBatches sends items in '|'-batched commands. TS6 batches are not
// atomic: on a query error, earlier items may or may not have been applied.
// So a failed batch is retried item by item (adding and removing are
// idempotent) and each item that still fails is reported as skipped.
func (r *restorer) permBatches(k groupKind, id, label, verb string, sids []string, addItem func(*ts.Command, string)) error {
	cmdName := k.permAdd
	if verb == "remove" {
		cmdName = k.permDel
	}
	build := func(items []string) *ts.Command {
		cmd := ts.NewCommand(cmdName).Arg(k.idKey, id)
		for i, sid := range items {
			if i > 0 {
				cmd.Next()
			}
			addItem(cmd, sid)
		}
		return cmd
	}

	for start := 0; start < len(sids); start += permBatchSize {
		end := min(start+permBatchSize, len(sids))
		batch := sids[start:end]
		desc := fmt.Sprintf("%s: %s %d permission%s", label, verb, len(batch), plural(len(batch)))
		_, err := r.write(desc, build(batch))
		if err == nil {
			continue
		}
		if !ts.IsQueryError(err) {
			return err
		}
		for _, sid := range batch {
			if _, err := r.q.Exec(build([]string{sid})); err != nil {
				if !ts.IsQueryError(err) {
					return fmt.Errorf("%s: %s %s: %w", label, verb, sid, err)
				}
				r.report.SkippedPerms = append(r.report.SkippedPerms,
					Skipped{Target: label, Name: sid, Reason: verb + " failed: " + queryMsg(err)})
			}
		}
	}
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
