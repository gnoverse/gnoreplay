package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-github/v89/github"
)

type PullRequest struct {
	Number  int
	Base    string // base branch
	HeadSHA string
	Title   string
	Author  string // login
}

// GitHub is what the server needs from the GitHub API: reads only. Results
// are kept on the server, never written back to GitHub.
type GitHub interface {
	BranchHead(ctx context.Context, repo, branch string) (string, error)
	OpenPulls(ctx context.Context, repo string) ([]PullRequest, error)
	Pull(ctx context.Context, repo string, number int) (PullRequest, error)
	// Token authenticates git fetches.
	Token() string
}

// tokenClient reads from GitHub with an access token.
type tokenClient struct {
	c     *github.Client
	token string
}

func newTokenClient(token string) (*tokenClient, error) {
	c, err := github.NewClient(
		github.WithTransport(readOnlyTransport{http.DefaultTransport}),
		github.WithAuthToken(token),
	)
	if err != nil {
		return nil, err
	}
	return &tokenClient{c: c, token: token}, nil
}

// readOnlyTransport refuses any request that is not a read, so the server
// cannot change anything on GitHub whatever the token would allow.
type readOnlyTransport struct{ next http.RoundTripper }

func (t readOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return nil, fmt.Errorf("refusing %s %s: the server only reads from GitHub", req.Method, req.URL.Path)
	}
	return t.next.RoundTrip(req)
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
	return PullRequest{
		Number: pr.GetNumber(), Base: pr.GetBase().GetRef(), HeadSHA: pr.GetHead().GetSHA(),
		Title: pr.GetTitle(), Author: pr.GetUser().GetLogin(),
	}
}

func (t *tokenClient) Token() string { return t.token }
