#!/usr/bin/env bash
# The embedded library uses AuthKit's standard-library-only contract packages.
# AuthKit's runtime belongs to an opting-in host or the standalone server.
set -euo pipefail
cd "$(dirname "$0")/.."
export GOWORK=off
check_authkit_dependencies() {
  local dep
  while IFS= read -r dep; do
    case "$dep" in
      github.com/open-rails/authkit/auth|github.com/open-rails/authkit/auth/authtest|github.com/open-rails/authkit/userinfo|github.com/open-rails/authkit/userinfo/userinfotest) ;;
      github.com/open-rails/authkit|github.com/open-rails/authkit/*)
        printf 'The embedded library links an AuthKit implementation package: %s\n' "$dep" >&2
        return 1
        ;;
    esac
  done
}
packages=(. ./adapters/http ./adapters/gin ./adapters/fiber ./internal/engine ./internal/config ./internal/billingauth ./internal/app ./internal/service ./internal/http/embedhttp ./internal/http/inprocess ./internal/hosttools ./openrailstest/...)
deps="$(go list -deps "${packages[@]}")"
check_authkit_dependencies <<<"$deps"
# Compile and run an independent consumer with its own authentication. The
# AuthKit module supplies only contracts; its server and database do not link.
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
 "github.com/open-rails/authkit/auth"
 "github.com/open-rails/openrails"
 "github.com/open-rails/openrails/billing"
 gin "github.com/open-rails/openrails/adapters/gin"
 fiber "github.com/open-rails/openrails/adapters/fiber"
)
var _ = gin.Mount
var _ = fiber.Mount
var staff = auth.Scope{Authority: "https://identity.example", ID: "billing"}
type ownAuth struct{}
type session struct{ signedIn time.Time }
func (ownAuth) RootScope(context.Context) (auth.Scope, error) { return staff, nil }
func (ownAuth) Authenticate(r *http.Request) (auth.Verified, error) {
 if r.Header.Get("Authorization") == "" { return nil, auth.ErrUnauthenticated }
 return session{signedIn: time.Now()}, nil
}
func (session) Identity() openrails.Identity {
 id := "11111111-1111-4111-8111-111111111111"
 return openrails.Identity{Issuer: "https://identity.example", Subject: id, SubjectKind: openrails.SubjectUser,
  Invoker: openrails.Invoker{Issuer: "https://identity.example", ID: id}, Credential: openrails.Credential{Kind: openrails.CredentialSession}}
}
func (session) Can(_ context.Context, scope auth.Scope, permission string) (bool, error) {
 return scope == staff && permission == string(billing.RootCustomerRead), nil
}
func (s session) CheckRecentSignIn(context.Context) error {
 if time.Since(s.signedIn) > 15*time.Minute { return &auth.Challenge{Err: auth.ErrStepUpRequired, MaxAge: 15 * time.Minute} }
 return nil
}
func main() {
 var a openrails.Authenticator = ownAuth{}
 routes := openrails.Routes{Auth: a, RouteGroups: openrails.RouteGroups{Admin: true}}
 cfg := openrails.Config{TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesReadOnly}
 if routes.Auth == nil || cfg.TestMode != openrails.Sandbox { panic("unreachable") }
}
GO
(cd "$consumer_dir" && go mod tidy && go run .)
consumer_deps="$(cd "$consumer_dir" && go list -deps -f '{{.ImportPath}}' .)"
check_authkit_dependencies <<<"$consumer_deps"
