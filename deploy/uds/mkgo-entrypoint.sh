#!/bin/sh
# Run schema migrations then exec the mk-go server.
# compose.uds.yaml から /app/.config/default.yml がマウントされる想定。
#
# 引数があるときは migrate も serve もせず、そのまま elythia に渡す (#3394)。
# `docker compose run --rm --no-deps mkgo backfill <名前> ...` が、本番と同じ設定で
# もう 1 つサーバーを起動しないようにするため。compose.uds.yaml の mkgo は
# command を持たないので、通常の起動は引数無しでここを通る。
set -e

cd /app

if [ "$#" -gt 0 ]; then
	exec /app/elythia "$@"
fi

echo "[mkgo-entrypoint] running migrations..."
/app/elythia migrate -config /app/.config/default.yml -direction up

echo "[mkgo-entrypoint] starting misskey server..."
exec /app/elythia serve -config /app/.config/default.yml
