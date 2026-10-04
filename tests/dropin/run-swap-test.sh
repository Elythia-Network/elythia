#!/bin/bash
# Phase 13-2 (#367) drop-in swap test orchestrator.
#
# 流れ:
#   1. TS-A + TS-B stack を起動 (Phase 13-1 と同じ)
#   2. test_swap_setup.py で alice/bob/follow/baseline note を作る
#   3. TS-A backend (app-a) を停止 — DB-A / Redis-A は走り続ける
#   4. mk-go overlay で app-a を mk-go ビルドに置き換えて起動
#   5. mk-A の healthy 待ち
#   6. test_swap_verify.py で state preserved + 新規 federation 動作を確認
#   7. mk-A backend を停止して TS-A に戻す (#1082 SHOULD shape)
#   8. TS-A 起動 healthy 待ち + nginx-a restart
#   9. test_swap_roundtrip_verify.py で TS 戻し後の連合継続を確認
#      (6b-9 の復路とその準備は守る対象ではなく測る対象。#3191、docs/dropin-e2e.md の
#      「復路は測る対象」)
#  10. cleanup
#
# pytest セッションを跨いで docker compose を切り替える必要があるため、
# orchestration を bash で外側に置く。test runner コンテナ内に docker CLI を
# 入れるとイメージが太るしソケットマウントの security trade-off があるので、
# bash + 複数回 pytest 起動の構成にした。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

BASE=tests/dropin/compose.yml
OVERLAY=tests/dropin/compose.mk.yml
# fedibird mock を同居させる (#2376)。mock は RSA と Ed25519 の両方の鍵を持つ
# ので、「mk-go が Ed25519 で連合していた相手と、TS に戻したあと RSA で継続
# できるか」を実測できる。TS-A / TS-B の挙動には影響しない (別サービス)。
FEDIBIRD=tests/dropin/compose.fedibird.yml

# 失敗時の診断情報を残してから stack を落とす。
#
# 以前は down -v だけを行っていたため、workflow 側の "Capture docker compose
# logs on failure" step が走る時点でコンテナが全滅しており、artifact の
# compose.log が 0 バイト / ps.log がヘッダ行のみになっていた。nightly が
# 80 日間失敗し続けても原因が追えなかった一因。
DIAG_DIR="${DROPIN_DIAG_DIR:-/tmp/dropin-logs}"
cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "===> capturing diagnostics (exit=$rc) -> $DIAG_DIR"
    mkdir -p "$DIAG_DIR" || true
    docker compose -f "$BASE" -f "$OVERLAY" -f "$FEDIBIRD" ps > "$DIAG_DIR/ps.log" 2>&1 || true
    docker compose -f "$BASE" -f "$OVERLAY" -f "$FEDIBIRD" logs --no-color --tail=2000 \
      > "$DIAG_DIR/compose.log" 2>&1 || true
  fi
  echo "===> cleanup"
  docker compose -f "$BASE" -f "$OVERLAY" -f "$FEDIBIRD" down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "===> stage 1: bring up TS-A + TS-B stack"
docker compose -f "$BASE" -f "$FEDIBIRD" up -d --build

