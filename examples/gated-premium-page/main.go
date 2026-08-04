// Command gated-premium-page is a minimal standalone-integration demo:
// a public page, a /premium page gated on an OpenRails entitlement, and a
// /buy page that runs the NMI tokenized-vault checkout (Collect.js).
//
// Living companion to docs/standalone-integration.md + docs/frontend-integration.md.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/open-rails/authkit/jwtkit"
	openrails "github.com/open-rails/openrails"
)

//go:embed static
var static embed.FS

type config struct {
	baseURL       string // OPENRAILS_BASE_URL
	apiKey        string // OPENRAILS_API_KEY (openrails_st_…)
	merchant      string // MERCHANT_SLUG
	userID        string // DEMO_USER_ID — the one fake logged-in user (must be a UUID)
	entitlement   string // ENTITLEMENT gating /premium
	issuer        string // DEMO_ISSUER — must match merchants.yaml remote_application.issuer
	addr          string
	sessionSecret []byte
	// There is deliberately NO rail/PSP/tokenization config here: the browser
	// discovers all of it from OpenRails' public GET /v1/checkout-config, which
	// serves the merchant's armed PSPs and each one's public tokenization
	// values. Duplicating them in this app's env is exactly the drift #829
	// removed.
}

type app struct {
	cfg    config
	client openrails.Client
	signer jwtkit.Signer
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := config{
		baseURL:     env("OPENRAILS_BASE_URL", "http://localhost:3053"),
		apiKey:      os.Getenv("OPENRAILS_API_KEY"),
		merchant:    env("MERCHANT_SLUG", "demo"),
		userID:      env("DEMO_USER_ID", "0b5e9f6e-2f1b-4c5e-9a51-0c9f6a3d7e21"),
		entitlement: env("ENTITLEMENT", "premium"),
		issuer:      env("DEMO_ISSUER", "https://gated-premium-page.example"),
		addr:        ":" + env("PORT", "8080"),
	}
	if s := os.Getenv("SESSION_SECRET"); s != "" {
		cfg.sessionSecret = []byte(s)
	} else {
		cfg.sessionSecret = make([]byte, 32)
		if _, err := rand.Read(cfg.sessionSecret); err != nil {
			log.Fatal(err)
		}
	}

	signer, err := loadOrCreateSigner(env("ISSUER_KEY_FILE", "issuer_key.pem"))
	if err != nil {
		log.Fatalf("issuer key: %v", err)
	}

	// Backend SDK client: API key auth, short per-call deadline, fail fast at
	// boot (docs/standalone-integration.md "Backend integration").
	client := openrails.NewRemote(cfg.baseURL,
		openrails.WithAPIKey(cfg.apiKey),
		openrails.WithCurrency("usd"),
		openrails.WithTimeout(5*time.Second),
	)
	if err := openrails.Verify(context.Background(), client); err != nil {
		log.Fatalf("openrails unreachable or bad OPENRAILS_API_KEY: %v", err)
	}

	a := &app{cfg: cfg, client: client, signer: signer}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.page("static/index.html"))
	mux.HandleFunc("GET /login", a.login)
	mux.HandleFunc("GET /logout", a.logout)
	mux.HandleFunc("GET /premium", a.premium)
	mux.HandleFunc("GET /buy", a.requireSession(a.page("static/buy.html")))
	mux.HandleFunc("GET /api/token", a.token)
	mux.HandleFunc("GET /api/config", a.configJSON)
	mux.Handle("GET /static/", http.FileServer(http.FS(static)))

	log.Printf("demo up on %s (merchant %s, entitlement %q)", cfg.addr, cfg.merchant, cfg.entitlement)
	log.Fatal(http.ListenAndServe(cfg.addr, mux))
}

// ---- fake session auth: a signed cookie holding the demo user id ----------

func (a *app) sign(v string) string {
	m := hmac.New(sha256.New, a.cfg.sessionSecret)
	m.Write([]byte(v))
	return hex.EncodeToString(m.Sum(nil))
}

func (a *app) sessionUser(r *http.Request) string {
	c, err := r.Cookie("session")
	if err != nil {
		return ""
	}
	user, mac, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(mac), []byte(a.sign(user))) {
		return ""
	}
	return user
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	u := a.cfg.userID
	http.SetCookie(w, &http.Cookie{Name: "session", Value: u + "." + a.sign(u), Path: "/", HttpOnly: true})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "session", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *app) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.sessionUser(r) == "" {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		next(w, r)
	}
}

func (a *app) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := static.ReadFile(name)
		if err != nil {
			http.Error(w, "missing page", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	}
}

// ---- the entitlement gate --------------------------------------------------

// premium is the whole point of the demo: one SDK call decides access.
// Entitlements are the access truth — never inspect subscription rows.
func (a *app) premium(w http.ResponseWriter, r *http.Request) {
	user := a.sessionUser(r)
	if user == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	ents, err := a.client.ListActiveEntitlements(r.Context(), []string{user}, time.Time{})
	if err != nil {
		http.Error(w, "billing lookup failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	for _, e := range ents[user] {
		if e.Entitlement == a.cfg.entitlement {
			a.page("static/premium.html")(w, r)
			return
		}
	}
	http.Redirect(w, r, "/buy", http.StatusFound)
}

// ---- delegated-token exchange (docs/frontend-integration.md) ---------------

// token swaps the demo session for a short-lived delegated JWT the browser
// sends straight to OpenRails. Claim contract: iss (the registered
// remote_application issuer), aud ["openrails"], delegated_sub (the user),
// JOSE typ "delegated-access+jwt", short TTL.
func (a *app) token(w http.ResponseWriter, r *http.Request) {
	user := a.sessionUser(r)
	if user == "" {
		http.Error(w, `{"error":"not logged in"}`, http.StatusUnauthorized)
		return
	}
	const ttl = 5 * time.Minute
	now := time.Now()
	tok, err := jwtkit.SignWithType(r.Context(), a.signer, jwt.MapClaims{
		"iss":           a.cfg.issuer,
		"aud":           []string{"openrails"},
		"iat":           now.Unix(),
		"exp":           now.Add(ttl).Unix(),
		"delegated_sub": user,
	}, jwtkit.DelegatedAccessTokenType, true)
	if err != nil {
		http.Error(w, `{"error":"mint failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"token": tok, "expires_in": int(ttl.Seconds())})
}

// configJSON hands the buy page the two things only THIS app knows: where
// OpenRails lives and which entitlement it gates on. Everything payment-shaped
// — which PSPs are armed, how to drive each one, the public tokenization key
// and script URL — the page reads straight from OpenRails'
// GET /v1/checkout-config, so it can never drift from the merchant manifest.
func (a *app) configJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"openrails_base_url": a.cfg.baseURL,
		"entitlement":        a.cfg.entitlement,
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ---- issuer keypair --------------------------------------------------------

// loadOrCreateSigner loads the RS256 issuer key, generating one on first run.
// The printed public key goes into merchants.yaml
// remote_application.public_keys (see README step 2).
func loadOrCreateSigner(path string) (jwtkit.Signer, error) {
	if pemBytes, err := os.ReadFile(path); err == nil {
		return jwtkit.NewSignerFromPEM("demo-1", pemBytes)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	priv := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, priv, 0o600); err != nil {
		return nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	pub := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	if err := os.WriteFile(path+".pub", pub, 0o644); err != nil {
		return nil, err
	}
	log.Printf("generated %s — register the public key with your merchant:\n%s", path, pub)
	return jwtkit.NewSignerFromPEM("demo-1", priv)
}
