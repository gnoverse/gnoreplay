#!/usr/bin/env bash
# Keeps the golden snapshots replays need, and removes the rest: the current
# target, the directories passed as arguments (the previous target, which a
# running job may still be copying from), and anything younger than an hour (a
# snapshot still being written, or left by a killed refresh).
#
#   GOLDEN_ROOT  where the snapshots live (default /var/lib/gnoreplay)
set -euo pipefail

GOLDEN_ROOT=${GOLDEN_ROOT:-/var/lib/gnoreplay}

target=$(readlink -f "$GOLDEN_ROOT/golden" 2>/dev/null || true)
now=$(date -u +%s)
# Newest first: their names are timestamps.
while IFS= read -r dir; do
	[ -n "$dir" ] || continue
	if [ "$dir" = "$target" ]; then
		continue
	fi
	skip=false
	for keep in "$@"; do
		if [ "$dir" = "$keep" ]; then
			skip=true
			break
		fi
	done
	if [ "$skip" = true ]; then
		continue
	fi
	if [ $((now - $(stat -c %Y "$dir"))) -le 3600 ]; then
		continue
	fi
	rm -rf "$dir"
done < <(ls -1d "$GOLDEN_ROOT"/golden-* 2>/dev/null | sort -r)
