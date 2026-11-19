package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fullReport = `{"chain_id":"gnoland-1","from_height":1,"to_height":1651500,"blocks":1651500,"txs":317910,
		"app_hash":"48E0FD15","counts":{"result":0,"gas":0,"block":0,"log":448},"diffs":[],
		"timing":{"genesis_seconds":110,"total_seconds":6771}}`
	resumedReport = `{"chain_id":"gnoland-1","from_height":1651501,"to_height":1657321,"blocks":5821,"txs":242,
		"app_hash":"7A7A","counts":{"result":0,"gas":0,"block":0,"log":1},"diffs":[],
		"timing":{"genesis_seconds":0,"total_seconds":12}}`
)

// A stand-in for git: a clone is a directory marked with its tag.
const fakeGit = `#!/bin/bash
if [ "$1" = clone ]; then
	# clone -q --depth 1 --branch TAG REPO DIR
	mkdir -p "$8/contribs"
	echo "$6" > "$8/TAG"
	exit 0
fi
if [ "$1" = -C ] && [ -f "$2/TAG" ]; then
	echo "commit-of-$(cat "$2/TAG")"
	exit 0
fi
exec "$REAL_GIT" "$@"
`

// A stand-in for go: building writes the fake tool, and says how it was built.
const fakeGo = `#!/bin/bash
if [ "$1" = mod ]; then exit 0; fi
# build -trimpath [-ldflags X] -o OUT .
while [ $# -gt 0 ]; do
	case $1 in
	-ldflags) ldflags=$2; shift ;;
	-o) out=$2; shift ;;
	esac
	shift
done
cp "$FAKE_TOOL" "$out"
echo "${ldflags:-none}" > "$out.ldflags"
`

// A stand-in for the replay tool: it leaves a checkpoint in its work dir, or
// needs one with --resume, logs how it was run, and writes the report the test
// asked for.
const fakeCheckpointTool = `#!/bin/bash
while [ $# -gt 0 ]; do
	case $1 in
	--data-dir) data=$2; shift ;;
	--work-dir) work=$2; shift ;;
	--out) out=$2; shift ;;
	--to) to=$2; shift ;;
	--resume) resume=true ;;
	--stop-on-first) stop=true ;;
	esac
	shift
done
echo "root=$GNOROOT limit=$GOMEMLIMIT data=$data to=${to:-} resume=${resume:-false} stop=${stop:-false}" >> "$FAKE_LOG"
[ -d "$data/db/blockstore.db" ] || exit 1
if [ "${resume:-false}" = true ]; then
	[ -d "$work/gnolang.db" ] && [ -d "$work/state.db" ] || exit 1
	echo "$FAKE_RESUMED" > "$out"
	exit "${FAKE_RESUMED_EXIT:-0}"
fi
mkdir -p "$work/gnolang.db" "$work/state.db"
echo made > "$work/gnolang.db/MARKER"
echo "$FAKE_FULL" > "$out"
exit "${FAKE_EXIT:-0}"
`

type checkpointRun struct {
	out    string // the script's output
	err    error
	outDir string
	log    []string // how the fake tool was run
	work   string
}

// runMakeCheckpoint runs scripts/make-checkpoint.sh with a fake git, go and
// tool. env adds to its environment.
func runMakeCheckpoint(t *testing.T, env map[string]string) checkpointRun {
	t.Helper()
	for _, tool := range []string{"bash", "jq", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	for name, body := range map[string]string{"git": fakeGit, "go": fakeGo, "tool": fakeCheckpointTool} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755))
	}
	data := filepath.Join(root, "data")
	for _, db := range []string{"blockstore.db", "state.db"} {
		require.NoError(t, os.MkdirAll(filepath.Join(data, "db", db), 0o755))
	}
	genesis := filepath.Join(root, "genesis.json")
	require.NoError(t, os.WriteFile(genesis, []byte("{}"), 0o644))

	r := checkpointRun{outDir: filepath.Join(root, "out"), work: filepath.Join(root, "work")}
	logPath := filepath.Join(root, "tool.log")
	cmd := exec.Command("bash", "scripts/make-checkpoint.sh", "v1.7.1", "1651500", data, genesis, r.outDir)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"), "REAL_GIT="+realGit, "FAKE_TOOL="+filepath.Join(bin, "tool"),
		"FAKE_LOG="+logPath, "WORK_DIR="+r.work, "FAKE_FULL="+fullReport, "FAKE_RESUMED="+resumedReport,
		"GOMEMLIMIT=", "GNO_REPO=https://example.invalid/gno",
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	r.out, r.err = string(out), err
	if bz, err := os.ReadFile(logPath); err == nil {
		r.log = strings.Split(strings.TrimSpace(string(bz)), "\n")
	}
	return r
}

