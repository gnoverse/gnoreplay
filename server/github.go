package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-github/v89/github"
)

type PullRequest struct {
	Number  int
	Base    string // base branch
	HeadSHA string
}

type Status struct {
	State       string // "pending", "success", "failure" or "error"
	Description string
	TargetURL   string
}

// GitHub is what the server needs from the GitHub API.
type GitHub interface {
	BranchHead(ctx context.Context, repo, branch string) (string, error)
	OpenPulls(ctx context.Context, repo string) ([]PullRequest, error)
	Pull(ctx context.Context, repo string, number int) (PullRequest, error)
	SetStatus(ctx context.Context, repo, sha string, st Status) error
	// Token authenticates git fetches.
	Token() string
}

// tokenClient talks to GitHub with a personal access token. Check runs are
// reserved to GitHub Apps, so results are reported as commit statuses.
type tokenClient struct {
	c       *github.Client
	token   string
	context string // status context
}

func newTokenClient(token, statusContext string) (*tokenClient, error) {
	c, err := github.NewClient(github.WithAuthToken(token))
	if err != nil {
		return nil, err
	}
	return &tokenClient{c: c, token: token, context: statusContext}, nil
}

func splitRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return "", "", fmt.Errorf("invalid repo %q", repo)
	}
	return owner, name, nil
}

func (t *tokenClient) BranchHead(ctx context.Context, repo, branch string) (string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	b, _, err := t.c.Repositories.GetBranch(ctx, owner, name, branch, 0)
	if err != nil {
		return "", err
	}
	return b.GetCommit().GetSHA(), nil
}

func (t *tokenClient) OpenPulls(ctx context.Context, repo string) ([]PullRequest, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	opts := &github.PullRequestListOptions{State: "open", ListOptions: github.ListOptions{PerPage: 100}}
	var out []PullRequest
	for {
		prs, res, err := t.c.PullRequests.List(ctx, owner, name, opts)
		if err != nil {
			return nil, err
		}
		for _, pr := range prs {
			out = append(out, toPullRequest(pr))
		}
		if res.NextPage == 0 {
			return out, nil
		}
		opts.Page = res.NextPage
	}
}

func (t *tokenClient) Pull(ctx context.Context, repo string, number int) (PullRequest, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return PullRequest{}, err
	}
	pr, _, err := t.c.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return PullRequest{}, err
	}
	return toPullRequest(pr), nil
}

func toPullRequest(pr *github.PullRequest) PullRequest {
	return PullRequest{Number: pr.GetNumber(), Base: pr.GetBase().GetRef(), HeadSHA: pr.GetHead().GetSHA()}
}

// maxDescriptionLen is GitHub's limit for a commit status description.
const maxDescriptionLen = 140

func (t *tokenClient) SetStatus(ctx context.Context, repo, sha string, st Status) error {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	desc := st.Description
	if r := []rune(desc); len(r) > maxDescriptionLen {
		desc = string(r[:maxDescriptionLen-1]) + "…"
	}
	status := github.RepoStatus{
		State:       github.Ptr(st.State),
		Description: github.Ptr(desc),
		Context:     github.Ptr(t.context),
	}
	if st.TargetURL != "" {
		status.TargetURL = github.Ptr(st.TargetURL)
	}
	_, _, err = t.c.Repositories.CreateStatus(ctx, owner, name, sha, status)
	return err
}

func (t *tokenClient) Token() string { return t.token }
