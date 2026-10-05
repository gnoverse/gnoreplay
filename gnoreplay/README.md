# gnoreplay — replay a recorded chain and diff every tx result

`gnoreplay` re-executes every block stored in a gnoland node's data directory with the application code it was built from, and compares each response against the one the chain recorded. It answers: *would this binary execute the chain's history the same way?*

It drives the gno.land ABCI application directly — `InitChain`, then `BeginBlock` / `DeliverTx` / `EndBlock` / `Commit` for every stored block — with the recorded headers, commits and signed txs. Nothing is faked: heights, timestamps, signatures, gas meters and EndBlocker all run as they did on the chain. No networking or consensus is involved.

## Building

The tool links the gno.land application, so it is built *inside* the gno tree under test: copy this directory to `contribs/gnoreplay` of a gno checkout (its `go.mod` replaces `github.com/gnolang/gno` with `../..`), then:

```bash
cd <gno checkout>/contribs/gnoreplay
go mod tidy   # go.sum was resolved against another gno tree
go build -o gnoreplay .
go test ./...
```

It only uses stable gno.land APIs (`gnoland.NewAppWithOptions`, `gnoland.LoadStreamingGenesisDoc`, the tm2 block and state stores), and builds unchanged on `master` and `chain/mainnet`.

## Usage

```bash
gnoreplay --data-dir <node data dir> --genesis genesis.json --out report.json
```

| Flag | Description |
|---|---|
| `--data-dir` | Source node data directory. Its `db/blockstore.db` and `db/state.db` are opened read-only; the node must be stopped (or use a copy/snapshot). |
| `--genesis` | The source chain's `genesis.json`. |
| `--work-dir` | Where the replayed app DB and genesis cache live (default: a temporary directory, removed on exit). |
| `--to` | Last height to replay (default: the source tip). |
| `--stop-on-first` | Stop at the first block with a result or app hash divergence. |
| `--max-diffs` | Diffs listed in the report (default 1000; counts stay exact). |
| `--out` | Write the JSON report here. |

Exit status: `0` no consensus-relevant diffs, `1` error, `2` result or block diffs found.

The stdlibs are read from `GNOROOT` at runtime (not embedded in the binary), so run it from — or with `GNOROOT` pointing at — the checkout it was built from.

Memory: the first commit writes the whole genesis state at once. For mainnet (3.26M genesis accounts) it peaks above 8 GB with Go's default GC settings; with `GOMEMLIMIT=7GiB` it completes within 9 GB.

## What is compared

| Kind | Meaning |
|---|---|
| `result` | A tx's `Error`, `Data` or `Events` differ. These feed `LastResultsHash`: the binary would fork from the recorded chain. |
| `gas` | Only `GasUsed` differs. Not consensus-relevant by itself, but a tx that now needs more gas than its `gas_wanted` fails. |
| `block` | Block-level: app hash mismatch (first one only), validator updates, tx count. |
| log-only | Only the nondeterministic `Log` differs: counted, not listed. |

Genesis txs are compared against the results the node stored at height 0.

The replay does not stop at a divergence. Diffs at heights after the first app hash mismatch have `after_divergence: true` — they may be consequences of an earlier diff rather than independent changes.

Governance halts recorded in the chain's history are ignored during replay.

## Obtaining a data directory

Sync a non-validator node from genesis with the binary the chain runs (see `misc/deployments/<chain>/VALIDATOR.md`); governance halts stop it along the way — restart it each time. Every node stores each height's tx responses in `state.db`, which is the ground truth `gnoreplay` compares against.
