#!/usr/bin/env bash
# base / head の比較 (#3544) で、各コンテナの CPU 時間とメモリを 1 秒ごとに記録する。
#
#   sampler.sh <out.csv> <heap URL or -> <name>=<container id> ...
#
# ヒープは最初の <name>=<container id> の行にだけ書く (run.sh は app を最初に渡す)。
#
# 出力は tools/benchcompare が読む resources.csv
# (t_ms,container,cpu_usage_usec,memory_peak,heap_inuse)。SIGTERM で止める。
#
# cgroup v2 のファイルを直接読む。`docker stats` は一時点の値しか返さず、CPU 時間の
# 累積も memory.peak も取れない。cgroup の場所はコンテナの PID の /proc/<pid>/cgroup
# から引くので、cgroup driver (systemd / cgroupfs) に依存しない。
#
# memory.peak は cgroup ができてからの最大で、手元の kernel (6.8) ではリセット
# できない。run.sh は計測の前にコンテナを止めて起動し直し、cgroup を作り直す。
#
# heap_inuse は app の Go のヒープ (pprof の HeapInuse) で、他のコンテナは空にする。
# ヒープのプロファイルを書き出させるのは app に負荷がかかるので、5 秒に 1 回だけ読む。
set -euo pipefail

out=$1
heap_url=$2
shift 2

declare -a names dirs
for arg in "$@"; do
  name=${arg%%=*}
  id=${arg#*=}
  pid=$(docker inspect -f '{{.State.Pid}}' "$id")
  rel=$(sed -n 's/^0:://p' "/proc/$pid/cgroup")
  dir=/sys/fs/cgroup$rel
  if [ -z "$rel" ] || [ ! -r "$dir/cpu.stat" ] || [ ! -r "$dir/memory.peak" ]; then
    echo "sampler: $name: no cgroup v2 cpu.stat / memory.peak under $dir" >&2
    exit 1
  fi
  names+=("$name")
  dirs+=("$dir")
done

sample() {
  local t heap='' k cpu peak h
  t=$(date +%s%3N)
  if [ "$heap_url" != "-" ] && [ $((tick % 5)) -eq 0 ]; then
    # 読めなかった回は空にする (benchcompare は空を「測っていない」として扱う)
    heap=$(curl -fsS -m 2 "$heap_url" 2>/dev/null | awk '/^# HeapInuse = /{print $4; exit}' || true)
  fi
  for k in "${!names[@]}"; do
    cpu=$(awk '$1 == "usage_usec" {print $2}' "${dirs[$k]}/cpu.stat")
    peak=$(cat "${dirs[$k]}/memory.peak")
    h=''
    if [ "$k" -eq 0 ]; then h=$heap; fi
    echo "$t,${names[$k]},$cpu,$peak,$h" >> "$out"
  done
  tick=$((tick + 1))
}

# CPU の上限 (cpu.max) に当たった区切りの数と、止められた時間。benchcompare は読まないが、
# 上限が律速になっていないかを後から確かめるために、開始と終了の差を <out> の隣に残す
throttled() {
  awk '$1 == "nr_periods" {p=$2} $1 == "nr_throttled" {n=$2} $1 == "throttled_usec" {u=$2} END {print p, n, u}' "$1/cpu.stat"
}
declare -a throttle_start
for k in "${!names[@]}"; do throttle_start[k]=$(throttled "${dirs[$k]}"); done

stop=0
trap 'stop=1' TERM INT

echo "t_ms,container,cpu_usage_usec,memory_peak,heap_inuse" > "$out"
tick=0
while [ "$stop" -eq 0 ]; do
  sample
  sleep 1 &
  wait $! || true
done
# 止められた時点でもう 1 回読む。読まないと、最後の 1 秒ぶんの CPU 時間が落ちる
sample

throttle_out="$(dirname "$out")/throttle.csv"
echo "container,nr_periods,nr_throttled,throttled_usec" > "$throttle_out"
for k in "${!names[@]}"; do
  read -r p0 n0 u0 <<< "${throttle_start[k]}"
  read -r p1 n1 u1 <<< "$(throttled "${dirs[$k]}")"
  # cpu controller が無いと cpu.stat に nr_* が出ない。0 と書くと「上限に当たらなかった」と
  # 読めてしまうので、空にする
  if [ -z "${p0:-}" ] || [ -z "${n0:-}" ] || [ -z "${u0:-}" ] || [ -z "${p1:-}" ] || [ -z "${n1:-}" ] || [ -z "${u1:-}" ]; then
    echo "${names[$k]},,," >> "$throttle_out"
    continue
  fi
  echo "${names[$k]},$((p1 - p0)),$((n1 - n0)),$((u1 - u0))" >> "$throttle_out"
done
