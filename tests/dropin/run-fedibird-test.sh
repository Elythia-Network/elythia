#!/bin/bash
# Drop-in fedibird-mock e2e orchestrator (#1083).
#
# 流れ:
#   1. base + mk + fedibird overlay で stack 起動 (TS-A / TS-B / mk-A / mock)
#      → mk-A は最初から mk-go 実装で動く (Phase 13-2 swap 経路は使わない)
#   2. test_swap_setup.py で alice / bob / follow を作る (= mk-A 上での setup)
#   3. test_fedibird_ed25519.py で mock ↔ mk-A の Ed25519 双方向検証
#   4. cleanup
#
# Phase 13-2 / P6 の swap test と独立した経路にすることで、Fedibird 互換の
# Ed25519 deliver / verify を最小 stack で walks through できる。

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

BASE=tests/dropin/compose.yml
MK_OVERLAY=tests/dropin/compose.mk.yml
FEDIBIRD_OVERLAY=tests/dropin/compose.fedibird.yml

cleanup() {
  echo "===> cleanup"
  docker compose -f "$BASE" -f "$MK_OVERLAY" -f "$FEDIBIRD_OVERLAY" down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "===> stage 1: bring up base + mk + fedibird overlays"
docker compose -f "$BASE" -f "$MK_OVERLAY" -f "$FEDIBIRD_OVERLAY" up -d --build --force-recreate

echo "===> stage 1b: wait for app-a (mk-go) + app-b (TS) + fedibird-mock healthy"
deadline=$(($(date +%s) + 240))
while :; do
  states=$(docker compose -f "$BASE" -f "$MK_OVERLAY" -f "$FEDIBIRD_OVERLAY" ps --format json | python3 -c "
import sys, json
ls=[json.loads(l) for l in sys.stdin if l.strip()]
healthy=[s for s in ls if s.get('Service') in ('app-a','app-b','fedibird-mock') and s.get('Health')=='healthy']
print(len(healthy))
")
  if [ "$states" = "3" ]; then
    break
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "FAIL: not all containers became healthy within 240s"
    docker compose -f "$BASE" -f "$MK_OVERLAY" -f "$FEDIBIRD_OVERLAY" ps
    docker compose -f "$BASE" -f "$MK_OVERLAY" -f "$FEDIBIRD_OVERLAY" logs fedibird-mock | tail -50
    exit 1
  fi
  sleep 3
done

# --build を付けないと runner image がキャッシュのままになり、
# requirements.txt を変えても古い image で走ってしまう。
echo "===> stage 2: alice / bob / follow setup on mk-A + TS-B"
docker compose -f "$BASE" -f "$MK_OVERLAY" -f "$FEDIBIRD_OVERLAY" --profile test run --rm --build test-runner pytest test_swap_setup.py -v

echo "===> stage 3: fedibird ↔ mk-A bidirectional Ed25519 verify"
docker compose -f "$BASE" -f "$MK_OVERLAY" -f "$FEDIBIRD_OVERLAY" --profile test run --rm --build test-runner pytest test_fedibird_ed25519.py -v

echo "===> all stages PASS"
