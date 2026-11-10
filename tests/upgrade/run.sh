#!/usr/bin/env bash
# 前のリリースで作った DB を、今のコードが引き継げるかを確かめる (#4123)。Makefile の
# upgrade-test から呼ぶ。手順は compose.yml の冒頭。
#
# UPGRADE_FROM で前の版を選べる。既定は HEAD から辿れる最新のリリースのタグ (x.y.z。
# HEAD そのものに付いたタグは除く。同じ版どうしを比べても意味が無いため)。版を書き
# 写さないのは、リリースのたびに直し忘れて古い版と比べ続けないため。
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
# make build が作るもの。HEAD からビルドしたものでないと、verify の mkGoCommit の照合で落ちる
bin=$root/built/elythia
out=$here/out
base=http://127.0.0.1:61814

if [ -z "${UPGRADE_FROM:-}" ]; then
	UPGRADE_FROM=$(git -C "$root" tag --list --merged HEAD --no-contains HEAD --sort=-v:refname | grep -Ex '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)
fi
[ -n "$UPGRADE_FROM" ] || { echo "HEAD から辿れるリリースのタグが無い (git fetch --tags)" >&2; exit 1; }
export UPGRADE_FROM
compose=(docker compose -f "$here/compose.yml")
head_commit=$(git -C "$root" rev-parse --short HEAD)
echo "==> upgrade from $UPGRADE_FROM to $head_commit"

# 前の版の image は、タグを push した後に docker の workflow が作る。その間は取れないので、
# 理由の分かる文言で止める
if ! docker pull -q "ghcr.io/elythia-network/elythia:$UPGRADE_FROM" > /dev/null; then
	echo "image ghcr.io/elythia-network/elythia:$UPGRADE_FROM を取れない (タグの push 直後なら docker の workflow の完了を待つ。別の版は UPGRADE_FROM で選ぶ)" >&2
	exit 1
fi

# 当たっている migration の版 (core の系列)。dirty なら t が付く
schema_version() {
	"${compose[@]}" exec -T postgres psql -U misskey -d misskey -tAc "SELECT version || CASE WHEN dirty THEN ' dirty' ELSE '' END FROM schema_migrations"
}

wait_healthy() {
	for _ in $(seq 1 90); do
		if curl -fsS "$base/healthz" > /dev/null 2>&1; then return 0; fi
		sleep 1
	done
	echo "the server did not become healthy" >&2
	return 1
}

rm -rf "$out"
mkdir -p "$out"
"${compose[@]}" --profile old down -v > /dev/null 2>&1 || true
"${compose[@]}" up -d --wait postgres redis

# 1. 前のリリースで migrate して起動し、データを作る
"${compose[@]}" --profile old run --rm -T old migrate -config .config/default.yml -direction up > "$out/old-migrate.log" 2>&1 ||
	{ cat "$out/old-migrate.log" >&2; exit 1; }
before=$(schema_version)
echo "==> $UPGRADE_FROM migrated the schema to $before"
# 前の回の Elythia などが残ってポートを握っていると、seed と verify がそちらへ向かう
if curl -fsS "$base/healthz" > /dev/null 2>&1; then echo "$base で既に何かが動いている" >&2; exit 1; fi
"${compose[@]}" --profile old up -d old
if ! wait_healthy; then "${compose[@]}" --profile old logs old | tail -50 >&2; exit 1; fi
UPGRADE_EXPECT_VERSION=$UPGRADE_FROM python3 "$here/scenario.py" seed ||
	{ "${compose[@]}" --profile old logs --no-log-prefix old > "$out/old.log" 2>&1; exit 1; }
"${compose[@]}" --profile old logs --no-log-prefix old > "$out/old.log" 2>&1
"${compose[@]}" --profile old rm -sf old > /dev/null

# 2. 今のコードで migrate up して起動し、確かめる。作業ディレクトリ (ドライブのファイルの
# 置き場所) は out/ にする
# migrate は作業ディレクトリの migration/ を読むので、リポジトリの直下で動かす
(cd "$root" && "$bin" migrate -config "$here/elythia.yml" -direction up) > "$out/migrate.log" 2>&1 || { cat "$out/migrate.log" >&2; exit 1; }
after=$(schema_version)
latest=$(for f in "$root"/migration/*.up.sql; do b=${f##*/}; echo "${b%%_*}"; done | sort -n | tail -1 | sed 's/^0*//')
echo "==> the current code migrated the schema from $before to $after (latest migration: $latest)"
# 新しい migration が当たらずに緑になるのを防ぐ。前の版と同じなら、確かめたのは
# 「新しいコードが古いスキーマのまま動くか」だけになるので知らせる
[ "$after" = "$latest" ] || { echo "migrate up の後の版 $after が、最新の migration $latest と違う" >&2; exit 1; }
[ "$before" != "$after" ] || echo "注意: $UPGRADE_FROM と今のコードの間に新しい migration が無い" >&2
(cd "$out" && exec "$bin" serve -config "$here/elythia.yml") > "$out/new.log" 2>&1 &
pid=$!
trap 'kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true' EXIT
if ! wait_healthy; then tail -50 "$out/new.log" >&2; exit 1; fi
UPGRADE_EXPECT_COMMIT=$head_commit python3 "$here/scenario.py" verify
