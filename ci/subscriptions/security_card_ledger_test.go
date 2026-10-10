//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
)

// saveFrom submits one NMI card save for c to replica r from client address ip.
func (c *customer) saveFrom(r *world, ip string, cd card) int {
	c.w.t.Helper()
	body, err := json.Marshal(map[string]any{"psp_id": r.psp["nmi"], "payment_token": r.nmi.Tokenize(cd), "billing_details": map[string]any{"name": "Card Tester"}})
	require.NoError(c.w.t, err)
	req, err := http.NewRequestWithContext(c.w.t.Context(), http.MethodPost, r.server.URL+mountPrefix+"/v1/me/payment-methods", bytes.NewReader(body))
	require.NoError(c.w.t, err)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	res, err := http.DefaultClient.Do(req)
	require.NoError(c.w.t, err)
	res.Body.Close()
	return res.StatusCode
}

func (f *fleet) refusedSaves() int {
	return f.base.nmi.RefusedSaves()
}

var refusedCard = card{Brand: "visa", Last4: "0119", Decline: "vault"}

// cardFleet is n instances of one merchant, over redis when named.
func cardFleet(t *testing.T, n int, redis string) *fleet {
	t.Helper()
	if redis == "" {
		return newFleet(t, n)
	}
	return startFleet(t, n, false, nil, func(w *world) {
		w.cfg = func(c *config.Config) { c.Redis = &config.RedisConfig{Addr: redis} }
	})
}

// noLedgerTable asserts PostgreSQL has nowhere to count card testing.
func (f *fleet) noLedgerTable() {
	f.t.Helper()
	var table *string
	require.NoError(f.t, f.base.pool.QueryRow(f.t.Context(), "SELECT to_regclass($1)::text", pgx.Identifier{f.base.schema, "card_attempt_failures"}.Sanitize()).Scan(&table))
	require.Nil(f.t, table, "card testing is never counted in PostgreSQL")
}

// Card testing is counted in the abuse state: six refusals in fifteen minutes
// (or ten in a day) per customer or client address block attempts before the
// gateway, checkout included, until the window passes. In attack mode any
// subject with a recent refusal is blocked; clean customers are not. One
// instance counts in its memory and instances with Redis share the counts,
// whichever instance served each attempt.
func TestSecurityCardTestingLedger(t *testing.T) {
	t.Parallel()
	redis := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_REDIS_ADDR"))
	if redis == "" {
		t.Fatal("OPENRAILS_E2E_REDIS_ADDR must point at a disposable Redis")
	}
	for name, shape := range map[string]struct {
		instances int
		redis     string
	}{"one instance without Redis": {1, ""}, "instances with Redis": {2, redis}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			t.Run("customer", func(t *testing.T) {
				t.Parallel()
				f := cardFleet(t, shape.instances, shape.redis)
				on := func(i int) *world { return f.replicas[i%len(f.replicas)] }
				price := on(0).membership("content:members", 9_990_000)
				c := on(0).newCustomer()
				for i := range 6 {
					require.Equal(t, http.StatusBadGateway, c.saveFrom(on(i), fmt.Sprintf("198.51.100.%d", i+1), refusedCard), "attempt %d reaches the gateway", i)
				}
				require.Equal(t, 6, f.refusedSaves())
				for _, r := range f.replicas {
					require.Equal(t, http.StatusTooManyRequests, c.saveFrom(r, "198.51.100.99", refusedCard), "blocked on replica %s", r.replica.name)
					require.Equal(t, http.StatusTooManyRequests, c.saveFrom(r, "198.51.100.99", visa), "a good card is refused while blocked")
					for _, tp := range []topology{embedded, remote} {
						link, err := r.client[tp].CreateCheckoutSession(t.Context(), billing.CreateCheckoutSessionParams{Customer: c.identity(), PriceID: price.ID})
						require.NoError(t, err)
						_, err = hostedSession{w: r, id: link.ID}.buy(c, order{rail: "nmi", token: r.nmi.Tokenize(refusedCard)})
						blocked := requireStatus(t, err, http.StatusTooManyRequests)
						require.Equal(t, "card_attempts_blocked", blocked.Code, "%s checkout for a blocked customer", tp)
					}
				}
				require.Equal(t, 6, f.refusedSaves(), "blocked attempts never reach the gateway")

				f.advance(16 * time.Minute)
				require.Equal(t, http.StatusCreated, c.saveFrom(on(1), "198.51.100.99", visa), "the burst block lifts with its window")
				for i := range 4 {
					require.Equal(t, http.StatusBadGateway, c.saveFrom(on(i), "203.0.113.7", refusedCard))
					f.advance(4 * time.Minute)
				}
				f.advance(16 * time.Minute)
				require.Equal(t, http.StatusTooManyRequests, c.saveFrom(on(0), "203.0.113.8", visa), "ten refusals in a day block for the day")
				require.Equal(t, 10, f.refusedSaves())
				f.noLedgerTable()
			})
			t.Run("address", func(t *testing.T) {
				t.Parallel()
				f := cardFleet(t, shape.instances, shape.redis)
				on := func(i int) *world { return f.replicas[i%len(f.replicas)] }
				for i := range 6 {
					c := on(0).newCustomer()
					require.Equal(t, http.StatusBadGateway, c.saveFrom(on(i), "192.0.2.10", refusedCard))
				}
				fresh := on(1).newCustomer()
				require.Equal(t, http.StatusTooManyRequests, fresh.saveFrom(on(1), "192.0.2.10", visa), "one address testing cards through many accounts is blocked")
				require.Equal(t, http.StatusCreated, fresh.saveFrom(on(0), "192.0.2.11", visa), "another address is not")
				require.Equal(t, 6, f.refusedSaves())
			})
			t.Run("merchant_attack", func(t *testing.T) {
				t.Parallel()
				f := cardFleet(t, shape.instances, shape.redis)
				on := func(i int) *world { return f.replicas[i%len(f.replicas)] }
				var last *customer
				for i := range 100 {
					last = on(0).newCustomer()
					require.Equal(t, http.StatusBadGateway, last.saveFrom(on(i), fmt.Sprintf("10.%d.%d.1", i/200, i%200), refusedCard))
				}
				require.Equal(t, http.StatusTooManyRequests, last.saveFrom(on(1), "10.9.9.9", visa), "in attack mode one recent refusal blocks")
				clean := on(1).newCustomer()
				require.Equal(t, http.StatusCreated, clean.saveFrom(on(0), "10.9.9.10", visa), "a customer with no refusals still saves a card")
				require.Equal(t, 100, f.refusedSaves())
			})
		})
	}

	// A declared Redis that does not answer leaves each instance counting its
	// own refusals: they block on the instance that saw them, not on another.
	t.Run("Redis down", func(t *testing.T) {
		t.Parallel()
		f := cardFleet(t, 2, "127.0.0.1:1")
		a, b := f.replicas[0], f.replicas[1]
		c := a.newCustomer()
		for i := range 6 {
			require.Equal(t, http.StatusBadGateway, c.saveFrom(a, fmt.Sprintf("198.51.100.%d", i+1), refusedCard), "attempt %d reaches the gateway", i)
		}
		require.Equal(t, http.StatusTooManyRequests, c.saveFrom(a, "198.51.100.99", visa), "blocked where the refusals were counted")
		require.Equal(t, http.StatusCreated, c.saveFrom(b, "198.51.100.99", visa), "another instance counts its own")
		require.Equal(t, 6, f.refusedSaves())
		f.noLedgerTable()
	})
}

