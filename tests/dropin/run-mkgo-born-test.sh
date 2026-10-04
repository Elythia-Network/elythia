#!/bin/bash
# mk-go 生まれの DB を Misskey TS に引き渡す e2e orchestrator (#2379)。
#
# ## 既存の swap test と何が違うか
#
#   swap test    : TS → mk-go → TS   DB を作ったのは TypeORM
#   本 test      : mk-go → TS        DB を作ったのは **mk-go の migration**
#
# 後者の方が難しい。TS は **一度も触っていない DB** を受け取るので、カラム型 /
# 制約 / enum / index 名 / default が TypeORM の期待と少しでも違えば起動しない。
#
# run-swap-test.sh のコメントが既にこの穴を明記していた:
#
#   「mk-go 由来 DB (= 000029 の seed だけがある状態) に TS を繋ぐ経路は
#     ここでは通らない」
#
# `TestMigrationSeed_CoversUpstream` は seed 一覧と upstream migration file の
# **静的な突き合わせ**であって、実際に TS を起動して確かめてはいない。本 test が
# その穴を埋める。
#
# ## なぜ重要か
#
# 「mk-go で始めた人が Misskey に移れるか」に答えるのはこの経路だけ。移れることは
# 保証しない (#3191) ので、守る対象ではなく、どこまで移れるかを測る対象として回す
# (落ちたときの手順は docs/dropin-e2e.md の「復路は測る対象」)。
#
# ## 流れ
#
#   1. clean volume から **mk-A** を起動 (DB-A は mk-go の migration が作る)
#   2. alice / bob / follow / note を作る
#   3. mk-A を止めて TS-A に差し替える
#   4. TS が起動するか / migration を再実行しないか / データを読めるか / 連合が続くか

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

BASE=docker-compose.dropin.yml
OVERLAY=docker-compose.dropin.mk.yml
DIAG_DIR=${DIAG_DIR:-/tmp/dropin-logs}

cleanup() {
  local code=$?
  if [ "$code" -ne 0 ]; then
    echo "===> capturing diagnostics (exit=$code) -> $DIAG_DIR"
    mkdir -p "$DIAG_DIR"
    docker compose -f "$BASE" -f "$OVERLAY" ps > "$DIAG_DIR/ps.log" 2>&1 || true
    docker compose -f "$BASE" -f "$OVERLAY" logs --no-color --tail=2000 \
      > "$DIAG_DIR/compose.log" 2>&1 || true
  fi
  echo "===> cleanup"
  docker compose -f "$BASE" -f "$OVERLAY" down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

# 前回の残骸があると「mk-go 生まれ」でなくなるので必ず volume ごと消す。
echo "===> stage 0: ensure clean volumes (mk-go 生まれにするため必須)"
docker compose -f "$BASE" -f "$OVERLAY" down -v >/dev/null 2>&1 || true

echo "===> stage 1: bring up mk-A (clean DB) + TS-B"
docker compose -f "$BASE" -f "$OVERLAY" up -d --build

echo "===> stage 1b: wait for mk-A + TS-B healthy"
deadline=$(($(date +%s) + 300))
while :; do
  healthy=$(docker compose -f "$BASE" -f "$OVERLAY" ps --format json | python3 -c "
import sys, json
ls=[json.loads(l) for l in sys.stdin if l.strip()]
print(len([s for s in ls if s.get('Service') in ('app-a','app-b') and s.get('Health')=='healthy']))
")
  if [ "$healthy" = "2" ]; then
    break
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "FAIL: mk-A / TS-B did not become healthy within 300s"
    docker compose -f "$BASE" -f "$OVERLAY" ps
    docker compose -f "$BASE" -f "$OVERLAY" logs app-a | tail -50
    exit 1
  fi
  sleep 3
done

echo "===> stage 2: setup alice / bob / follow / baseline note on mk-A"
docker compose -f "$BASE" -f "$OVERLAY" --profile test run --rm test-runner pytest test_swap_setup.py -v

# TypeORM の bookkeeping を控える。mk-go 生まれの DB では 000029 の seed だけが
# 入っている状態のはず。TS がここに行を足したら migration を再実行している。
migrations_digest() {
  local sql
  sql=$(cat <<'SQL'
SELECT count(*) || ':' || coalesce(md5(string_agg("name", ',' ORDER BY "name")), '-') FROM "migrations";
SQL
)
  docker compose -f "$BASE" exec -T postgres-a \
    psql -U misskey -d misskey -t -A -c "$sql" 2>/dev/null | tr -d '[:space:]'
}
MIGRATIONS_BEFORE=$(migrations_digest)
echo "     migrations bookkeeping (mk-go 生まれ): $MIGRATIONS_BEFORE"

echo "===> stage 3: stop mk-A backend"
docker compose -f "$BASE" -f "$OVERLAY" stop app-a

echo "===> stage 4: bring up TS-A on the mk-go-created DB"
# overlay を指定せず base のみで up することで app-a が TS の image になる。
# **ここが本 test の核心。** TS は自分が作っていない schema を受け取る。
docker compose -f "$BASE" up -d --force-recreate app-a

echo "===> stage 4b: wait for TS-A healthy"
deadline=$(($(date +%s) + 240))
while :; do
  state=$(docker compose -f "$BASE" ps --format json | python3 -c "
import sys, json
ls=[json.loads(l) for l in sys.stdin if l.strip()]
ms=[s for s in ls if s.get('Service')=='app-a']
print(ms[0].get('Health') if ms else 'missing')
")
  if [ "$state" = "healthy" ]; then
    break
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "FAIL: TS-A did not become healthy on the mk-go-created DB within 240s"
    echo "      = mk-go の migration が作った schema を TypeORM が受け付けなかった"
    docker compose -f "$BASE" logs app-a | tail -80
    exit 1
  fi
  sleep 3
done

echo "===> stage 4c: restart nginx-a so the upstream re-resolves to TS-A"
docker compose -f "$BASE" restart nginx-a

echo "===> stage 4d: assert TS did not re-run migrations"
MIGRATIONS_AFTER=$(migrations_digest)
echo "     migrations bookkeeping (TS 起動後):    $MIGRATIONS_AFTER"
if [ -z "$MIGRATIONS_BEFORE" ] || [ -z "$MIGRATIONS_AFTER" ]; then
  echo "FAIL: could not read the migrations bookkeeping table"
  exit 1
fi
if [ "$MIGRATIONS_BEFORE" != "$MIGRATIONS_AFTER" ]; then
  echo "FAIL: TS re-ran migrations on the mk-go-created DB"
  echo "  before: $MIGRATIONS_BEFORE"
  echo "  after:  $MIGRATIONS_AFTER"
  echo "  = mk-go の migration seed (000029) に漏れがある"
  docker compose -f "$BASE" logs app-a | tail -80
  exit 1
fi

echo "===> stage 5: verify TS can serve the mk-go-created データ"
docker compose -f "$BASE" --profile test run --rm test-runner pytest test_mkgo_born_verify.py -v

echo "===> all stages PASS"
