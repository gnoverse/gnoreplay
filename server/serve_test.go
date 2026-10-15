package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A worker endpoint that cannot bind must fail the process, not look like a
// clean stop: gnoreplay-server.service restarts only on failure, so an exit 0
// with the endpoint down leaves the coordinator down too.
func TestServeWorkerEndpointFailure(t *testing.T) {
	old := newWorkerProvisioner
	newWorkerProvisioner = func(string, *HetznerConfig) Provisioner { return newFakeMachines(t.TempDir()) }
	t.Cleanup(func() { newWorkerProvisioner = old })

	root := t.TempDir()
	golden := filepath.Join(root, "golden")
	require.NoError(t, os.MkdirAll(filepath.Join(golden, "db"), 0o755))
	overlay := filepath.Join(root, "overlay")
	require.NoError(t, os.MkdirAll(overlay, 0o755))
	cfgPath := writeFile(t, "config.toml", []byte(`
listen = "127.0.0.1:0"
data_dir = "`+filepath.Join(root, "data")+`"
[github]
token_file = "`+writeFile(t, "gh-token", []byte("t"))+`"
[chain]
golden_dir = "`+golden+`"
genesis = "`+writeFile(t, "genesis.json", []byte("{}"))+`"
[job]
gnoreplay_overlay = "`+overlay+`"
[hetzner]
token_file = "`+writeFile(t, "h-token", []byte("t"))+`"
location = "hel1"
server_type = "cpx42"
network = 1
ssh_keys = [1]
listen = "`+busyAddr(t)+`"
url = "http://10.0.0.2:8081"
`))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	err := serve(context.Background(), cfgPath, logger)
	assert.ErrorContains(t, err, "worker endpoint", "serve must report the failure, not return nil")
	assert.Contains(t, logBuf.String(), "worker endpoint")
}

// busyAddr returns a listen address already in use.
func busyAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

func writeFile(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