# fedibird mock も healthy を待つ (#2376)。Ed25519 で連合する相手役。
echo "===> stage 1b: wait for both TS instances healthy"
deadline=$(($(date +%s) + 240))
while :; do
  states=$(docker compose -f "$BASE" ps --format json | python3 -c "
import sys, json
ls=[json.loads(l) for l in sys.stdin if l.strip()]
healthy=[s for s in ls if s.get('Service') in ('app-a','app-b') and s.get('Health')=='healthy']
print(len(healthy))
")
  if [ "$states" = "2" ]; then
    break
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "FAIL: TS instances did not become healthy within 240s"
    docker compose -f "$BASE" ps
    exit 1
  fi
  sleep 3
done

echo "===> stage 2: setup alice / bob / follow / baseline note"
docker compose -f "$BASE" -f "$FEDIBIRD" --profile test run --rm test-runner pytest test_swap_setup.py -v

echo "===> stage 3: stop TS-A backend (DB-A / Redis-A keep state)"
docker compose -f "$BASE" -f "$FEDIBIRD" stop app-a

echo "===> stage 4: bring up mk-go overlay on instance A"
# --force-recreate で旧 TS-A image の停止 container を確実に破棄し、新規
# mk-A container として起動する。image label 一致時に compose が container
# を reuse して image が更新されない曖昧さを回避 (#1085 review #1)。
docker compose -f "$BASE" -f "$OVERLAY" -f "$FEDIBIRD" up -d --build --force-recreate app-a

echo "===> stage 5: wait for mk-A healthy"
deadline=$(($(date +%s) + 180))
while :; do
  state=$(docker compose -f "$BASE" -f "$OVERLAY" -f "$FEDIBIRD" ps --format json | python3 -c "
import sys, json
ls=[json.loads(l) for l in sys.stdin if l.strip()]
ms=[s for s in ls if s.get('Service')=='app-a']
print(ms[0].get('Health') if ms else 'missing')
")
  if [ "$state" = "healthy" ]; then
    break
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "FAIL: mk-A did not become healthy within 180s"
    docker compose -f "$BASE" -f "$OVERLAY" -f "$FEDIBIRD" logs app-a | tail -50
    exit 1
  fi
  sleep 3
done

echo "===> stage 5b: restart nginx-a so the upstream re-resolves to mk-A"
# stop app-a 中に nginx-a が backend を unreachable と覚え込む可能性があるので
# 念のため restart する。
docker compose -f "$BASE" -f "$OVERLAY" -f "$FEDIBIRD" restart nginx-a

echo "===> stage 6: verify state preserved on mk-A"
docker compose -f "$BASE" -f "$OVERLAY" -f "$FEDIBIRD" --profile test run --rm test-runner pytest test_swap_verify.py -v

# mk-go 独自機能が upstream 共有テーブルに残すデータを、mk-A 上で作っておく
# (#2372)。ここで作った行は TS-A に戻したあとも DB に残るので、stage 9 が
# 「TS がそれで壊れないこと」を検証する。
#
# 「独自機能は戻したら失われる、それでよい」は半分しか正しくない。機能は
# 失われるが**機能が書いたデータは残る**。chat / reversi は upstream にも
# ある機能で、テーブルも upstream のもの。連合部分だけが mk-go の追加なので、
# TS には upstream が想定していないリモート参照を含む行が残る。
echo "===> stage 6b: seed mk-go-only feature data (残留データを作る)"
docker compose -f "$BASE" -f "$OVERLAY" -f "$FEDIBIRD" --profile test run --rm test-runner pytest test_swap_seed_mkgo_only.py test_swap_seed_ed25519_peer.py -v

echo "===> stage 7: stop mk-A backend (roundtrip 戻し前準備)"
docker compose -f "$BASE" -f "$OVERLAY" -f "$FEDIBIRD" stop app-a

# TypeORM の bookkeeping テーブルを控える (#2244)。TS に戻したとき、seed した
# migration が「未実行」と判定されて再実行されると、適用済み DDL への
# ADD COLUMN 重複や DROP COLUMN によるデータ喪失が起きうる。再実行されれば
# TypeORM が migrations 行を追加するので、行数と内容の hash で検知できる。
#
# 注: 本 shape は TS-A が作った DB から始まるので、TS 由来の正式名の行が
# 最初から入っている。mk-go 由来 DB (= 000029 の seed だけがある状態) に TS を
# 繋ぐ経路はここでは通らないため、この check は general guard であって
# #2244 そのものの regression test ではない (あちらは
# internal/entitycompat の TestMigrationSeed_CoversUpstream が担保する)。
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
echo "     migrations bookkeeping before roundtrip: $MIGRATIONS_BEFORE"

echo "===> stage 8: bring up TS-A backend (overlay 解除で TS に戻す)"
# overlay を指定せず base のみで up することで、app-a が TS-A の image に戻る。
# --force-recreate で停止中の mk-A container を確実に破棄し、新規 TS-A
# container を起動する (#1085 review #1)。
docker compose -f "$BASE" -f "$FEDIBIRD" up -d --force-recreate app-a

echo "===> stage 8b: wait for TS-A healthy"
deadline=$(($(date +%s) + 180))
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
    echo "FAIL: TS-A (roundtrip) did not become healthy within 180s"
    docker compose -f "$BASE" logs app-a | tail -50
    exit 1
  fi
  sleep 3
done

echo "===> stage 8c: restart nginx-a so the upstream re-resolves to TS-A"
docker compose -f "$BASE" -f "$FEDIBIRD" restart nginx-a

echo "===> stage 8d: assert TS did not re-run migrations (#2244)"
MIGRATIONS_AFTER=$(migrations_digest)
echo "     migrations bookkeeping after roundtrip:  $MIGRATIONS_AFTER"
if [ -z "$MIGRATIONS_BEFORE" ] || [ -z "$MIGRATIONS_AFTER" ]; then
  echo "FAIL: could not read the migrations bookkeeping table"
  exit 1
fi
if [ "$MIGRATIONS_BEFORE" != "$MIGRATIONS_AFTER" ]; then
  echo "FAIL: TS re-ran migrations after the roundtrip"
  echo "  before: $MIGRATIONS_BEFORE"
  echo "  after:  $MIGRATIONS_AFTER"
  docker compose -f "$BASE" logs app-a | tail -50
  exit 1
fi

echo "===> stage 9: verify federation continuity after TS roundtrip (#1082)"
docker compose -f "$BASE" -f "$FEDIBIRD" --profile test run --rm test-runner pytest test_swap_roundtrip_verify.py -v

echo "===> all stages PASS"
