#!/usr/bin/env bash
# Deletes gnoreplay worker servers that have run one job, or been idle, for
# too long. A failsafe independent of gnoreplay-server (which enforces the
# same limits itself): run it from a timer on the coordinator, and from
# somewhere else (e.g. the scheduled GitHub workflow) in case the coordinator
# is down. The server labels each worker with the job it runs (gnoreplay-job,
# absent while idle), and since when (gnoreplay-since, in Unix seconds).
#
#   HCLOUD_TOKEN or HCLOUD_TOKEN_FILE  Hetzner Cloud API token (read & write)
#   MAX_AGE     max time on one job, in seconds     (default 14400, 4h)
#   MAX_IDLE    max time idle, in seconds           (default 3900, 65 min: the
#               server deletes idle workers before their billing hour ends)
#   HCLOUD_API  API base URL                        (default https://api.hetzner.cloud/v1)
#
# Exits non-zero if the API could not be read or a deletion failed.
set -euo pipefail

MAX_AGE=${MAX_AGE:-14400}
MAX_IDLE=${MAX_IDLE:-3900}
HCLOUD_API=${HCLOUD_API:-https://api.hetzner.cloud/v1}
if [ -z "${HCLOUD_TOKEN:-}" ] && [ -n "${HCLOUD_TOKEN_FILE:-}" ]; then
	HCLOUD_TOKEN=$(tr -d '[:space:]' <"$HCLOUD_TOKEN_FILE")
fi
: "${HCLOUD_TOKEN:?HCLOUD_TOKEN or HCLOUD_TOKEN_FILE is required}"

api() {
	# The token goes through a curl config on a pipe, not the argv every local
	# user can read from /proc (the convention the git fetches follow).
	curl -fsS --retry 3 --config <(printf 'header = "Authorization: Bearer %s"\n' "$HCLOUD_TOKEN") "$@"
}

now=$(date -u +%s)
failed=0
page=1
while [ -n "$page" ]; do
	res=$(api "$HCLOUD_API/servers?label_selector=gnoreplay%3Dworker&per_page=50&page=$page")
	while read -r id name job since created; do
		[ -n "$id" ] || continue
		# Without a valid label, from the server's creation.
		case $since in
		'' | *[!0-9]*) since=$(date -u -d "$created" +%s) ;;
		esac
		if [ "$job" = - ]; then
			state=idle limit=$MAX_IDLE
		else
			state="on job $job" limit=$MAX_AGE
		fi
		age=$((now - since))
		if [ "$age" -le "$limit" ]; then
			continue
		fi
		echo "deleting server $id ($name): $state for ${age}s > ${limit}s"
		if ! api -X DELETE "$HCLOUD_API/servers/$id" -o /dev/null; then
			echo "failed to delete server $id ($name)" >&2
			failed=1
		fi
	done < <(jq -r '.servers[] | "\(.id) \(.name) \(.labels["gnoreplay-job"] // "-") \(.labels["gnoreplay-since"] // "-") \(.created)"' <<<"$res")
	page=$(jq -r '.meta.pagination.next_page // empty' <<<"$res")
done
exit $failed
