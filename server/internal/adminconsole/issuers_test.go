package adminconsole

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// An issuer shows while its probe passes, hides when it breaks, shows again
// once it recovers, and leaves when root stops trusting it; each change is
// reported once.
func TestDirectoryFollowsEachIssuersProbe(t *testing.T) {
	var broken atomic.Bool
	var issuer string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case broken.Load():
			http.Error(w, "down", http.StatusServiceUnavailable)
		case r.URL.Path == "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/jwks", "code_challenge_methods_supported": []string{"S256"}})
		case r.URL.Path == "/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "OKP", "crv": "Ed25519", "x": "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer idp.Close()
	issuer = idp.URL
	trusted := []Source{{Issuer: issuer, Operator: true}}
	var mu sync.Mutex
	var reports []string
	d := newDirectory(nil, func(context.Context) ([]Source, error) { return trusted, nil }, "https://billing.test", "https://billing.test/admin/callback",
		func(issuer string, err error) {
			mu.Lock()
			defer mu.Unlock()
			reports = append(reports, issuer+" "+reason(err))
		})
	now := time.Unix(1_800_000_000, 0)
	d.now = func() time.Time { return now }
	reported := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), reports...)
	}

	expect := func(step string, shown int, reports ...string) {
		t.Helper()
		if got := d.Issuers(); len(got) != shown {
			t.Fatalf("%s: shows %d issuers, want %d", step, len(got), shown)
		}
		got := reported()
		if len(got) != len(reports) {
			t.Fatalf("%s: reports %q, want %d", step, got, len(reports))
		}
		for i, want := range reports {
			if !strings.HasPrefix(got[i], issuer+" "+want) {
				t.Fatalf("%s: report %d is %q, want %q", step, i, got[i], issuer+" "+want)
			}
		}
	}

	d.refresh()
	want := []Issuer{{Key: issuer, URL: issuer, ClientID: ClientID, Name: idp.Listener.Addr().String(), Resource: "https://billing.test", Scope: DefaultScope, RedirectURI: "https://billing.test/admin/callback"}}
	if got := d.Issuers(); !reflect.DeepEqual(got, want) {
		t.Fatalf("buttons %+v, want %+v", got, want)
	}
	expect("working", 1)

	broken.Store(true)
	now = now.Add(okTTL)
	d.refresh()
	const down = "OpenID Connect discovery: GET "
	expect("broken", 0, down)
	now = now.Add(failedTTL / 2)
	d.refresh()
	expect("not probed again before failedTTL", 0, down)

	broken.Store(false)
	now = now.Add(failedTTL)
	d.refresh()
	expect("recovered", 1, down, "")

	broken.Store(true)
	now = now.Add(okTTL)
	d.refresh()
	trusted = nil
	now = now.Add(listTTL)
	d.refresh()
	expect("no longer trusted", 0, down, "", down, "")
}

// A redirect URI that is not an absolute URL hides every issuer.
func TestDirectoryNeedsAnAbsoluteRedirectURI(t *testing.T) {
	var got error
	d := newDirectory(nil, func(context.Context) ([]Source, error) {
		return []Source{{Issuer: "https://idp.test", Operator: true}}, nil
	}, "https://billing.test", "/admin/callback", func(_ string, err error) { got = err })
	d.refresh()
	if len(d.Issuers()) != 0 || got == nil || !strings.Contains(got.Error(), "redirect URI") {
		t.Fatalf("shown %v, reported %v", d.Issuers(), got)
	}
}
