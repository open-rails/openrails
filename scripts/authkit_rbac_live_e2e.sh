#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

export OPENRAILS_HOST_PORT="${OPENRAILS_HOST_PORT:-3053}"
BASE_URL="${BASE_URL:-http://127.0.0.1:${OPENRAILS_HOST_PORT}}"
COMPOSE=(docker compose -f docker-compose.yaml)

TMPDIR="$(mktemp -d)"
cleanup() {
  rm -rf "$TMPDIR"
}
trap cleanup EXIT

uuid() {
  cat /proc/sys/kernel/random/uuid
}

require_body() {
  local desc="$1" body="$2" needle="$3"
  if ! printf '%s' "$body" | grep -qF "$needle"; then
    echo "FAIL $desc"
    echo "expected: $needle"
    echo "body: $body"
    exit 1
  fi
  echo "PASS $desc"
}

request() {
  local method="$1" url="$2" token="${3:-}" origin="${4:-}" body="${5:-}"
  local args=(-sS -m 30 -w $'\n%{http_code}' -X "$method")
  if [ -n "$token" ]; then
    args+=(-H "Authorization: Bearer $token")
  fi
  if [ -n "$origin" ]; then
    args+=(-H "Origin: $origin")
  fi
  if [ -n "$body" ]; then
    args+=(-H "Content-Type: application/json" -d "$body")
  fi
  curl "${args[@]}" "$url"
}

expect_http() {
  local desc="$1" want="$2" output="$3"
  local code body
  code="$(printf '%s' "$output" | tail -n1)"
  body="$(printf '%s' "$output" | sed '$d')"
  if [ "$code" != "$want" ]; then
    echo "FAIL $desc [HTTP $code, want $want]"
    echo "body: $body"
    exit 1
  fi
  echo "$body"
}

cat > "$TMPDIR/token_tool.go" <<'GO'
package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"

	authcore "github.com/open-rails/authkit/core"
	jwtkit "github.com/open-rails/authkit/jwt"
)

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func write(path string, data []byte) {
	if err := os.WriteFile(path, data, 0600); err != nil {
		fatalf("write %s: %v", path, err)
	}
}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: token_tool keys|token ...")
	}
	switch os.Args[1] {
	case "keys":
		if len(os.Args) != 5 {
			fatalf("usage: token_tool keys KID PRIVATE_PEM PUBLIC_PEM")
		}
		signer, err := jwtkit.NewRSASigner(2048, os.Args[2])
		if err != nil {
			fatalf("new signer: %v", err)
		}
		priv := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(signer.PrivateKey())})
		pubBytes, err := x509.MarshalPKIXPublicKey(signer.PublicKey())
		if err != nil {
			fatalf("marshal public key: %v", err)
		}
		pub := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
		write(os.Args[3], priv)
		write(os.Args[4], pub)
	case "token":
		if len(os.Args) != 7 {
			fatalf("usage: token_tool token KID PRIVATE_PEM ISSUER SUBJECT PERMISSIONS_CSV")
		}
		raw, err := os.ReadFile(os.Args[3])
		if err != nil {
			fatalf("read private key: %v", err)
		}
		signer, err := jwtkit.NewRSASignerFromPEM(os.Args[2], raw)
		if err != nil {
			fatalf("load signer: %v", err)
		}
		var perms []string
		if csv := strings.TrimSpace(os.Args[6]); csv != "" {
			for _, p := range strings.Split(csv, ",") {
				if p = strings.TrimSpace(p); p != "" {
					perms = append(perms, p)
				}
			}
		}
		tok, err := authcore.MintDelegatedAccessToken(context.Background(), signer, authcore.DelegatedAccessParams{
			Issuer:           os.Args[4],
			Audiences:        []string{"openrails"},
			DelegatedSubject: os.Args[5],
			Permissions:      perms,
		})
		if err != nil {
			fatalf("mint token: %v", err)
		}
		fmt.Println(tok)
	default:
		fatalf("unknown command %q", os.Args[1])
	}
}
GO

RUN_ID="$(date +%s)-$RANDOM"
MERCHANT="rbac-$RUN_ID"
APP_A="$MERCHANT-a"
APP_B="$MERCHANT-b"
ISS_A="https://$APP_A.example"
ISS_B="https://$APP_B.example"
ORIGIN_A="$ISS_A"
ORIGIN_B="$ISS_B"
KID_A="$APP_A-key"
KID_B="$APP_B-key"
SUBJECT="$(uuid)"
SOURCE_ID="$(uuid)"
AMOUNT=12345

go run "$TMPDIR/token_tool.go" keys "$KID_A" "$TMPDIR/a.key" "$TMPDIR/a.pub"
go run "$TMPDIR/token_tool.go" keys "$KID_B" "$TMPDIR/b.key" "$TMPDIR/b.pub"

{
  cat <<YAML
version: 1
merchants:
  - slug: $MERCHANT
    display_name: RBAC live E2E $RUN_ID
    issuer:
      uri: $ISS_A
      slug: $APP_A
      allowed_origins:
        - $ORIGIN_A
      public_keys:
        - kid: $KID_A
          public_key_pem: |
YAML
  sed 's/^/            /' "$TMPDIR/a.pub"
} > "$TMPDIR/merchant.yaml"

echo "== Starting OpenRails compose stack =="
"${COMPOSE[@]}" --profile all up -d --build --wait

