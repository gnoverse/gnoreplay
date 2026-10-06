#!/usr/bin/env bash
# Deletes gnoreplay worker droplets older than MAX_AGE, whatever their job's
# state. A failsafe independent of gnoreplay-server (which enforces the same
# limit itself): run it from a timer on the coordinator, and from somewhere
# else (e.g. the scheduled GitHub workflow) in case the coordinator is down.
#
#   DO_TOKEN or DO_TOKEN_FILE  DigitalOcean API token (droplet read + delete)
#   TAG                        worker tag                 (default gnoreplay-worker)
#   MAX_AGE                    max age in seconds         (default 14400, 4h)
#   DO_API                     API base URL               (default https://api.digitalocean.com)
#
# Exits non-zero if the API could not be read or a deletion failed.
set -euo pipefail

TAG=${TAG:-gnoreplay-worker}
MAX_AGE=${MAX_AGE:-14400}
DO_API=${DO_API:-https://api.digitalocean.com}
if [ -z "${DO_TOKEN:-}" ] && [ -n "${DO_TOKEN_FILE:-}" ]; then
	DO_TOKEN=$(tr -d '[:space:]' <"$DO_TOKEN_FILE")
fi
: "${DO_TOKEN:?DO_TOKEN or DO_TOKEN_FILE is required}"

api() {
	curl -fsS --retry 3 -H "Authorization: Bearer $DO_TOKEN" "$@"
}

now=$(date -u +%s)
failed=0
url="$DO_API/v2/droplets?tag_name=$TAG&per_page=200"
while [ -n "$url" ]; do
	page=$(api "$url")
	while read -r id name created; do
		[ -n "$id" ] || continue
		age=$((now - $(date -u -d "$created" +%s)))
		if [ "$age" -le "$MAX_AGE" ]; then
			continue
		fi
		echo "deleting droplet $id ($name): created $created, ${age}s ago > ${MAX_AGE}s"
		if ! api -X DELETE "$DO_API/v2/droplets/$id" -o /dev/null; then
			echo "failed to delete droplet $id ($name)" >&2
			failed=1
		fi
	done < <(jq -r '.droplets[] | "\(.id) \(.name) \(.created_at)"' <<<"$page")
	url=$(jq -r '.links.pages.next // empty' <<<"$page")
done
exit $failed
