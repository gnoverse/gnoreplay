package main

import (
	"bytes"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// sourceURL is this server's repository, linked from every page.
const sourceURL = "https://github.com/gnoverse/gnoreplay"

// Results only live here: the server never writes to GitHub.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.serveIndex)
	mux.HandleFunc("GET /jobs/{id}", s.serveJob)
	mux.HandleFunc("GET /reports/{id}", s.serveReport)
	// The same paths as on GitHub: a PR's or a branch's replays.
	mux.HandleFunc("GET /{owner}/{repo}/pull/{pr}", s.serveTarget)
	mux.HandleFunc("GET /{owner}/{repo}/tree/{branch...}", s.serveTarget)
	mux.HandleFunc("GET /search", s.serveSearch)
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

func (s *Server) visible(r *http.Request, jobs []*Job) []*Job {
	var out []*Job
	for _, j := range jobs {
		if s.canSee(r, j.Repo) {
			out = append(out, j)
		}
	}
	return out
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

// targetURL is the page of a job's PR or branch, here.
func targetURL(j *Job) string {
	if j.Event == eventPullRequest {
		return fmt.Sprintf("/%s/pull/%d", j.Repo, j.PR)
	}
	return fmt.Sprintf("/%s/tree/%s", j.Repo, j.Branch)
}

// githubURL is a job's PR or branch on GitHub.
func githubURL(j *Job) string { return "https://github.com" + targetURL(j) }

func commitURL(j *Job) string { return fmt.Sprintf("https://github.com/%s/commit/%s", j.Repo, j.SHA) }

// badge is a job's state, as one word with a CSS class.
func badge(j *Job) template.HTML {
	label := j.State
	switch j.State {
	case stateDone:
		label = j.Outcome
	case stateFailed:
		label = "error"
	}
	return template.HTML(fmt.Sprintf(`<span class="badge %s">%s</span>`, label, label))
}

// result is a job's state as shown to users.
func result(j *Job) string {
	switch j.State {
	case stateDone:
		return j.Summary
	case stateFailed:
		return "the replay could not run: " + truncateStr(firstLine(j.Error), 140)
	case stateSuperseded:
		return "not checked: superseded by a newer push, or the PR was closed"
	case stateRunning:
		return "replaying"
	}
	return "waiting for a worker"
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// timestamp renders t for the page's script to show in the reader's time
// zone (UTC without it).
func timestamp(t time.Time) template.HTML {
	if t.IsZero() {
		return "—"
	}
	t = t.UTC()
	return template.HTML(fmt.Sprintf(`<time datetime="%s">%s</time>`, t.Format(time.RFC3339), t.Format("2006-01-02 15:04 UTC")))
}

// took is how long a job ran, or has been running.
func took(j *Job) string {
	switch {
	case j.StartedAt.IsZero():
		return "—"
	case j.FinishedAt.IsZero() && j.State == stateRunning:
		return formatDuration(time.Since(j.StartedAt))
	case j.FinishedAt.IsZero():
		return "—"
	}
	return formatDuration(j.FinishedAt.Sub(j.StartedAt))
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}

//go:embed page.html
var pageHTML string

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"result": result, "badge": badge, "short": shortSHA,
	"targetURL": targetURL, "githubURL": githubURL, "commitURL": commitURL,
	"ts": timestamp, "took": took, "source": func() string { return sourceURL },
	"pr":  func(event string) bool { return event == eventPullRequest },
	"inc": func(i int) int { return i + 1 },
}).Parse(pageHTML))

// base is what every page has: its title and the search box's text.
type base struct {
	Title string
	Query string
}

