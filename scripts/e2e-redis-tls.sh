#!/usr/bin/env bash
# Starts a disposable Redis that takes only TLS and one ACL user, for the e2e
# tests named by OPENRAILS_E2E_REDIS_TLS_URL and OPENRAILS_E2E_REDIS_TLS_CA:
#   scripts/e2e-redis-tls.sh DIR [PORT] [IMAGE]
# writes the CA to DIR/ca.pem and prints the URL. Remove it with
#   docker rm -f "${OPENRAILS_E2E_REDIS_TLS_NAME:-e2e-redis-tls}"
set -euo pipefail
dir="${1:?usage: e2e-redis-tls.sh DIR [PORT] [IMAGE]}"
port="${2:-6380}"
image="${3:-redis:7-alpine}"
name="${OPENRAILS_E2E_REDIS_TLS_NAME:-e2e-redis-tls}"
mkdir -p "$dir"
cd "$dir"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj /CN=e2e-redis-ca \
  -keyout ca.key -out ca.pem 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=localhost \
  -keyout server.key -out server.csr 2>/dev/null
printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\n' >san.ext
openssl x509 -req -in server.csr -CA ca.pem -CAkey ca.key -CAcreateserial -days 2 -extfile san.ext \
  -out server.pem 2>/dev/null
# The container's redis user reads the key; it is a throwaway.
chmod 0644 server.key
docker run -d --pull never --name "$name" -p "127.0.0.1:$port:6380" -v "$PWD:/tls:ro" "$image" \
  redis-server --port 0 --tls-port 6380 --tls-auth-clients no \
  --tls-cert-file /tls/server.pem --tls-key-file /tls/server.key --tls-ca-cert-file /tls/ca.pem \
  --user default off --user openrails on '>e2e-secret' '~*' '&*' '+@all' >/dev/null
echo "rediss://openrails:e2e-secret@127.0.0.1:$port/0"
