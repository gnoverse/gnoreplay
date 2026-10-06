#!/bin/bash
# gnoreplay worker: replays one job and posts the report back to the
# coordinator, which then deletes this machine. Rendered by the coordinator
# for each job and run once at boot (cloud-init user data).
set -uo pipefail

URL={{q .URL}}
JOB={{q .JobID}}
TOKEN={{q .Token}}
WORK={{q .WorkDir}}
GO_VERSION={{q .GoVersion}}
export GOMEMLIMIT={{q .GoMemLimit}}
export HOME=${HOME:-/root}

mkdir -p "$WORK"
LOG="$WORK/worker.log"
exec >"$LOG" 2>&1

# api <method> <path> [curl args...] talks to the coordinator for this job.
api() {
	local method=$1 path=$2
	shift 2
	curl -fsS --retry 5 --retry-all-errors -X "$method" \
		-H "Authorization: Bearer $TOKEN" "$URL/worker/$JOB/$path" "$@"
}

fail() {
	echo "FAILED: $*"
	api POST fail --data-binary @"$LOG" || true
	exit 1
}

step() { echo "==> $* ($(date -u +%H:%M:%S))"; }

if ! command -v go >/dev/null; then
	step "install go $GO_VERSION"
	curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | tar -C /usr/local -xz || fail "install go"
	export PATH=/usr/local/go/bin:$PATH
fi

cd "$WORK" || fail "cd $WORK"
mkdir -p src data
step "download source"
api GET source | tar -x -C src || fail "download source"
step "download chain data"
api GET golden | tar -x -C data || fail "download chain data"
step "download genesis"
api GET genesis -o genesis.json || fail "download genesis"

step "build"
go -C src/contribs/gnoreplay mod tidy || fail "go mod tidy"
go -C src/contribs/gnoreplay build -o "$WORK/gnoreplay" . || fail "build"

step "replay"
GNOROOT="$WORK/src" ./gnoreplay --data-dir data --genesis genesis.json --work-dir work \
	--max-diffs 0 --out report.json --log-level warn
rc=$?
# 2: diffs found, a result like any other.
if [ $rc -ne 0 ] && [ $rc -ne 2 ]; then
	fail "gnoreplay exited with $rc"
fi

step "upload report"
api POST report --data-binary @report.json || fail "upload report"
step "done"