func (s *Server) writePage(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, data); err != nil {
		s.logger.Error("render page", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

// row is a queued or running job: a private one is listed without details.
type row struct {
	*Job
	Visible bool
}

// branchView is a tracked branch's latest result.
type branchView struct {
	Repo, Branch string
	Latest       *Job // latest replay that ran to an end, or nil
	Pending      *Job // a replay queued or running, or nil
}

const recentSize = 50

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	recent, err := s.queue.Recent(recentSize)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	pending, err := s.queue.Pending()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data := struct {
		base
		Rules             []string
		Branches          []branchView
		Running, Queued   []row
		Recent            []*Job
		PollInterval      string
		Workers           int
		RunTimeoutMinutes int
	}{
		base:              base{Title: "gnoreplay: mainnet replay checks"},
		Recent:            s.visible(r, recent),
		PollInterval:      s.cfg.GitHub.PollInterval.String(),
		Workers:           s.cfg.Workers,
		RunTimeoutMinutes: int(s.cfg.Job.RunTimeout.Minutes()),
	}
	for _, j := range pending {
		rw := row{Job: j, Visible: s.canSee(r, j.Repo)}
		if j.State == stateRunning {
			data.Running = append(data.Running, rw)
		} else {
			data.Queued = append(data.Queued, rw)
		}
	}
	for _, rule := range s.cfg.Rules {
		if !s.canSee(r, rule.Repo) {
			continue
		}
		if rule.Event == eventPullRequest {
			data.Rules = append(data.Rules, fmt.Sprintf("PRs into %s %s", rule.Repo, rule.Branch))
			continue
		}
		data.Rules = append(data.Rules, fmt.Sprintf("pushes to %s %s", rule.Repo, rule.Branch))
		jobs, err := s.queue.ByKey(jobKey(rule.Repo, eventPush, rule.Branch, 0), 20)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		b := branchView{Repo: rule.Repo, Branch: rule.Branch}
		for _, j := range jobs {
			switch {
			case (j.State == stateQueued || j.State == stateRunning) && b.Pending == nil:
				b.Pending = j
			case (j.State == stateDone || j.State == stateFailed) && b.Latest == nil:
				b.Latest = j
			}
		}
		data.Branches = append(data.Branches, b)
	}
	s.writePage(w, "index", data)
}

// serveTarget serves a PR's or a branch's page: its replays, and the latest
// result in full.
func (s *Server) serveTarget(w http.ResponseWriter, r *http.Request) {
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
	if !s.canSee(r, repo) {
		http.NotFound(w, r)
		return
	}
	jobs, err := s.queue.ByKey(key, 100)
	if err != nil || len(jobs) == 0 {
		http.NotFound(w, r)
		return
	}
	data := struct {
		base
		First  *Job // the newest replay, for the PR's or branch's details
		Jobs   []*Job
		Latest *Job // the newest replay that ran to an end, or nil
		Body   template.HTML
	}{First: jobs[0], Jobs: jobs}
	data.Title = fmt.Sprintf("%s %s", repo, describe(jobs[0]))
	for _, j := range jobs {
		if j.State == stateDone || j.State == stateFailed {
			data.Latest = j
			break
		}
	}
	if data.Latest != nil {
		if data.Body, err = jobBody(data.Latest); err != nil {
			http.Error(w, "report unavailable", http.StatusInternalServerError)
			return
		}
	}
	s.writePage(w, "target", data)
}

func (s *Server) serveJob(w http.ResponseWriter, r *http.Request) {
	j := s.visibleJob(r)
	if j == nil {
		http.NotFound(w, r)
		return
	}
	body, err := jobBody(j)
	if err != nil {
		http.Error(w, "report unavailable", http.StatusInternalServerError)
		return
	}
	data := struct {
		base
		Job  *Job
		Base *Job // the baseline it was compared with, or nil
		Body template.HTML
	}{base: base{Title: fmt.Sprintf("Replay of %s %s at %s", j.Repo, describe(j), shortSHA(j.SHA))}, Job: j, Body: body}
	if j.BaseID != 0 {
		if b, err := s.queue.Get(j.BaseID); err == nil {
			data.Base = b
		}
	}
	s.writePage(w, "job", data)
}

// jobBody renders a job's result: the report's page body once done.
func jobBody(j *Job) (template.HTML, error) {
	var md string
	switch j.State {
	case stateDone:
		bz, err := os.ReadFile(bodyPath(j.ReportPath))
		if err != nil {
			return "", err
		}
		md = string(bz)
	case stateFailed:
		md = "The replay did not complete, so nothing was checked.\n\n```\n" + j.Error + "\n```\n"
	case stateSuperseded:
		md = "Not checked: superseded by a newer push, or the PR was closed."
	default:
		return "", nil
	}
	var body bytes.Buffer
	// The markdown quotes tx errors, which other people control: goldmark
	// omits raw HTML and dangerous link URLs unless told otherwise.
	if err := goldmark.New(goldmark.WithExtensions(extension.GFM)).Convert([]byte(md), &body); err != nil {
		return "", err
	}
	return template.HTML(body.String()), nil
}

var (
	// GitHub URLs, with or without the scheme and host.
	githubPath = regexp.MustCompile(`^(?:https?://)?(?:www\.)?(?:github\.com/)?([\w.-]+/[\w.-]+)/(pull|tree|commit)/(.+?)/?$`)
	hexPrefix  = regexp.MustCompile(`^[0-9a-fA-F]{4,40}$`)
)

const searchSize = 100

// serveSearch finds replays by PR number, branch, commit or PR title, or a
// GitHub URL of one. A single match leads straight to its page.
func (s *Server) serveSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	jobs, bySHA, err := s.search(r, q)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	keys := map[string]bool{}
	for _, j := range jobs {
		keys[j.Key] = true
	}
	switch {
	case bySHA && len(jobs) == 1:
		http.Redirect(w, r, fmt.Sprintf("/jobs/%d", jobs[0].ID), http.StatusFound)
		return
	case !bySHA && len(keys) == 1:
		http.Redirect(w, r, targetURL(jobs[0]), http.StatusFound)
		return
	}
	s.writePage(w, "search", struct {
		base
		Jobs []*Job
	}{base{Title: "Search: " + q, Query: q}, jobs})
}

