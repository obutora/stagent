package github_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/obutora/stagent/internal/github"
	"github.com/obutora/stagent/internal/stubcmd"
	"github.com/obutora/stagent/internal/wire"
)

var statusStub = []stubcmd.Rule{
	{Args: "issue view 7 -R o/r --json state", Stdout: `{"state":"CLOSED"}` + "\n"},
	{Args: "pr view 7-fix -R o/r --json state", Stdout: `{"state":"MERGED"}` + "\n"},
	{Args: "issue view 8 -R ghe.example.com/o/r --json state", Stdout: `{"state":"OPEN"}` + "\n"},
	{Args: "pr view 9 -R o/r --json state", Stdout: `{"state":"MERGED"}` + "\n"},
	{Args: "pr view 10 -R o/r --json state", Stdout: `{"state":"CLOSED"}` + "\n"},
	{Args: "*", Stderr: "GraphQL: Could not resolve to an issue or pull request\n", Exit: 1},
}

func TestStatusReportsIssueAndPullStates(t *testing.T) {
	r := runner(t, map[string][]stubcmd.Rule{"gh": statusStub})
	got, err := github.Status(context.Background(), r, []wire.GitHubStatusQuery{
		{Repo: "o/r", Kind: wire.GitHubKindIssue, Number: 7, Branch: "7-fix"},
		{Repo: "ghe.example.com/o/r", Kind: wire.GitHubKindIssue, Number: 8, Branch: "8-no-pull"},
		{Repo: "o/r", Kind: wire.GitHubKindPR, Number: 9},
		{Repo: "o/r", Kind: wire.GitHubKindIssue, Number: 99}, // gh cannot tell
		{Repo: "o/r", Kind: wire.GitHubKindPR, Number: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []wire.GitHubStatusItem{
		{Repo: "o/r", Kind: wire.GitHubKindIssue, Number: 7, Branch: "7-fix", State: "CLOSED", PRState: "MERGED"},
		{Repo: "ghe.example.com/o/r", Kind: wire.GitHubKindIssue, Number: 8, Branch: "8-no-pull", State: "OPEN"},
		{Repo: "o/r", Kind: wire.GitHubKindPR, Number: 9, State: "MERGED"},
		{Repo: "o/r", Kind: wire.GitHubKindPR, Number: 10, State: "CLOSED"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Status = %+v\nwant %+v", got, want)
	}
}

func TestStatusWithoutGhReportsNothing(t *testing.T) {
	got, err := github.Status(context.Background(), runner(t, nil), []wire.GitHubStatusQuery{
		{Repo: "o/r", Kind: wire.GitHubKindPR, Number: 9},
	})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("Status = %#v, %v; want an empty list", got, err)
	}
}

func TestStatusRefusesMalformedItems(t *testing.T) {
	r := runner(t, map[string][]stubcmd.Rule{"gh": statusStub})
	for _, q := range []wire.GitHubStatusQuery{
		{Repo: "o/r", Kind: "commit", Number: 1},
		{Repo: "o/r", Kind: wire.GitHubKindPR, Number: 0},
		{Repo: "--repo", Kind: wire.GitHubKindPR, Number: 1},
		{Repo: "o/r", Kind: wire.GitHubKindIssue, Number: 1, Branch: "-b"},
	} {
		_, err := github.Status(context.Background(), r, []wire.GitHubStatusQuery{q})
		var we *wire.Error
		if !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
			t.Errorf("Status(%+v) error = %v, want bad_request", q, err)
		}
	}
}
