#!/usr/bin/env bash
# Collect docker stats + storage usage for mk-go (UDS stack) and Misskey TS
# (bench stack), excluding nginx per the bench spec.
#
# Usage: bash collect.sh [output_md]

set -eu

OUT="${1:-/tmp/resource-bench.md}"

# Container names. mk-go runs in project `mk` (compose.uds.yaml), TS bench
# runs in project `mk-bench-ts` (compose.ts.yaml).
MKGO_APP="mk-mkgo-1"
MKGO_DB="mk-postgres-1"
MKGO_REDIS="mk-valkey-1"

TS_APP="mk-bench-ts-web-1"
TS_DB="mk-bench-ts-db-1"
TS_REDIS="mk-bench-ts-redis-1"

ALL=("$MKGO_APP" "$MKGO_DB" "$MKGO_REDIS" "$TS_APP" "$TS_DB" "$TS_REDIS")

# Ensure all targets exist.
for c in "${ALL[@]}"; do
	if ! docker inspect --format '{{.Name}}' "$c" >/dev/null 2>&1; then
		echo "WARN: container $c not found" >&2
	fi
done

# Take 3 snapshots 5s apart and keep the second/third (skip the first
# bootstrap reading which is often 0 or noisy).
SNAP_FILE="$(mktemp)"
trap 'rm -f "$SNAP_FILE"' EXIT

# `docker stats --no-stream` でも一発目はホスト全体 cgroup の都合で
# memory が高めに出ることがあるので 2 回取って 2 回目を採用する。
docker stats --no-stream \
	--format "{{.Name}}|{{.CPUPerc}}|{{.MemUsage}}|{{.MemPerc}}|{{.BlockIO}}|{{.PIDs}}" \
	"${ALL[@]}" >/dev/null 2>&1 || true
sleep 2
docker stats --no-stream \
	--format "{{.Name}}|{{.CPUPerc}}|{{.MemUsage}}|{{.MemPerc}}|{{.BlockIO}}|{{.PIDs}}" \
	"${ALL[@]}" > "$SNAP_FILE"

get_stat() {
	local name="$1" field="$2"
	awk -F'|' -v n="$name" -v f="$field" '$1==n {print $f}' "$SNAP_FILE"
}

# Storage: writable layer (SizeRw) via `docker ps -s` + volume usage from
# inside the container. Volumes are not container-attributed by `docker ps`
# so we count them explicitly via `du` on the relevant mount paths.
get_rw_size() {
	local name="$1"
	docker ps -as --filter "name=^/${name}$" --format '{{.Size}}' | head -n1
}

# du -sb で取ると見やすい単位にならないので numfmt で人間可読化する。
human_bytes() {
	numfmt --to=iec --suffix=B --format='%.1f' "$1" 2>/dev/null || echo "${1}B"
}

du_inside() {
	local name="$1" path="$2"
	docker exec "$name" sh -c "du -sb '$path' 2>/dev/null | awk '{print \$1}'" 2>/dev/null || echo 0
}

echo "# Resource bench: mk-go (UDS) vs Misskey TS (just started)" > "$OUT"
echo >> "$OUT"
echo "Captured at: $(date -Is)" >> "$OUT"
echo >> "$OUT"
echo "nginx は集計対象外。mk-go は compose.uds.yaml の長期稼働 stack、TS は compose.ts.yaml の起動直後 stack。" >> "$OUT"
echo >> "$OUT"

print_table() {
	local title="$1" app="$2" db="$3" redis="$4"
	echo "## $title" >> "$OUT"
	echo >> "$OUT"
	echo "| Component | Container | CPU% | Memory | Mem% | Block I/O | PIDs |" >> "$OUT"
	echo "|---|---|---:|---|---:|---|---:|" >> "$OUT"
	for c in "$app" "$db" "$redis"; do
		role="app"
		[ "$c" = "$db" ] && role="db"
		[ "$c" = "$redis" ] && role="redis"
		cpu=$(get_stat "$c" 2)
		mem=$(get_stat "$c" 3)
		memp=$(get_stat "$c" 4)
		blk=$(get_stat "$c" 5)
		pids=$(get_stat "$c" 6)
		echo "| $role | \`$c\` | $cpu | $mem | $memp | $blk | $pids |" >> "$OUT"
	done
	echo >> "$OUT"
}

print_table "mk-go (UDS stack)" "$MKGO_APP" "$MKGO_DB" "$MKGO_REDIS"
print_table "Misskey TS (bench stack)" "$TS_APP" "$TS_DB" "$TS_REDIS"

echo "## Storage" >> "$OUT"
echo >> "$OUT"
echo "| Stack | Container | Writable layer | Data volume |" >> "$OUT"
echo "|---|---|---|---|" >> "$OUT"

# mk-go
rw=$(get_rw_size "$MKGO_APP" || true)
echo "| mk-go | \`$MKGO_APP\` | ${rw:-N/A} | (drive-files volume, see below) |" >> "$OUT"
rw=$(get_rw_size "$MKGO_DB" || true)
vol=$(du_inside "$MKGO_DB" /var/lib/postgresql/data)
echo "| mk-go | \`$MKGO_DB\` | ${rw:-N/A} | $(human_bytes "$vol") |" >> "$OUT"
rw=$(get_rw_size "$MKGO_REDIS" || true)
vol=$(du_inside "$MKGO_REDIS" /data)
echo "| mk-go | \`$MKGO_REDIS\` | ${rw:-N/A} | $(human_bytes "$vol") |" >> "$OUT"

# TS
rw=$(get_rw_size "$TS_APP" || true)
vol=$(du_inside "$TS_APP" /misskey/files)
echo "| TS | \`$TS_APP\` | ${rw:-N/A} | $(human_bytes "$vol") |" >> "$OUT"
rw=$(get_rw_size "$TS_DB" || true)
vol=$(du_inside "$TS_DB" /var/lib/postgresql)
echo "| TS | \`$TS_DB\` | ${rw:-N/A} | $(human_bytes "$vol") |" >> "$OUT"
rw=$(get_rw_size "$TS_REDIS" || true)
vol=$(du_inside "$TS_REDIS" /data)
echo "| TS | \`$TS_REDIS\` | ${rw:-N/A} | $(human_bytes "$vol") |" >> "$OUT"

echo >> "$OUT"
echo "## Image sizes (for reference)" >> "$OUT"
echo >> "$OUT"
echo '```' >> "$OUT"
{
	echo "REPO:TAG  SIZE"
	for ref in mk-mkgo:latest mk-bench-ts-web:latest postgres:18-alpine redis:7-alpine valkey/valkey:8-alpine; do
		size=$(docker image inspect "$ref" --format '{{.Size}}' 2>/dev/null || echo 0)
		printf '%s  %s\n' "$ref" "$(human_bytes "$size")"
	done
} >> "$OUT"
echo '```' >> "$OUT"

echo "wrote $OUT" >&2
cat "$OUT"
