package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// Results only live here: the server never writes to GitHub.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.serveIndex)
	mux.HandleFunc("GET /jobs/{id}", s.serveJob)
	mux.HandleFunc("GET /reports/{id}", s.serveReport)
	// The same paths as on GitHub: the latest replay of a PR or a branch.
	mux.HandleFunc("GET /{owner}/{repo}/pull/{pr}", s.serveLatest)
	mux.HandleFunc("GET /{owner}/{repo}/tree/{branch...}", s.serveLatest)
	mux.HandleFunc("GET /queue", s.serveQueue)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	return s.withViewerKey(mux)
}

const viewerCookie = "gnoreplay_key"

// withViewerKey turns a valid ?key= into a cookie, so links between pages
// keep working without carrying the key.
func (s *Server) withViewerKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key := r.URL.Query().Get("key"); key != "" && s.validKey(key) {
			http.SetCookie(w, &http.Cookie{
				Name: viewerCookie, Value: key, Path: "/", HttpOnly: true,
				Secure: strings.HasPrefix(s.cfg.PublicURL, "https://"), SameSite: http.SameSiteLaxMode,
				MaxAge: 90 * 24 * 3600,
			})
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) validKey(key string) bool {
	return s.viewerKey != "" && subtle.ConstantTimeCompare([]byte(key), []byte(s.viewerKey)) == 1
}

// canSee reports whether the request may see repo's results.
func (s *Server) canSee(r *http.Request, repo string) bool {
	if s.cfg.Repos[repo].PublicReports {
		return true
	}
	if s.validKey(r.URL.Query().Get("key")) {
		return true
	}
	c, err := r.Cookie(viewerCookie)
	return err == nil && s.validKey(c.Value)
}

// reportURL is the job's full JSON report, linked from its page.
func (s *Server) reportURL(j *Job) string {
	if s.cfg.PublicURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/reports/%d", strings.TrimRight(s.cfg.PublicURL, "/"), j.ID)
}

// visibleJob returns the job a request is for, or nil if it does not exist
// or the request may not see it.
func (s *Server) visibleJob(r *http.Request) *Job {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil
	}
	j, err := s.queue.Get(id)
	if err != nil || !s.canSee(r, j.Repo) {
		return nil
	}
	return j
}

func describe(j *Job) string {
	if j.Event == eventPullRequest {
		return fmt.Sprintf("PR #%d (into %s)", j.PR, j.Branch)
	}
	return "branch " + j.Branch
}

// result is a job's state as shown to users.
func result(j *Job) string {
	switch {
	case j.State == stateDone && j.Outcome == outcomeDiverges:
		return "✗ " + j.Summary
	case j.State == stateDone:
		return "✓ " + j.Summary
	case j.State == stateFailed:
		return "replay could not run"
	case j.State == stateSuperseded:
		return "not checked: superseded, or the PR was closed"
	}
	return j.State
}

