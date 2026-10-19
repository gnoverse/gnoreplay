package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWatchdogScript runs deploy/watchdog.sh against a fake Hetzner Cloud API.
func TestWatchdogScript(t *testing.T) {
	for _, tool := range []string{"bash", "curl", "jq", "date"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	type server struct {
		ID      int               `json:"id"`
		Name    string            `json:"name"`
		Created string            `json:"created"`
		Labels  map[string]string `json:"labels"`
	}
	ago := func(d time.Duration) time.Time { return time.Now().Add(-d) }
	worker := func(id int, job int64, since time.Duration) server {
		return server{
			id, "gnoreplay-worker-" + strconv.Itoa(id), ago(10 * time.Hour).UTC().Format(time.RFC3339),
			workerLabels(job, ago(since)),
		}
	}
	pages := [][]server{
		{
			worker(1, 7, time.Hour),                // on a job for 1h
			worker(2, 8, 4*time.Hour+time.Minute),  // on a job for over 4h
			worker(3, 0, 30*time.Minute),           // idle for 30 min
			worker(4, 0, time.Hour+10*time.Minute), // idle for over 65 min
		},
		{
			// Unlabelled, created 2h ago: idle for that long.
			{5, "gnoreplay-worker-5", ago(2 * time.Hour).UTC().Format(time.RFC3339), map[string]string{labelRole: "worker"}},
			worker(6, 9, 3*time.Hour),
		},
	}

	var mu sync.Mutex
	var deleted []string
	failDelete := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer hcloud-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/servers":
			assert.Equal(t, "gnoreplay=worker", r.URL.Query().Get("label_selector"))
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			var next any
			if page < len(pages) {
				next = page + 1
			}
			json.NewEncoder(w).Encode(map[string]any{
				"servers": pages[page-1],
				"meta":    map[string]any{"pagination": map[string]any{"page": page, "next_page": next}},
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/servers/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/servers/")
			mu.Lock()
			defer mu.Unlock()
			if id == failDelete {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			deleted = append(deleted, id)
			json.NewEncoder(w).Encode(map[string]any{"action": map[string]any{"id": 1}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	run := func(token string) (string, error) {
		cmd := exec.Command("../deploy/watchdog.sh")
		cmd.Env = append(os.Environ(), "HCLOUD_API="+srv.URL+"/v1", "HCLOUD_TOKEN="+token)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run("hcloud-token")
	require.NoError(t, err, out)
	assert.Equal(t, []string{"2", "4", "5"}, deleted, "workers on a job for over 4h, or idle for over 65 min, across pages")
	assert.Contains(t, out, "deleting server 2 (gnoreplay-worker-2): on job 8 for")
	assert.Contains(t, out, "deleting server 4 (gnoreplay-worker-4): idle for")

	// A failed deletion is an error (so the timer or workflow shows it).
	mu.Lock()
	deleted = nil
	failDelete = "4"
	mu.Unlock()
	out, err = run("hcloud-token")
	assert.Error(t, err, out)
	assert.Equal(t, []string{"2", "5"}, deleted)
	assert.Contains(t, out, "failed to delete server 4")

	// So is an unreadable API.
	_, err = run("wrong")
	assert.Error(t, err)
}

// The watchdog reaches the Hetzner API without putting the token in curl's
// argv, where every local user can read it from /proc.
func TestWatchdogTokenNotInArgv(t *testing.T) {
	for _, tool := range []string{"bash", "curl", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	var mu sync.Mutex
	auth := ""
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		<-release
		w.Write([]byte(`{"servers":[],"meta":{"pagination":{"page":1,"next_page":null}}}`))
	}))
	defer srv.Close()

	cmd := exec.Command("../deploy/watchdog.sh")
	cmd.Env = append(os.Environ(), "HCLOUD_API="+srv.URL, "HCLOUD_TOKEN=super-secret-token")
	require.NoError(t, cmd.Start())
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	// Scan /proc while the request is being made: the token must not be in
	// any process's argv.
	found := ""
	scans := 0
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && found == ""; scans++ {
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			if _, err := strconv.Atoi(e.Name()); err != nil {
				continue
			}
			bz, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
			if err != nil {
				continue
			}
			if strings.Contains(string(bz), "super-secret-token") {
				found = strings.ReplaceAll(string(bz), "\x00", " ")
				break
			}
		}
		mu.Lock()
		seen := auth != ""
		mu.Unlock()
		if seen && scans >= 5 {
			break
		}
		if found == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	mu.Lock()
	gotAuth := auth
	mu.Unlock()
	close(release)
	select {
	case <-waitDone:
	case <-time.After(10 * time.Second):
		t.Fatal("watchdog.sh did not exit")
	}
	assert.Empty(t, found, "the token must not be in a process's argv")
	assert.Equal(t, "Bearer super-secret-token", gotAuth, "and must still reach the API")
}

// The golden prune keeps the current target, the previous target it is given
// (a running job may still be copying from it), and anything younger than an
// hour (a snapshot still being written, or left by a killed refresh);
// everything else goes.
func TestGoldenPrune(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := t.TempDir()
	mk := func(name string, age time.Duration) string {
		dir := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "db"), 0o755))
		mtime := time.Now().Add(-age)
		require.NoError(t, os.Chtimes(dir, mtime, mtime))
		return dir
	}
	old := mk("golden-20261013T000000", 3*time.Hour)
	prev := mk("golden-20261014T000000", 2*time.Hour)
	target := mk("golden-20261015T000000", 2*time.Hour)
	recent := mk("golden-20261015T010000", time.Minute)
	require.NoError(t, os.Symlink(target, filepath.Join(root, "golden")))

	cmd := exec.Command("scripts/golden-prune.sh", prev)
	cmd.Env = append(os.Environ(), "GOLDEN_ROOT="+root)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, out)

	for _, dir := range []string{target, prev, recent} {
		assert.DirExists(t, dir, dir)
	}
	assert.NoDirExists(t, old)
}
