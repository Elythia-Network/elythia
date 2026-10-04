#!/bin/sh
# Phase 14-1 (#381): generate self-signed certs for three dropin-frontend domains.
# Phase 13 の `tests/dropin/gen-certs.sh` を 3 instance 用 (a / b / c) に拡張した版。
set -e

CERT_DIR=/certs
mkdir -p "$CERT_DIR"

DOMAINS="a b c"

for domain in $DOMAINS; do
  if [ ! -f "$CERT_DIR/$domain.crt" ]; then
    openssl req -x509 -newkey rsa:2048 -nodes \
      -keyout "$CERT_DIR/$domain.key" \
      -out "$CERT_DIR/$domain.crt" \
      -days 1 \
      -subj "/CN=$domain" \
      -addext "subjectAltName=DNS:$domain" \
      2>/dev/null
    echo "Generated cert for $domain"
  fi
done

# Phase 14-3 で mk-A 差し替え時に mk-go が SSL_CERT_FILE 経由で B / C の
# 自己署名 cert を検証できるようにするためのバンドル。Phase 14-1 では未使用
# だが冪等に生成しておく。
: > "$CERT_DIR/bundle.pem"
for domain in $DOMAINS; do
  cat "$CERT_DIR/$domain.crt" >> "$CERT_DIR/bundle.pem"
done
echo "Wrote $CERT_DIR/bundle.pem"
