#!/usr/bin/env bash
# A package graph fence: standalone composition may use AuthKit; billing may not.
set -euo pipefail
cd "$(dirname "$0")/.."
packages=(. ./config ./embed ./embed/operator ./pkg/billingauth ./adapters/http)
if [[ "${1:-}" == "--adapters" ]]; then
  packages+=(./adapters/gin ./adapters/fiber)
fi
deps="$(go list -deps "${packages[@]}")"
if forbidden="$(printf '%s\n' "$deps" | grep -E '^github.com/open-rails/authkit(/|$)')"; then
  printf 'Embedded billing imports AuthKit:\n%s\n' "$forbidden" >&2
  exit 1
fi
# Compile an independent consumer against the exact source. Temporary workspace
# selection never writes a replace directive into a distributed module.
consumer_dir="$(mktemp -d)"
trap 'rm -rf "$consumer_dir"' EXIT
cat > "$consumer_dir/go.mod" <<'MOD'
module example.org/independent-billing-host

go 1.26.6

require (
 github.com/open-rails/openrails v0.155.0
 github.com/open-rails/helpers v0.3.0
)
MOD
cat > "$consumer_dir/main.go" <<'GO'
package main
import (
 "context"
 "net/http"
 auth "github.com/open-rails/helpers/auth"
 "github.com/open-rails/openrails/embed"
 "github.com/open-rails/openrails/pkg/billingauth"
)
type provider struct{}
type principal struct{}
func (principal) Identity() auth.Identity { return auth.Identity{Kind:auth.KindUser,Issuer:"https://identity.example",Subject:"11111111-1111-4111-8111-111111111111"} }
func (provider) AuthenticateRequest(context.Context,*http.Request)(auth.Principal,error) { return principal{},nil }
var _ billingauth.Verifier = provider{}
var _ = embed.New
func main() { if _,err:=billingauth.NewIntegration(billingauth.IntegrationOptions{Verifier:provider{},Customer:billingauth.SubjectCustomerID});err!=nil {panic(err)} }
GO
GOWORK="$consumer_dir/go.work" go work init "$PWD" "$consumer_dir"
consumer_deps="$(cd "$consumer_dir" && GOWORK="$consumer_dir/go.work" go list -deps .)"
if forbidden="$(printf '%s\n' "$consumer_deps" | grep -E '^github.com/open-rails/authkit(/|$)')"; then
  printf 'Independent consumer imports AuthKit:\n%s\n' "$forbidden" >&2
  exit 1
fi
(cd "$consumer_dir" && GOWORK="$consumer_dir/go.work" go run .)
