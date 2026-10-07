package main

import (
	"context"
	"fmt"
	"time"
)

// pollLoop polls GitHub every interval until ctx is done.
func (s *Server) pollLoop(ctx context.Context) {
	for {
		for _, repo := range s.cfg.repos() {
			if err := s.pollRepo(ctx, repo); err != nil && ctx.Err() == nil {
				s.logger.Error("poll", "repo", repo, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.GitHub.PollInterval.Duration):
		}
	}
}

// pollRepo compares repo's tracked branch heads and open PRs with the heads
// seen last time, and enqueues a replay for each new head: a push, a new PR,
// or a push to a PR. Jobs of PRs that were closed are cancelled.
func (s *Server) pollRepo(ctx context.Context, repo string) error {
	synced, err := s.queue.Synced(repo)
	if err != nil {
		return err
	}
	heads, err := s.queue.Heads(repo)
	if err != nil {
		return err
	}

	// Collect every current head first: a failed API call must abort the
	// pass before anything is treated as gone.
	type head struct {
		event, branch string
		pr            int
		sha           string
		title, author string
	}
	var current []head
	hasPRRules := false
	for _, r := range s.cfg.Rules {
		switch {
		case r.Repo != repo:
		case r.Event == eventPush:
			sha, err := s.gh.BranchHead(ctx, repo, r.Branch)
			if err != nil {
				return fmt.Errorf("branch %s: %w", r.Branch, err)
			}
			current = append(current, head{event: eventPush, branch: r.Branch, sha: sha})
		case r.Event == eventPullRequest:
			hasPRRules = true
		}
	}
	if hasPRRules {
		prs, err := s.gh.OpenPulls(ctx, repo)
		if err != nil {
			return fmt.Errorf("pull requests: %w", err)
		}
		for _, pr := range prs {
			if _, ok := s.cfg.priority(repo, eventPullRequest, pr.Base); ok {
				current = append(current, head{eventPullRequest, pr.Base, pr.Number, pr.HeadSHA, pr.Title, pr.Author})
			}
		}
	}

	seen := map[string]bool{}
	for _, h := range current {
		key := jobKey(repo, h.event, h.branch, h.pr)
		seen[key] = true
		if heads[key] == h.sha {
			continue
		}
		// On the first pass, PRs that are already open are only recorded:
		// replaying the whole backlog would take days (`enqueue` replays one
		// on demand). Branches are always replayed, as the baselines PRs are
		// compared against.
		switch {
		case h.event == eventPush:
			if err := s.enqueueFor(repo, h.event, h.branch, h.pr, h.sha); err != nil {
				return err
			}
		case synced:
			if err := s.enqueuePull(ctx, repo, h.branch, h.pr, h.sha); err != nil {
				return err
			}
		}
		if err := s.queue.SetHead(key, repo, h.sha); err != nil {
			return err
		}
	}
	// Titles go on the PRs' jobs: new ones, those queued with the enqueue
	// command, and renamed PRs'.
	for _, h := range current {
		if h.event != eventPullRequest {
			continue
		}
		if err := s.queue.SetPRInfo(jobKey(repo, h.event, h.branch, h.pr), h.title, h.author); err != nil {
			return err
		}
	}

	// Closed PRs, PRs retargeted to an untracked branch, branches no longer
	// tracked.
	for key := range heads {
		if seen[key] {
			continue
		}
		cancelled, err := s.queue.CancelKey(key)
		if err != nil {
			return err
		}
		for _, j := range cancelled {
			s.cancel(j.ID)
			s.logger.Info("cancelled", "job", j.ID, "key", key, "reason", "no longer tracked")
		}
		if err := s.queue.DeleteHead(key); err != nil {
			return err
		}
	}

	if !synced {
		return s.queue.MarkSynced(repo)
	}
	return nil
}

// enqueuePull queues a PR's new head, unless none of the PR's changes can
// affect the node: then it is recorded as skipped.
func (s *Server) enqueuePull(ctx context.Context, repo, base string, pr int, sha string) error {
	files, err := s.gh.PullFiles(ctx, repo, pr)
	if err != nil {
		return fmt.Errorf("files of PR %d: %w", pr, err)
	}
	reason := skipReason(files)
	if reason == "" {
		return s.enqueueFor(repo, eventPullRequest, base, pr, sha)
	}
	j, err := s.newJob(repo, eventPullRequest, base, pr, sha)
	if err != nil {
		return err
	}
	superseded, err := s.queue.Skip(j, reason)
	if err != nil {
		return err
	}
	s.logger.Info("skipped", "job", j.ID, "key", j.Key, "sha", sha, "reason", reason)
	for _, old := range superseded {
		s.cancel(old.ID)
	}
	return nil
}

func (s *Server) enqueueFor(repo, event, branch string, pr int, sha string) error {
	j, err := s.newJob(repo, event, branch, pr, sha)
	if err != nil {
		return err
	}
	return s.enqueue(j)
}

func (s *Server) newJob(repo, event, branch string, pr int, sha string) (*Job, error) {
	prio, ok := s.cfg.priority(repo, event, branch)
	if !ok {
		return nil, fmt.Errorf("no rule for %s %s %s", repo, event, branch)
	}
	return &Job{
		Key: jobKey(repo, event, branch, pr), Repo: repo, Event: event, Branch: branch,
		PR: pr, SHA: sha, Priority: prio,
	}, nil
}
