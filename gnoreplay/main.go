// Command gnoreplay re-executes the blocks recorded in a gnoland node's data
// directory with the application code it was built from, and reports every tx
// whose result differs from the one the chain recorded.
//
// Usage:
//
//	gnoreplay --data-dir gnoland-data --genesis genesis.json [--out report.json]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"

	"github.com/gnolang/gno/gno.land/pkg/gnoland"
	"github.com/gnolang/gno/gnovm/pkg/gnoenv"
	abci "github.com/gnolang/gno/tm2/pkg/bft/abci/types"
	"github.com/gnolang/gno/tm2/pkg/bft/store"
	dbm "github.com/gnolang/gno/tm2/pkg/db"
	"github.com/gnolang/gno/tm2/pkg/db/pebbledb"
	"github.com/gnolang/gno/tm2/pkg/events"
	"github.com/gnolang/gno/tm2/pkg/store/types"
)

func main() {
	var (
		dataDir     = flag.String("data-dir", "gnoland-data", "source node data directory (its db/ is opened read-only)")
		genesisFile = flag.String("genesis", "genesis.json", "genesis.json of the source chain")
		workDir     = flag.String("work-dir", "", "directory for the replay's app and state DBs and genesis cache (default: a temporary directory, removed on exit)")
		toHeight    = flag.Int64("to", 0, "last height to replay (default: source tip)")
		stopOnFirst = flag.Bool("stop-on-first", false, "stop at the first block with a result or app hash divergence")
		maxDiffs    = flag.Int("max-diffs", 1000, "maximum number of diffs listed in the report (counts stay exact); 0 for no limit")
		out         = flag.String("out", "", "write the JSON report to this file")
		logLevel    = flag.String("log-level", "info", "log level: debug, info, warn, error")
		progress    = flag.String("progress", "", "keep this file updated with the replay's progress (JSON: height, tip, blocks_per_second)")
	)
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fatalf("invalid --log-level: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	opts := replayOptions{
		ToHeight:    *toHeight,
		StopOnFirst: *stopOnFirst,
		MaxDiffs:    *maxDiffs,
		Logger:      logger,
	}
	if *progress != "" {
		opts.Progress = func(height, tip int64, blocksPerSecond float64) {
			writeProgress(*progress, height, tip, blocksPerSecond, logger)
		}
	}
	report, err := run(logger, *dataDir, *genesisFile, *workDir, opts)
	if err != nil {
		fatalf("%v", err)
	}

	fmt.Print(report.Summary())
	if *out != "" {
		bz, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fatalf("marshal report: %v", err)
		}
		if err := os.WriteFile(*out, bz, 0o644); err != nil {
			fatalf("write report: %v", err)
		}
	}
	if report.Counts.Result > 0 || report.Counts.Block > 0 {
		os.Exit(2)
	}
}

// writeProgress replaces path with the progress, whole: whoever reads it
// (the worker, which posts it to the coordinator) never sees half of it.
func writeProgress(path string, height, tip int64, blocksPerSecond float64, logger *slog.Logger) {
	bz, err := json.Marshal(struct {
		Height          int64   `json:"height"`
		Tip             int64   `json:"tip"`
		BlocksPerSecond float64 `json:"blocks_per_second"`
	}{height, tip, blocksPerSecond})
	if err == nil {
		err = os.WriteFile(path+".tmp", bz, 0o644)
	}
	if err == nil {
		err = os.Rename(path+".tmp", path)
	}
	if err != nil {
		logger.Warn("write progress", "err", err)
	}
}

