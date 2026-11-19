#!/usr/bin/env bash
# Makes the checkpoint that replays resume from (see server/checkpoint.go).
#
# A hardfork can leave a chain whose new release cannot replay what came
# before it (gno.land mainnet's v1.8.0 refuses transactions in an encoding the
# earlier blocks hold). The release that ran the chain up to the halt then
# replays it once, from genesis, to the halt height, and what it leaves, the
# application's and tm2's DBs, is the checkpoint: every later replay resumes
# from it with the code under test and replays only the blocks since. Run
# this once per hardfork, on a machine with 16 GB of RAM: the replay of the
# whole history took 1h53m on 4 dedicated vCPUs (gno.land mainnet to height
# 1651500, 317,910 txs).
#
#   make-checkpoint.sh RELEASE HEIGHT DATA_DIR GENESIS OUT_DIR
#
#   RELEASE   the release tag that ran the chain up to HEIGHT (v1.7.1 for
#             mainnet's v1.8.0 hardfork). Its tree is cloned, and the replay
#             tool built against it: the stdlibs it loads at genesis are its
#             own.
#   HEIGHT    the halt height, where the chain switched to the next release.
#   DATA_DIR  a node's data dir past HEIGHT, with db/blockstore.db and
#             db/state.db: the golden copy (fetch-golden.sh), whose snapshot
#             must be past HEIGHT. Opened read-only.
#   GENESIS   the chain's genesis.json (for mainnet, the release's: see
#             misc/deployments/mainnet.gno.land/VALIDATOR.md for its sha256).
#   OUT_DIR   where the checkpoint is written, a new directory: gnolang.db,
#             state.db and checkpoint.json. Point chain.checkpoint_dir at it
#             (a new directory, not over one jobs are copying from).
#
# The replay stops at the first divergence (--stop-on-first), and the
# checkpoint is only written if it reached HEIGHT with no result, gas or block
# diff: its state is then the chain's, which the app hash says too.
#
#   CONTINUE    a release tag, e.g. v1.8.0: afterwards, resume a copy of the
#               checkpoint with that release, built as a replay is (without a
#               release version), over the blocks after HEIGHT up to the data
#               dir's tip, and require no diff. It proves the checkpoint
#               resumes under the code that continues the chain, and is
#               recorded in checkpoint.json.
#   GNO_REPO    where releases are cloned from (default
#               https://github.com/gnolang/gno).
#   GOMEMLIMIT  for the replay (default 12GiB): the first commit after genesis
#               peaks above 8 GB without one.
#   WORK_DIR    scratch space for the clones, the genesis cache and the DBs
#               while they are made, about 10 GB (default: a temporary
#               directory, removed at the end unless the replay failed).
#
# Needs bash, git, go, jq and sha256sum.
set -euo pipefail

