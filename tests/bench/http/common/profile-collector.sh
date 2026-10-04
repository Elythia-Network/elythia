#!/bin/sh
# Capture pprof profiles from the mk-go app container while k6 is running
# the per-scenario load. See `tests/bench/http/compose.yml` for how
# this is wired into the bench profile.
#
# Output layout (under $OUT, default /output/profiles):
#   heap-pre.pb.gz / heap-post.pb.gz       — heap snapshots before & after
#   allocs-post.pb.gz                       — total alloc samples after run
#   goroutine-pre.pb.gz / goroutine-post.pb.gz
#   cpu-<scenario>.pb.gz                    — CPU profile for each k6 scenario
#
# busybox の wget だけで動かす (alpine:3.21 に最初から入っている)。
# `apk add curl` を経由しないので、ネット越しのパッケージ取得失敗で bench
# パイプライン全体が block する事故を避ける (Devin #501 FLAG-2)。
#
# Misskey TS 側はここでは取らない (Node.js は別ツール、本ロードマップ #413 は
# Go 側のみが対象)。
set -eu

HOST=${HOST:-app-mkgo:3000}
OUT=${OUT:-/output/profiles}
SCENARIO_DURATION=${SCENARIO_DURATION:-31}
PROFILE_SECONDS=${PROFILE_SECONDS:-25}
SCENARIOS=${SCENARIOS:-"ping meta local-timeline users-show i notes-create home-timeline"}

# シナリオ数を $SCENARIOS から動的に算出する。
# SCENARIOS を上書きしても sleep / log のロジックがそのまま動くようにする
# (Devin #501 BUG-1 / INFO-3)。
# shellcheck disable=SC2086 # word-splitting is intentional here
set -- $SCENARIOS
SCENARIO_COUNT=$#

mkdir -p "$OUT"
rm -f "$OUT"/*.pb.gz 2>/dev/null || true

ts() { date -Is; }
log() { echo "[$(ts)] profile-collector: $*"; }

snap() {
  endpoint=$1
  outfile=$2
  if wget -q -T 30 -O "$outfile" "http://${HOST}/debug/pprof/${endpoint}"; then
    log "saved $(basename "$outfile") ($(wc -c < "$outfile") bytes)"
  else
    log "WARN: failed to fetch /debug/pprof/${endpoint}"
    rm -f "$outfile"
  fi
}

cpu_snap() {
  scenario=$1
  outfile=$2
  if wget -q -T $((PROFILE_SECONDS + 10)) -O "$outfile" \
    "http://${HOST}/debug/pprof/profile?seconds=${PROFILE_SECONDS}"; then
    log "saved $(basename "$outfile") ($(wc -c < "$outfile") bytes)"
  else
    log "WARN: CPU profile failed for ${scenario}"
    rm -f "$outfile"
  fi
}

# k6 が ramp-up を始めるまで少し待つ。k6-mkgo と本コンテナは
# seed-mkgo が success した直後に同時起動するので、3秒で十分。
log "waiting 3s for k6 ramp-up (host=${HOST}, scenarios=${SCENARIO_COUNT})"
sleep 3

# Pre-bench snapshots は ~1-2s 程度で完了するので、後続の per-scenario CPU
# profile は k6 の startTime 基準から数秒遅れる。default (31s scenario, 25s
# profile) なら steady-state 内に収まるが、PROFILE_SECONDS を SCENARIO_DURATION
# に近づけると次シナリオの ramp-up に食い込みうる (Devin #501 INFO-1)。
snap heap "$OUT/heap-pre.pb.gz"
snap goroutine "$OUT/goroutine-pre.pb.gz"

i=0
for s in $SCENARIOS; do
  i=$((i + 1))
  log "scenario ${i}/${SCENARIO_COUNT} (${s}): capturing CPU profile for ${PROFILE_SECONDS}s"
  cpu_snap "$s" "$OUT/cpu-${s}.pb.gz"
  # Sleep through the remainder of the scenario block (ramp-down + buffer)
  # so that the next CPU profile aligns with the next scenario's steady state.
  remaining=$((SCENARIO_DURATION - PROFILE_SECONDS - 1))
  if [ "$remaining" -gt 0 ] && [ "$i" -lt "$SCENARIO_COUNT" ]; then
    sleep "$remaining"
  fi
done

snap heap "$OUT/heap-post.pb.gz"
snap allocs "$OUT/allocs-post.pb.gz"
snap goroutine "$OUT/goroutine-post.pb.gz"

log "done. profiles in $OUT:"
ls -la "$OUT"
