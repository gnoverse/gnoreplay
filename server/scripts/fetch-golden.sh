#!/usr/bin/env bash
# Refreshes the golden copy of the chain data from a snapshot site such as
# snapshots.gno.land, instead of a reference node (refresh-golden.sh): the
# newest snapshot its index.json lists is downloaded, checked against its
# sha256, and only the two DBs replays read are kept. Run from a systemd timer;
# each run downloads a whole snapshot (~2 GB for gnoland-1), so not too often.
# The swap is atomic: jobs copy from whatever `golden` points at when they
# start.
#
#   SNAPSHOTS_URL  the snapshot site      (default https://snapshots.gno.land)
#   GOLDEN_ROOT    where snapshots live   (default /var/lib/gnoreplay)
set -euo pipefail

SNAPSHOTS_URL=${SNAPSHOTS_URL:-https://snapshots.gno.land}
GOLDEN_ROOT=${GOLDEN_ROOT:-/var/lib/gnoreplay}

read -r file sum < <(curl -fsS --retry 5 "$SNAPSHOTS_URL/index.json" |
	jq -er '.snapshots | max_by(.height) | "\(.file) \(.sha256)"')

snap="$GOLDEN_ROOT/golden-$(date +%Y%m%dT%H%M%S)"
mkdir -p "$snap"
trap 'rm -rf "$snap"' EXIT
curl -fsS --retry 5 -o "$snap/snapshot.tar.zst" "$SNAPSHOTS_URL/$file"
echo "$sum  $snap/snapshot.tar.zst" | sha256sum -c --quiet
# The archive is a node's whole db/ (with the app state): keep what replays
# read, readable by the server.
zstd -dc "$snap/snapshot.tar.zst" | tar -x -C "$snap" --no-same-owner db/blockstore.db db/state.db
chmod -R a+rX "$snap/db"
rm "$snap/snapshot.tar.zst"
trap - EXIT
echo "$snap: $file"

ln -sfn "$snap" "$GOLDEN_ROOT/golden.tmp"
mv -T "$GOLDEN_ROOT/golden.tmp" "$GOLDEN_ROOT/golden"

# Keep the two most recent snapshots (a running job may still be copying
# from the previous one).
ls -1d "$GOLDEN_ROOT"/golden-* | sort | head -n -2 | xargs -r rm -rf
