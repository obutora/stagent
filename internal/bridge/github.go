package bridge

import (
	"cmp"
	"context"
	"os"
	"slices"
	"time"

	"github.com/obutora/stagent/internal/github"
	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// githubWait bounds one github.* request (git and gh included).
	githubWait = 60 * time.Second
	// recentConversations is how many conversations count as candidates.
	recentConversations = 50
)

// serveGitHub answers a github.* request (ADR 0007). They are read-only, so
// agent descendants may call them (ADR 0004). Their own queue keeps slow gh
// calls from delaying the daemon, spawn and holder queues.
func (b *Bridge) serveGitHub(m *wire.Msg) {
	ctx, cancel := context.WithTimeout(b.ctx, githubWait)
	defer cancel()
	var res any
	var err error
	switch m.Method {
	case wire.MethodGitHubRepos:
		res = github.Repos(ctx, b.githubRunner(), b.repoCandidates(ctx))
	case wire.MethodGitHubActivity:
		var p wire.GitHubActivityParams
		if err = rpc.Decode(m, &p); err == nil {
			res, err = github.Activity(ctx, b.githubRunner(), p)
		}
	}
	b.replyResult(m.ID, res, err)
}

// githubRunner runs git and gh with the environment sessions start from,
// the tool directories supplementing its PATH as for session.spawn.
func (b *Bridge) githubRunner() github.Runner {
	env := b.sessionEnv()
	for _, name := range []string{"git", "gh"} {
		env = ensureOnPath(env, name, b.l.Home)
	}
	return github.Runner{Env: env}
}

// repoCandidates collects the directories the user worked in: those of
// the processes under their SSH sessions and holders (now), the sessions'
// cwd (last activity) and the cwd of the latest conversations (last
// update). Without a daemon only the processes count.
func (b *Bridge) repoCandidates(ctx context.Context) []github.Candidate {
	var sessions wire.SessionsListResult
	var convs wire.ConversationsListResult
	if dc, err := b.daemonConn(b.dialDaemon); err == nil {
		b.rewatch(dc)
		dc.c.Call(ctx, wire.MethodSessionsList, struct{}{}, &sessions)
		dc.c.Call(ctx, wire.MethodConversationsList, wire.ConversationsListParams{Limit: recentConversations}, &convs)
	}
	var cands []github.Candidate
	var holders []int
	for _, s := range sessions.Sessions {
		if s.State != wire.StateExited && s.HolderPID > 0 {
			holders = append(holders, s.HolderPID)
		}
	}
	now := time.Now().UnixMilli()
	for _, dir := range b.liveDirs(holders) {
		cands = append(cands, github.Candidate{Dir: dir, At: now})
	}
	for _, s := range sessions.Sessions {
		cands = append(cands, github.Candidate{Dir: s.Cwd, At: cmp.Or(s.LastActivityAt, s.StartedAt)})
	}
	// conversations.list limits each harness, not the total.
	slices.SortStableFunc(convs.Conversations, func(a, b wire.Conversation) int { return cmp.Compare(b.UpdatedAt, a.UpdatedAt) })
	for i, c := range convs.Conversations {
		if i == recentConversations {
			break
		}
		cands = append(cands, github.Candidate{Dir: c.Cwd, At: c.UpdatedAt})
	}
	return cands
}

// liveDirs is the default Bridge.liveDirs: the working directories of the
// user's processes under an SSH session or a holder.
func liveDirs(holders []int) []string {
	s, err := ptable.Take()
	if err != nil {
		return nil
	}
	owner, err := ptable.CurrentOwner()
	if err != nil {
		return nil
	}
	return github.LiveDirs(s, owner, holders, os.Getpid())
}
