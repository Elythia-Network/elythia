#!/bin/bash
# mk-go ↔ Misskey TS の実連合 e2e orchestrator。
#
# 流れ:
#   1. stack 起動 (mk-go + Misskey TS + 各 postgres/redis/nginx)
#   2. 両インスタンスが healthy になるまで待つ
#   3. pytest で連合シナリオを検証
#   4. cleanup (trap)
#
# `make federation-misskey-up` → `federation-misskey-test` を手で叩くのと同じだが、
# 失敗しても必ず後始末する / healthy 待ちを内包する点が違う。CI から 1 コマンドで
# 呼べるようにするために用意した。手元でも `make federation-misskey-e2e` で通しで走る。

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

COMPOSE=tests/federation/compose.misskey.yml

cleanup() {
  echo "===> cleanup"
  docker compose -f "$COMPOSE" down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "===> stage 1: bring up mk-go + Misskey TS"
docker compose -f "$COMPOSE" up -d --build

echo "===> stage 1b: wait for app-mkgo + misskey-app healthy"
# Misskey TS は初回起動でマイグレーションを流すので mk-go より遅い。
deadline=$(($(date +%s) + 300))
while :; do
  healthy=0
  for c in mk-federation-app-mkgo-1 mk-federation-misskey-app-1; do
    state=$(docker inspect --format '{{.State.Health.Status}}' "$c" 2>/dev/null || echo missing)
    [ "$state" = "healthy" ] && healthy=$((healthy + 1))
  done
  if [ "$healthy" = "2" ]; then
    break
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "FAIL: not all containers became healthy within 300s"
    docker compose -f "$COMPOSE" ps
    docker compose -f "$COMPOSE" logs misskey-app | tail -50
    exit 1
  fi
  sleep 3
done

echo "===> stage 2: federation scenarios (pytest)"
# --build を付けないと runner image がキャッシュのままになり、requirements.txt を
# 変えても古い image で走る (drop-in fedibird で実際に踏んだ)。
docker compose -f "$COMPOSE" --profile test run --rm --build test-runner

echo "===> all stages PASS"