func TestMakeCheckpoint(t *testing.T) {
	r := runMakeCheckpoint(t, nil)
	require.NoError(t, r.err, r.out)
	require.Len(t, r.log, 1, "the replay alone")
	assert.Contains(t, r.log[0], "to=1651500 resume=false stop=true", "to the halt, stopping at the first divergence")
	assert.Contains(t, r.log[0], "limit=12GiB", "the memory limit the first commit needs")
	assert.Contains(t, r.log[0], "release", "run on the release's tree")

	// Built with the release's own version, as its binary was.
	bz, err := os.ReadFile(filepath.Join(r.work, "release", "gnoreplay.ldflags"))
	require.NoError(t, err)
	assert.Contains(t, string(bz), "tm2/pkg/version.Version=v1.7.1")

	assert.FileExists(t, filepath.Join(r.outDir, "gnolang.db", "MARKER"))
	assert.DirExists(t, filepath.Join(r.outDir, "state.db"))
	bz, err = os.ReadFile(filepath.Join(r.outDir, "checkpoint.json"))
	require.NoError(t, err)
	var m struct {
		Checkpoint
		BuiltWith struct {
			Release, Commit, Tool string
		} `json:"built_with"`
		GenesisSHA256 string `json:"genesis_sha256"`
		Replay        struct {
			Blocks      int64
			Txs         int64
			ResultDiffs int64 `json:"result_diffs"`
			GasDiffs    int64 `json:"gas_diffs"`
			BlockDiffs  int64 `json:"block_diffs"`
			LogOnly     int64 `json:"log_only"`
		} `json:"replay"`
		ResumedCheck json.RawMessage `json:"resumed_check"`
	}
	require.NoError(t, json.Unmarshal(bz, &m))
	assert.Equal(t, Checkpoint{ChainID: "gnoland-1", Height: 1651500, AppHash: "48E0FD15"}, m.Checkpoint)
	assert.Equal(t, "v1.7.1", m.BuiltWith.Release)
	assert.Equal(t, "commit-of-v1.7.1", m.BuiltWith.Commit)
	assert.Contains(t, m.BuiltWith.Tool, "gnoreplay at ")
	assert.Len(t, m.GenesisSHA256, 64)
	assert.EqualValues(t, 1651500, m.Replay.Blocks)
	assert.EqualValues(t, 317910, m.Replay.Txs)
	assert.EqualValues(t, 448, m.Replay.LogOnly)
	assert.Zero(t, m.Replay.ResultDiffs+m.Replay.GasDiffs+m.Replay.BlockDiffs)
	assert.JSONEq(t, "null", string(m.ResumedCheck))

	// What it writes is what the server loads.
	c, err := loadCheckpoint(r.outDir)
	require.NoError(t, err)
	assert.Equal(t, m.Checkpoint, c)
}

