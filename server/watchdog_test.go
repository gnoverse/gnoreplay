package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
