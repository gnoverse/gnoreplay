package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v89/github"
)

type checkUpdate struct {
	Status     string // "queued", "in_progress" or "completed"
	Conclusion string // with "completed": "success", "neutral", "cancelled", ...
	Title      string
	Summary    string
	Text       string
	DetailsURL string
}

// GitHub is what the server needs from the GitHub API, as the app
// installation of the repo concerned.
type GitHub interface {
	CreateCheck(ctx context.Context, installation int64, repo, sha string, u checkUpdate) (int64, error)
	UpdateCheck(ctx context.Context, installation int64, repo string, checkRunID int64, u checkUpdate) error
	// Token is a short-lived installation token, used to fetch private repos.
	Token(ctx context.Context, installation int64) (string, error)
}

type ghApp struct {
	appID     int64
	key       []byte
	checkName string

	mu         sync.Mutex
	transports map[int64]*ghinstallation.Transport
}

func newGHApp(appID int64, key []byte, checkName string) *ghApp {
	return &ghApp{appID: appID, key: key, checkName: checkName, transports: map[int64]*ghinstallation.Transport{}}
}

func (g *ghApp) transport(installation int64) (*ghinstallation.Transport, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if tr, ok := g.transports[installation]; ok {
		return tr, nil
	}
	tr, err := ghinstallation.New(http.DefaultTransport, g.appID, installation, g.key)
	if err != nil {
		return nil, err
	}
	g.transports[installation] = tr
	return tr, nil
}

func (g *ghApp) client(installation int64) (*github.Client, error) {
	tr, err := g.transport(installation)
	if err != nil {
		return nil, err
	}
	return github.NewClient(github.WithTransport(tr))
}

func splitRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return "", "", fmt.Errorf("invalid repo %q", repo)
	}
	return owner, name, nil
}

func (u checkUpdate) output() *github.CheckRunOutput {
	if u.Title == "" {
		return nil
	}
	return &github.CheckRunOutput{
		Title:   github.Ptr(u.Title),
		Summary: github.Ptr(u.Summary),
		Text:    optional(u.Text),
	}
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func completedAt(u checkUpdate) *github.Timestamp {
	if u.Status != "completed" {
		return nil
	}
	return &github.Timestamp{Time: time.Now()}
}

func (g *ghApp) CreateCheck(ctx context.Context, installation int64, repo, sha string, u checkUpdate) (int64, error) {
	c, err := g.client(installation)
	if err != nil {
		return 0, err
	}
	owner, name, err := splitRepo(repo)
	if err != nil {
		return 0, err
	}
	run, _, err := c.Checks.CreateCheckRun(ctx, owner, name, github.CreateCheckRunOptions{
		Name:        g.checkName,
		HeadSHA:     sha,
		Status:      optional(u.Status),
		Conclusion:  optional(u.Conclusion),
		CompletedAt: completedAt(u),
		DetailsURL:  optional(u.DetailsURL),
		Output:      u.output(),
	})
	if err != nil {
		return 0, err
	}
	return run.GetID(), nil
}

func (g *ghApp) UpdateCheck(ctx context.Context, installation int64, repo string, checkRunID int64, u checkUpdate) error {
	c, err := g.client(installation)
	if err != nil {
		return err
	}
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	_, _, err = c.Checks.UpdateCheckRun(ctx, owner, name, checkRunID, github.UpdateCheckRunOptions{
		Name:        g.checkName,
		Status:      optional(u.Status),
		Conclusion:  optional(u.Conclusion),
		CompletedAt: completedAt(u),
		DetailsURL:  optional(u.DetailsURL),
		Output:      u.output(),
	})
	return err
}

func (g *ghApp) Token(ctx context.Context, installation int64) (string, error) {
	tr, err := g.transport(installation)
	if err != nil {
		return "", err
	}
	return tr.Token(ctx)
}
