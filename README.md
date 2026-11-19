# gnoreplay

Checks whether a change to [gnolang/gno](https://github.com/gnolang/gno) would execute gno.land mainnet differently: every block since its last hardfork is re-executed with the changed code, from the state the chain had at the halt, and every tx whose result differs from the one recorded on chain is reported.

- [`gnoreplay/`](gnoreplay/) — the replay tool. It is built inside the gno tree under test (copied to `contribs/gnoreplay`), so each commit is checked with its own application code.
- [`server/`](server/) — a service that replays every push and PR on the tracked branches in the background, and publishes the results on its own pages. It only reads from GitHub. Replays run either on the server's box, or each on its own disposable cloud machine.
- [`deploy/`](deploy/) — deploying it on Hetzner Cloud.

Until the v1.8.0 hardfork (halt at height 1651500, November 2026) a replay executed the whole history from genesis: 1h40m–2h on a 2-vCPU worker in October 2026, and growing. v1.8.0 cannot replay the history before its halt, so replays now resume from a checkpoint of the state v1.7.1 left there ([`server/scripts/make-checkpoint.sh`](server/scripts/make-checkpoint.sh)) and replay only the blocks since: 5,821 blocks took 12 s. The chain adds about 24,000 blocks a day, so that grows again, which is why this runs as a background service rather than a CI job.

## License

Same as gnolang/gno: the GNO Network General Public License, see [LICENSE.md](LICENSE.md).
