#!/bin/sh
# Generate self-signed certs for queue-bench domains and emit a bundle so
# Go-side stacks can trust each other via SSL_CERT_FILE.
set -e

CERT_DIR=/certs
mkdir -p "$CERT_DIR"

DOMAINS="mk-mkq ts faker"

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

: > "$CERT_DIR/bundle.pem"
for domain in $DOMAINS; do
  cat "$CERT_DIR/$domain.crt" >> "$CERT_DIR/bundle.pem"
done
chmod -R a+r "$CERT_DIR"
echo "Bundle written to $CERT_DIR/bundle.pem"