var page = template.Must(template.New("page").Funcs(template.FuncMap{
	"describe": describe, "result": result, "short": shortSHA,
	"time": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
}).Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
body { font: 15px/1.5 system-ui, sans-serif; max-width: 64rem; margin: 2rem auto; padding: 0 1rem; color: #1f2328; }
code, pre { font: 13px ui-monospace, monospace; background: #f6f8fa; }
pre { padding: .75rem; overflow-x: auto; white-space: pre-wrap; }
table { border-collapse: collapse; } td, th { border: 1px solid #d0d7de; padding: .25rem .6rem; text-align: left; }
.meta { color: #59636e; }
</style></head><body>
<h1>{{.Title}}</h1>
{{with .Meta}}<p class="meta">{{.}}</p>{{end}}
{{if .Jobs}}<table>
<tr><th>Repo</th><th>What</th><th>Commit</th><th>Enqueued</th><th>Result</th></tr>
{{range .Jobs}}<tr><td>{{.Repo}}</td><td><a href="/jobs/{{.ID}}">{{describe .}}</a></td><td><code>{{short .SHA}}</code></td><td>{{time .EnqueuedAt}}</td><td>{{result .}}</td></tr>
{{end}}</table>{{end}}
{{.Body}}
</body></html>`))

type pageData struct {
	Title, Meta string
	Jobs        []*Job
	Body        template.HTML
}

func (s *Server) writePage(w http.ResponseWriter, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page.Execute(w, data)
}

const indexSize = 200

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.queue.Recent(indexSize)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var visible []*Job
	for _, j := range jobs {
		if s.canSee(r, j.Repo) {
			visible = append(visible, j)
		}
	}
	s.writePage(w, pageData{
		Title: "Mainnet replays",
		Meta:  "Each push and PR on the tracked branches is replayed against gno.land mainnet's history. Latest of a PR: /<owner>/<repo>/pull/<number>; of a branch: /<owner>/<repo>/tree/<branch>.",
		Jobs:  visible,
	})
}

func (s *Server) serveLatest(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	var key string
	if pr := r.PathValue("pr"); pr != "" {
		n, err := strconv.Atoi(pr)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		key = jobKey(repo, eventPullRequest, "", n)
	} else {
		key = jobKey(repo, eventPush, r.PathValue("branch"), 0)
	}
	j, err := s.queue.Latest(key)
	if err != nil || j == nil || !s.canSee(r, repo) {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/jobs/%d", j.ID), http.StatusFound)
}

func (s *Server) serveJob(w http.ResponseWriter, r *http.Request) {
	j := s.visibleJob(r)
	if j == nil {
		http.NotFound(w, r)
		return
	}
	var md string
	switch j.State {
	case stateDone:
		bz, err := os.ReadFile(bodyPath(j.ReportPath))
		if err != nil {
			http.Error(w, "report unavailable", http.StatusInternalServerError)
			return
		}
		md = string(bz)
	case stateFailed:
		md = "The replay did not complete, so nothing was checked.\n\n```\n" + j.Error + "\n```\n"
	case stateSuperseded:
		md = "Not checked: superseded by a newer push, or the PR was closed."
	default:
		md = fmt.Sprintf("Waiting (%s, priority %d, enqueued %s).", j.State, j.Priority, j.EnqueuedAt.UTC().Format(time.RFC3339))
	}
	var body bytes.Buffer
	if err := goldmark.New(goldmark.WithExtensions(extension.GFM)).Convert([]byte(md), &body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writePage(w, pageData{
		Title: fmt.Sprintf("Replay of %s %s", j.Repo, describe(j)),
		Meta:  fmt.Sprintf("commit %s · job %d · %s", j.SHA, j.ID, result(j)),
		// The markdown quotes tx errors, which other people control: goldmark
		// omits raw HTML and dangerous link URLs unless told otherwise.
		Body: template.HTML(body.String()),
	})
}

func (s *Server) serveReport(w http.ResponseWriter, r *http.Request) {
	j := s.visibleJob(r)
	if j == nil || j.ReportPath == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(j.ReportPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/json")
	http.ServeContent(w, r, "", time.Time{}, f)
}

// serveQueue lists pending jobs; jobs the request may not see are anonymized.
func (s *Server) serveQueue(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.queue.Pending()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	type entry struct {
		State    string    `json:"state"`
		Priority int       `json:"priority"`
		Repo     string    `json:"repo,omitempty"`
		Event    string    `json:"event,omitempty"`
		Branch   string    `json:"branch,omitempty"`
		PR       int       `json:"pr,omitempty"`
		SHA      string    `json:"sha,omitempty"`
		Enqueued time.Time `json:"enqueued_at"`
	}
	out := make([]entry, 0, len(jobs))
	for _, j := range jobs {
		e := entry{State: j.State, Priority: j.Priority, Enqueued: j.EnqueuedAt}
		if s.canSee(r, j.Repo) {
			e.Repo, e.Event, e.Branch, e.PR, e.SHA = j.Repo, j.Event, j.Branch, j.PR, j.SHA
		}
		out = append(out, e)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
