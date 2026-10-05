package main

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const (
	stateQueued     = "queued"
	stateRunning    = "running"
	stateDone       = "done"
	stateFailed     = "failed"
	stateSuperseded = "superseded"
)

type Job struct {
	ID int64
	// Key identifies what the job checks: a branch or a PR. A newer job with
	// the same key supersedes older ones.
	Key            string
	Repo           string // "owner/name"
	Event          string // eventPush or eventPullRequest
	Branch         string // pushed branch, or the PR's base branch
	PR             int
	SHA            string
	InstallationID int64
	Priority       int
	State          string
	EnqueuedAt     time.Time
	CheckRunID     int64
	ReportPath     string
	Error          string
}

func jobKey(repo, event, branch string, pr int) string {
	if event == eventPullRequest {
		return fmt.Sprintf("%s#pr/%d", repo, pr)
	}
	return fmt.Sprintf("%s#branch/%s", repo, branch)
}

type Queue struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	key             TEXT    NOT NULL,
	repo            TEXT    NOT NULL,
	event           TEXT    NOT NULL,
	branch          TEXT    NOT NULL,
	pr              INTEGER NOT NULL DEFAULT 0,
	sha             TEXT    NOT NULL,
	installation_id INTEGER NOT NULL,
	priority        INTEGER NOT NULL,
	state           TEXT    NOT NULL,
	enqueued_at     INTEGER NOT NULL,
	finished_at     INTEGER NOT NULL DEFAULT 0,
	check_run_id    INTEGER NOT NULL DEFAULT 0,
	report_path     TEXT    NOT NULL DEFAULT '',
	error           TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS jobs_state ON jobs (state, priority, enqueued_at);
CREATE INDEX IF NOT EXISTS jobs_key ON jobs (key, state);
`

func openQueue(path string) (*Queue, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// One connection: every write is a short transaction, and this keeps
	// claims serialized without relying on SQLite locking semantics.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Queue{db: db}, nil
}

func (q *Queue) Close() error { return q.db.Close() }

// Enqueue adds j as queued and supersedes queued or running jobs with the
// same key, which it returns (their state is already updated).
func (q *Queue) Enqueue(j *Job) (superseded []*Job, err error) {
	tx, err := q.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	superseded, err = scanJobs(tx.Query(`SELECT `+jobColumns+` FROM jobs
		WHERE key = ? AND state IN (?, ?)`, j.Key, stateQueued, stateRunning))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE jobs SET state = ?, finished_at = ? WHERE key = ? AND state IN (?, ?)`,
		stateSuperseded, time.Now().UnixNano(), j.Key, stateQueued, stateRunning); err != nil {
		return nil, err
	}

	if j.EnqueuedAt.IsZero() {
		j.EnqueuedAt = time.Now()
	}
	j.State = stateQueued
	res, err := tx.Exec(`INSERT INTO jobs (key, repo, event, branch, pr, sha, installation_id, priority, state, enqueued_at, check_run_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.Key, j.Repo, j.Event, j.Branch, j.PR, j.SHA, j.InstallationID, j.Priority, j.State, j.EnqueuedAt.UnixNano(), j.CheckRunID)
	if err != nil {
		return nil, err
	}
	if j.ID, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	for _, s := range superseded {
		s.State = stateSuperseded
	}
	return superseded, tx.Commit()
}

// Claim marks the next job as running and returns it: lowest priority value
// first, then oldest. It returns nil if nothing is queued.
func (q *Queue) Claim() (*Job, error) {
	tx, err := q.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	jobs, err := scanJobs(tx.Query(`SELECT `+jobColumns+` FROM jobs WHERE state = ?
		ORDER BY priority, enqueued_at, id LIMIT 1`, stateQueued))
	if err != nil || len(jobs) == 0 {
		return nil, err
	}
	j := jobs[0]
	if _, err := tx.Exec(`UPDATE jobs SET state = ? WHERE id = ?`, stateRunning, j.ID); err != nil {
		return nil, err
	}
	j.State = stateRunning
	return j, tx.Commit()
}

// Finish records the outcome of a running job. It is a no-op if the job was
// superseded meanwhile, and reports whether the outcome was recorded.
func (q *Queue) Finish(id int64, state, reportPath, errMsg string) (bool, error) {
	res, err := q.db.Exec(`UPDATE jobs SET state = ?, report_path = ?, error = ?, finished_at = ?
		WHERE id = ? AND state = ?`, state, reportPath, errMsg, time.Now().UnixNano(), id, stateRunning)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (q *Queue) SetCheckRun(id, checkRunID int64) error {
	_, err := q.db.Exec(`UPDATE jobs SET check_run_id = ? WHERE id = ?`, checkRunID, id)
	return err
}

// Baseline returns the latest successful push job on repo/branch, or nil.
func (q *Queue) Baseline(repo, branch string) (*Job, error) {
	jobs, err := scanJobs(q.db.Query(`SELECT `+jobColumns+` FROM jobs
		WHERE repo = ? AND event = ? AND branch = ? AND state = ?
		ORDER BY finished_at DESC LIMIT 1`, repo, eventPush, branch, stateDone))
	if err != nil || len(jobs) == 0 {
		return nil, err
	}
	return jobs[0], nil
}

func (q *Queue) Get(id int64) (*Job, error) {
	jobs, err := scanJobs(q.db.Query(`SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, sql.ErrNoRows
	}
	return jobs[0], nil
}

// ByCheckRun finds the job that owns a check run.
func (q *Queue) ByCheckRun(checkRunID int64) (*Job, error) {
	jobs, err := scanJobs(q.db.Query(`SELECT `+jobColumns+` FROM jobs WHERE check_run_id = ?
		ORDER BY id DESC LIMIT 1`, checkRunID))
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, sql.ErrNoRows
	}
	return jobs[0], nil
}

// Requeue puts jobs left running by a previous process back in the queue.
func (q *Queue) Requeue() (int64, error) {
	res, err := q.db.Exec(`UPDATE jobs SET state = ? WHERE state = ?`, stateQueued, stateRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Pending lists queued and running jobs in the order they run.
func (q *Queue) Pending() ([]*Job, error) {
	return scanJobs(q.db.Query(`SELECT `+jobColumns+` FROM jobs WHERE state IN (?, ?)
		ORDER BY state = ? DESC, priority, enqueued_at, id`, stateRunning, stateQueued, stateRunning))
}

const jobColumns = `id, key, repo, event, branch, pr, sha, installation_id, priority, state, enqueued_at, check_run_id, report_path, error`

func scanJobs(rows *sql.Rows, err error) ([]*Job, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []*Job
	for rows.Next() {
		j := &Job{}
		var enq int64
		if err := rows.Scan(&j.ID, &j.Key, &j.Repo, &j.Event, &j.Branch, &j.PR, &j.SHA, &j.InstallationID,
			&j.Priority, &j.State, &enq, &j.CheckRunID, &j.ReportPath, &j.Error); err != nil {
			return nil, err
		}
		j.EnqueuedAt = time.Unix(0, enq)
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
