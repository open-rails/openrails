// Package testauth issues real AuthKit delegated tokens and creates independent
// ES256 sender proofs for HTTP workflow tests. It is not an application client.
package testauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/open-rails/authkit/jwtkit"
)

// senderKeys belongs only to the test process, so old workflow helpers returning
// token strings can still attach the corresponding proof to each actual request.
var senderKeys sync.Map

type Sender struct {
	key        *ecdsa.PrivateKey
	jwk        map[string]string
	Thumbprint [32]byte
}

func NewSender() (*Sender, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	enc := base64.RawURLEncoding.EncodeToString
	x, y := enc(key.X.FillBytes(make([]byte, 32))), enc(key.Y.FillBytes(make([]byte, 32)))
	canonical := fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":"%s","y":"%s"}`, x, y)
	return &Sender{key: key, jwk: map[string]string{"kty": "EC", "crv": "P-256", "x": x, "y": y}, Thumbprint: sha256.Sum256([]byte(canonical))}, nil
}

func (s *Sender) Proof(method, target, token string) (string, error) {
	enc := base64.RawURLEncoding.EncodeToString
	sum := sha256.Sum256([]byte(token))
	header, err := json.Marshal(map[string]any{"typ": "dpop+jwt", "alg": "ES256", "jwk": s.jwk})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{"htm": method, "htu": target, "ath": enc(sum[:]), "iat": time.Now().Unix(), "jti": uuid.NewString()})
	if err != nil {
		return "", err
	}
	input := enc(header) + "." + enc(claims)
	digest := sha256.Sum256([]byte(input))
	r, v, err := ecdsa.Sign(rand.Reader, s.key, digest[:])
	if err != nil {
		return "", err
	}
	signature := append(r.FillBytes(make([]byte, 32)), v.FillBytes(make([]byte, 32))...)
	return input + "." + enc(signature), nil
}

// Authorize attaches a fresh proof for a token from MintDelegated, or a normal
// Bearer header for ordinary user/service/API-key credentials.
func Authorize(r *http.Request, token string) error {
	sender, ok := senderKeys.Load(token)
	if !ok {
		r.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
	target := *r.URL
	target.RawQuery, target.Fragment = "", ""
	proof, err := sender.(*Sender).Proof(r.Method, target.String(), token)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "DPoP "+token)
	r.Header.Set("DPoP", proof)
	return nil
}

func Request(ctx context.Context, method, target, token string) *http.Request {
	r, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		panic(err)
	}
	if err := Authorize(r, token); err != nil {
		panic(err)
	}
	return r
}

func MintDelegated(ctx context.Context, signer jwtkit.Signer, params authkit.DelegatedAccessParams) (string, error) {
	sender, err := NewSender()
	if err != nil {
		return "", err
	}
	params.ConfirmationJWKThumbprintSHA256 = &sender.Thumbprint
	token, err := authcore.MintDelegatedAccessToken(ctx, signer, params)
	if err == nil {
		senderKeys.Store(token, sender)
	}
	return token, err
}

// ClientHandler is the in-process test client's sender boundary. It builds the
// externally visible URL and attaches proof before invoking the real server.
// Protocol refusal/replay tests use the server directly instead.
func ClientHandler(baseURL string, server http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next := r.Clone(r.Context())
		if !next.URL.IsAbs() {
			base, err := url.Parse(baseURL)
			if err != nil {
				panic(err)
			}
			next.URL.Scheme, next.URL.Host = base.Scheme, base.Host
		}
		if token, ok := strings.CutPrefix(next.Header.Get("Authorization"), "Bearer "); ok {
			if err := Authorize(next, token); err != nil {
				panic(err)
			}
		}
		server.ServeHTTP(w, next)
	})
}
