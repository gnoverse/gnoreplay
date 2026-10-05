package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestQueue(t *testing.T) *Queue {
	t.Helper()
	q, err := openQueue(filepath.Join(t.TempDir(), "jobs.db"))
	require.NoError(t, err)
	t.Cleanup(func() { q.Close() })
	return q
}

// testJob builds the job the webhook handler would enqueue for an event.
func testJob(t *testing.T, cfg *Config, repo, event, branch string, pr int, sha string, at time.Time) *Job {
	t.Helper()
	prio, ok := cfg.priority(repo, event, branch)
	require.True(t, ok, "no rule for %s %s %s", repo, event, branch)
	return &Job{
		Key: jobKey(repo, event, branch, pr), Repo: repo, Event: event, Branch: branch,
		PR: pr, SHA: sha, Priority: prio, EnqueuedAt: at,
	}
}

func TestPriorityOrder(t *testing.T) {
	cfg := &Config{}
	cfg.setDefaults()
	q := newTestQueue(t)
	t0 := time.Unix(1_000_000, 0)

	// Enqueued lowest priority first, so the order is not insertion order.
	enqueue := []*Job{
		testJob(t, cfg, "gnolang/gno-fixes", eventPullRequest, "develop", 7, "pr-fixes", t0),
		testJob(t, cfg, "gnolang/gno", eventPullRequest, "master", 100, "pr-master-100", t0.Add(1*time.Second)),
		testJob(t, cfg, "gnolang/gno", eventPullRequest, "master", 101, "pr-master-101", t0.Add(2*time.Second)),
		testJob(t, cfg, "gnolang/gno-fixes", eventPush, "develop", 0, "push-fixes", t0.Add(3*time.Second)),
		testJob(t, cfg, "gnolang/gno", eventPush, "master", 0, "push-master", t0.Add(4*time.Second)),
		testJob(t, cfg, "gnolang/gno", eventPullRequest, "chain/mainnet", 200, "pr-mainnet", t0.Add(5*time.Second)),
		testJob(t, cfg, "gnolang/gno", eventPush, "chain/mainnet", 0, "push-mainnet", t0.Add(6*time.Second)),
	}
	for _, j := range enqueue {
		sup, err := q.Enqueue(j)
		require.NoError(t, err)
		assert.Empty(t, sup)
	}

	// A new push to PR 100 supersedes its queued job and goes to the back of
	// its priority level (behind PR 101, enqueued earlier).
	sup, err := q.Enqueue(testJob(t, cfg, "gnolang/gno", eventPullRequest, "master", 100, "pr-master-100b", t0.Add(7*time.Second)))
	require.NoError(t, err)
	require.Len(t, sup, 1)
	assert.Equal(t, "pr-master-100", sup[0].SHA)
	assert.Equal(t, stateSuperseded, sup[0].State)

	var order []string
	for {
		j, err := q.Claim()
		require.NoError(t, err)
		if j == nil {
			break
		}
		assert.Equal(t, stateRunning, j.State)
		order = append(order, j.SHA)
	}
	assert.Equal(t, []string{
		"push-mainnet",   // 1. commits on chain/mainnet
		"pr-mainnet",     // 2. PRs to chain/mainnet
		"push-master",    // 3. commits on master
		"push-fixes",     // 4. commits on gno-fixes/develop
		"pr-master-101",  // 5. PRs to master, FIFO
		"pr-master-100b", //    (the superseding push, enqueued later)
		"pr-fixes",       // 6. PRs to gno-fixes/develop
	}, order)
}

func TestSupersedeRunning(t *testing.T) {
	cfg := &Config{}
	cfg.setDefaults()
	q := newTestQueue(t)
	now := time.Now()

	_, err := q.Enqueue(testJob(t, cfg, "gnolang/gno", eventPush, "master", 0, "a", now))
	require.NoError(t, err)
	running, err := q.Claim()
	require.NoError(t, err)
	require.NotNil(t, running)

	sup, err := q.Enqueue(testJob(t, cfg, "gnolang/gno", eventPush, "master", 0, "b", now.Add(time.Second)))
	require.NoError(t, err)
	require.Len(t, sup, 1)
	assert.Equal(t, running.ID, sup[0].ID)

	// The superseded job's late result is discarded.
	recorded, err := q.Finish(running.ID, stateDone, "report.json", "")
	require.NoError(t, err)
	assert.False(t, recorded)
	got, err := q.Get(running.ID)
	require.NoError(t, err)
	assert.Equal(t, stateSuperseded, got.State)

	next, err := q.Claim()
	require.NoError(t, err)
	assert.Equal(t, "b", next.SHA)
}

func TestRequeueAndBaseline(t *testing.T) {
	cfg := &Config{}
	cfg.setDefaults()
	path := filepath.Join(t.TempDir(), "jobs.db")
	q, err := openQueue(path)
	require.NoError(t, err)

	_, err = q.Enqueue(testJob(t, cfg, "gnolang/gno", eventPush, "master", 0, "base1", time.Now()))
	require.NoError(t, err)
	j, err := q.Claim()
	require.NoError(t, err)
	ok, err := q.Finish(j.ID, stateDone, "r1.json", "")
	require.NoError(t, err)
	require.True(t, ok)

	_, err = q.Enqueue(testJob(t, cfg, "gnolang/gno", eventPush, "master", 0, "base2", time.Now()))
	require.NoError(t, err)
	_, err = q.Claim()
	require.NoError(t, err)
	require.NoError(t, q.Close())

	// Restart: the interrupted job runs again; the baseline is the last
	// completed one.
	q, err = openQueue(path)
	require.NoError(t, err)
	defer q.Close()
	n, err := q.Requeue()
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)

	base, err := q.Baseline("gnolang/gno", "master")
	require.NoError(t, err)
	require.NotNil(t, base)
	assert.Equal(t, "base1", base.SHA)

	again, err := q.Claim()
	require.NoError(t, err)
	assert.Equal(t, "base2", again.SHA)

	none, err := q.Baseline("gnolang/gno", "chain/mainnet")
	require.NoError(t, err)
	assert.Nil(t, none)
}

func TestPriorityIgnoresUnknown(t *testing.T) {
	cfg := &Config{}
	cfg.setDefaults()
	_, ok := cfg.priority("gnolang/gno", eventPullRequest, "feature-branch")
	assert.False(t, ok)
	_, ok = cfg.priority("someone/gno", eventPush, "master")
	assert.False(t, ok)
}
