#!/usr/bin/env bash
# base と head の 2 つの commit を、同じスタックで交互に流して比べる (#3544)。
#
#   run.sh --base <ref> --head <ref> [--rounds N] [--scenario prod-like|unlimited] [--out DIR] [--smoke] [--delivery]
#
# --smoke は seed を 10 人 × 10 ノートにし、各エンドポイントを 5 秒だけ流す。手順が
# 最後まで通るかを見るためのもので、結果を比較に使わない。
#
# 1. base / head をそれぞれ git worktree に出し、app の image をビルドする
# 2. ラウンドごとに順番を入れ替えて流す (1 回目は base → head、2 回目は head → base …)。
#    同じ順で流すと、後に流す側だけがホストの温まり方 (page cache など) の影響を受ける
# 3. 各回は down -v → up → seed → 止めて起動し直す → ウォームアップ → k6 → down の順で、
#    まっさらな DB から始める。起動し直すのは cgroup を作り直して memory.peak を
#    リセットするため (seed のピークを計測に混ぜない)。seed の間だけは利用者ごとの
#    制限も外す (admin/accounts/create が 1 時間に 30 なので、prod-like では 50 人を
#    作れない)。起動し直すときにシナリオの設定へ戻す
# 4. tools/benchcompare で集計し、report.md / report.json を書く
#
# 終了コードは benchcompare に揃える: 0 = 比較できた、1 = 入力を読めない (シナリオの
# 不備を含む)、2 = 比較できない (途中で失敗した回がある、など)。準備 (ビルドなど) の
# 失敗も 1 にする。
#
# **このスクリプトは負荷をかける。** 本番が動いているホストでは、--rounds 2 の動作確認
# までにとどめ、比較の実測は GitHub-hosted runner で行う。
set -Eeuo pipefail
# 準備の失敗は、落ちたコマンドの終了コード (git の 128 など) ではなく 1 で終える
trap 'exit 1' ERR

usage() {
  echo "usage: $0 --base <ref> --head <ref> [--rounds N] [--scenario prod-like|unlimited] [--out DIR] [--smoke] [--delivery] [--project-name NAME]" >&2
  exit 1
}

here=$(cd "$(dirname "$0")" && pwd)
repo=$(git -C "$here" rev-parse --show-toplevel)
. "$here/run_helpers.sh"

base_ref='' head_ref='' rounds=2 scenario=prod-like out='' smoke=0 delivery=0
project_name="mk-bench-ab-$(date -u +%s)-$$" delivery_notes=2 delivery_followers=2
while [ $# -gt 0 ]; do
  case $1 in
    --base) base_ref=$2; shift 2 ;;
    --head) head_ref=$2; shift 2 ;;
    --rounds) rounds=$2; shift 2 ;;
    --scenario) scenario=$2; shift 2 ;;
    --out) out=$2; shift 2 ;;
    --smoke) smoke=1; shift ;;
    --delivery) delivery=1; shift ;;
    --delivery-notes) delivery_notes=$2; shift 2 ;;
    --delivery-followers) delivery_followers=$2; shift 2 ;;
    --project-name) project_name=$2; shift 2 ;;
    *) usage ;;
  esac
done
if [ -z "$base_ref" ] || [ -z "$head_ref" ]; then usage; fi
case $rounds in '' | *[!0-9]*) usage ;; esac
# benchcompare は 2 回未満を比較できないとする。流してから分かるのは時間の無駄なので先に止める
[ "$rounds" -ge 2 ] || { echo "run.sh: --rounds must be 2 or more" >&2; exit 1; }
for count in "$delivery_notes" "$delivery_followers"; do
  case $count in '' | *[!0-9]*) usage ;; esac
  [ "$count" -gt 0 ] || usage
done
bench_validate_project_name "$project_name" || {
  echo "run.sh: --project-name must start with 'mk-bench-ab-' and contain at most 32 lowercase letters, digits, '_' or '-'" >&2
  exit 1
}
case $scenario in
  prod-like) disable_limits=false ;;
  unlimited) disable_limits=true ;;
  *) usage ;;
esac

if [ ! -r /sys/fs/cgroup/cgroup.controllers ]; then
  echo "run.sh: cgroup v2 is required (memory.peak and cpu.stat are read directly)" >&2
  exit 1
fi

# Compose's project-scoped `down -v` must never target an existing stack.
# Check both running and stopped projects before any destructive operation.
bench_assert_project_available "$project_name"

