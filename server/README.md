# gnoreplay-server

Replays gno.land mainnet's full history with the binary of each pushed commit and each PR on `gnolang/gno` and `gnolang/gno-fixes`, and reports every tx whose result would change as a GitHub check run (`mainnet-replay`). The check is **advisory**: it concludes `success` or `neutral`, never `failure`.

The replay itself is [`contribs/gnoreplay`](https://github.com/gnolang/gno/tree/master/contribs/gnoreplay) in the gno repo; this server schedules it.

## How a job runs

1. A webhook (`push`, `pull_request` opened/synchronize, `check_run` rerequested) is matched against the priority rules and enqueued; a `queued` check run is created on the commit.
2. A worker fetches the commit into a bare mirror (with the app's installation token, so private repos work), and extracts it.
3. It builds `contribs/gnoreplay` from that tree. Commits that predate the tool get `job.gnoreplay_overlay` copied in.
4. It copies the golden chain data (blockstore + state DBs, via reflinks) into the job dir and runs the replay with `GNOROOT` set to the checkout (stdlibs are read from disk at runtime).
5. The report's diffs are classified against the latest completed replay of the base branch (for a push: the branch's previous commit):
   - **new** — diverges here, not on the base
   - **inherited** — diverges on the base too
   - **fixed** — diverges on the base, not here
6. The check run is completed with the new divergences first. Full JSON reports are served at `/reports/<job id>` for repos with `public_reports = true` only.

## Queue

Persisted in SQLite (`<data_dir>/jobs.db`), so it survives restarts; jobs interrupted by a restart run again.

Priority, highest first (the `rules` config; these are the defaults):

1. Commits on `gnolang/gno` `chain/mainnet`
2. PRs to `chain/mainnet`
3. Commits on `gnolang/gno` `master`
4. Commits on `gnolang/gno-fixes` `develop`
5. PRs to `master`
6. PRs to `gno-fixes` `develop`

Within a level, jobs run in arrival order. A new push to the same branch or PR supersedes the queued or running job for it (its check run is closed as `cancelled`), so bursts of pushes don't pile up. Jobs are not preempted by higher-priority arrivals.

`GET /queue` lists pending jobs; jobs of private repos are shown without repo, branch or SHA.

## Deployment

What the box needs:

- **A reference node**: a non-validator `gnoland` node on the mainnet binary, syncing continuously (see `misc/deployments/mainnet.gno.land/VALIDATOR.md`; its governance halts stop it during the initial sync — restart it each time). Every node stores per-height tx results in `state.db`: these are what replays are compared against.
- **The golden copy**: `scripts/refresh-golden.sh` (timer, e.g. every 4h) stops the node, reflinks its `blockstore.db` and `state.db` into a new snapshot, restarts it and atomically repoints `golden`. Use XFS or btrfs so copies are instant and free.
- **The server**: `Dockerfile` or `go build`; config in `config.example.toml`. It needs Go and git (jobs build gno), and `bwrap` if `run_sandbox` uses it.
- **A GitHub App** installed on `gnolang/gno` and `gnolang/gno-fixes`:
  - Permissions: *Checks: write*, *Contents: read*, *Pull requests: read*, *Metadata: read*.
  - Events: *Push*, *Pull request*, *Check run*.
  - Webhook URL: `<public_url>/webhook`, with a secret (`github.webhook_secret_file`).

### Security

PR code is untrusted and runs on the box. The server passes jobs only a minimal environment (no tokens: the fetch token goes to git through the environment of the fetch only), and `build_sandbox` / `run_sandbox` prefix the build and replay commands — run the replay at least without network or access to the server's secrets (the example uses bubblewrap). The GitHub App private key must not be readable from inside the sandbox.

`gno-fixes` results appear only in its own (private) check runs; its reports are never served.

## Development

```bash
go test ./...
```

`TestServerEndToEnd` drives the whole pipeline — signed webhooks, queue, git fetch, build, chain data copy, replay, baseline classification, check run and report serving — with a local git remote, a fake GitHub and a stand-in `gnoreplay`.
