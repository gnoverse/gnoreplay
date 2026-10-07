# Deploying on DigitalOcean

One always-on **coordinator** droplet runs the server, a mainnet reference node and its chain snapshots. Each replay runs on its own **worker** droplet, created by the coordinator and deleted when the replay ends.

| | Size | Cost |
|---|---|---|
| Coordinator | Basic `s-2vcpu-4gb` | $24/mo |
| Worker | Memory-Optimized `m-2vcpu-16gb` | $0.125/h, about $0.25–0.30 per replay; at most `workers` at once |

## 1. Account setup (once)

With [`doctl`](https://docs.digitalocean.com/reference/doctl/) authenticated:

```bash
REGION=fra1

# A VPC of their own: PR code runs on the workers, and can reach anything in it.
doctl vpcs create --name gnoreplay --region $REGION --ip-range 10.120.0.0/20

# Workers accept no inbound connections; they need outbound access for Go
# and its modules, and the coordinator over the VPC.
doctl compute firewall create --name gnoreplay-worker --tag-names gnoreplay-worker \
  --outbound-rules "protocol:tcp,ports:all,address:0.0.0.0/0,address:::/0 protocol:udp,ports:all,address:0.0.0.0/0,address:::/0"

# Your SSH key (its fingerprint goes in the config: without one, DigitalOcean
# emails a root password for every worker).
doctl compute ssh-key list
```

API token for the server: *API → Generate New Token → Custom scopes*: `droplet` (create, read, delete) and `tag` (create, read).

Set a spend alert too (*Billing → Spend alerts*), and keep the account's droplet limit low: DigitalOcean has no spending cap. See [Limits on worker droplets](#limits-on-worker-droplets).

## 2. Coordinator

```bash
doctl compute droplet create gnoreplay --region $REGION --size s-2vcpu-4gb \
  --image ubuntu-24-04-x64 --vpc-uuid <vpc id> --ssh-keys <fingerprint> --wait
```

Its firewall: inbound 22 (SSH) and 80/443 (the result pages, through Caddy); port 8081 is only bound to the private address.

On the droplet:

```bash
apt-get update && apt-get install -y git caddy
curl -fsSL https://go.dev/dl/go1.26.1.linux-amd64.tar.gz | tar -C /usr/local -xz
useradd --system --create-home --home-dir /var/lib/gnoreplay gnoreplay
mkdir -p /opt/gnoreplay/bin /etc/gnoreplay /var/lib/gnoland
git clone https://github.com/gnoverse/gnoreplay /opt/gnoreplay/src && ln -s src/gnoreplay /opt/gnoreplay/gnoreplay && ln -s src/server /opt/gnoreplay/server
(cd /opt/gnoreplay/server && /usr/local/go/bin/go build -o /opt/gnoreplay/bin/gnoreplay-server .)
# The binary mainnet runs: the last row of misc/deployments/mainnet.gno.land/upgrades.json.
git clone --depth 1 --branch chain/mainnet https://github.com/gnolang/gno /opt/gno
(cd /opt/gno && CGO_ENABLED=0 /usr/local/go/bin/go build -o /opt/gnoreplay/bin/gnoland ./gno.land/cmd/gnoland)
```

**Chain data.** Syncing a node from genesis loads 3.26M genesis accounts at once, which needs far more than 4 GB. Seed `/var/lib/gnoland` instead with the whole data dir (`config/`, `secrets/`, `db/`) of a mainnet node that is already past genesis, and put the `genesis.json` it was started with in `/var/lib/gnoreplay/`. The node then only syncs the blocks since. Mainnet's node settings are in `misc/deployments/mainnet.gno.land/VALIDATOR.md`; keep the RPC on `127.0.0.1:26657`.

```bash
chown -R gnoreplay: /var/lib/gnoland /var/lib/gnoreplay
cp /opt/gnoreplay/src/deploy/systemd/* /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now gnoland
# Once the node has caught up with mainnet:
systemctl start gnoreplay-golden && systemctl enable --now gnoreplay-golden.timer
```

**Server.** `/etc/gnoreplay/config.toml` from [`config.digitalocean.example.toml`](../server/config.digitalocean.example.toml) (the VPC id, your SSH key fingerprint, the droplet's private address for `[digitalocean] listen/url`), then the secrets, readable by `gnoreplay` only:

| File | Content |
|---|---|
| `/etc/gnoreplay/github-token` | a read-only GitHub token (see the server README) |
| `/etc/gnoreplay/digitalocean-token` | the API token above |
| `/etc/gnoreplay/github-oauth-secret` | if private repos are tracked: the client secret of the sign-in OAuth app (below) |
| `/etc/gnoreplay/<repo>-viewers` | if private repos are tracked: who may see each one's results, from [`scripts/sync-viewers.sh`](../server/scripts/sync-viewers.sh) (rerun when its collaborators change) |

**Private repos' results** are shown to the users their `viewers_file` lists, signed in with GitHub. Register an OAuth app (any account can, e.g. the server's GitHub account: *Settings → Developer settings → OAuth Apps → New OAuth App*; the organization needn't approve it, as it reads no organization data), with homepage `https://<host>` and callback URL `https://<host>/auth/callback`. Put its client ID in `[github.oauth]` and a generated client secret in the file above. It asks users for no scopes.

```bash
# The watchdog first: it deletes workers older than 4h even if the server misbehaves.
systemctl enable --now gnoreplay-watchdog.timer
systemctl enable --now gnoreplay-server
# Replay a PR on demand (PRs open before the first poll are not replayed):
sudo -u gnoreplay /opt/gnoreplay/bin/gnoreplay-server -config /etc/gnoreplay/config.toml enqueue gnolang/gno 1234
```

**HTTPS.** `/etc/caddy/Caddyfile`, with a DNS name for the droplet (`<ip with dashes>.sslip.io` works without DNS):

```
replay.example.org {
	reverse_proxy 127.0.0.1:8080
}
```

## Limits on worker droplets

DigitalOcean has no spending cap, so the limits are ours. A worker droplet lives at most **4 hours** (`[digitalocean] max_age`), enforced four times over, each independent of the others:

| | Enforced by | When |
|---|---|---|
| 1 | the job: no report within `job.run_timeout` (3h30m) → the job fails and its worker is deleted | at 3h30m |
| 2 | the server: every 5 minutes, deletes any worker older than `max_age`, even if its job still runs (the job then fails) | ≤ 4h05m |
| 3 | `gnoreplay-watchdog.timer` on the coordinator ([`watchdog.sh`](watchdog.sh), a separate process from the server, needing only the DigitalOcean token) | ≤ 4h05m |
| 4 | the [`worker-watchdog`](../.github/workflows/worker-watchdog.yml) GitHub workflow (the same script), for when the coordinator itself is down: set a `DO_TOKEN` repository secret to enable it | every 15 min (GitHub may delay scheduled runs) |

Workers also power themselves off at 4h, which stops them working but not billing: only deletion does.

Beyond that, spending is bounded by `workers` (at most `workers` × $0.125/h: about $6/day for 2, $12/day for 4) and by the account's droplet limit (3 for new accounts: the coordinator and 2 workers; ask support to raise it before setting more workers), and you can set a **spend alert** (*Billing → Spend alerts*, email only) as a last notice.

To stop everything at once: `systemctl stop gnoreplay-server && doctl compute droplet delete --tag-name gnoreplay-worker` (stopping the server alone leaves running workers to the next start, see below).

## Operating

- Worker droplets are named `gnoreplay-job-<id>` and tagged `gnoreplay-worker`. The server deletes each when its replay ends, fails, is superseded or times out, and at startup and every 5 minutes deletes any it does not own or that is over `max_age`.
- Restarting the server (e.g. to deploy) keeps running replays: it leaves their workers running, and the next process resumes them, within the same time limits. Deploy within a few minutes of stopping: that is how long workers retry posting a result to a stopped server.
- After a change to how results are rendered, run `gnoreplay-server -config /etc/gnoreplay/config.toml rerender` (as the `gnoreplay` user) to update the pages of past replays.
- A worker's log is in its job's page when it fails, and on the worker in `/root/gnoreplay/<job>/worker.log` while it runs (SSH in with your key).
- A governance halt on mainnet stops the reference node: it needs the upgraded binary, like any node.