// search returns the jobs q designates, and whether it designates commits
// (rather than PRs or branches).
func (s *Server) search(r *http.Request, q string) (jobs []*Job, bySHA bool, err error) {
	if m := githubPath.FindStringSubmatch(q); m != nil {
		repo, kind, rest := m[1], m[2], m[3]
		switch kind {
		case "pull":
			n, perr := strconv.Atoi(strings.SplitN(rest, "/", 2)[0])
			if perr != nil {
				return nil, false, nil
			}
			jobs, err = s.queue.ByKey(jobKey(repo, eventPullRequest, "", n), searchSize)
		case "tree":
			jobs, err = s.queue.ByKey(jobKey(repo, eventPush, rest, 0), searchSize)
		case "commit":
			jobs, err = s.queue.BySHA(rest, searchSize)
			bySHA = true
		}
		return s.visible(r, jobs), bySHA, err
	}
	if n, err := strconv.Atoi(strings.TrimPrefix(q, "#")); err == nil && n > 0 {
		jobs, err := s.queue.ByPR(n, searchSize)
		if err != nil {
			return nil, false, err
		}
		// A number that matches no PR may be a commit's start.
		if jobs = s.visible(r, jobs); len(jobs) > 0 || strings.HasPrefix(q, "#") {
			return jobs, false, nil
		}
	}
	if hexPrefix.MatchString(q) {
		jobs, err := s.queue.BySHA(q, searchSize)
		if err != nil {
			return nil, false, err
		}
		if jobs = s.visible(r, jobs); len(jobs) > 0 {
			return jobs, true, nil
		}
	}
	if jobs, err = s.queue.ByBranch(q, searchSize); err != nil {
		return nil, false, err
	}
	if jobs = s.visible(r, jobs); len(jobs) > 0 {
		return jobs, false, nil
	}
	jobs, err = s.queue.ByTitle(q, searchSize)
	return s.visible(r, jobs), false, err
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
		Started  time.Time `json:"started_at,omitzero"`
	}
	out := make([]entry, 0, len(jobs))
	for _, j := range jobs {
		e := entry{State: j.State, Priority: j.Priority, Enqueued: j.EnqueuedAt, Started: j.StartedAt}
		if s.canSee(r, j.Repo) {
			e.Repo, e.Event, e.Branch, e.PR, e.SHA = j.Repo, j.Event, j.Branch, j.PR, j.SHA
		}
		out = append(out, e)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
