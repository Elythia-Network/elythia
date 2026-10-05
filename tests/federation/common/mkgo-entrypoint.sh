#!/bin/sh
# Run schema migrations then exec the mk-go server.
# 使われる docker-compose から .config/default.yml がマウントされる想定。
set -e

cd /app

echo "[mkgo-entrypoint] running migrations..."
/app/elythia migrate -config /app/.config/default.yml -direction up

echo "[mkgo-entrypoint] starting misskey server..."
exec /app/elythia serve -config /app/.config/default.yml
