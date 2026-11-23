#!/bin/bash
# CherryPick 4.18.0-beta.1 が migration を流した DB の形を、テストの fixture に書き出す (#4247)。
#
#   tests/cherrypick/gen-fixture.sh
#
# 書き出すのは schema (表・型・索引・制約) と migrations の表の記録だけで、行は含めない。
# テストは自分の schema に読み込むので、`public.` の修飾と、search_path を空にする
# 行を落とす。
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
out="$root/internal/testutil/testdata/cherrypick-4.18.0-beta.1.sql.gz"
compose=(docker compose -f "$here/compose.yml")

cleanup() { "${compose[@]}" down -v > /dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

"${compose[@]}" up -d --wait postgres redis
"${compose[@]}" up -d cherrypick
# CherryPick は migration を流し終えてから待ち受ける
deadline=$(($(date +%s) + 600))
until curl -fsS -m 3 -X POST -H 'Content-Type: application/json' -d '{}' http://127.0.0.1:53095/api/ping 2> /dev/null | grep -q pong; do
	if [ "$(date +%s)" -ge "$deadline" ]; then
		"${compose[@]}" logs --tail 50 cherrypick >&2
		echo "CherryPick did not start" >&2
		exit 1
	fi
	sleep 5
done
"${compose[@]}" stop cherrypick > /dev/null

dump() { "${compose[@]}" exec -T postgres pg_dump -U misskey -d misskey --no-owner --no-privileges "$@"; }
{
	echo "-- CherryPick 4.18.0-beta.1 の DB の形。tests/cherrypick/gen-fixture.sh が作る。手で直さない"
	{
		dump --schema-only
		dump --data-only --inserts -t migrations
	} | grep -vE '^(--|\\restrict |\\unrestrict |SET |SELECT pg_catalog\.set_config\()' |
		sed -E 's/(^|[^A-Za-z0-9_"])public\./\1/g' | cat -s
} | gzip -n -9 > "$out"
echo "wrote $out"
