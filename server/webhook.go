package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v89/github"
)

func (s *Server) routes(webhookSecret []byte) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", func(w http.ResponseWriter, r *http.Request) {
		payload, err := github.ValidatePayload(r, webhookSecret)
		if err != nil {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		event, err := github.ParseWebHook(github.WebHookType(r), payload)
		if err != nil {
			// Event types we don't subscribe to parse fine; this is malformed.
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Enqueueing talks to GitHub; don't tie it to the webhook delivery.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
		defer cancel()
		if err := s.handleEvent(ctx, event); err != nil {
			s.logger.Error("handle webhook", "delivery", github.DeliveryID(r), "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /reports/{id}", s.serveReport)
	mux.HandleFunc("GET /queue", s.serveQueue)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	return mux
}

// handleEvent turns a webhook event into a job, if a rule matches it.
func (s *Server) handleEvent(ctx context.Context, event any) error {
	switch e := event.(type) {
	case *github.PushEvent:
		branch, ok := strings.CutPrefix(e.GetRef(), "refs/heads/")
		if !ok || e.GetDeleted() {
			return nil
		}
		return s.enqueueFor(ctx, e.GetRepo().GetFullName(), eventPush, branch, 0, e.GetAfter(), e.GetInstallation().GetID())

	case *github.PullRequestEvent:
		switch e.GetAction() {
		case "opened", "synchronize":
		default:
			return nil
		}
		pr := e.GetPullRequest()
		return s.enqueueFor(ctx, e.GetRepo().GetFullName(), eventPullRequest, pr.GetBase().GetRef(),
			pr.GetNumber(), pr.GetHead().GetSHA(), e.GetInstallation().GetID())

	case *github.CheckRunEvent:
		if e.GetAction() != "rerequested" || e.GetCheckRun().GetApp().GetID() != s.cfg.GitHub.AppID {
			return nil
		}
		old, err := s.queue.ByCheckRun(e.GetCheckRun().GetID())
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		return s.enqueue(ctx, &Job{
			Key: old.Key, Repo: old.Repo, Event: old.Event, Branch: old.Branch, PR: old.PR,
			SHA: old.SHA, InstallationID: e.GetInstallation().GetID(), Priority: old.Priority,
		})
	}
	return nil
}

func (s *Server) enqueueFor(ctx context.Context, repo, event, branch string, pr int, sha string, installation int64) error {
	prio, ok := s.cfg.priority(repo, event, branch)
	if !ok || sha == "" {
		return nil
	}
	return s.enqueue(ctx, &Job{
		Key: jobKey(repo, event, branch, pr), Repo: repo, Event: event, Branch: branch,
		PR: pr, SHA: sha, InstallationID: installation, Priority: prio,
	})
}

func (s *Server) serveReport(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	j, err := s.queue.Get(id)
	// Reports of private repos are only summarized in their check runs.
	if err != nil || j.ReportPath == "" || !s.cfg.Repos[j.Repo].PublicReports {
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

// serveQueue lists pending jobs; private repos' jobs are anonymized.
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
