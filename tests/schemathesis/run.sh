#!/usr/bin/env bash
# /api.json から入力を作る検査 (#4118)。Makefile の schemathesis-test から呼ぶ。
#
# 1. compose.yml の DB と Redis を作り直し、go build した Elythia をホストで起動する
# 2. 最初の利用者として管理者を作り、その token で schemathesis を流す
# 3. exclude.tsv のエンドポイントは外す
# 4. 宣言どおりの入力だけを送る (--mode positive)。壊れた JSON や型の違う入力も送ると、
#    本家と同じ Fastify の形の 400 が宣言のエラーの形と合わず、ほぼ全部の口で食い違いが
#    出て、ほかの失敗が埋もれる。文字列は UTF-8 で表せるものだけにする
#    (--generation-codec utf-8)。対になっていないサロゲートを含む文字列は JSON の本文に
#    ならず、同じ Fastify の形の 400 になる
#
# 結果は tests/schemathesis/out/ (gitignore 済み) に残る。まとめは summarize.py。
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
bin=${ELYTHIA_BIN:-$root/built/elythia}
out=$here/out
base=http://127.0.0.1:61813
compose=(docker compose -f "$here/compose.yml")

rm -rf "$out"
mkdir -p "$out/report"

# 前の実行の利用者や行が残っていると、最初の利用者として管理者を作れず、結果も揃わない。
# 毎回 DB と Redis を作り直す
"${compose[@]}" --profile test down -v > /dev/null 2>&1
"${compose[@]}" up -d --wait
"$bin" migrate -config "$here/mkgo.yml" -direction up > "$out/migrate.log" 2>&1

"$bin" serve -config "$here/mkgo.yml" > "$out/mkgo.log" 2>&1 &
pid=$!
trap 'kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true' EXIT

for _ in $(seq 1 60); do
	if curl -fsS "$base/healthz" > /dev/null 2>&1; then break; fi
	if ! kill -0 "$pid" 2>/dev/null; then echo "elythia exited" >&2; tail -50 "$out/mkgo.log" >&2; exit 1; fi
	sleep 1
done
curl -fsS "$base/healthz" > /dev/null

# 最初の利用者は管理者になる (admin/accounts/create は、利用者がいないときだけ認証なしで通る)
password=$(head -c 18 /dev/urandom | base64 | tr -d '/+=')
token=$(curl -fsS -H 'Content-Type: application/json' \
	-d "{\"username\":\"stadmin\",\"password\":\"$password\"}" "$base/api/admin/accounts/create" |
	python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')

curl -fsS "$base/api.json" > "$out/api.json"

# exclude.tsv の行を 1 つの正規表現にまとめる。schemathesis の path は /api の後ろ。
# どの path にも当たらない行 (改名や削除で残った行) があれば落とす
python3 - "$here/exclude.tsv" "$out/api.json" <<'PY'
import json, re, sys
paths = list(json.load(open(sys.argv[2], encoding="utf-8"))["paths"])
unused = []
for line in open(sys.argv[1], encoding="utf-8"):
    if line.startswith("#") or not line.strip():
        continue
    pattern = line.split("\t")[0]
    if not any(re.fullmatch("/" + pattern, p) for p in paths):
        unused.append(pattern)
if unused:
    sys.exit("exclude.tsv の行が /api.json のどの path にも当たらない: " + ", ".join(unused))
PY
exclude=$(grep -v '^#' "$here/exclude.tsv" | grep -v '^$' | cut -f1 | sed 's|^|/|' | paste -sd'|')

# SCHEMATHESIS_ARGS は空白で分けて、複数の引数として渡す
# shellcheck disable=SC2086
"${compose[@]}" --profile test run --rm -T --user "$(id -u):$(id -g)" schemathesis run /work/out/api.json \
	--url "$base/api" \
	--header "Authorization: Bearer $token" \
	--exclude-path-regex "^($exclude)\$" \
	--mode positive \
	--generation-codec utf-8 \
	--checks not_a_server_error,status_code_conformance,response_schema_conformance \
	--max-examples "${SCHEMATHESIS_MAX_EXAMPLES:-20}" \
	--continue-on-failure \
	--request-timeout 10 \
	--report junit,ndjson \
	--report-dir /work/out/report \
	${SCHEMATHESIS_ARGS:-}
