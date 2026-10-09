#!/usr/bin/env bash
# The embedded library needs no AuthKit: AuthKit belongs to the host, and to
# the server module (server/) for standalone. The root module's graph holds
# none, so a host never inherits OpenRails' AuthKit version.
set -euo pipefail
cd "$(dirname "$0")/.."
export GOWORK=off
if authkit="$(go list -m all | grep -E '^github.com/open-rails/authkit( |$)')"; then
  printf 'The root module requires AuthKit; it belongs to server/:\n%s\n' "$authkit" >&2
  exit 1
fi
packages=(. ./adapters/http ./adapters/gin ./adapters/fiber ./internal/engine ./internal/config ./internal/billingauth ./internal/app ./internal/service ./internal/http/embedhttp ./internal/http/inprocess ./internal/hosttools ./openrailstest/...)
deps="$(go list -deps "${packages[@]}")"
if forbidden="$(printf '%s\n' "$deps" | grep -E '^github.com/open-rails/authkit(/|$)')"; then
  printf 'The billing engine links AuthKit:\n%s\n' "$forbidden" >&2
  exit 1
fi
# Compile and run an independent consumer against the exact source, with its
# own authentication: its module graph, and so its go.sum, holds no AuthKit.
consumer_dir="$(mktemp -d)"
trap 'rm -rf "$consumer_dir"' EXIT
cat > "$consumer_dir/go.mod" <<MOD
module example.org/independent-billing-host

go 1.26.9

require github.com/open-rails/openrails v0.0.0-00010101000000-000000000000

replace github.com/open-rails/openrails => $PWD
MOD
cat > "$consumer_dir/main.go" <<'GO'
package main
import (
 "context"
 "net/http"
 "github.com/open-rails/openrails"
 gin "github.com/open-rails/openrails/adapters/gin"
 fiber "github.com/open-rails/openrails/adapters/fiber"
)
var _ = gin.Mount
var _ = fiber.Mount
type ownAuth struct{}
type userKey struct{}
type perm string
func (p perm) String() string { return string(p) }
func (ownAuth) gate(next http.Handler) http.Handler {
 return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
  if r.Header.Get("Authorization") == "" { http.Error(w, "sign in", http.StatusUnauthorized); return }
  next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, "11111111-1111-4111-8111-111111111111")))
 })
}
func (a ownAuth) Required() func(http.Handler) http.Handler { return a.gate }
func (a ownAuth) RequirePermission(string) func(http.Handler) http.Handler { return a.gate }
func (a ownAuth) Sensitive() func(http.Handler) http.Handler { return a.gate }
func (ownAuth) Identity(ctx context.Context) (openrails.Identity, bool) {
 id, ok := ctx.Value(userKey{}).(string)
 return openrails.Identity{Issuer: "https://identity.example", Subject: id, SubjectKind: openrails.SubjectUser,
  Invoker: openrails.Invoker{Issuer: "https://identity.example", ID: id}, Credential: openrails.Credential{Kind: openrails.CredentialSession}}, ok
}
func main() {
 routes := openrails.Routes{Auth: ownAuth{}, Permissions: openrails.Permissions{AdminRead: perm("billing:read")}}
 cfg := openrails.Config{TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesReadOnly}
 if routes.Auth == nil || cfg.TestMode != openrails.Sandbox { panic("unreachable") }
}
GO
(cd "$consumer_dir" && go mod tidy && go run .)
if grep -q 'github.com/open-rails/authkit' "$consumer_dir/go.sum"; then
  echo "An embedded host's go.sum holds AuthKit" >&2
  exit 1
fi
