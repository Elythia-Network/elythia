#!/bin/bash
# Phase 14-3 (#394) drop-in frontend swap test orchestrator.
#
# 流れ:
#   1. TS-A / B / C stack を起動、cypress で baseline spec を実行
#   2. TS-A の backend を停止 (DB / Redis は維持)
#   3. mk-go overlay で app-a を差し替えて起動
#   4. cypress で swap spec を実行 (DB-A / Redis-A 共有のまま)
#   5. 両 run の結果を比較
#
# cypress runner は tests/dropin-frontend/compose.yml の `--profile test`
# service。CYPRESS_MODE env で spec 側から mode を識別する。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

BASE=tests/dropin-frontend/compose.yml
OVERLAY=tests/dropin-frontend/compose.mk.yml

cleanup() {
  echo "===> cleanup"
  # down -v でコンテナを消す前にログを保存する (Devin #398 #1)。
  # CI の "Capture logs on failure" は down 後に実行されてしまうので、
  # orchestrator 側でログを tmp に書き出しておく必要がある。
  #
  # 書き込み先は `DROPIN_LOG_DIR` (CI から指定) または repo 配下の
  # `.dropin-logs/` (gitignore 済)。失敗時のみ保存したいので
  # `SCRIPT_EXIT_CODE` を参照する。
  if [ "${SCRIPT_EXIT_CODE:-0}" != "0" ]; then
    local log_dir="${DROPIN_LOG_DIR:-$REPO_ROOT/.dropin-logs}"
    mkdir -p "$log_dir"
    echo "===> saving compose logs to $log_dir"
    docker compose -f "$BASE" -f "$OVERLAY" logs \
      > "$log_dir/compose.log" 2>&1 || true
    docker compose -f "$BASE" -f "$OVERLAY" ps \
      > "$log_dir/ps.log" 2>&1 || true
  fi
  docker compose -f "$BASE" -f "$OVERLAY" down -v >/dev/null 2>&1 || true
}
# set -e で exit した時 ERR が先に走らないので EXIT trap に値を渡す。
track_exit() { SCRIPT_EXIT_CODE=$?; }
trap 'track_exit; cleanup' EXIT

# -----------------------------------------------------------------------
# stage 1: TS-A / B / C stack 起動
# -----------------------------------------------------------------------
echo "===> stage 1: bring up TS-A / TS-B / TS-C stack"
docker compose -f "$BASE" up -d --build

echo "===> stage 1b: wait for three TS instances to become healthy"
deadline=$(($(date +%s) + 300))
while :; do
  healthy=$(docker compose -f "$BASE" ps --format json | python3 -c "
import sys, json
ls=[json.loads(l) for l in sys.stdin if l.strip()]
h=[s for s in ls if s.get('Service') in ('app-a','app-b','app-c') and s.get('Health')=='healthy']
print(len(h))
")
  if [ "$healthy" = "3" ]; then
    break
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "FAIL: TS instances did not become healthy within 300s"
    docker compose -f "$BASE" ps
    exit 1
  fi
  sleep 3
done

# -----------------------------------------------------------------------
# stage 2: baseline cypress
# -----------------------------------------------------------------------
echo "===> stage 2: cypress baseline spec"
CYPRESS_MODE=baseline \
  docker compose -f "$BASE" --profile test run --rm \
  -e CYPRESS_MODE=baseline \
  cypress-runner

# -----------------------------------------------------------------------
# stage 3: swap to mk-go
# -----------------------------------------------------------------------
echo "===> stage 3: stop TS-A backend (DB-A / Redis-A keep state)"
docker compose -f "$BASE" stop app-a

echo "===> stage 4: bring up mk-A via overlay"
docker compose -f "$BASE" -f "$OVERLAY" up -d --build app-a

echo "===> stage 4b: wait for mk-A healthy"
deadline=$(($(date +%s) + 180))
while :; do
  state=$(docker compose -f "$BASE" -f "$OVERLAY" ps --format json | python3 -c "
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
    docker compose -f "$BASE" -f "$OVERLAY" logs app-a | tail -50
    exit 1
  fi
  sleep 3
done

echo "===> stage 4c: restart nginx-a so its upstream re-resolves to mk-A"
docker compose -f "$BASE" -f "$OVERLAY" restart nginx-a

# -----------------------------------------------------------------------
# stage 5: swap cypress
# -----------------------------------------------------------------------
echo "===> stage 5: cypress swap spec"
CYPRESS_MODE=swap \
  docker compose -f "$BASE" -f "$OVERLAY" --profile test run --rm \
  -e CYPRESS_MODE=swap \
  cypress-runner

echo "===> all stages PASS (baseline + swap)"