base_commit=$(git -C "$repo" rev-parse --verify "$base_ref^{commit}")
head_commit=$(git -C "$repo" rev-parse --verify "$head_ref^{commit}")

out=${out:-$here/results/$(date -u +%Y%m%dT%H%M%SZ)-$scenario}
mkdir -p "$out"
out=$(cd "$out" && pwd)

compose=(docker compose -p "$project_name" -f "$here/compose.yml")
if [ "$smoke" -eq 1 ]; then
  export BENCH_SEED_USERS=10 BENCH_SEED_NOTES=10
  scenario=$scenario-smoke
fi
export BENCH_UID BENCH_GID
BENCH_UID=$(id -u)
BENCH_GID=$(id -g)

work=$(mktemp -d)
delivery_projects=()
delivery_compose_files=()
delivery_cleanup_failed=0
# shellcheck disable=SC2317 # trap から呼ぶ
cleanup() {
  # 後始末の失敗で、比較の終了コード (0 / 2) を ERR trap の 1 に書き換えない
  trap - ERR
  for i in "${!delivery_projects[@]}"; do
    bench_cleanup_delivery_project "${delivery_projects[$i]}" "${delivery_compose_files[$i]}" || true
  done
  "${compose[@]}" --profile tools down -v --remove-orphans >/dev/null 2>&1 || true
  for side in base head; do
    if [ -d "$work/$side" ]; then git -C "$repo" worktree remove --force "$work/$side" >/dev/null 2>&1 || true; fi
  done
  rm -rf "$work"
  docker image rm "$(image_of base)" "$(image_of head)" >/dev/null 2>&1 || true
}
trap cleanup EXIT

log() { echo "[run.sh $(date -u +%H:%M:%S)] $*" >&2; }

# ── 1. image ─────────────────────────────────────────────
image_of() { echo "mk-bench-ab-app:${project_name}-$1-$$"; }
build() {
  local side=$1 commit=$2
  log "building $side ($commit)"
  git -C "$repo" worktree add --detach "$work/$side" "$commit" >/dev/null
  docker build -q -f "$here/Dockerfile.app" -t "$(image_of "$side")" "$work/$side" >/dev/null
}
build base "$base_commit"
if [ "$head_commit" = "$base_commit" ]; then
  # 同じ commit なら同じ image を使う。ビルドの差を比較に持ち込まない
  docker tag "$(image_of base)" "$(image_of head)"
else
  build head "$head_commit"
fi
"${compose[@]}" --profile tools build -q seed
"${compose[@]}" --profile tools pull -q k6 postgres redis web

# ref は利用者が渡す文字列なので、JSON は python で組み立てる
python3 - "$out/run.json" "$base_ref" "$base_commit" "$head_ref" "$head_commit" "$scenario" "$rounds" \
  "$delivery" "$delivery_notes" "$delivery_followers" \
  "$(uname -r)" "$(nproc)" "$(awk -F': ' '/^model name/ {print $2; exit}' /proc/cpuinfo)" "$(docker version -f '{{.Server.Version}}')" <<'PY'
import json, sys
path, bref, bcommit, href, hcommit, scenario, rounds, delivery, delivery_notes, delivery_followers, kernel, cpus, model, docker = sys.argv[1:]
with open(path, "w") as f:
    json.dump({
        "base": {"ref": bref, "commit": bcommit},
        "head": {"ref": href, "commit": hcommit},
        "scenario": scenario,
        "rounds": int(rounds),
        "deliveryEnabled": delivery == "1",
        "deliveryNotes": int(delivery_notes) if delivery == "1" else None,
        "deliveryFollowers": int(delivery_followers) if delivery == "1" else None,
        "runner": {"kernel": kernel, "cpus": cpus, "cpu_model": model, "docker": docker},
    }, f, indent=2)
PY

# ── 2. 1 回分 ────────────────────────────────────────────
# 失敗しても止めずに meta.json に理由を残し、次の回へ進む。benchcompare が
# 「比較できない」として理由ごと報告する
services=(app postgres redis web)

# 止まったコンテナも含めて引く。落ちたまま止まっていると ps -q は空を返す
started_at() {
  local s id
  for s in "${services[@]}"; do
    id=$("${compose[@]}" ps -a -q "$s")
    if [ -z "$id" ]; then echo "$s=missing"; continue; fi
    docker inspect -f "$s={{.State.Status}} {{.State.StartedAt}}" "$id"
  done
}

