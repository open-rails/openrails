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
 "time"
 "github.com/open-rails/helpers/auth"
 "github.com/open-rails/openrails"
 gin "github.com/open-rails/openrails/adapters/gin"
 fiber "github.com/open-rails/openrails/adapters/fiber"
)
var _ = gin.Mount
var _ = fiber.Mount
var staff = openrails.Scope{Authority: "https://identity.example", ID: "billing"}
type ownAuth struct{}
type session struct{ signedIn time.Time }
type perm string
func (p perm) String() string { return string(p) }
func (ownAuth) Authenticate(r *http.Request) (auth.Verified, error) {
 if r.Header.Get("Authorization") == "" { return nil, auth.ErrUnauthenticated }
 return session{signedIn: time.Now()}, nil
}
func (session) Identity() openrails.Identity {
 id := "11111111-1111-4111-8111-111111111111"
 return openrails.Identity{Issuer: "https://identity.example", Subject: id, SubjectKind: openrails.SubjectUser,
  Invoker: openrails.Invoker{Issuer: "https://identity.example", ID: id}, Credential: openrails.Credential{Kind: openrails.CredentialSession}}
}
func (session) Can(_ context.Context, scope openrails.Scope, permission string) (bool, error) {
 return scope == staff && permission == "billing:read", nil
}
func (s session) CheckRecentSignIn(context.Context) error {
 if time.Since(s.signedIn) > 15*time.Minute { return &auth.Challenge{Err: auth.ErrStepUpRequired, MaxAge: 15 * time.Minute} }
 return nil
}
func main() {
 var a openrails.Authenticator = ownAuth{}
 routes := openrails.Routes{Auth: a, Scope: staff, RouteGroups: openrails.RouteGroups{Admin: true}, Permissions: openrails.Permissions{AdminRead: perm("billing:read")}}
 cfg := openrails.Config{TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesReadOnly}
 if routes.Auth == nil || cfg.TestMode != openrails.Sandbox { panic("unreachable") }
}
GO
(cd "$consumer_dir" && go mod tidy && go run .)
if grep -q 'github.com/open-rails/authkit' "$consumer_dir/go.sum"; then
  echo "An embedded host's go.sum holds AuthKit" >&2
  exit 1
fi
