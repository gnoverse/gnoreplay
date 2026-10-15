# Deploying on Hetzner Cloud

One always-on **coordinator** server runs the server and keeps a recent copy of mainnet's chain data, from [snapshots.gno.land](https://snapshots.gno.land). Replays run on **worker** servers that the coordinator creates. Hetzner bills each server by the started hour from its creation, so a worker isn't deleted when its replay ends: it runs the next queued replay, reinstalled from scratch first, and is deleted once idle in the last 5 minutes of its billing hour.

| | Type | Cost |
|---|---|---|
| Coordinator | 1 vCPU, 2 GB, 40 GB disk (`cpx12`) | €13.49/month |
| Worker | 16 GB of RAM (`[hetzner] server_type`, e.g. `cpx42`) | €0.13/h for a `cpx42`, by the started hour; at most `workers` at once; a replay takes ~2 h |

## 1. Project setup (once)

In a Hetzner Cloud project of its own (its API token can manage every server in the project), with the [`hcloud`](https://github.com/hetznercloud/cli) CLI and an API token of the project (*Security → API tokens*, read & write):

```bash
hcloud context create gnoreplay

# A private network of their own: PR code runs on the workers, and can reach
# anything in it.
hcloud network create --name gnoreplay --ip-range 10.0.0.0/16
hcloud network add-subnet gnoreplay --type cloud --network-zone eu-central --ip-range 10.0.0.0/24

# Workers accept no inbound connections (a firewall with no inbound rules); they
# need outbound access for Go and its modules, and the coordinator over the
# private network (which firewalls don't filter).
hcloud firewall create --name gnoreplay-worker
hcloud firewall apply-to-resource gnoreplay-worker --type label_selector --label-selector gnoreplay=worker

# Your SSH key: its ID goes in the config (without one, Hetzner emails a root
# password for every worker). So does the network's.
hcloud ssh-key create --name <you> --public-key-from-file ~/.ssh/id_ed25519.pub
hcloud ssh-key list
hcloud network list
```

The server uses an API token of the project too (read & write). Hetzner has no spending cap: see [Limits on worker servers](#limits-on-worker-servers).

## 2. Coordinator

```bash
hcloud firewall create --name gnoreplay-coordinator
hcloud firewall add-rule gnoreplay-coordinator --direction in --protocol tcp --port 22 --source-ips 0.0.0.0/0 --source-ips ::/0
hcloud firewall add-rule gnoreplay-coordinator --direction in --protocol tcp --port 80 --source-ips 0.0.0.0/0 --source-ips ::/0
hcloud firewall add-rule gnoreplay-coordinator --direction in --protocol tcp --port 443 --source-ips 0.0.0.0/0 --source-ips ::/0
hcloud server create --name gnoreplay --type cpx12 --image ubuntu-24.04 --location hel1 \
  --network gnoreplay --ssh-key <you> --firewall gnoreplay-coordinator
```

Inbound: 22 (SSH) and 80/443 (the result pages, through Caddy); port 8081 is only bound to the private address. Use the workers' `location` (or one in the same network zone).

On the server:

```bash
apt-get update && apt-get install -y git caddy jq zstd
curl -fsSL https://go.dev/dl/go1.26.1.linux-amd64.tar.gz | tar -C /usr/local -xz
useradd --system --create-home --home-dir /var/lib/gnoreplay gnoreplay
mkdir -p /opt/gnoreplay/bin /etc/gnoreplay
git clone https://github.com/gnoverse/gnoreplay /opt/gnoreplay/src && ln -s src/gnoreplay /opt/gnoreplay/gnoreplay && ln -s src/server /opt/gnoreplay/server
(cd /opt/gnoreplay/server && /usr/local/go/bin/go build -o /opt/gnoreplay/bin/gnoreplay-server .)
```

**Chain data.** Replays run the blocks a mainnet node stores in its `blockstore.db`, and compare against the tx results in its `state.db`. `gnoreplay-golden` ([`fetch-golden.sh`](../server/scripts/fetch-golden.sh)) downloads them from the newest snapshot on snapshots.gno.land every 12 hours: each snapshot is ~2 GB, served by the gno.land team, so not more often. If the download fails, replays keep the last copy.

```bash
cd /var/lib/gnoreplay
curl -fsS -O https://snapshots.gno.land/genesis.json -O https://snapshots.gno.land/genesis.json.sha256
sha256sum -c genesis.json.sha256
chown -R gnoreplay: /var/lib/gnoreplay
cp /opt/gnoreplay/src/deploy/systemd/* /etc/systemd/system/
systemctl daemon-reload
systemctl start gnoreplay-golden && systemctl enable --now gnoreplay-golden.timer
```

Or from a reference node of the coordinator's own, without depending on snapshots.gno.land: it takes ~2 GB of the coordinator's RAM, and must be upgraded at each mainnet governance halt, like any node. Syncing it from genesis loads 3.26M genesis accounts at once, which needs far more than 4 GB: seed `/var/lib/gnoland` instead with the whole data dir (`config/`, `secrets/`, `db/`) of a mainnet node that is already past genesis, started with the same `genesis.json`. The node then only syncs the blocks since. Mainnet's node settings are in `misc/deployments/mainnet.gno.land/VALIDATOR.md`; keep the RPC on `127.0.0.1:26657`. [`refresh-golden.sh`](../server/scripts/refresh-golden.sh) stops the node for each copy.

```bash
# The binary mainnet runs: the last row of misc/deployments/mainnet.gno.land/upgrades.json.
git clone --depth 1 --branch chain/mainnet https://github.com/gnolang/gno /opt/gno
(cd /opt/gno && CGO_ENABLED=0 /usr/local/go/bin/go build -o /opt/gnoreplay/bin/gnoland ./gno.land/cmd/gnoland)
chown -R gnoreplay: /var/lib/gnoland
systemctl enable --now gnoland
mkdir -p /etc/systemd/system/gnoreplay-golden.service.d
printf '[Service]\nExecStart=\nExecStart=/opt/gnoreplay/server/scripts/refresh-golden.sh\n' > /etc/systemd/system/gnoreplay-golden.service.d/node.conf
systemctl daemon-reload
# Once the node has caught up with mainnet:
systemctl start gnoreplay-golden
```

**Server.** `/etc/gnoreplay/config.toml` from [`config.hetzner.example.toml`](../server/config.hetzner.example.toml) (the worker server type, the network and SSH key IDs, the coordinator's private address for `[hetzner] listen/url`: `hcloud server describe gnoreplay`), then the secrets, readable by `gnoreplay` only:

| File | Content |
|---|---|
| `/etc/gnoreplay/github-token` | a read-only GitHub token (see the server README) |
| `/etc/gnoreplay/hetzner-token` | the project's API token |
| `/etc/gnoreplay/github-oauth-secret` | if private repos are tracked: the client secret of the sign-in OAuth app (below) |
| `/etc/gnoreplay/<repo>-viewers` | if private repos are tracked: who may see each one's results, from [`scripts/sync-viewers.sh`](../server/scripts/sync-viewers.sh) (rerun when its collaborators change) |

**Private repos' results** are shown to the users their `viewers_file` lists, signed in with GitHub. Register an OAuth app (any account can, e.g. the server's GitHub account: *Settings → Developer settings → OAuth Apps → New OAuth App*; the organization needn't approve it, as it reads no organization data), with homepage `https://<host>` and callback URL `https://<host>/auth/callback`. Put its client ID in `[github.oauth]` and a generated client secret in the file above. It asks users for no scopes.

```bash
# The watchdog first: it deletes workers on a job for over 4h, or idle for over
# 65 min, even if the server misbehaves.
systemctl enable --now gnoreplay-watchdog.timer
systemctl enable --now gnoreplay-server
# Replay a PR on demand (PRs open before the first poll are not replayed):
sudo -u gnoreplay /opt/gnoreplay/bin/gnoreplay-server -config /etc/gnoreplay/config.toml enqueue gnolang/gno 1234
```

**HTTPS.** `/etc/caddy/Caddyfile`, with a DNS name for the server (`<ip with dashes>.sslip.io` works without DNS):

```
replay.example.org {
	reverse_proxy 127.0.0.1:8080
}
```

## Limits on worker servers

Hetzner has no spending cap, so the limits are ours. A worker runs one replay at a time, for at most **4 hours** (`[hetzner] max_age`), and stays idle for at most the rest of its billing hour. Each limit is enforced by several mechanisms, independent of each other:

| | Enforced by | When |
|---|---|---|
| 1 | the job: no report within `job.run_timeout` (3h30m) → the job fails, and its worker is idle (reinstalled before any reuse) | at 3h30m |
| 2 | the server: every minute, deletes a worker that has run its job for longer than `max_age` (the job fails), and an idle worker in the last 5 minutes of its billing hour | ≤ 4h01m on a job; idle < 1h |
| 3 | `gnoreplay-watchdog.timer` on the coordinator ([`watchdog.sh`](watchdog.sh), a separate process from the server, needing only the API token), from the workers' labels: deletes a worker on a job for over 4h, or idle for over 65 min | ≤ 4h05m; ≤ 70 min idle |
| 4 | the [`worker-watchdog`](../.github/workflows/worker-watchdog.yml) GitHub workflow (the same script), for when the coordinator itself is down: set an `HCLOUD_TOKEN` repository secret to enable it | every 15 min (GitHub may delay scheduled runs) |

Workers also power themselves off 4h into a replay, which stops them working but not billing: only deletion does.

Beyond that, spending is bounded by `workers` (at most `workers` servers at once, each billed by the started hour) and by the account's server limit; set a usage alert too, if the account has one.

To stop everything at once: `systemctl stop gnoreplay-server && hcloud server list -l gnoreplay=worker -o noheader -o columns=id | xargs -r hcloud server delete` (stopping the server alone leaves workers to the next start, see below).

## Operating

- Worker servers are named `gnoreplay-worker-<random>` and labelled `gnoreplay=worker`, `gnoreplay-job=<id>` while they run a job, and `gnoreplay-since=<Unix time>` (when they got their job, or became idle). A worker whose replay ends (done, failed, superseded or timed out) becomes idle: the next replay reinstalls it with its own startup script (Hetzner's rebuild, which wipes its disk), so that nothing of a replay, whose PR code ran as root, reaches the next one. Idle workers are deleted in the last 5 minutes of their billing hour.
- Restarting the server (e.g. to deploy) keeps running replays: it leaves their workers running, and the next process resumes them, within the same time limits; it takes the other workers as idle ones. Deploy within a few minutes of stopping: that is how long workers retry posting a result to a stopped server.
- After a change to how results are rendered, run `gnoreplay-server -config /etc/gnoreplay/config.toml rerender` (as the `gnoreplay` user) to update the pages of past replays.
- A worker's log is in its job's page when it fails, and on the worker in `/root/gnoreplay/<job>/worker.log` while it runs (SSH in with your key, over its public address).
- The chain data refreshes are logged in `journalctl -u gnoreplay-golden`: a failed one leaves replays on the previous copy.