measure() {
  local dir=$1
  export BENCH_RUN_DIR=$dir

  # この関数は `if ! reason=$(measure ...)` の中で呼ぶので set -e が効かない。失敗しうる
  # 工程には、すべて `|| { echo <理由>; return 1; }` を付ける
  "${compose[@]}" --profile tools down -v --remove-orphans >/dev/null 2>&1 || { echo "could not tear down the previous stack"; return 1; }
  BENCH_DISABLE_ENDPOINT_RATE_LIMITS=true "${compose[@]}" up -d --wait "${services[@]}" >/dev/null 2>&1 \
    || { echo "stack did not start"; return 1; }
  # SEED_STRICT により、作れなかったものがあれば seed は非 0 で終わる
  "${compose[@]}" --profile tools run --rm seed > "$dir/seed.log" 2>&1 || { echo "seed failed"; return 1; }

  "${compose[@]}" stop "${services[@]}" >/dev/null 2>&1 || { echo "stack did not stop"; return 1; }
  # 止めてから起動するので、どのコンテナも cgroup が新しくなる (prod-like では制限の
  # 設定が変わるので app は作り直される)
  BENCH_DISABLE_ENDPOINT_RATE_LIMITS=$disable_limits "${compose[@]}" up -d --wait "${services[@]}" >/dev/null 2>&1 \
    || { echo "stack did not restart"; return 1; }

  "${compose[@]}" --profile tools run --rm -e SHORT=1 k6 run -q --no-summary /scripts/ab/scenarios.js > "$dir/warmup.log" 2>&1 \
    || { echo "warmup failed"; return 1; }

  local before after port heap_url ids=() s id
  before=$(started_at)
  port=$("${compose[@]}" port app 3000) || { echo "app port not published"; return 1; }
  heap_url="http://$port/debug/pprof/heap?debug=1"
  for s in "${services[@]}"; do
    id=$("${compose[@]}" ps -q "$s") || id=''
    if [ -z "$id" ]; then echo "$s is not running"; return 1; fi
    ids+=("$s=$id")
  done
  "$here/sampler.sh" "$dir/resources.csv" "$heap_url" "${ids[@]}" &
  local sampler=$!
  local k6_status=0
  "${compose[@]}" --profile tools run --rm -e SHORT="$smoke" k6 run -q --summary-export=/out/k6-summary.json /scripts/ab/scenarios.js > "$dir/k6.log" 2>&1 || k6_status=$?
  kill -TERM "$sampler"
  local sampler_status=0
  wait "$sampler" || sampler_status=$?
  after=$(started_at)

  # k6 がすぐ落ちると sampler も trap を張る前に止められて失敗するので、k6 を先に見る
  [ "$k6_status" -eq 0 ] || { echo "k6 exited with $k6_status"; return 1; }
  [ "$sampler_status" -eq 0 ] || { echo "sampler failed"; return 1; }
  # sampler は CPU 時間の逆行で再起動を検出するが、1 秒の間に落ちて戻ると見えない
  [ "$before" = "$after" ] || { echo "a container stopped or restarted during the run"; return 1; }
  # アクセスログで 1 回数十 MB になるので圧縮して残す
  "${compose[@]}" logs --no-color app 2>&1 | gzip > "$dir/app.log.gz" || true
}

measure_delivery() {
  local dir=$1 round=$2 side=$3
  local queue_compose="$repo/tests/bench/queue/compose.yml"
  local delivery_project="${project_name}-delivery-r${round}-${side}"
  local qcompose=(docker compose -p "$delivery_project" -f "$queue_compose")
  local MKGO_IMAGE QUEUE_BENCH_DELIVERY=1 QUEUE_BENCH_STACKS=mkq QUEUE_BENCH_SEED_FAKER=0
  local OUTBOUND_NOTES=$delivery_notes OUTBOUND_FOLLOWERS=$delivery_followers
  local QUEUE_BENCH_EXPORT_DELIVERY=1 QUEUE_BENCH_DELIVERY_STACK=mkq QUEUE_BENCH_RESULTS=$dir
  MKGO_IMAGE=$(image_of "$side")
  export MKGO_IMAGE QUEUE_BENCH_DELIVERY QUEUE_BENCH_STACKS QUEUE_BENCH_SEED_FAKER
  export OUTBOUND_NOTES OUTBOUND_FOLLOWERS QUEUE_BENCH_EXPORT_DELIVERY
  export QUEUE_BENCH_DELIVERY_STACK QUEUE_BENCH_RESULTS

  "${qcompose[@]}" --profile bench --profile outbound --profile delivery \
    build -q seed driver-outbound deliveryreceiver-mkq \
    > "$dir/delivery-build.log" 2>&1 || { delivery_reason="delivery fixture build failed"; return 1; }
  "${qcompose[@]}" --profile delivery --no-build up -d --wait \
    certs postgres-mkq redis-mkq app-mkq nginx-mkq deliveryreceiver-mkq \
    > "$dir/delivery-compose.log" 2>&1 || { delivery_reason="delivery fixture stack failed to start"; return 1; }
  "${qcompose[@]}" --profile bench run --no-deps --rm seed \
    > "$dir/delivery-seed.log" 2>&1 || { delivery_reason="delivery seed failed"; return 1; }
  bench_restart_delivery_stack "${qcompose[@]}" \
    >> "$dir/delivery-compose.log" 2>&1 || {
      delivery_reason="delivery stack restart or health check failed"
      return 1
    }
  "${qcompose[@]}" --profile outbound run --no-deps --rm driver-outbound \
    > "$dir/delivery-driver.log" 2>&1 || { delivery_reason="delivery driver failed"; return 1; }
  [ -s "$dir/delivery.json" ] || { delivery_reason="delivery.json was not written"; return 1; }
}