echo "== Provisioning merchant $MERCHANT =="
"${COMPOSE[@]}" exec -T openrails sh -c 'cat > /tmp/authkit-rbac-live-e2e.yaml && openrails push-merchant-config --file /tmp/authkit-rbac-live-e2e.yaml --insert --overwrite >/tmp/authkit-rbac-live-e2e.out && cat /tmp/authkit-rbac-live-e2e.out' < "$TMPDIR/merchant.yaml"

echo "== Registering second issuer on the same merchant org =="
"${COMPOSE[@]}" exec -T postgres psql -U admin -d openrails_db -v ON_ERROR_STOP=1 \
  -v merchant="$MERCHANT" \
  -v app_b="$APP_B" \
  -v iss_b="$ISS_B" \
  -v origin_b="$ORIGIN_B" \
  -v kid_b="$KID_B" \
  -v public_b="$(cat "$TMPDIR/b.pub")" <<'SQL'
WITH org_row AS (
  SELECT owner_org_id::uuid AS org_id
    FROM openrails.merchants
   WHERE slug = :'merchant'
),
remote_app AS (
  INSERT INTO profiles.remote_applications
    (slug, org_id, issuer, jwks_uri, mode, public_keys, allowed_origins, enabled)
  SELECT :'app_b',
         org_id,
         :'iss_b',
         '',
         'static',
         jsonb_build_array(jsonb_build_object('kid', :'kid_b', 'public_key_pem', :'public_b')),
         ARRAY[:'origin_b']::text[],
         true
    FROM org_row
  ON CONFLICT (issuer) DO UPDATE
    SET slug = EXCLUDED.slug,
        org_id = EXCLUDED.org_id,
        jwks_uri = EXCLUDED.jwks_uri,
        mode = EXCLUDED.mode,
        public_keys = EXCLUDED.public_keys,
        allowed_origins = EXCLUDED.allowed_origins,
        enabled = EXCLUDED.enabled,
        updated_at = now()
  RETURNING id, org_id
)
INSERT INTO profiles.org_memberships (org_id, member_id, member_kind, role)
SELECT org_id, id, 'remote_application', 'owner'
  FROM remote_app
ON CONFLICT (org_id, member_id, member_kind) DO UPDATE
  SET role = EXCLUDED.role,
      deleted_at = NULL,
      updated_at = now();
SQL

echo "== Restarting OpenRails so the verifier reloads registered issuers =="
"${COMPOSE[@]}" restart openrails >/dev/null
for _ in {1..60}; do
  if curl -fsS "$BASE_URL/health/ready" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl -fsS "$BASE_URL/health/ready" >/dev/null

echo "== Minting merchant API key =="
API_JSON="$("${COMPOSE[@]}" exec -T openrails openrails mint-merchant-api-key --org "$MERCHANT" --merchant "$MERCHANT" --permission org:credits:read --permission org:credits:update)"
API_KEY="$(printf '%s' "$API_JSON" | sed -n 's/.*"api_key"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
if [ -z "$API_KEY" ]; then
  echo "failed to parse API key from: $API_JSON"
  exit 1
fi

TOKEN_A="$(go run "$TMPDIR/token_tool.go" token "$KID_A" "$TMPDIR/a.key" "$ISS_A" "$SUBJECT" "")"
TOKEN_B="$(go run "$TMPDIR/token_tool.go" token "$KID_B" "$TMPDIR/b.key" "$ISS_B" "$SUBJECT" "")"
TOKEN_BAD_PERM="$(go run "$TMPDIR/token_tool.go" token "$KID_A" "$TMPDIR/a.key" "$ISS_A" "$SUBJECT" "platform:orgs:update")"

echo "== Seeding balance through live service API =="
deposit_body="{\"customer_id\":\"$SUBJECT\",\"invoker\":\"authkit-rbac-live-e2e\",\"currency\":\"usd\",\"amount\":$AMOUNT,\"source\":\"authkit-rbac-live-e2e\",\"source_id\":\"$SOURCE_ID\"}"
deposit="$(request POST "$BASE_URL/v1/service/credits/deposit" "$API_KEY" "" "$deposit_body")"
deposit_body="$(expect_http "deposit credits" 200 "$deposit")"
require_body "deposit posted" "$deposit_body" "\"TransactionType\":\"deposit\""

echo "== Reading self account with permissionless delegated tokens =="
self_a="$(request GET "$BASE_URL/v1/me/account?currency=usd" "$TOKEN_A" "$ORIGIN_A")"
self_a_body="$(expect_http "issuer A self account without self permissions" 200 "$self_a")"
require_body "issuer A sees seeded balance" "$self_a_body" "\"balance_amount\":$AMOUNT"

self_b="$(request GET "$BASE_URL/v1/me/account?currency=usd" "$TOKEN_B" "$ORIGIN_B")"
self_b_body="$(expect_http "issuer B self account without self permissions" 200 "$self_b")"
require_body "issuer B shares customer identity" "$self_b_body" "\"balance_amount\":$AMOUNT"

bad="$(request GET "$BASE_URL/v1/me/account?currency=usd" "$TOKEN_BAD_PERM" "$ORIGIN_A")"
bad_body="$(expect_http "delegated token with platform permission is rejected" 401 "$bad")"
require_body "bad permission failure is sanitized" "$bad_body" "delegated_token_invalid"

echo "== authkit RBAC live E2E passed for merchant=$MERCHANT subject=$SUBJECT =="
