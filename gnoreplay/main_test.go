package main

import (
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