run_one() {
  local round=$1 side=$2
  local dir=$out/r$round-$side
  mkdir -p "$dir"
  log "round $round/$rounds: $side"
  export APP_IMAGE
  APP_IMAGE=$(image_of "$side")
  local reason completed=true
  if ! reason=$(measure "$dir"); then
    completed=false
    reason=${reason:-failed}
    log "round $round $side: $reason"
    "${compose[@]}" logs --no-color > "$dir/compose.log" 2>&1 || true
  fi
  local delivery_reason=''
  if [ "$delivery" -eq 1 ] && [ "$completed" = true ]; then
    "${compose[@]}" --profile tools down -v --remove-orphans >/dev/null 2>&1 || true
    local delivery_project="${project_name}-delivery-r${round}-${side}"
    local queue_compose="$repo/tests/bench/queue/compose.yml"
    if [ "${#delivery_project}" -gt 63 ]; then
      delivery_reason="delivery Compose project name exceeds 63 characters"
    elif ! bench_assert_project_available "$delivery_project"; then
      delivery_reason="delivery Compose project name is already in use"
    else
      delivery_projects+=("$delivery_project")
      delivery_compose_files+=("$queue_compose")
      if ! bench_run_delivery_with_cleanup "$delivery_project" "$queue_compose" \
        measure_delivery "$dir" "$round" "$side"; then
        delivery_reason=${delivery_reason:-failed}
      fi
    fi
    if [ -n "$delivery_reason" ]; then
      delivery_reason=${delivery_reason:-failed}
      log "round $round $side delivery: $delivery_reason"
      printf '%s\n' "$delivery_reason" > "$dir/delivery-error.txt"
    fi
    if [ "$delivery_cleanup_failed" -eq 1 ]; then
      echo "run.sh: delivery fixture cleanup failed; stopping before another measurement" >&2
      exit 1
    fi
  fi
  "${compose[@]}" --profile tools down -v --remove-orphans >/dev/null 2>&1 || true
  python3 -c 'import json,sys; print(json.dumps({"side": sys.argv[1], "round": int(sys.argv[2]), "completed": sys.argv[3] == "true", "reason": sys.argv[4]}))' \
    "$side" "$round" "$completed" "${reason:-}" > "$dir/meta.json"
}

# 最初の回だけが image の読み込みなどの初回の負担を負わないよう、計測しない起動を 1 回挟む
log "burn-in"
APP_IMAGE=$(image_of base) BENCH_DISABLE_ENDPOINT_RATE_LIMITS=$disable_limits "${compose[@]}" up -d --wait "${services[@]}" >/dev/null 2>&1 \
  || { echo "run.sh: the stack did not start" >&2; exit 1; }
"${compose[@]}" --profile tools down -v --remove-orphans >/dev/null 2>&1

for round in $(seq 1 "$rounds"); do
  if [ $((round % 2)) -eq 1 ]; then order=(base head); else order=(head base); fi
  for side in "${order[@]}"; do run_one "$round" "$side"; done
done

# ── 3. 集計 ──────────────────────────────────────────────
status=0
# go run は子の終了コードを 1 に潰す (2 = 比較できない が区別できなくなる) ので、ビルドしてから実行する
(cd "$repo" && go build -o "$work/benchcompare" ./tools/benchcompare)
"$work/benchcompare" -dir "$out" -md "$out/report.md" -json "$out/report.json" || status=$?
log "report: $out/report.md (exit $status)"
exit "$status"
