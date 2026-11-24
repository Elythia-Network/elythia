#!/usr/bin/env bash
# CherryPick 4.18.0-beta.1 の DB を、今のコードで Elythia へ移せるかを確かめる (#4249)。
# Makefile の cherrypick-test から呼ぶ。
#
# 1. compose.yml で CherryPick を立て、scenario.py seed でデータを作る (承認待ち、予約削除、
#    通報の自動処理の規則、隔離したホスト)。CherryPick を止める
# 2. make build したものを、運営者の手順 (docs/migration-from-cherrypick.md) どおりに流し、
#    それぞれの終了コードを確かめる: check → migrate up (止まる) → migrate up
#    -allow-cherrypick → migrate up と serve (convert まで止まる) → convert -apply → migrate up
# 3. 起動して scenario.py verify で確かめ、止めてから cleanup -apply を流す。もう一度
#    migrate up して起動し、列と表を消した後も読み書きできることを確かめる
#
# gen-fixture.sh と同じ compose (project elythia-cherrypick) を使うので、同時に流さない。
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
bin=$root/built/elythia
cfg=$here/elythia.yml
out=$here/out
cherrypick=http://127.0.0.1:53095
base=http://127.0.0.1:61815
compose=(docker compose -f "$here/compose.yml")

rm -rf "$out"
mkdir -p "$out"
serve_pid=
start_serve() {
	(cd "$out" && exec "$bin" serve -config "$cfg") >> "$out/serve.log" 2>&1 &
	serve_pid=$!
	for _ in $(seq 1 90); do
		if curl -fsS "$base/healthz" > /dev/null 2>&1; then return 0; fi
		sleep 1
	done
	tail -50 "$out/serve.log" >&2
	echo "Elythia did not start" >&2
	exit 1
}
stop_serve() {
	if [ -n "$serve_pid" ]; then
		kill "$serve_pid" 2> /dev/null || true
		wait "$serve_pid" 2> /dev/null || true
		serve_pid=
	fi
}
trap stop_serve EXIT

# expect <終了コード> <名前> <コマンド...>: コマンドを流し、終了コードが違えば出力を出して止める
expect() {
	local want=$1 name=$2
	shift 2
	local rc=0
	(cd "$root" && "$@") > "$out/$name.log" 2>&1 || rc=$?
	if [ "$rc" != "$want" ]; then
		cat "$out/$name.log" >&2
		echo "$name: exit $rc, want $want" >&2
		exit 1
	fi
	echo "==> $name: exit $rc"
}

# expect_log <名前> <文字列>: そのログにその文字列が無ければ、ログを出して止める
expect_log() {
	if ! grep -qF -- "$2" "$out/$1.log"; then
		cat "$out/$1.log" >&2
		echo "$1: \"$2\" not in the output" >&2
		exit 1
	fi
}

"${compose[@]}" down -v > /dev/null 2>&1 || true
"${compose[@]}" up -d --wait postgres redis
"${compose[@]}" up -d cherrypick
deadline=$(($(date +%s) + 600))
until curl -fsS -m 3 -X POST -H 'Content-Type: application/json' -d '{}' "$cherrypick/api/ping" 2> /dev/null | grep -q pong; do
	if [ "$(date +%s)" -ge "$deadline" ]; then
		"${compose[@]}" logs --tail 50 cherrypick >&2
		echo "CherryPick did not start" >&2
		exit 1
	fi
	sleep 5
done

python3 "$here/scenario.py" seed "$cherrypick" "$out/state.json"
scheduled=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["notes"]["scheduled"])' "$out/state.json")
psql() { "${compose[@]}" exec -T postgres psql -U misskey -d misskey -v ON_ERROR_STOP=1 -qtAc "$1"; }
# 隔離は CherryPick に設定する口が無いので、行を直接入れる。予約削除は期限を過去にする
psql "INSERT INTO instance (id, \"firstRetrievedAt\", host, \"quarantineLimited\") VALUES ('cherrypicktestq01', now(), 'remote.example', true)"
psql "UPDATE note SET \"deleteAt\" = now() - interval '1 hour' WHERE id = '$scheduled'"
# 承認制より前からの会員 (承認の印が無く、申請の理由も無い)。CherryPick は approved を
# 既定値 false で足し、既存の行を埋めないので、この形になる
psql "UPDATE \"user\" SET approved = false WHERE \"usernameLower\" = 'dave' AND host IS NULL"
# 初めの管理者 (初期ユーザー) も承認の印が無いことがある。convert は凍結しない
psql "UPDATE \"user\" SET approved = false WHERE \"usernameLower\" = 'admin' AND host IS NULL"
"${compose[@]}" stop cherrypick > /dev/null

expect 3 check "$bin" cherrypick check -config "$cfg"
expect 1 migrate-refused "$bin" migrate -config "$cfg"
expect 0 migrate-allowed "$bin" migrate -config "$cfg" -allow-cherrypick
expect 1 migrate-before-convert "$bin" migrate -config "$cfg"
expect 1 serve-before-convert "$bin" serve -config "$cfg"
expect_log serve-before-convert 'refusing to start'
expect_log migrate-before-convert 'run `elythia cherrypick convert`'
expect 2 convert-unchosen "$bin" cherrypick convert -config "$cfg"
expect_log convert-unchosen '-unapproved-members suspend|delete|approve'
# 対象の会員は名前で出し、初期ユーザーは対象から外して別に出す
expect_log convert-unchosen 'users without approval      1  -> (choose)'
expect_log convert-unchosen '    @dave'
expect_log convert-unchosen 'left as they are: the root user, administrators and moderators without approval  1'
expect_log convert-unchosen '    @admin'
expect 0 convert "$bin" cherrypick convert -config "$cfg" -apply -unapproved-members suspend -quarantined block \
	-resolvers-out "$out/resolvers.json" -accept-manual-reports
expect 0 migrate-after-convert "$bin" migrate -config "$cfg"
expect 0 check-after-convert "$bin" cherrypick check -config "$cfg"

start_serve
python3 "$here/scenario.py" verify "$base" "$out/state.json" "$out/resolvers.json"
stop_serve

expect 0 cleanup "$bin" cherrypick cleanup -config "$cfg" -apply -backup-taken
expect_log cleanup 'dropped 4 column(s) and table(s)'
expect 0 check-after-cleanup "$bin" cherrypick check -config "$cfg"
expect 0 migrate-after-cleanup "$bin" migrate -config "$cfg"
start_serve
python3 "$here/scenario.py" verify-after-cleanup "$base" "$out/state.json"
stop_serve
echo "==> moved from CherryPick 4.18.0-beta.1"
