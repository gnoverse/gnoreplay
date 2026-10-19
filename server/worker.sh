#!/bin/bash
# gnoreplay worker: replays one job and posts the report back to the
# coordinator, which then reinstalls this machine for another job, or deletes
# it. Rendered by the coordinator for each job and run once at boot
# (cloud-init user data).
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

# Last resort if nothing stops this job in time: power off at the max age.
# (A powered-off server is still billed; the coordinator and the watchdog
# delete it.)
if [ {{.PowerOffMinutes}} -gt 0 ]; then
	shutdown -P +{{.PowerOffMinutes}} "gnoreplay: worker max age reached"
fi

# api <method> <path> [curl args...] talks to the coordinator for this job.
# Retries span about 4 minutes, so a coordinator restart loses nothing.
api() {
	local method=$1 path=$2
	shift 2
	curl -fsS --retry 8 --retry-all-errors -X "$method" \
		-H "Authorization: Bearer $TOKEN" "$URL/worker/$JOB/$path" "$@"
}

fail() {
	echo "FAILED: $*"
	# A bounded tail: the server shows the last 4k of it, and a whole log
	# over the fail route's cap would be rejected, leaving the job to wait
	# out its run timeout.
	tail -c 262144 "$LOG" >"$WORK/fail.log"
	api POST fail --data-binary @"$WORK/fail.log" || true
	exit 1
}

step() { echo "==> $* ($(date -u +%H:%M:%S))"; }

if ! command -v go >/dev/null; then
	step "install go $GO_VERSION"
	curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | tar -C /usr/local -xz || fail "install go"
	export PATH=/usr/local/go/bin:$PATH
fi

# fetch_tar <path> <dir> downloads and extracts a tar into dir, from scratch
# on each attempt: a coordinator restart cuts the stream.
fetch_tar() {
	local attempt
	for attempt in 1 2 3 4 5; do
		rm -rf "$2" && mkdir -p "$2" || return 1
		api GET "$1" | tar -x -C "$2" && return 0
		sleep 30
	done
	return 1
}

cd "$WORK" || fail "cd $WORK"
step "download source"
fetch_tar source src || fail "download source"
step "download chain data"
fetch_tar golden data || fail "download chain data"
step "download genesis"
api GET genesis -o genesis.json || fail "download genesis"

step "build"
go -C src/contribs/gnoreplay mod tidy || fail "go mod tidy"
go -C src/contribs/gnoreplay build -o "$WORK/gnoreplay" . || fail "build"

step "replay"
# The replay's progress, for the coordinator's pages, as the replay updates
# it. One try each: the next one comes soon enough.
(
	while sleep {{.ProgressSeconds}}; do
		[ -s progress.json ] && curl -fsS -m 10 -o /dev/null -H "Authorization: Bearer $TOKEN" \
			--data-binary @progress.json "$URL/worker/$JOB/progress"
	done
) &
reporter=$!
GNOROOT="$WORK/src" ./gnoreplay {{.ReplayArgs}}
rc=$?
kill $reporter
# 2: diffs found, a result like any other.
if [ $rc -ne 0 ] && [ $rc -ne 2 ]; then
	fail "gnoreplay exited with $rc"
fi

step "upload report"
api POST report --data-binary @report.json || fail "upload report"
step "done"
