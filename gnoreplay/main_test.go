package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gnolang/gno/tm2/pkg/log"
)

// Reusing a work dir keeps the genesis cache but never an earlier run's app
// or state DB: the handshake would trust the stale state, and every block
// would then fail against it.
func TestPrepareWorkDir(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "genesis-cache", "x")
	require.NoError(t, os.MkdirAll(filepath.Dir(cache), 0o755))
	require.NoError(t, os.WriteFile(cache, []byte("cache"), 0o644))
	for _, name := range []string{"gnolang", "state"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, name+".db"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name+".db", "CURRENT"), []byte("stale"), 0o644))
	}

	require.NoError(t, prepareWorkDir(dir))
	assert.NoFileExists(t, filepath.Join(dir, "gnolang.db", "CURRENT"))
	assert.NoFileExists(t, filepath.Join(dir, "state.db", "CURRENT"))
	assert.FileExists(t, cache, "the genesis cache survives")
}

// A replay that compared nothing must not read as a clean pass.
func TestCheckReplayCoverage(t *testing.T) {
	assert.ErrorContains(t, checkReplayCoverage(&Report{Blocks: 0}, "/d"), "no blocks")
	// A source that ends where the checkpoint it is resumed from does.
	assert.ErrorContains(t, checkReplayCoverage(&Report{FromHeight: 1651501, ToHeight: 1651500}, "/d"),
		"it ends at height 1651500, before the first to replay, 1651501")
	assert.ErrorContains(t, checkReplayCoverage(&Report{Blocks: 3, MissingResponses: 3}, "/d"), "nothing to compare")
	assert.NoError(t, checkReplayCoverage(&Report{Blocks: 3, MissingResponses: 1}, "/d"))
	assert.NoError(t, checkReplayCoverage(&Report{Blocks: 3}, "/d"))
}

// run twice over one work dir, as keeping the genesis cache invites: the
// second run must replay the same history, not reject block 1 against the
// first run's state.
func TestRunReusesWorkDir(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a node and replays its chain twice")
	}
	root := t.TempDir()
	dataDir, genesisFile := recordChainOnDisk(t, root)
	workDir := filepath.Join(root, "work")

	first, err := run(log.NewNoopLogger(), dataDir, genesisFile, workDir, replayOptions{})
	require.NoError(t, err)
	require.Empty(t, first.Diffs)

	second, err := run(log.NewNoopLogger(), dataDir, genesisFile, workDir, replayOptions{})
	require.NoError(t, err)
	assert.Empty(t, second.Diffs, "a reused work dir replays the same history")
	assert.Equal(t, first.ToHeight, second.ToHeight)
	assert.Equal(t, first.Blocks, second.Blocks)
}

// A replay stopped at a height and resumed from its work dir ends where an
// uninterrupted one does: the same blocks and txs between them, and the same
// app hash, wherever it was stopped.
func TestRunResume(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a node and replays its chain several times")
	}
	root := t.TempDir()
	dataDir, genesisFile := recordChainOnDisk(t, root)

	full, err := run(log.NewNoopLogger(), dataDir, genesisFile, filepath.Join(root, "full"), replayOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, full.AppHash)
	require.Greater(t, full.ToHeight, int64(1))

	for split := int64(1); split < full.ToHeight; split++ {
		t.Run(fmt.Sprintf("at %d", split), func(t *testing.T) {
			workDir := filepath.Join(root, fmt.Sprintf("work-%d", split))
			first, err := run(log.NewNoopLogger(), dataDir, genesisFile, workDir, replayOptions{ToHeight: split})
			require.NoError(t, err)
			require.Equal(t, split, first.ToHeight)

			// No genesis file: a checkpoint is resumed without it.
			second, err := run(log.NewNoopLogger(), dataDir, "", workDir, replayOptions{Resume: true})
			require.NoError(t, err)
			assert.Empty(t, second.Diffs)
			assert.Equal(t, full.ChainID, second.ChainID)
			assert.Equal(t, split+1, second.FromHeight)
			assert.Equal(t, full.ToHeight, second.ToHeight)
			assert.Equal(t, full.Blocks, first.Blocks+second.Blocks)
			assert.Equal(t, full.Txs, first.Txs+second.Txs)
			assert.Equal(t, full.AppHash, second.AppHash)
		})
	}
}

// A checkpoint of another chain is refused up front, with the cause, and
// stays usable for its own.
func TestRunResumeOtherChain(t *testing.T) {
	if testing.Short() {
		t.Skip("boots two nodes and replays a chain")
	}
	rootA, rootB := t.TempDir(), t.TempDir()
	dataA, genesisA := recordChainOnDisk(t, rootA)
	dataB, _ := recordChainOnDisk(t, rootB)
	workDir := filepath.Join(rootA, "work")

	_, err := run(log.NewNoopLogger(), dataA, genesisA, workDir, replayOptions{ToHeight: 1})
	require.NoError(t, err)

	_, err = run(log.NewNoopLogger(), dataB, "", workDir, replayOptions{Resume: true})
	assert.ErrorContains(t, err, "not of this chain")

	r, err := run(log.NewNoopLogger(), dataA, "", workDir, replayOptions{Resume: true})
	require.NoError(t, err)
	assert.Empty(t, r.Diffs)
}

// Resuming needs the checkpoint's DBs: opening a missing one would create an
// empty one.
func TestCheckCheckpointDir(t *testing.T) {
	assert.ErrorContains(t, checkCheckpointDir(""), "--work-dir")

	dir := t.TempDir()
	assert.ErrorContains(t, checkCheckpointDir(dir), "no checkpoint to resume")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "gnolang.db"), 0o755))
	assert.ErrorContains(t, checkCheckpointDir(dir), "no checkpoint to resume", "the state DB is missing")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "state.db"), 0o755))
	assert.NoError(t, checkCheckpointDir(dir))
}
