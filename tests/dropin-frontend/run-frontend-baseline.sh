#!/bin/bash
# Phase 14-1 (#381): 3 TS インスタンス frontend e2e の baseline 実行スクリプト。
#
# 流れ:
#   1. 3 TS (A, B, C) + certs + nginx を起動
#   2. 各 app が healthy になるまで待つ
#   3. cypress-runner を走らせる (profile test)
#   4. 終了コードを伝搬
#   5. cleanup
#
# Phase 14-3 で mk-go overlay を足したらこのスクリプトを拡張する。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

COMPOSE=tests/dropin-frontend/compose.yml

cleanup() {
  echo "===> cleanup"
  # down -v でコンテナを消す前にログを保存する (Devin #398 #1 の同パターン)。
  if [ "${SCRIPT_EXIT_CODE:-0}" != "0" ]; then
    local log_dir="${DROPIN_LOG_DIR:-$REPO_ROOT/.dropin-logs}"
    mkdir -p "$log_dir"
    echo "===> saving compose logs to $log_dir"
    docker compose -f "$COMPOSE" logs > "$log_dir/compose.log" 2>&1 || true
    docker compose -f "$COMPOSE" ps > "$log_dir/ps.log" 2>&1 || true
  fi
  docker compose -f "$COMPOSE" down -v >/dev/null 2>&1 || true
}
track_exit() { SCRIPT_EXIT_CODE=$?; }
trap 'track_exit; cleanup' EXIT

echo "===> stage 1: bring up TS-A/B/C stack"
docker compose -f "$COMPOSE" up -d

echo "===> stage 2: wait for three instances to become healthy"
deadline=$(($(date +%s) + 300))
while :; do
  healthy=$(docker compose -f "$COMPOSE" ps --format json | python3 -c "
import sys, json
ls=[json.loads(l) for l in sys.stdin if l.strip()]
h=[s for s in ls if s.get('Service') in ('app-a','app-b','app-c') and s.get('Health')=='healthy']
print(len(h))
")
  if [ "$healthy" = "3" ]; then
    break
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "FAIL: app-a/b/c did not become healthy within 300s"
    docker compose -f "$COMPOSE" ps
    exit 1
  fi
  sleep 3
done

echo "===> stage 3: run cypress baseline spec"
docker compose -f "$COMPOSE" --profile test run --rm cypress-runner

echo "===> baseline PASS"
