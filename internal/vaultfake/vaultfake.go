// Package vaultfake is a loopback Vault for tests: token self-lookup and the
// Transit read-key/sign endpoints over one Ed25519 key per name. SetUp(false)
// makes it drop every connection, as an unreachable Vault would.
package vaultfake

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
)

type Server struct {
	server *httptest.Server
	up     atomic.Bool
	mu     sync.Mutex
	keys   map[string]ed25519.PrivateKey
	Token  string
}

// New starts a Vault that accepts token and is up.
func New(token string) *Server {
	s := &Server{keys: map[string]ed25519.PrivateKey{}, Token: token}
	s.up.Store(true)
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

func (s *Server) URL() string   { return s.server.URL }
func (s *Server) Close()        { s.server.Close() }
func (s *Server) SetUp(up bool) { s.up.Store(up) }

// PublicKey returns key's public half, creating the key on first use.
func (s *Server) PublicKey(name string) ed25519.PublicKey {
	return s.key(name).Public().(ed25519.PublicKey)
}

// Rotate replaces key name with a fresh key, as an operator recreating it would.
func (s *Server) Rotate(name string) {
	_, k, _ := ed25519.GenerateKey(nil)
	s.mu.Lock()
	s.keys[name] = k
	s.mu.Unlock()
}

func (s *Server) key(name string) ed25519.PrivateKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[name]
	if !ok {
		_, k, _ = ed25519.GenerateKey(nil)
		s.keys[name] = k
	}
	return k
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if !s.up.Load() {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	if r.Header.Get("X-Vault-Token") != s.Token {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	switch {
	case path == "auth/token/lookup-self":
		write(w, map[string]any{"renewable": false, "ttl": 0})
	case strings.HasPrefix(path, "transit/keys/"):
		pub := s.PublicKey(strings.TrimPrefix(path, "transit/keys/"))
		write(w, map[string]any{"latest_version": 1, "keys": map[string]any{"1": map[string]any{"public_key": base64.StdEncoding.EncodeToString(pub)}}})
	case strings.HasPrefix(path, "transit/sign/"):
		var body struct{ Input string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		input, _ := base64.StdEncoding.DecodeString(body.Input)
		sig := ed25519.Sign(s.key(strings.TrimPrefix(path, "transit/sign/")), input)
		write(w, map[string]any{"signature": "vault:v1:" + base64.StdEncoding.EncodeToString(sig)})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[]}`))
	}
}

func write(w http.ResponseWriter, data map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}