// A checkout counts each pay's client address as the trusted proxy forwards
// it: one address testing cards through many accounts is blocked; in attack
// mode a tester's next card is refused before the gateway while a clean buyer
// pays. A forwarded address that is not an IP is refused.
func TestSecurityCardTestingThroughTheHost(t *testing.T) {
	t.Parallel()
	declined := card{Brand: "visa", Last4: "0002", Decline: "202"}
	pay := func(w *world, tp topology, price billing.PriceID, c *customer, ip string, cd card) error {
		return succeeded(c.checkout(tp, order{price: price, rail: "nmi", token: w.nmi.Tokenize(cd), ip: ip}))
	}
	isDecline := func(t *testing.T, err error) {
		t.Helper()
		require.ErrorContains(t, err, "checkout failed")
	}
	refused := func(t *testing.T, err error, status int) {
		t.Helper()
		var se *billing.StatusError
		require.ErrorAs(t, err, &se)
		require.Equal(t, status, se.Status, "%v", err)
		if status == http.StatusTooManyRequests {
			require.Equal(t, "card_attempts_blocked", se.Code)
		}
	}
	t.Run("address", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		price := w.membership("content:members", 9_990_000).ID
		for range 6 {
			isDecline(t, pay(w, embedded, price, w.newCustomer(), "192.0.2.20", declined))
		}
		sales := len(w.nmi.Sales())
		refused(t, pay(w, embedded, price, w.newCustomer(), "192.0.2.20", visa), http.StatusTooManyRequests)
		require.Len(t, w.nmi.Sales(), sales, "a blocked attempt never reaches the gateway")
		require.NoError(t, pay(w, embedded, price, w.newCustomer(), "192.0.2.21", visa), "another address pays")
	})
	t.Run("wave", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		price := w.membership("content:members", 9_990_000).ID
		var tester *customer
		for i := range 100 {
			tester = w.newCustomer()
			isDecline(t, pay(w, embedded, price, tester, fmt.Sprintf("2001:db8:77:%x::1", i+1), declined))
		}
		sales := len(w.nmi.Sales())
		refused(t, pay(w, embedded, price, tester, "2001:db8:77:64::1", visa), http.StatusTooManyRequests)
		require.Len(t, w.nmi.Sales(), sales, "in attack mode one recent decline blocks, before the gateway")
		require.NoError(t, pay(w, embedded, price, w.newCustomer(), "2001:db8:77:1000::1", visa), "a clean buyer still pays")
	})
	t.Run("invalid", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		price := w.membership("content:members", 9_990_000).ID
		for _, tp := range []topology{embedded, remote} {
			refused(t, pay(w, tp, price, w.newCustomer(), "not-an-ip", visa), http.StatusBadRequest)
		}
		require.Empty(t, w.nmi.Sales())
	})
}
