#!/bin/bash
# Mastodon の初期化 (#3234 の e2e)。1 回だけ走る one-shot コンテナで行う。
#
# 1. 秘密鍵を作って /shared/env に置く (web / sidekiq はこれを読んで起動する)
# 2. DB を用意する
# 3. テスト用のアカウント alice と API トークンを作り、/shared/mastodon_token に置く
set -euo pipefail

if [ ! -f /shared/env ]; then
  gen() { ruby -rsecurerandom -e "puts SecureRandom.hex($1)"; }
  {
    echo "SECRET_KEY_BASE=$(gen 64)"
    echo "OTP_SECRET=$(gen 64)"
    echo "ACTIVE_RECORD_ENCRYPTION_DETERMINISTIC_KEY=$(gen 16)"
    echo "ACTIVE_RECORD_ENCRYPTION_KEY_DERIVATION_SALT=$(gen 16)"
    echo "ACTIVE_RECORD_ENCRYPTION_PRIMARY_KEY=$(gen 16)"
  } > /shared/env
  chmod 644 /shared/env
fi
set -a
. /shared/env
set +a

bundle exec rails db:prepare
bundle exec rails runner /setup/create_token.rb
chmod 644 /shared/mastodon_token
echo "mastodon setup done"
