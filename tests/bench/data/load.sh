#!/usr/bin/env bash
# Load a dataset made by gen.py into a running bench stack, or check that it
# is still there (#4016).
#
#   load.sh load        <data-dir> <compose command...>
#   load.sh check-redis <data-dir> <compose command...>
#
# e.g. load.sh load /tmp/data docker compose -p mk-bench-ab-x -f compose.yml
#
# load: the stack's postgres and redis must be up, and the app must have run
# its migrations once (the tables exist) and then be stopped: rows loaded
# behind a running app's back would miss its caches. Rows go in with COPY in
# the order gen.py lists them (foreign keys first), the Redis commands with
# `redis-cli --pipe`. Afterwards the row counts are compared with stats.json,
# so a silently short load fails here instead of skewing a comparison.
#
# check-redis: counts the generated keys by their prefix. run.sh calls it after
# restarting the stack, because Redis keeps the keys across the restart only
# through its snapshot on shutdown.
set -Eeuo pipefail

if [ $# -lt 3 ]; then
  echo "usage: $0 (load|check-redis) <data-dir> <compose command...>" >&2
  exit 1
fi
mode=$1
data=$2
shift 2
compose=("$@")

[ -r "$data/stats.json" ] || { echo "load.sh: $data/stats.json not found" >&2; exit 1; }

stat() { python3 -I -c 'import json,sys; v=json.load(open(sys.argv[1])); [v := v[k] for k in sys.argv[2:]]; print(" ".join(v) if isinstance(v, list) else v)' "$data/stats.json" "$@"; }
psql() { "${compose[@]}" exec -T postgres psql -U misskey -d misskey -v ON_ERROR_STOP=1 -qAt "$@"; }
redis_count() { "${compose[@]}" exec -T redis redis-cli --scan --pattern "$1" | wc -l | tr -d '[:space:]'; }

check_redis() {
  local prefix want got pattern kind
  prefix=$(stat prefix)
  # 読み込みが足りない、または起動し直して消えたときに止める。本家はタイムラインが
  # 無いと DB から引き直して黙って動くので、API の応答では気付けない。数えるのは gen.py に
  # 渡した接頭辞 (run.sh の --host web) で、app の設定 (url) との一致はここでは見ない
  for kind in listKeys:list notificationKeys:notificationTimeline; do
    want=$(stat redis "${kind%%:*}")
    pattern="${prefix}${kind#*:}:*"
    got=$(redis_count "$pattern")
    if [ "$got" -lt "$want" ]; then
      echo "load.sh: redis has $got keys matching $pattern, expected $want" >&2
      return 1
    fi
  done
}

case $mode in
  check-redis)
    check_redis
    exit
    ;;
  load) ;;
  *)
    echo "load.sh: unknown mode '$mode'" >&2
    exit 1
    ;;
esac

# 表と列は gen.py の COLUMNS / TABLE_ORDER から取る (二重に書かない)
for table in $(stat tableOrder); do
  cols=$(python3 -I -c 'import json,sys; s=json.load(open(sys.argv[1])); print(",".join("\"%s\"" % c for c in s["columns"][sys.argv[2]]))' "$data/stats.json" "$table")
  psql -c "COPY \"$table\" ($cols) FROM STDIN" < "$data/$table.tsv"
done

# 管理者がいない (meta.rootUserId が空) と、本家の /api/meta は requireSetup: true を返す。
# small の seed は最初に管理者を作るので、それに揃える。Elythia は user.isRoot も見る
root=$(stat rootUserId)
[ "$(psql -c "UPDATE meta SET \"rootUserId\" = '$root' RETURNING 1")" = 1 ] \
  || { echo "load.sh: could not set meta.rootUserId" >&2; exit 1; }
psql -c "DO \$\$ BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'user' AND column_name = 'isRoot') THEN
    UPDATE \"user\" SET \"isRoot\" = true WHERE id = '$root';
  END IF;
END \$\$"

# 統計を取り、可視性マップを作り、行を凍結しておく。しないと、COPY をきっかけにした
# autovacuum と hint bit の書き込みが、回ごとに違う時刻 (計測の最中) に走る
psql -c "VACUUM (FREEZE, ANALYZE)"

"${compose[@]}" exec -T redis redis-cli --pipe < "$data/redis.resp" > /dev/null

for table in $(stat tableOrder); do
  want=$(stat rows "$table")
  got=$(psql -c "SELECT count(*) FROM \"$table\"")
  if [ "$got" != "$want" ]; then
    echo "load.sh: $table has $got rows, expected $want" >&2
    exit 1
  fi
done
check_redis
