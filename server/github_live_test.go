package main

import (
	"context"
	"os"
	"testing"

	"github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTokenClientLive exercises the real GitHub API, reading gnolang/gno. It
// only runs with GNOREPLAY_LIVE_TOKEN set.
func TestTokenClientLive(t *testing.T) {
	token := os.Getenv("GNOREPLAY_LIVE_TOKEN")
	if token == "" {
		t.Skip("GNOREPLAY_LIVE_TOKEN not set")
	}
	gh, err := newTokenClient(token)
	require.NoError(t, err)
	ctx := context.Background()

	sha, err := gh.BranchHead(ctx, "gnolang/gno", "master")
	require.NoError(t, err)
	assert.Len(t, sha, 40)

	prs, err := gh.OpenPulls(ctx, "gnolang/gno")
	require.NoError(t, err)
	require.NotEmpty(t, prs)
	t.Logf("gnolang/gno: master at %s, %d open PRs", sha[:9], len(prs))
	pr, err := gh.Pull(ctx, "gnolang/gno", prs[0].Number)
	require.NoError(t, err)
	assert.Equal(t, prs[0].Number, pr.Number)
	assert.NotEmpty(t, pr.Base)
	assert.Len(t, pr.HeadSHA, 40)
	files, err := gh.PullFiles(ctx, "gnolang/gno", pr.Number)
	require.NoError(t, err)
	assert.NotEmpty(t, files)
	t.Logf("PR #%d: mergeable %v, test merge %q, %d files", pr.Number, pr.Mergeable, pr.MergeSHA, len(files))
}

// TestTokenClientReadOnly checks that the client refuses writes before they
// leave the process, whatever the token allows.
func TestTokenClientReadOnly(t *testing.T) {
	gh, err := newTokenClient("not-a-real-token")
	require.NoError(t, err)
	ctx := context.Background()

	_, _, err = gh.c.Repositories.CreateStatus(ctx, "gnolang", "gno", "0000000000000000000000000000000000000000",
		github.RepoStatus{State: new("success")})
	require.ErrorContains(t, err, "refusing POST")
	_, _, err = gh.c.Issues.CreateComment(ctx, "gnolang", "gno", 1, &github.IssueComment{Body: new("x")})
	require.ErrorContains(t, err, "refusing POST")
	_, err = gh.c.Repositories.Delete(ctx, "gnolang", "gno")
	require.ErrorContains(t, err, "refusing DELETE")
}