if [ $# -ne 5 ]; then
	sed -n '2,/^set -euo/{/^set -euo/d;s/^# \{0,1\}//;p}' "$0" >&2
	exit 2
fi
RELEASE=$1 HEIGHT=$2 DATA_DIR=$3 GENESIS=$4 OUT_DIR=$5
GNO_REPO=${GNO_REPO:-https://github.com/gnolang/gno}
export GOMEMLIMIT=${GOMEMLIMIT:-12GiB}
REPO_DIR=$(cd "$(dirname "$0")/../.." && pwd)

die() { echo "make-checkpoint: $*" >&2; exit 1; }
step() { echo "==> $* ($(date -u +%H:%M:%S))" >&2; }

[[ $HEIGHT =~ ^[1-9][0-9]*$ ]] || die "HEIGHT must be a height, not '$HEIGHT'"
[ -d "$DATA_DIR/db/blockstore.db" ] && [ -d "$DATA_DIR/db/state.db" ] || die "$DATA_DIR has no db/blockstore.db and db/state.db"
[ -f "$GENESIS" ] || die "no genesis at $GENESIS"
[ ! -e "$OUT_DIR/checkpoint.json" ] && [ ! -e "$OUT_DIR/gnolang.db" ] || die "$OUT_DIR already holds a checkpoint: write a new one"
for tool in git go jq sha256sum; do
	command -v "$tool" >/dev/null || die "$tool is needed"
done

KEEP_WORK=false
if [ -z "${WORK_DIR:-}" ]; then
	WORK_DIR=$(mktemp -d)
	trap '$KEEP_WORK || rm -rf "$WORK_DIR"' EXIT
fi
mkdir -p "$WORK_DIR"
fail() { KEEP_WORK=true; echo "make-checkpoint: $* (kept $WORK_DIR)" >&2; exit 1; }

# build_tool TAG DIR [GOFLAGS...] clones the release TAG into DIR, copies the
# replay tool into it and builds it there, as the server does for a job (its
# go.sum was resolved against another tree): the binary is DIR/gnoreplay.
build_tool() {
	local tag=$1 dir=$2
	shift 2
	git clone -q --depth 1 --branch "$tag" "$GNO_REPO" "$dir" || fail "clone $tag"
	cp -r "$REPO_DIR/gnoreplay" "$dir/contribs/gnoreplay"
	(cd "$dir/contribs/gnoreplay" && go mod tidy && CGO_ENABLED=0 go build -trimpath "$@" -o "$dir/gnoreplay" .) || fail "build $tag"
}

# run_tool GNO_DIR ARGS... runs the tool built there: exit 2 is "diffs found",
# which the report says; anything else non-zero is a failure.
run_tool() {
	local dir=$1 rc=0
	shift
	GNOROOT=$dir "$dir/gnoreplay" "$@" || rc=$?
	[ "$rc" -eq 0 ] || [ "$rc" -eq 2 ] || fail "gnoreplay exited with $rc"
}

# clean REPORT FROM TO says whether the report covers FROM..TO with no result,
# gas or block diff.
clean() {
	jq -e --argjson from "$2" --argjson to "$3" '
		.from_height == $from and .to_height == $to and .blocks == ($to - $from + 1)
		and .counts.result == 0 and .counts.gas == 0 and .counts.block == 0
		and ((.first_app_hash_mismatch // 0) == 0) and ((.app_hash // "") != "")' "$1" >/dev/null
}

step "build the tool against $RELEASE"
# Built with the release's own version, as its binary was: a node of this
# release would report it.
build_tool "$RELEASE" "$WORK_DIR/release" -ldflags "-X github.com/gnolang/gno/tm2/pkg/version.Version=$RELEASE"
RELEASE_COMMIT=$(git -C "$WORK_DIR/release" rev-parse HEAD)

step "replay genesis to $HEIGHT with $RELEASE ($RELEASE_COMMIT)"
run_tool "$WORK_DIR/release" --data-dir "$DATA_DIR" --genesis "$GENESIS" --work-dir "$WORK_DIR/work" \
	--to "$HEIGHT" --stop-on-first --out "$WORK_DIR/report.json"
[ -s "$WORK_DIR/report.json" ] || fail "no report"
clean "$WORK_DIR/report.json" 1 "$HEIGHT" ||
	fail "the replay did not reach $HEIGHT without diffs: $(jq -c '{from_height, to_height, blocks, counts, first_app_hash_mismatch}' "$WORK_DIR/report.json")"

RESUMED=null
if [ -n "${CONTINUE:-}" ]; then
	step "resume it with $CONTINUE"
	# As a replay of a PR is built: no release version.
	build_tool "$CONTINUE" "$WORK_DIR/continue"
	mkdir "$WORK_DIR/resume"
	cp -a "$WORK_DIR/work/gnolang.db" "$WORK_DIR/work/state.db" "$WORK_DIR/resume/"
	run_tool "$WORK_DIR/continue" --data-dir "$DATA_DIR" --work-dir "$WORK_DIR/resume" --resume --out "$WORK_DIR/resume-report.json"
	[ -s "$WORK_DIR/resume-report.json" ] || fail "no report from the resumed replay"
	TIP=$(jq -r .to_height "$WORK_DIR/resume-report.json")
	clean "$WORK_DIR/resume-report.json" $((HEIGHT + 1)) "$TIP" ||
		fail "resuming with $CONTINUE did not replay the blocks after $HEIGHT without diffs: $(jq -c '{from_height, to_height, blocks, counts}' "$WORK_DIR/resume-report.json")"
	RESUMED=$(jq -c --arg release "$CONTINUE" --arg commit "$(git -C "$WORK_DIR/continue" rev-parse HEAD)" \
		'{release: $release, commit: $commit, from_height, to_height, blocks, txs, result_diffs: .counts.result, gas_diffs: .counts.gas, block_diffs: .counts.block}' \
		"$WORK_DIR/resume-report.json")
	rm -rf "$WORK_DIR/resume"
fi

step "write $OUT_DIR"
mkdir -p "$OUT_DIR"
mv "$WORK_DIR/work/gnolang.db" "$WORK_DIR/work/state.db" "$OUT_DIR/"
TOOL_COMMIT=$(git -C "$REPO_DIR" rev-parse HEAD 2>/dev/null || echo unknown)
if [ -n "$(git -C "$REPO_DIR" status --porcelain -- gnoreplay 2>/dev/null)" ]; then
	TOOL_COMMIT="$TOOL_COMMIT+uncommitted"
fi
jq -n --slurpfile r "$WORK_DIR/report.json" --argjson resumed "$RESUMED" \
	--arg release "$RELEASE" --arg commit "$RELEASE_COMMIT" --arg tool "$TOOL_COMMIT" \
	--arg genesis "$(sha256sum "$GENESIS" | cut -d' ' -f1)" --arg created "$(date -u +%FT%TZ)" '
	{
		chain_id: $r[0].chain_id,
		height: $r[0].to_height,
		app_hash: $r[0].app_hash,
		built_with: {release: $release, commit: $commit, tool: ("gnoreplay at " + $tool)},
		genesis_sha256: $genesis,
		replay: {
			from_height: $r[0].from_height, to_height: $r[0].to_height, blocks: $r[0].blocks, txs: $r[0].txs,
			result_diffs: $r[0].counts.result, gas_diffs: $r[0].counts.gas, block_diffs: $r[0].counts.block,
			log_only: $r[0].counts.log, missing_responses: ($r[0].missing_responses // 0),
			seconds: $r[0].timing.total_seconds
		},
		resumed_check: $resumed,
		created: $created
	}' >"$OUT_DIR/checkpoint.json"
step "done: $OUT_DIR holds the checkpoint of $(jq -r '.chain_id' "$OUT_DIR/checkpoint.json") at height $HEIGHT, app hash $(jq -r .app_hash "$OUT_DIR/checkpoint.json")"
