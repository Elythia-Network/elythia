#!/bin/bash
# mk-go ↔ Mastodon の実連合 e2e orchestrator (#3234)。
#
# 流れ:
#   1. stack 起動 (mk-go + Mastodon + 各 postgres/redis/nginx)。Mastodon 側は
#      masto-setup が秘密鍵の生成・migration・テスト用アカウントとトークンの
#      作成を済ませてから web / sidekiq が立ち上がる
#   2. 両インスタンスが healthy になるまで待つ
#   3. pytest で引用の承認 (FEP-044f) を検証
#   4. cleanup (trap)
#
# run-misskey-test.sh と同じ形。CI から 1 コマンドで呼べるようにするために用意した。

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

COMPOSE=docker-compose.federation.mastodon.yml

cleanup() {
  echo "===> cleanup"
  docker compose -f "$COMPOSE" --profile test down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "===> stage 1: bring up mk-go + Mastodon"
docker compose -f "$COMPOSE" up -d --build

echo "===> stage 1b: wait for app-mkgo + masto-web healthy"
# Mastodon は初回に db:prepare を流してから puma が上がるので mk-go より遅い。
deadline=$(($(date +%s) + 300))
while :; do
  healthy=0
  for c in mk-federation-mastodon-app-mkgo-1 mk-federation-mastodon-masto-web-1; do
    state=$(docker inspect --format '{{.State.Health.Status}}' "$c" 2>/dev/null || echo missing)
    [ "$state" = "healthy" ] && healthy=$((healthy + 1))
  done
  if [ "$healthy" = "2" ]; then
    break
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "FAIL: not all containers became healthy within 300s"
    docker compose -f "$COMPOSE" ps
    docker compose -f "$COMPOSE" logs masto-setup masto-web | tail -80
    exit 1
  fi
  sleep 3
done

echo "===> stage 2: quote authorization scenarios (pytest)"
# --build を付けないと runner image がキャッシュのままになり、requirements.txt を
# 変えても古い image で走る (run-misskey-test.sh と同じ)。
docker compose -f "$COMPOSE" --profile test run --rm --build test-runner

echo "===> all stages PASS"
