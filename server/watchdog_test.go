package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWatchdogScript runs deploy/watchdog.sh against a fake DigitalOcean API.
func TestWatchdogScript(t *testing.T) {
	for _, tool := range []string{"bash", "curl", "jq", "date"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	type droplet struct {
		ID      int    `json:"id"`
		Name    string `json:"name"`
		Created string `json:"created_at"`
	}
	ago := func(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }
	pages := [][]droplet{
		{{1, "gnoreplay-job-1", ago(time.Hour)}, {2, "gnoreplay-job-2", ago(5 * time.Hour)}},
		{{3, "gnoreplay-job-3", ago(4*time.Hour + time.Minute)}, {4, "gnoreplay-job-4", ago(3 * time.Hour)}},
	}

	var mu sync.Mutex
	var deleted []string
	failDelete := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer do-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/droplets":
			assert.Equal(t, "gnoreplay-worker", r.URL.Query().Get("tag_name"))
			page := 0
			if r.URL.Query().Get("page") == "2" {
				page = 1
			}
			res := map[string]any{"droplets": pages[page], "links": map[string]any{}}
			if page == 0 {
				res["links"] = map[string]any{"pages": map[string]string{
					"next": "http://" + r.Host + "/v2/droplets?tag_name=gnoreplay-worker&per_page=200&page=2",
				}}
			}
			json.NewEncoder(w).Encode(res)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v2/droplets/"):
			id := strings.TrimPrefix(r.URL.Path, "/v2/droplets/")
			mu.Lock()
			defer mu.Unlock()
			if id == failDelete {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			deleted = append(deleted, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	run := func() (string, error) {
		cmd := exec.Command("../deploy/watchdog.sh")
		cmd.Env = append(os.Environ(), "DO_API="+srv.URL, "DO_TOKEN=do-token")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run()
	require.NoError(t, err, out)
	assert.Equal(t, []string{"2", "3"}, deleted, "only workers older than 4h, across pages")
	assert.Contains(t, out, "deleting droplet 2 (gnoreplay-job-2)")

	// A failed deletion is an error (so the timer or workflow shows it).
	mu.Lock()
	deleted = nil
	failDelete = "3"
	mu.Unlock()
	out, err = run()
	assert.Error(t, err, out)
	assert.Equal(t, []string{"2"}, deleted)
	assert.Contains(t, out, "failed to delete droplet 3")

	// So is an unreadable API.
	cmd := exec.Command("../deploy/watchdog.sh")
	cmd.Env = append(os.Environ(), "DO_API="+srv.URL, "DO_TOKEN=wrong")
	assert.Error(t, cmd.Run())
}
