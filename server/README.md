# gnoreplay-server

Replays gno.land mainnet's full history with the binary of each pushed commit and each PR on `gnolang/gno` and `gnolang/gno-fixes`, and reports every tx whose result would change as a commit status (`mainnet-replay`) linking to a page with the details.

The status is **advisory**: it is not a required check, so it never blocks a merge. It fails when a commit would change a tx result or a block compared to its base branch, and passes otherwise, including when only gas usage changes (the description says how many txs).

It works with a plain access token: it polls GitHub instead of receiving webhooks, and reports commit statuses rather than check runs (which only GitHub Apps can create).

## How a job runs

1. Every `github.poll_interval`, the poller reads the head of each tracked branch and the open PRs of each tracked repo, and enqueues a replay for each new head: a push to a branch, a new PR, or a push to a PR. The commit gets a `pending` status.
2. A worker fetches the commit into a bare mirror (with the token, so private repos work), and extracts it.
3. It copies [`gnoreplay/`](../gnoreplay/) into the checkout at `contribs/gnoreplay` and builds it there, so the replay runs that commit's application code.
4. It copies the golden chain data (blockstore + state DBs, via reflinks) into the job dir and runs the replay with `GNOROOT` set to the checkout (stdlibs are read from disk at runtime).
5. The report's diffs are classified against the latest completed replay of the base branch (for a push: the branch's previous commit):
   - **new** — diverges here, not on the base
   - **inherited** — diverges on the base too
   - **fixed** — diverges on the base, not here
6. The commit status is set to the outcome, linking to `/jobs/<id>`: the new divergences first, then the fixed ones. The full JSON report is at `/reports/<id>`.

On its first pass over a repo, the poller only records PRs that are already open (replaying the backlog would take days); branches are replayed, as the baselines PRs are compared against. To replay an older PR, or re-run anything:

```bash
gnoreplay-server -config config.toml enqueue gnolang/gno 1234     # a PR
gnoreplay-server -config config.toml enqueue gnolang/gno master   # a branch
```

A running server picks the job up from the shared queue.

## Queue

Persisted in SQLite (`<data_dir>/jobs.db`), so it survives restarts; jobs interrupted by a restart run again, and pushes made while the server was down are seen on its first poll.

Priority, highest first (the `rules` config; these are the defaults):

1. Commits on `gnolang/gno` `chain/mainnet`
2. PRs to `chain/mainnet`
3. Commits on `gnolang/gno` `master`
4. Commits on `gnolang/gno-fixes` `develop`
5. PRs to `master`
6. PRs to `gno-fixes` `develop`

Within a level, jobs run in arrival order. A new push to the same branch or PR supersedes the queued or running job for it (the old commit's status becomes `error`, "superseded"), so bursts of pushes don't pile up; closing a PR cancels its job. Jobs are not preempted by higher-priority arrivals.

`GET /queue` lists pending jobs; jobs of private repos are shown without repo, branch or SHA.

## Deployment

What the box needs:

- **A reference node**: a non-validator `gnoland` node on the mainnet binary, syncing continuously (see `misc/deployments/mainnet.gno.land/VALIDATOR.md`; its governance halts stop it during the initial sync — restart it each time). Every node stores per-height tx results in `state.db`: these are what replays are compared against.
- **The golden copy**: `scripts/refresh-golden.sh` (timer, e.g. every 4h) stops the node, reflinks its `blockstore.db` and `state.db` into a new snapshot, restarts it and atomically repoints `golden`. Use XFS or btrfs so copies are instant and free.
- **The server**: `go build` in this directory; config in `config.example.toml`. It runs on the host (not in a container), with git, and [microsandbox](https://microsandbox.dev) (`msb`, which needs KVM) for the job sandboxes in the example config. Each replay worker needs a 9 GB VM.
- **A GitHub token** in `github.token_file`, able to read the tracked repos' contents and pull requests and write their commit statuses:
  - fine-grained: repositories `gnolang/gno` (and `gnolang/gno-fixes`), permissions *Contents: read*, *Pull requests: read*, *Commit statuses: read and write* (the org may need to approve it);
  - or classic: `repo:status` is enough for public repos; `repo` for `gno-fixes`.

  Statuses are posted as the token's owner: use a bot account beyond a demo.

### Security

PR code is untrusted and runs on the box. Builds and replays run in microsandbox VMs that mount only the data dir, with a minimal environment; the replay VM has no network. The token never enters a VM: it goes to `git fetch` (on the host) through that command's environment only. Keep the token file outside `data_dir`.

Private repos' job pages and reports need the job's secret (`?k=…`), which only the commit status, visible to the repo's members, links to.

## Development

```bash
go test ./...
```

`TestServerEndToEnd` drives the whole pipeline — polling, queue, supersede and cancel, git fetch, build, chain data copy, replay, baseline classification, commit statuses, job pages and reports, private-repo links — with local git remotes, a fake GitHub and a stand-in `gnoreplay`.
