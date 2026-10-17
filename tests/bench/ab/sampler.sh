#!/usr/bin/env bash
# base / head の比較 (#3544) で、各コンテナの CPU 時間とメモリを 1 秒ごとに記録する。
#
#   sampler.sh [--may-exit <name>]... <out.csv> <heap URL or -> <name>=<container id> ...
#
# ヒープは最初の <name>=<container id> の行にだけ書く (run.sh は app を最初に渡す)。
#
# --may-exit で名前を渡したコンテナ (k6) は、記録の途中で終わってよい。終わると cgroup が
# 消えるので、読めなくなった時点でそのコンテナだけ記録をやめる。それ以外のコンテナの
# cgroup が読めなくなったら、測定が成り立たないので失敗で終わる。
#
# 出力は tools/benchcompare が読む resources.csv
# (t_ms,container,cpu_usage_usec,memory_peak,heap_inuse,nr_periods,nr_throttled,throttled_usec)。
# SIGTERM で止める。
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
#
# nr_periods / nr_throttled / throttled_usec は cpu.stat の累積値で、CPU の上限 (cpu.max) に
# 当たった区切りを数える。benchcompare は k6 の行から、エンドポイントの区間ごとに k6 が
# 止められた区切りの割合を出し、k6 が律速になったエンドポイントを比較不能にする (#3656)。
# cpu controller が無いと cpu.stat に nr_* が出ないので、そのときは空にする (0 と書くと
# 「上限に当たらなかった」と読めてしまう)。
set -euo pipefail

declare -A may_exit=()
while [ "${1:-}" = --may-exit ]; do
  may_exit[$2]=1
  shift 2
done

out=$1
heap_url=$2
shift 2

declare -a names dirs alive
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
  alive+=(1)
done

# cpu.stat を 1 回だけ読み、usage_usec nr_periods nr_throttled throttled_usec を空白区切りで返す。
# nr_* が無いときは、その欄が空になる (区切りの数は崩さない)
read_cpu() {
  awk '$1 == "usage_usec" {c=$2} $1 == "nr_periods" {p=$2} $1 == "nr_throttled" {n=$2} $1 == "throttled_usec" {u=$2}
    END {if (c == "") exit 1; print c "," p "," n "," u}' "$1/cpu.stat"
}

declare -a last_cpu
sample() {
  local t heap='' k stat peak h
  t=$(date +%s%3N)
  if [ "$heap_url" != "-" ] && [ $((tick % 5)) -eq 0 ]; then
    # 読めなかった回は空にする (benchcompare は空を「測っていない」として扱う)
    heap=$(curl -fsS -m 2 "$heap_url" 2>/dev/null | awk '/^# HeapInuse = /{print $4; exit}' || true)
  fi
  for k in "${!names[@]}"; do
    [ "${alive[$k]}" -eq 1 ] || continue
    if [ -n "${may_exit[${names[$k]}]:-}" ]; then
      # 終わってよいコンテナは、cgroup が消えたら (読めなくなったら) 記録をやめる
      if ! stat=$(read_cpu "${dirs[$k]}" 2>/dev/null) || ! peak=$(cat "${dirs[$k]}/memory.peak" 2>/dev/null); then
        alive[k]=0
        continue
      fi
    else
      stat=$(read_cpu "${dirs[$k]}")
      peak=$(cat "${dirs[$k]}/memory.peak")
    fi
    last_cpu[k]=$stat
    h=''
    if [ "$k" -eq 0 ]; then h=$heap; fi
    IFS=, read -r c p n u <<< "$stat"
    echo "$t,${names[$k]},$c,$peak,$h,$p,$n,$u" >> "$out"
  done
  tick=$((tick + 1))
}

# CPU の上限 (cpu.max) に当たった区切りの数と、止められた時間の、開始と終了の差を <out> の隣に
# 残す (run 全体の目安。benchcompare は読まず、resources.csv の 1 秒ごとの値を使う)
declare -a throttle_start

stop=0
trap 'stop=1' TERM INT

echo "t_ms,container,cpu_usage_usec,memory_peak,heap_inuse,nr_periods,nr_throttled,throttled_usec" > "$out"
tick=0
sample
for k in "${!names[@]}"; do throttle_start[k]=${last_cpu[k]:-}; done
while [ "$stop" -eq 0 ]; do
  sleep 1 &
  wait $! || true
  if [ "$stop" -eq 0 ]; then sample; fi
done
# 止められた時点でもう 1 回読む。読まないと、最後の 1 秒ぶんの CPU 時間が落ちる
sample

throttle_out="$(dirname "$out")/throttle.csv"
echo "container,nr_periods,nr_throttled,throttled_usec" > "$throttle_out"
for k in "${!names[@]}"; do
  # 終わったコンテナ (k6) は、最後に読めた値までの差にする
  IFS=, read -r _ p0 n0 u0 <<< "${throttle_start[k]:-}"
  IFS=, read -r _ p1 n1 u1 <<< "${last_cpu[k]:-}"
  # cpu controller が無いと cpu.stat に nr_* が出ない。0 と書くと「上限に当たらなかった」と
  # 読めてしまうので、空にする
  if [ -z "${p0:-}" ] || [ -z "${n0:-}" ] || [ -z "${u0:-}" ] || [ -z "${p1:-}" ] || [ -z "${n1:-}" ] || [ -z "${u1:-}" ]; then
    echo "${names[$k]},,," >> "$throttle_out"
    continue
  fi
  echo "${names[$k]},$((p1 - p0)),$((n1 - n0)),$((u1 - u0))" >> "$throttle_out"
done