func run(logger *slog.Logger, dataDir, genesisFile, workDir string, opts replayOptions) (*Report, error) {
	if workDir == "" {
		tmp, err := os.MkdirTemp("", "gnoreplay-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		workDir = tmp
	}
	if err := prepareWorkDir(workDir); err != nil {
		return nil, err
	}

	srcDBDir := filepath.Join(dataDir, "db")
	bsDB, err := openReadOnly("blockstore", srcDBDir)
	if err != nil {
		return nil, fmt.Errorf("open source blockstore: %w", err)
	}
	defer bsDB.Close()
	stateDB, err := openReadOnly("state", srcDBDir)
	if err != nil {
		return nil, fmt.Errorf("open source state: %w", err)
	}
	defer stateDB.Close()

	genDoc, err := gnoland.LoadStreamingGenesisDoc(genesisFile, filepath.Join(workDir, "genesis-cache"), logger)
	if err != nil {
		return nil, fmt.Errorf("load genesis: %w", err)
	}
	opts.Genesis = genDoc

	appDB, err := pebbledb.NewPebbleDBWithOpts("gnolang", workDir, &pebble.Options{FS: noSyncFS{vfs.Default}})
	if err != nil {
		return nil, fmt.Errorf("open app db: %w", err)
	}
	app, err := newApp(appDB, logger)
	if err != nil {
		return nil, err
	}
	defer app.Close()
	replayStateDB, err := pebbledb.NewPebbleDBWithOpts("state", workDir, &pebble.Options{FS: noSyncFS{vfs.Default}})
	if err != nil {
		return nil, fmt.Errorf("open replay state db: %w", err)
	}
	defer replayStateDB.Close()

	r, err := replay(Source{Blocks: store.NewBlockStore(bsDB), StateDB: stateDB}, app, replayStateDB, opts)
	if err != nil {
		return nil, err
	}
	if err := checkReplayCoverage(r, dataDir); err != nil {
		return nil, err
	}
	return r, nil
}

// prepareWorkDir removes the app and state DBs of an earlier run, keeping the
// genesis cache: a stale DB would be taken for this replay's own state (the
// handshake trusts what it finds), and every block would then fail against
// it, or the handshake would panic.
func prepareWorkDir(workDir string) error {
	for _, name := range []string{"gnolang", "state"} {
		if err := os.RemoveAll(filepath.Join(workDir, name+".db")); err != nil {
			return fmt.Errorf("clear %s of an earlier run: %w", name, err)
		}
	}
	return nil
}

// checkReplayCoverage rejects a report that compared nothing: an empty
// blockstore, or a state DB with no recorded responses, would otherwise be
// published as a clean pass.
func checkReplayCoverage(r *Report, dataDir string) error {
	if r.Blocks == 0 {
		return fmt.Errorf("no blocks to replay in %s", filepath.Join(dataDir, "db"))
	}
	if r.MissingResponses == r.Blocks {
		return fmt.Errorf("none of the %d replayed heights has recorded responses in %s: nothing to compare",
			r.Blocks, filepath.Join(dataDir, "db", "state.db"))
	}
	return nil
}

// newApp builds the application the way `gnoland start` does (see
// gnoland.NewApp), on a fresh DB.
func newApp(db dbm.DB, logger *slog.Logger) (abci.Application, error) {
	return gnoland.NewAppWithOptions(&gnoland.AppOptions{
		DB:          db,
		Logger:      logger.With("module", "app"),
		EventSwitch: events.NewEventSwitch(),
		InitChainerConfig: gnoland.InitChainerConfig{
			// Genesis tx failures are reported as diffs, not fatal.
			GenesisTxResultHandler: gnoland.NoopGenesisTxResultHandler,
			StdlibDir:              filepath.Join(gnoenv.RootDir(), "gnovm", "stdlibs"),
		},
		// Mainnet genesis txs carry placeholder signatures; nodes run with
		// --skip-genesis-sig-verification.
		SkipGenesisSigVerification: true,
		// Old versions are never read back: keep only the latest.
		PruneStrategy: types.PruneEverythingStrategy,
	})
}

func openReadOnly(name, dir string) (dbm.DB, error) {
	path := filepath.Join(dir, name+".db")
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return pebbledb.NewPebbleDBWithOpts(name, dir, &pebble.Options{ReadOnly: true})
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gnoreplay: "+format+"\n", args...)
	os.Exit(1)
}
