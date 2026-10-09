package github

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// ghWorkers bounds the gh processes Status runs at once.
const ghWorkers = 4

// Status asks gh for the state of each issue and pull request, in
// parallel: a pull request's state, an issue's state and, with a branch,
// the state of that branch's pull request (as gh reports them: OPEN,
// CLOSED, MERGED). Items gh cannot tell about (no gh, not signed in, not
// found) are left out; the rest keep their order.
func Status(ctx context.Context, r Runner, items []wire.GitHubStatusQuery) ([]wire.GitHubStatusItem, error) {
	for _, q := range items {
		if !validRepo(q.Repo) || q.Number <= 0 || strings.HasPrefix(q.Branch, "-") ||
			q.Kind != wire.GitHubKindIssue && q.Kind != wire.GitHubKindPR {
			return nil, wire.Errorf(wire.ErrBadRequest, "github.status: invalid item %+v", q)
		}
	}
	out := []wire.GitHubStatusItem{}
	g, ok := lookGH(r, "")
	if !ok {
		return out, nil
	}
	found := make([]*wire.GitHubStatusItem, len(items))
	parallel(len(items), ghWorkers, func(i int) {
		q := items[i]
		cmd := "pr"
		if q.Kind == wire.GitHubKindIssue {
			cmd = "issue"
		}
		state := g.state(ctx, cmd, strconv.Itoa(q.Number), q.Repo)
		if state == "" {
			return
		}
		it := &wire.GitHubStatusItem{Repo: q.Repo, Kind: q.Kind, Number: q.Number, State: state}
		if q.Kind == wire.GitHubKindIssue && q.Branch != "" {
			it.Branch = q.Branch
			it.PRState = g.state(ctx, "pr", q.Branch, q.Repo)
		}
		found[i] = it
	})
	for _, it := range found {
		if it != nil {
			out = append(out, *it)
		}
	}
	return out, nil
}

// state returns `gh <cmd> view <ref> -R <repo>`'s state, "" when gh fails.
func (g gh) state(ctx context.Context, cmd, ref, repo string) string {
	stdout, _, err := g.run(ctx, cmd, "view", ref, "-R", repo, "--json", "state")
	var v struct {
		State string `json:"state"`
	}
	if err != nil || json.Unmarshal(stdout, &v) != nil {
		return ""
	}
	return v.State
}
