package main

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTokenClientLive exercises the real GitHub API. It only runs with
// GNOREPLAY_LIVE_TOKEN set; it reads gnolang/gno, and sets a status only if
// GNOREPLAY_LIVE_STATUS_REPO and GNOREPLAY_LIVE_STATUS_SHA name a commit to
// set it on (use a scratch repo).
func TestTokenClientLive(t *testing.T) {
	token := os.Getenv("GNOREPLAY_LIVE_TOKEN")
	if token == "" {
		t.Skip("GNOREPLAY_LIVE_TOKEN not set")
	}
	gh, err := newTokenClient(token, "gnoreplay-live-test")
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

	repo, statusSHA := os.Getenv("GNOREPLAY_LIVE_STATUS_REPO"), os.Getenv("GNOREPLAY_LIVE_STATUS_SHA")
	if repo == "" || statusSHA == "" {
		return
	}
	long := "a description longer than the 140 characters GitHub accepts for a commit status, which the client has to truncate rather than fail on, ✓ unicode"
	require.NoError(t, gh.SetStatus(ctx, repo, statusSHA, Status{
		State: "success", Description: long, TargetURL: "https://example.org/jobs/1",
	}))
}
