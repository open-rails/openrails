// Command mintadmin mints a merchant-ADMIN delegated access token from the
// demo's issuer key (issuer-as-owner: the manifest-registered
// remote_application is merchant owner, so a delegated token carrying a
// merchant:* permissions claim administers exactly that merchant).
// E2E-harness helper: used once at provisioning time to authenticate
// POST /v1/merchant/api-keys. Prints the JWT to stdout.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/open-rails/authkit/jwtkit"
)

func main() {
	key := flag.String("key", "issuer_key.pem", "RS256 private key PEM (the demo's issuer key)")
	issuer := flag.String("issuer", "https://gated-premium-page.example", "registered remote_application issuer")
	sub := flag.String("sub", "9e6f5f7a-1111-4e0e-9c39-000000000e2e", "acting admin subject (delegated_sub, any UUID)")
	perms := flag.String("perms", "merchant:credentials:manage", "comma-separated permissions claim")
	ttl := flag.Duration("ttl", 10*time.Minute, "token TTL")
	flag.Parse()

	pemBytes, err := os.ReadFile(*key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read key:", err)
		os.Exit(1)
	}
	signer, err := jwtkit.NewSignerFromPEM("demo-1", pemBytes)
	if err != nil {
		fmt.Fprintln(os.Stderr, "signer:", err)
		os.Exit(1)
	}
	now := time.Now()
	tok, err := jwtkit.SignWithType(context.Background(), signer, jwt.MapClaims{
		"iss":           *issuer,
		"aud":           []string{"openrails"},
		"iat":           now.Unix(),
		"exp":           now.Add(*ttl).Unix(),
		"delegated_sub": *sub,
		"permissions":   strings.Split(*perms, ","),
	}, jwtkit.DelegatedAccessTokenType, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		os.Exit(1)
	}
	fmt.Println(tok)
}
