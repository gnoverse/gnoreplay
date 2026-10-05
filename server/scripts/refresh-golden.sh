#!/usr/bin/env bash
# Refreshes the golden copy of the reference node's chain data.
#
# The node holds an exclusive lock on its DBs, so it is stopped for the copy
# (seconds with reflinks) and restarted. Run from cron/systemd timer, e.g.
# every 4 hours. The swap is atomic: jobs copy from whatever `golden` points
# at when they start.
#
#   NODE_SERVICE   systemd unit of the reference node   (default gnoland)
#   NODE_DATA_DIR  its data dir                         (default /var/lib/gnoland)
#   GOLDEN_ROOT    where snapshots live                 (default /var/lib/gnoreplay)
set -euo pipefail

NODE_SERVICE=${NODE_SERVICE:-gnoland}
NODE_DATA_DIR=${NODE_DATA_DIR:-/var/lib/gnoland}
GOLDEN_ROOT=${GOLDEN_ROOT:-/var/lib/gnoreplay}

snap="$GOLDEN_ROOT/golden-$(date +%Y%m%dT%H%M%S)"
mkdir -p "$snap/db"

systemctl stop "$NODE_SERVICE"
trap 'systemctl start "$NODE_SERVICE"' EXIT
for db in blockstore.db state.db; do
	cp -a --reflink=auto "$NODE_DATA_DIR/db/$db" "$snap/db/$db"
done
systemctl start "$NODE_SERVICE"
trap - EXIT

ln -sfn "$snap" "$GOLDEN_ROOT/golden.tmp"
mv -T "$GOLDEN_ROOT/golden.tmp" "$GOLDEN_ROOT/golden"

# Keep the two most recent snapshots (a running job may still be copying
# from the previous one).
ls -1d "$GOLDEN_ROOT"/golden-* | sort | head -n -2 | xargs -r rm -rf
