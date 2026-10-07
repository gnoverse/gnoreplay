#!/bin/bash
# Prints a repo's viewers file: the GitHub user ID and login of each of its
# collaborators (outside collaborators, team members and org owners
# included), who may then see the repo's results once signed in. Run it as
# someone with push access to the repo, with the gh CLI: GitHub only lists
# collaborators to them, which is why the server can't do it with its
# read-only token. For example:
#
#   scripts/sync-viewers.sh gnolang/gno-fixes |
#     ssh root@coordinator 'cat >/etc/gnoreplay/gno-fixes-viewers.new &&
#       chmod 640 /etc/gnoreplay/gno-fixes-viewers.new && chgrp gnoreplay /etc/gnoreplay/gno-fixes-viewers.new &&
#       mv /etc/gnoreplay/gno-fixes-viewers.new /etc/gnoreplay/gno-fixes-viewers'
#
# The server rereads the file when it changes. Rerun when collaborators change.
set -euo pipefail
repo=${1:?usage: sync-viewers.sh owner/name}
list=$(gh api --paginate "repos/$repo/collaborators?per_page=100" --jq '.[] | "\(.id) \(.login)"')
# Refuse to write an empty list over a good one (e.g. a token without access).
[ -n "$list" ] || { echo "no collaborators listed for $repo" >&2; exit 1; }
echo "# Viewers of $repo's results: its collaborators on $(date -u +%Y-%m-%d)."
sort -n <<<"$list"
