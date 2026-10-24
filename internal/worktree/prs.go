package worktree

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

// PRs asks gh, in the source checkout, for the pull request of each
// worktree's branch of the repositories with a GitHub remote (`gh pr view
// <branch> --json number,state,url,headRefOid`; a PR task's worktree by
// the task's number, as task.remove asks: gh finds a fork's pull request
// by number only), at most parallel at once. A branch gh has nothing for
// (or gh missing, or failing) is left out. Merged: MERGED with the pull
// request's head the worktree's commit, as task.remove deletes a
// squash-merged branch.
func PRs(ctx context.Context, env []string, inv *Inventory, tasks []wire.Task) []wire.WorktreePR {
	type ref struct {
		repo  string
		w     Worktree
		which string // the branch, or a PR task's number
	}
	var refs []ref
	for _, r := range inv.Repos {
		if !r.GitHub {
			continue
		}
		for _, w := range r.Worktrees {
			if w.Branch == "" {
				continue
			}
			which := w.Branch
			if t := TaskOf(tasks, w.Path); t != nil && t.Kind == wire.TaskKindPR {
				which = strconv.Itoa(t.Number)
			}
			refs = append(refs, ref{r.Path, w, which})
		}
	}
	found := make([]*wire.WorktreePR, len(refs))
	each(len(refs), func(i int) {
		w := refs[i].w
		out, err := task.GH(ctx, env, refs[i].repo, "pr", "view", refs[i].which, "--json", "number,state,url,headRefOid")
		if err != nil {
			return
		}
		var pr struct {
			Number int    `json:"number"`
			State  string `json:"state"`
			URL    string `json:"url"`
			Head   string `json:"headRefOid"`
		}
		if json.Unmarshal(out, &pr) != nil || pr.Number == 0 {
			return
		}
		found[i] = &wire.WorktreePR{
			Path: w.Path, Number: pr.Number, State: pr.State, URL: pr.URL,
			Merged: pr.State == "MERGED" && pr.Head != "" && pr.Head == w.Head,
		}
	})
	prs := []wire.WorktreePR{}
	for _, p := range found {
		if p != nil {
			prs = append(prs, *p)
		}
	}
	return prs
}
