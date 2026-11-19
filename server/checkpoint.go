package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// checkpointDBs are the DBs of a checkpoint: the application's, and tm2's
// state, as the replay tool leaves them in its work dir.
var checkpointDBs = []string{"gnolang.db", "state.db"}

// Checkpoint describes the state replays start from. A hardfork can leave a
// chain whose new release cannot replay what came before it (gno.land
// mainnet's v1.8.0 refuses transactions in an encoding earlier blocks hold):
// the release that ran the chain up to the halt replays it once, from
// genesis, to the halt height (scripts/make-checkpoint.sh), and every replay
// after that resumes from the state it left, with the code under test, and
// replays only the blocks since.
type Checkpoint struct {
	ChainID string `json:"chain_id"`
	// Height is the last block the checkpoint includes: replays start at the
	// next one.
	Height  int64  `json:"height"`
	AppHash string `json:"app_hash"`
}

// loadCheckpoint reads a checkpoint dir: its checkpoint.json (which may say
// more than the server needs) and the presence of its DBs. The replay tool
// checks that the state is the golden chain's at its height.
func loadCheckpoint(dir string) (Checkpoint, error) {
	var c Checkpoint
	bz, err := os.ReadFile(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		return c, fmt.Errorf("checkpoint: %w", err)
	}
	if err := json.Unmarshal(bz, &c); err != nil {
		return c, fmt.Errorf("checkpoint.json: %w", err)
	}
	if c.ChainID == "" || c.Height <= 0 || c.AppHash == "" {
		return c, fmt.Errorf("checkpoint.json: chain_id, height and app_hash are required")
	}
	for _, name := range checkpointDBs {
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || !fi.IsDir() {
			return c, fmt.Errorf("checkpoint: %s is missing from %s", name, dir)
		}
	}
	return c, nil
}
