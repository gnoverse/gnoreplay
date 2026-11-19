package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeCheckpoint makes a checkpoint dir at dir, as scripts/make-checkpoint.sh
// leaves one, with a MARKER in each DB for the fake tool to look for.
func writeCheckpoint(t *testing.T, dir string, height int64) string {
	t.Helper()
	for _, name := range checkpointDBs {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, name), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name, "MARKER"), nil, 0o644))
	}
	bz, err := json.Marshal(Checkpoint{ChainID: "gnoland-1", Height: height, AppHash: "AB12"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "checkpoint.json"), bz, 0o644))
	return dir
}

func TestLoadCheckpoint(t *testing.T) {
	dir := writeCheckpoint(t, t.TempDir(), 1651500)
	c, err := loadCheckpoint(dir)
	require.NoError(t, err)
	assert.Equal(t, Checkpoint{ChainID: "gnoland-1", Height: 1651500, AppHash: "AB12"}, c)

	// What else the manifest says (how it was made) is not the server's.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "checkpoint.json"),
		[]byte(`{"chain_id":"gnoland-1","height":7,"app_hash":"CD","built_with":{"release":"v1.7.1"}}`), 0o644))
	c, err = loadCheckpoint(dir)
	require.NoError(t, err)
	assert.EqualValues(t, 7, c.Height)

	for name, manifest := range map[string]string{
		"no chain":    `{"height":7,"app_hash":"CD"}`,
		"no height":   `{"chain_id":"gnoland-1","app_hash":"CD"}`,
		"no app hash": `{"chain_id":"gnoland-1","height":7}`,
		"not json":    `height 7`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "checkpoint.json"), []byte(manifest), 0o644))
		_, err := loadCheckpoint(dir)
		assert.Error(t, err, name)
	}

	_, err = loadCheckpoint(filepath.Join(t.TempDir(), "absent"))
	assert.ErrorContains(t, err, "checkpoint")

	// A DB that is not there, or is not a directory.
	dir = writeCheckpoint(t, t.TempDir(), 7)
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "state.db")))
	_, err = loadCheckpoint(dir)
	assert.ErrorContains(t, err, "state.db is missing")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.db"), nil, 0o644))
	_, err = loadCheckpoint(dir)
	assert.ErrorContains(t, err, "state.db is missing")
}
