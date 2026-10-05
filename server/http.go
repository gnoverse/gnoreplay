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

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /jobs/{id}", s.serveJob)
	mux.HandleFunc("GET /reports/{id}", s.serveReport)
	mux.HandleFunc("GET /queue", s.serveQueue)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	return mux
}

// jobURL is the page a job's commit status links to.
func (s *Server) jobURL(j *Job) string { return s.url(j, "jobs") }

// reportURL is the job's full JSON report.
func (s *Server) reportURL(j *Job) string { return s.url(j, "reports") }

func (s *Server) url(j *Job, kind string) string {
	if s.cfg.PublicURL == "" {
		return ""
	}
	u := fmt.Sprintf("%s/%s/%d", strings.TrimRight(s.cfg.PublicURL, "/"), kind, j.ID)
	if !s.cfg.Repos[j.Repo].PublicReports {
		u += "?k=" + j.Secret
	}
	return u
}

// authorizedJob returns the job a request is for, or nil if it does not
// exist or the request may not see it.
func (s *Server) authorizedJob(r *http.Request) *Job {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil
	}
	j, err := s.queue.Get(id)
	if err != nil {
		return nil
	}
	if !s.cfg.Repos[j.Repo].PublicReports &&
		subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("k")), []byte(j.Secret)) != 1 {
		return nil
	}
	return j
}

var jobPage = template.Must(template.New("job").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
body { font: 15px/1.5 system-ui, sans-serif; max-width: 60rem; margin: 2rem auto; padding: 0 1rem; color: #1f2328; }
code, pre { font: 13px ui-monospace, monospace; background: #f6f8fa; }
pre { padding: .75rem; overflow-x: auto; white-space: pre-wrap; }
table { border-collapse: collapse; } td, th { border: 1px solid #d0d7de; padding: .25rem .6rem; }
.meta { color: #59636e; }
</style></head><body>
<h1>{{.Title}}</h1>
<p class="meta">{{.Meta}}</p>
{{.Body}}
</body></html>`))

func (s *Server) serveJob(w http.ResponseWriter, r *http.Request) {
	j := s.authorizedJob(r)
	if j == nil {
		http.NotFound(w, r)
		return
	}
	what := "branch " + j.Branch
	if j.Event == eventPullRequest {
		what = fmt.Sprintf("PR #%d (into %s)", j.PR, j.Branch)
	}
	data := struct {
		Title, Meta string
		Body        template.HTML
	}{
		Title: fmt.Sprintf("Replay of %s %s", j.Repo, what),
		Meta:  fmt.Sprintf("commit %s · job %d · %s", j.SHA, j.ID, j.State),
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
	// The markdown quotes tx errors, which other people control: goldmark
	// omits raw HTML and dangerous link URLs unless told otherwise.
	data.Body = template.HTML(body.String())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	jobPage.Execute(w, data)
}

func (s *Server) serveReport(w http.ResponseWriter, r *http.Request) {
	j := s.authorizedJob(r)
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

// serveQueue lists pending jobs; jobs of repos without public reports are
// anonymized.
func (s *Server) serveQueue(w http.ResponseWriter, _ *http.Request) {
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
		if s.cfg.Repos[j.Repo].PublicReports {
			e.Repo, e.Event, e.Branch, e.PR, e.SHA = j.Repo, j.Event, j.Branch, j.PR, j.SHA
		}
		out = append(out, e)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
