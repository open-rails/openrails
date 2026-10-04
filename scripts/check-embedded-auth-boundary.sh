#!/usr/bin/env bash
# Engine packages only: root links AuthKit for the opt-in control plane (#1121); billing itself must not.
set -euo pipefail
cd "$(dirname "$0")/.."
packages=(./internal/config ./internal/billingauth ./internal/app ./internal/service ./internal/http/embedhttp ./internal/http/inprocess ./internal/hosttools)
deps="$(go list -deps "${packages[@]}")"
if forbidden="$(printf '%s\n' "$deps" | grep -E '^github.com/open-rails/authkit(/|$)' | grep -vE '^github.com/open-rails/authkit/(iam|internal/wireform|internal/errmodel)$')"; then
  printf 'The billing engine links AuthKit:\n%s\n' "$forbidden" >&2
  exit 1
fi
# Compile and run an independent consumer against the exact source, with its
# own authentication and no AuthKit code. Temporary workspace selection never
# writes a replace directive into a distributed module.
consumer_dir="$(mktemp -d)"
trap 'rm -rf "$consumer_dir"' EXIT
cat > "$consumer_dir/go.mod" <<'MOD'
module example.org/independent-billing-host

go 1.26.6

require github.com/open-rails/openrails v0.157.1
MOD
cat > "$consumer_dir/main.go" <<'GO'
package main
import (
 "net/http"
 "github.com/open-rails/openrails"
 gin "github.com/open-rails/openrails/adapters/gin"
 fiber "github.com/open-rails/openrails/adapters/fiber"
)
var _ = gin.Mount
var _ = fiber.Mount
func main() {
 deps := openrails.Deps{Authenticate: func(r *http.Request) (openrails.Identity, error) {
  if r.Header.Get("Authorization") == "" { return openrails.Identity{}, openrails.ErrUnauthenticated }
  return openrails.Identity{Kind: openrails.User, Issuer: "https://identity.example", SubjectID: "11111111-1111-4111-8111-111111111111"}, nil
 }}
 cfg := openrails.Config{TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesReadOnly, River: openrails.RiverHostOwned}
 if deps.Authenticate == nil || cfg.TestMode != openrails.Sandbox { panic("unreachable") }
}
GO
GOWORK="$consumer_dir/go.work" go work init "$PWD" "$consumer_dir"
(cd "$consumer_dir" && GOWORK="$consumer_dir/go.work" go run .)
