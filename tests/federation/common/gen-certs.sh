#!/bin/sh
# Generate self-signed certs for federation test domains.
# Also emit a bundle.pem that mk-go (Go) uses via SSL_CERT_FILE so the
# connected instances' self-signed certs are trusted by crypto/x509.
set -e

CERT_DIR=/certs
mkdir -p "$CERT_DIR"

# 対象は相手ごとに違う (Misskey / Mastodon) ので、compose から渡せるようにする。
DOMAINS="${CERT_DOMAINS:-mkgo misskey}"

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

# Bundle すべての self-signed cert を 1 ファイルにまとめる。
# Go の SSL_CERT_FILE は上書きなので、mk-go が federation 相手の
# 証明書を検証するにはこのバンドルだけで足りるようにする。
: > "$CERT_DIR/bundle.pem"
for domain in $DOMAINS; do
  cat "$CERT_DIR/$domain.crt" >> "$CERT_DIR/bundle.pem"
done
echo "Wrote $CERT_DIR/bundle.pem"
