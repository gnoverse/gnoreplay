# gnoreplay

Checks whether a change to [gnolang/gno](https://github.com/gnolang/gno) would execute gno.land mainnet's history differently: every recorded block is re-executed with the changed code, and every tx whose result differs from the one recorded on chain is reported.

- [`gnoreplay/`](gnoreplay/) — the replay tool. It is built inside the gno tree under test (copied to `contribs/gnoreplay`), so each commit is checked with its own application code.
- [`server/`](server/) — a service that replays every push and PR on the tracked branches in the background, and publishes the results on its own pages. It only reads from GitHub. Replays run either on the server's box, or each on its own disposable cloud machine.
- [`deploy/`](deploy/) — deploying it on DigitalOcean.

A full replay of mainnet takes about an hour on 4 cores (402,180 blocks / 63,172 txs in 63–70 min, 2026-09-28) and grows with the chain, which is why this runs as a background service rather than a CI job.

## License

Same as gnolang/gno: the GNO Network General Public License, see [LICENSE.md](LICENSE.md).
