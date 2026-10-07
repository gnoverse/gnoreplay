package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The overlay is usually a symlink (deploy/README.md links it into the
// gnoreplay checkout).
func TestCopyDirSymlinkedRoot(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(real, "main.go"), []byte("package main"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(real, "sub", "x.go"), []byte("package sub"), 0o644))
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(real, link))

	dst := filepath.Join(root, "dst")
	require.NoError(t, copyDir(link, dst))
	bz, err := os.ReadFile(filepath.Join(dst, "main.go"))
	require.NoError(t, err)
	assert.Equal(t, "package main", string(bz))
	assert.FileExists(t, filepath.Join(dst, "sub", "x.go"))
}

// Jobs start together on a fresh server (e.g. both branch baselines on the
// first poll): their checkouts share one mirror that does not exist yet.
func TestConcurrentCheckouts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	remote := filepath.Join(root, "remote", "gnolang", "gno.git")
	require.NoError(t, os.MkdirAll(remote, 0o755))
	git(t, remote, "init", "-q")
	git(t, remote, "config", "uploadpack.allowAnySHA1InWant", "true")
	var shas []string
	for i := range 4 {
		require.NoError(t, os.WriteFile(filepath.Join(remote, "f"), fmt.Appendf(nil, "%d", i), 0o644))
		git(t, remote, "add", ".")
		git(t, remote, "commit", "-q", "-m", fmt.Sprintf("c%d", i))
		shas = append(shas, git(t, remote, "rev-parse", "HEAD"))
	}

	cfg := &Config{DataDir: filepath.Join(root, "data"), GitHub: GitHubConfig{GitURL: "file://" + filepath.Join(root, "remote") + "/"}}
	cfg.setDefaults()
	srv := newServer(cfg, nil, newFakeGitHub(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	var wg sync.WaitGroup
	errs := make([]error, len(shas))
	for i, sha := range shas {
		wg.Go(func() {
			parents, err := srv.checkout(context.Background(), "gnolang/gno", sha, filepath.Join(root, "src", fmt.Sprint(i)), io.Discard)
			if err == nil && i > 0 && (len(parents) != 1 || parents[0] != shas[i-1]) {
				err = fmt.Errorf("parents %v, want [%s]", parents, shas[i-1])
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "checkout %d", i)
		bz, err := os.ReadFile(filepath.Join(root, "src", fmt.Sprint(i), "f"))
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprint(i), string(bz))
	}
}
