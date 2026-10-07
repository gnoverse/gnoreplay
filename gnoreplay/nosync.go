package main

import "github.com/cockroachdb/pebble/vfs"

// noSyncFS is the filesystem of the replay's own app DB: its files are
// written as usual, but never synced. tm2 syncs each block commit, which on
// cloud disks costs ~1.7ms a block, about 15% of a mainnet replay; the DB is
// thrown away after the replay, so durability buys nothing. (Pebble's
// DisableWAL would refuse tm2's synced writes instead.)
type noSyncFS struct{ vfs.FS }

func (fs noSyncFS) Create(name string) (vfs.File, error) {
	return noSync(fs.FS.Create(name))
}

func (fs noSyncFS) OpenReadWrite(name string, opts ...vfs.OpenOption) (vfs.File, error) {
	return noSync(fs.FS.OpenReadWrite(name, opts...))
}

func (fs noSyncFS) OpenDir(name string) (vfs.File, error) {
	return noSync(fs.FS.OpenDir(name))
}

func (fs noSyncFS) ReuseForWrite(oldname, newname string) (vfs.File, error) {
	return noSync(fs.FS.ReuseForWrite(oldname, newname))
}

func noSync(f vfs.File, err error) (vfs.File, error) {
	if err != nil {
		return nil, err
	}
	return noSyncFile{f}, nil
}

type noSyncFile struct{ vfs.File }

func (noSyncFile) Sync() error                { return nil }
func (noSyncFile) SyncData() error            { return nil }
func (noSyncFile) SyncTo(int64) (bool, error) { return false, nil }
