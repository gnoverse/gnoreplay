# AGENTS.md — AI agent guide for gnoreplay

gnoreplay checks whether a change to [gnolang/gno](https://github.com/gnolang/gno) would execute gno.land mainnet's history differently: every recorded block is re-executed with the changed code, and every tx whose result differs from the one recorded on chain is reported. Read [README.md](README.md), [server/README.md](server/README.md) and [deploy/README.md](deploy/README.md) first; this file adds what they don't say: why things are the way they are, and the traps.

## Layout

| Path | What |
|---|---|
| `gnoreplay/` | The replay tool. Not buildable on its own: its `go.mod` has `replace github.com/gnolang/gno => ../..`, so it is copied into a gno checkout at `contribs/gnoreplay` and built there, against the tree under test. |
| `server/` | The coordinator: polls GitHub, queues replays (SQLite), runs each on a worker (a disposable DigitalOcean droplet, or a local microsandbox VM), classifies the report against the base branch's, serves the results. |
| `server/worker.sh` | The worker's cloud-init script (a Go template): downloads its inputs from the coordinator, builds the tool, replays, posts the report back. |
| `server/page.html` | All HTML templates (embedded). |
| `server/scripts/` | `refresh-golden.sh` (chain snapshot, systemd timer), `sync-viewers.sh` (who may see a private repo's results). |
| `deploy/` | DigitalOcean deployment, systemd units, `watchdog.sh` (deletes workers older than 4h, independently of the server). |

## Build & test

```bash
cd server && go test ./...                                   # ~10s
cd server && CGO_ENABLED=1 go test -race ./...               # before deploying
cd server && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o gnoreplay-server .
```

The tool's tests only run inside a gno checkout: copy `gnoreplay/` to `<gno>/contribs/gnoreplay`, then `go test .` there (and delete the copy). `go vet` errors about `../../go.mod` when run in `gnoreplay/` itself are expected.

Server tests drive real code against fakes, not mocks of it: `fakeGitHub` (server_test.go), `fakeMachines` runs `worker.sh` with bash as a droplet's cloud-init would (remote_test.go), `fakeOAuth` (auth_test.go), and `fakeTool` stands in for the replay tool in local git remotes. `TestServerEndToEnd` covers the whole pipeline; keep it passing and extend it rather than adding parallel ones.

## Design decisions — don't undo these without discussing

- **Read-only on GitHub.** The server never writes to GitHub (no statuses, comments, check runs): its API client's transport refuses anything but GET/HEAD (`TestTokenClientReadOnly`), and git only fetches. Results live on the server's own pages. Never post anything to GitHub on the user's behalf without asking.
- **PRs are replayed merged into their base**: GitHub's test merge (`merge_commit_sha`, fetchable shallow by SHA; parents are [base, head]), as CI tests it. A PR's head alone shows whatever its branch lacks from the base as divergences (a branch 41 commits behind master couldn't decode newer mainnet txs). On a conflict, or if GitHub hasn't computed the merge, the head is replayed and the job's `ReplayNote` says why.
- **Skipped PRs** (`server/skip.go`): a PR whose changed files all `cannotAffectNode` is recorded as skipped. Never add anything under `gnovm/stdlibs/` to the skip rules, not even tests or READMEs: the keeper loads stdlibs with `gno.MPStdlibAll` and stores the whole package, so they are in the app state. `gnovm/tests/stdlibs` is linked into the node (via `gnovm/pkg/packages`). New skip dirs must be absent from both `go list -deps ./gno.land/cmd/gnoland` and the replay tool's deps. Branch pushes and `enqueue` always run.
- **Private repos** (gno-fixes): results only for users in the repo's `viewers_file`, signed in with GitHub. The OAuth app asks for no scopes (identity only; no org approval needed). Access isn't asked of GitHub because its collaborator API needs push access, and returns 404 to read-only tokens, even for actual collaborators. Viewers are matched by numeric user ID: a login can be renamed, then registered by someone else. `scripts/sync-viewers.sh`, run by someone with push access, writes the file.
- **Restarts keep running replays.** Stopping cancels with `errShutdown`: jobs on worker machines are left running, their token hash and start time are in the `sessions` table, results are saved to the job dir before being acknowledged, and workers retry uploads and downloads for minutes; the next process resumes them (`TestRemoteResume`). Anything new that tears down jobs on cancellation must check `shuttingDown(ctx)`.
- **Workers can't run past 4h**, enforced independently four times: the job's `run_timeout` (must stay below `max_age`), the server's reaper (every 5 min), `deploy/watchdog.sh` on a systemd timer, and the `worker-watchdog` GitHub workflow; workers also power themselves off. Keep all of them when changing worker lifecycle code.
- **Outcome**: a commit *diverges* when a tx result or a block (app hash, validator updates) differs where the base branch's latest replay matched; gas-only differences pass. A divergence with every tx result unchanged is reported as an app-hash-only change (typically a stdlib change, which alters the genesis state).
- **Rendering is stored**: a job's page body is rendered to `<report>.md` when it finishes (classification needs the baseline at that time). After changing rendering, run `gnoreplay-server rerender` to update past pages.
- **Database migrations** are additive: new `jobs` columns go in both the `CREATE TABLE` and `addedColumns` (`TestMigrate`).

## Things learned the hard way

- **Replay speed.** The replay is sequential: one core runs it, the other mostly runs Go's GC (~58% of CPU), driven by the node's per-block commit (tm2's B+tree copy-on-write) over the ~2.6 GB live heap left by genesis. Measured and rejected: raising `GOGC` (400 or off: ~30% slower from page faults), bigger pebble caches (no gain), prefetching source blocks (reads are <1%). pebble's `DisableWAL` breaks tm2 (it errors on synced writes); the replay's app DB uses `noSyncFS` instead. The real lever left is sharding history across workers from checkpoints. Replay time varies ±20% between identical runs on different droplets: never compare a single before/after pair.
- **Memory.** The first commit after genesis (3.26M accounts) peaks above 8 GB with the default GC: workers set `GOMEMLIMIT` (12GiB on 16 GB droplets; 7GiB in a 9 GB VM).
- **Log-only diffs are noise** (Go stack traces whose depth differs because the replay calls the app directly, goroutine IDs, `%#v` heap pointers in errors); they are counted, not classified.
- **A gas cliff.** Mainnet tx at height 214792 (gnoswap `CollectFee` ×2 + `CollectReward` ×2) used 246,051,054 of 246,051,805 gas: any change adding more than 751 gas on that path makes it run out of gas, and everything after it diverges. Two changes have hit it already.
- **Golden snapshots** of the reference node refresh every 4h, so replays started in the same window cover the same heights, and a PR replayed against a newer snapshot than its base's is classified as *partial*.
- `doctl ... --format Created` is not a valid column: use `-o json` and `created_at`.

## Deploying

See [deploy/README.md](deploy/README.md). In short: build the server binary locally (above), copy it and a `git archive` of the repo to the coordinator (`/opt/gnoreplay/bin/`, `/opt/gnoreplay/src/`), `systemctl restart gnoreplay-server` (safe: running replays resume). Changes to `gnoreplay/` only need the source copy updated, no restart: each job copies the tool when it starts. Production config is validated strictly by `loadExample` in tests; check a config change the same way before installing it.

## Conventions

- Conventional commits with a scope: `feat(server):`, `fix(deploy):`, `perf(gnoreplay):`. Commit messages say why, and what was measured.
- No `Co-authored-by` or other agent credit in commits.
- Format with gofmt/gofumpt. Comments explain why, briefly, in the style of the surrounding code.
- Markdown for GitHub: one paragraph or bullet per line, no hard wrapping.
- Never print secrets (tokens, OAuth secret, viewer files' contents aside from counts) in logs, commands or output; pass git credentials through the environment, not argv.
