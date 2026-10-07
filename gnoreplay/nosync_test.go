package main

import (
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gnolang/gno/tm2/pkg/db/pebbledb"
)

// tm2 commits blocks with synced writes: they must work on noSyncFS (unlike
// with pebble's DisableWAL), and what they wrote must read back.
func TestNoSyncFS(t *testing.T) {
	dir := t.TempDir()
	open := func() *pebbledb.PebbleDB {
		db, err := pebbledb.NewPebbleDBWithOpts("app", dir, &pebble.Options{FS: noSyncFS{vfs.Default}})
		require.NoError(t, err)
		return db
	}

	db := open()
	batch := db.NewBatch()
	require.NoError(t, batch.Set([]byte("k1"), []byte("v1")))
	require.NoError(t, batch.WriteSync())
	require.NoError(t, db.SetSync([]byte("k2"), []byte("v2")))
	require.NoError(t, db.Close())

	db = open()
	defer db.Close()
	for k, v := range map[string]string{"k1": "v1", "k2": "v2"} {
		got, err := db.Get([]byte(k))
		require.NoError(t, err)
		assert.Equal(t, v, string(got), k)
	}
}
