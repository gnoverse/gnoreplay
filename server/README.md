# gnoreplay-server

Replays gno.land mainnet's full history with the binary of each pushed commit and each PR on `gnolang/gno` and `gnolang/gno-fixes`, and lists every tx whose result would change.

**It only reads from GitHub.** It polls instead of receiving webhooks, and its access token only needs read permissions; the GitHub client refuses any request other than a read, whatever the token allows. Results are published on the server's own pages, nowhere else:

| Page | Shows |
|---|---|
| `/` | the latest replays and their results |
| `/<owner>/<repo>/pull/<n>` | the latest replay of a PR (the same path as on GitHub) |
| `/<owner>/<repo>/tree/<branch>` | the latest replay of a branch |
| `/jobs/<id>` | one replay: what diverges, transaction by transaction |
| `/reports/<id>` | its full JSON report |
| `/queue` | pending replays |

A commit *diverges* when it would change a tx result or a block compared to its base branch; gas-only changes don't count, but their number is shown.

## How a job runs

1. Every `github.poll_interval`, the poller reads the head of each tracked branch and the open PRs of each tracked repo, and enqueues a replay for each new head: a push to a branch, a new PR, or a push to a PR.
2. A worker fetches the commit into a bare mirror (with the token, so private repos work), and extracts it.
3. It copies [`gnoreplay/`](../gnoreplay/) into the checkout at `contribs/gnoreplay` and builds it there, so the replay runs that commit's application code.
4. It copies the golden chain data (blockstore + state DBs, via reflinks) into the job dir and runs the replay with `GNOROOT` set to the checkout (stdlibs are read from disk at runtime).
5. The report's diffs are classified against the latest completed replay of the base branch (for a push: the branch's previous commit):
   - **new** — diverges here, not on the base
   - **inherited** — diverges on the base too
   - **fixed** — diverges on the base, not here
6. The result is stored: `/jobs/<id>` lists the new divergences first, then the fixed ones.

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

Within a level, jobs run in arrival order. A new push to the same branch or PR supersedes the queued or running job for it, so bursts of pushes don't pile up; closing a PR cancels its job. Jobs are not preempted by higher-priority arrivals.

## Deployment

What the box needs:

- **A reference node**: a non-validator `gnoland` node on the mainnet binary, syncing continuously (see `misc/deployments/mainnet.gno.land/VALIDATOR.md`; its governance halts stop it during the initial sync — restart it each time). Every node stores per-height tx results in `state.db`: these are what replays are compared against.
- **The golden copy**: `scripts/refresh-golden.sh` (timer, e.g. every 4h) stops the node, reflinks its `blockstore.db` and `state.db` into a new snapshot, restarts it and atomically repoints `golden`. Use XFS or btrfs so copies are instant and free.
- **The server**: `go build` in this directory; config in `config.example.toml`. It runs on the host (not in a container), with git, and [microsandbox](https://microsandbox.dev) (`msb`, which needs KVM) for the job sandboxes in the example config. Each replay worker needs a 9 GB VM.
- **A read-only GitHub token** in `github.token_file`, for the GitHub API's rate limit and for private repos:
  - fine-grained (preferred): repositories `gnolang/gno` and `gnolang/gno-fixes`, permissions *Contents: read-only* and *Pull requests: read-only* (the org may need to approve it);
  - or classic with **no scopes**, if only public repos are tracked: it can read them, and nothing else. (A classic token that can read a private repo needs the `repo` scope, which can also write: don't.)
- **A viewer key** in `viewer_key_file`, if private repos are tracked: anyone who opens a page once with `?key=<key>` can see their results (it is kept in a cookie). Without it, they are not served at all.

### Security

PR code is untrusted and runs on the box. Builds and replays run in microsandbox VMs that mount only the data dir, with a minimal environment; the replay VM has no network. The token never enters a VM: it goes to `git fetch` (on the host) through that command's environment only. Keep the token and viewer key files outside `data_dir`.

The server never writes to GitHub: its client refuses any request other than `GET`/`HEAD` before it leaves the process (`TestTokenClientReadOnly`), and git only fetches.

## Development

```bash
go test ./...
```

`TestServerEndToEnd` drives the whole pipeline — polling, queue, supersede and cancel, git fetch, build, chain data copy, replay, baseline classification, result pages and reports, the viewer key — with local git remotes, a fake GitHub and a stand-in `gnoreplay`.

`TestTokenClientLive` reads `gnolang/gno` through the real API when `GNOREPLAY_LIVE_TOKEN` is set.