// With CONTINUE, the checkpoint is resumed, on a copy, with the release that
// continues the chain, built as a replay is: without a release version.
func TestMakeCheckpointContinue(t *testing.T) {
	r := runMakeCheckpoint(t, map[string]string{"CONTINUE": "v1.8.0"})
	require.NoError(t, r.err, r.out)
	require.Len(t, r.log, 2)
	assert.Contains(t, r.log[1], "resume=true")
	assert.Contains(t, r.log[1], "/continue ", "with the later release's tree")

	bz, err := os.ReadFile(filepath.Join(r.work, "continue", "gnoreplay.ldflags"))
	require.NoError(t, err)
	assert.Equal(t, "none", strings.TrimSpace(string(bz)))

	bz, err = os.ReadFile(filepath.Join(r.outDir, "checkpoint.json"))
	require.NoError(t, err)
	var m struct {
		ResumedCheck struct {
			Release, Commit string
			FromHeight      int64 `json:"from_height"`
			ToHeight        int64 `json:"to_height"`
			Blocks, Txs     int64
		} `json:"resumed_check"`
	}
	require.NoError(t, json.Unmarshal(bz, &m))
	assert.Equal(t, "v1.8.0", m.ResumedCheck.Release)
	assert.Equal(t, "commit-of-v1.8.0", m.ResumedCheck.Commit)
	assert.EqualValues(t, 1651501, m.ResumedCheck.FromHeight)
	assert.EqualValues(t, 1657321, m.ResumedCheck.ToHeight)
	assert.EqualValues(t, 5821, m.ResumedCheck.Blocks)
	assert.EqualValues(t, 242, m.ResumedCheck.Txs)
	// The resumed replay worked on a copy: the checkpoint is as the first
	// replay left it.
	assert.FileExists(t, filepath.Join(r.outDir, "gnolang.db", "MARKER"))
}

// A checkpoint is only written for a replay that reached the halt height with
// no diff: otherwise every later replay would start from a state that is not
// the chain's.
func TestMakeCheckpointRefuses(t *testing.T) {
	withCounts := func(report, counts string) string {
		return strings.Replace(report, `"counts":{"result":0,"gas":0,"block":0,"log":448}`, `"counts":`+counts, 1)
	}
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"a result diff": {
			map[string]string{"FAKE_EXIT": "2", "FAKE_FULL": withCounts(fullReport, `{"result":1,"gas":0,"block":0,"log":0}`)},
			"did not reach 1651500 without diffs",
		},
		"a gas diff": {
			map[string]string{"FAKE_FULL": withCounts(fullReport, `{"result":0,"gas":1,"block":0,"log":0}`)},
			"did not reach 1651500 without diffs",
		},
		"a block diff": {
			map[string]string{"FAKE_EXIT": "2", "FAKE_FULL": withCounts(fullReport, `{"result":0,"gas":0,"block":1,"log":0}`)},
			"did not reach 1651500 without diffs",
		},
		"stopped short": {
			map[string]string{"FAKE_FULL": strings.Replace(strings.Replace(fullReport, `"to_height":1651500`, `"to_height":900000`, 1), `"blocks":1651500`, `"blocks":900000`, 1)},
			"did not reach 1651500 without diffs",
		},
		"no app hash": {
			map[string]string{"FAKE_FULL": strings.Replace(fullReport, `"app_hash":"48E0FD15",`, "", 1)},
			"did not reach 1651500 without diffs",
		},
		"an error of the tool": {
			map[string]string{"FAKE_EXIT": "1"},
			"gnoreplay exited with 1",
		},
		"a diff after the halt, with CONTINUE": {
			map[string]string{
				"CONTINUE":     "v1.8.0",
				"FAKE_RESUMED": strings.Replace(resumedReport, `"counts":{"result":0,`, `"counts":{"result":3,`, 1),
			},
			"did not replay the blocks after 1651500 without diffs",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := runMakeCheckpoint(t, tc.env)
			require.Error(t, r.err, r.out)
			assert.Contains(t, r.out, tc.want)
			assert.NoFileExists(t, filepath.Join(r.outDir, "checkpoint.json"))
			assert.NoDirExists(t, filepath.Join(r.outDir, "gnolang.db"))
			assert.Contains(t, r.out, "kept", "the work dir is kept to see why")
		})
	}
}

func TestMakeCheckpointUsage(t *testing.T) {
	cmd := exec.Command("bash", "scripts/make-checkpoint.sh", "v1.7.1")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 2, exit.ExitCode())
	assert.Contains(t, string(out), "make-checkpoint.sh RELEASE HEIGHT DATA_DIR GENESIS OUT_DIR")
}
