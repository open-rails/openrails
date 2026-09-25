//go:build greenfield && integration

package subscriptions_test

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// defaults reads the invariant straight from the database: how many of the
// customer's methods are default, and how many are usable.
func (w *world) defaults(customerID string) (defaults, usable int, id string) {
	w.t.Helper()
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT count(*) FILTER (WHERE is_default), count(*) FILTER (WHERE park_reason = ''),
		coalesce((SELECT id::text FROM openrails.payment_methods WHERE customer_id = $1::uuid AND is_default), '')
		FROM openrails.payment_methods WHERE customer_id = $1::uuid`), customerID).Scan(&defaults, &usable, &id))
	return defaults, usable, id
}

// requireOneDefault asserts exactly one default whenever a usable method
// exists (none otherwise) and returns it.
func (c *customer) requireOneDefault(what string) string {
	c.w.t.Helper()
	defaults, usable, id := c.w.defaults(c.id)
	require.Equal(c.w.t, min(1, usable), defaults, "%s: exactly one default among %d usable methods", what, usable)
	if id == "" {
		return ""
	}
	parsed, err := uuid.Parse(id)
	require.NoError(c.w.t, err)
	return openrails.PaymentMethodID(parsed).String()
}

// listedDefault is the default as the Client reports it; it is listed first.
func (c *customer) listedDefault(tp topology) string {
	c.w.t.Helper()
	page, err := c.w.client[tp].ListPaymentMethods(c.w.t.Context(), c.id, openrails.PageOptions{Limit: 100})
	require.NoError(c.w.t, err)
	var out []string
	for i, m := range page.Data {
		if m.Default {
			require.Zero(c.w.t, i, "the default is listed first")
			out = append(out, m.ID)
		}
	}
	require.LessOrEqual(c.w.t, len(out), 1)
	if len(out) == 0 {
		return ""
	}
	return out[0]
}

func (c *customer) deleteCard(method string) {
	c.w.t.Helper()
	status, body := c.call(http.MethodDelete, "/payment-methods/"+method, "", nil)
	require.Contains(c.w.t, []int{http.StatusNoContent, http.StatusAccepted}, status, "%v", body)
	c.w.until(func() bool {
		var n int
		require.NoError(c.w.t, c.w.pool.QueryRow(c.w.t.Context(), c.w.q(`SELECT count(*) FROM openrails.payment_methods WHERE id = $1::uuid`), strings.TrimPrefix(method, "pm_")).Scan(&n))
		return n == 0
	}, "the card is deleted")
}

// removeCard takes a card away the way its rail does: NMI through the
// durable delete, Stripe by detaching it at Stripe (Stripe cards are managed
// in Stripe's own portal) and notifying OpenRails.
func (c *customer) removeCard(method string) {
	c.w.t.Helper()
	var rail, ref, stripeCustomer string
	require.NoError(c.w.t, c.w.pool.QueryRow(c.w.t.Context(), c.w.q(`SELECT rail, rail_method_ref FROM openrails.payment_methods WHERE id = $1::uuid`), strings.TrimPrefix(method, "pm_")).Scan(&rail, &ref))
	if rail != "stripe" {
		c.deleteCard(method)
		return
	}
	c.w.stripe.mu.Lock()
	stripeCustomer = fmt.Sprint(c.w.stripe.methods[ref]["customer"])
	c.w.stripe.methods[ref]["customer"] = nil
	detached := c.w.stripe.methods[ref]
	c.w.stripe.mu.Unlock()
	event := stripeEvent("payment_method.detached", detached)
	event["data"].(obj)["previous_attributes"] = obj{"customer": stripeCustomer}
	require.Equal(c.w.t, http.StatusOK, c.w.deliver("stripe", event))
	var parked string
	require.NoError(c.w.t, c.w.pool.QueryRow(c.w.t.Context(), c.w.q(`SELECT park_reason FROM openrails.payment_methods WHERE id = $1::uuid`), strings.TrimPrefix(method, "pm_")).Scan(&parked))
	require.NotEmpty(c.w.t, parked, "the detached Stripe card is parked")
}

func (c *customer) setDefault(tp topology, method string) {
	c.w.t.Helper()
	id, err := openrails.ParsePaymentMethodID(method)
	require.NoError(c.w.t, err)
	out, err := c.w.client[tp].SetDefaultPaymentMethod(c.w.t.Context(), c.id, id)
	require.NoError(c.w.t, err)
	require.True(c.w.t, out.Default)
	require.Equal(c.w.t, method, out.ID)
}

// The first saved card is the default; later ones are not; set-default
// switches atomically; deleting the default promotes the most recently used
// card over a newer unused one; an in-place replacement keeps the default.
func TestDefaultPaymentMethod(t *testing.T) {
	t.Parallel()
	forEach(t, func(t *testing.T, rail string, tp topology) {
		w := newWorld(t)
		c := w.newCustomer()
		require.Empty(t, c.requireOneDefault("no methods"))

		first := c.saveCard(rail, visa)
		require.Equal(t, first, c.requireOneDefault("first save"))
		require.Equal(t, first, c.listedDefault(tp))
		used := c.saveCard(rail, mastercard)
		newest := c.saveCard(rail, visa)
		require.Equal(t, first, c.requireOneDefault("later saves"), "a later save is not the default")

		c.setDefault(tp, newest)
		require.Equal(t, newest, c.requireOneDefault("set default"))
		require.Equal(t, newest, c.listedDefault(tp))
		// The customer's own route switches it back.
		mine := unwrap(c.must(http.MethodPut, "/default-payment-method", "", map[string]any{"payment_method_id": first}))
		require.Equal(t, true, mine["default"])
		require.Equal(t, first, c.requireOneDefault("customer set default"))

		// The second card funds a membership and is charged: most recently used.
		price := w.membership("content:default-"+rail, 9_990_000)
		c.subscribeAgain(tp, rail, price.ID, "content:default-"+rail, used)
		c.removeCard(first)
		require.Equal(t, used, c.requireOneDefault("delete default"), "the most recently used card is promoted over a newer unused one")

		if rail == "nmi" {
			c.must(http.MethodPut, "/payment-methods/"+used, "", map[string]any{"provider": "nmi", "payment_token": w.nmi.Tokenize(visa), "last_four": visa.Last4, "card_type": visa.Brand, "expiry_date": "12/35"})
			w.settle()
			require.Equal(t, used, c.requireOneDefault("in-place replacement"), "a replaced card keeps its default")
		}
		c.removeCard(newest)
		require.Equal(t, used, c.requireOneDefault("remove a non-default"))
	})
}

// Two first saves racing each other end with exactly one default.
func TestDefaultPaymentMethodConcurrentFirstSaves(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			c := w.newCustomer()
			type request struct {
				path, key string
				body      any
			}
			var requests []request
			for range 2 {
				switch rail {
				case "nmi":
					requests = append(requests, request{"/payment-methods", "", map[string]any{"provider": "nmi", "psp_id": w.psp["nmi"], "payment_token": w.nmi.Tokenize(visa), "name_on_card": "Greenfield Payer"}})
				case "stripe":
					setup := c.must(http.MethodPost, "/payment-methods/stripe-setup", "setup-"+uuid.NewString(), map[string]any{"psp_id": w.psp["stripe"], "consent": true})
					w.stripe.completeSetup(strings.TrimSuffix(setup["client_secret"].(string), "_secret_gf"), visa)
					requests = append(requests, request{fmt.Sprintf("/payment-methods/stripe-setup/%s/confirm", setup["id"]), "", nil})
				}
			}
			statuses := make([]int, len(requests))
			var wg sync.WaitGroup
			for i, r := range requests {
				wg.Add(1)
				go func() {
					defer wg.Done()
					statuses[i], _ = c.call(http.MethodPost, r.path, r.key, r.body)
				}()
			}
			wg.Wait()
			for _, s := range statuses {
				require.Equal(t, http.StatusOK, s)
			}
			defaults, usable, _ := w.defaults(c.id)
			require.Equal(t, 2, usable)
			require.Equal(t, 1, defaults, "exactly one default after concurrent first saves")
		})
	}
}

// An imported book with no recorded default lands with one.
func TestDefaultPaymentMethodImport(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	l := importLegacy(t, w, "nmi", remote)
	require.NotEmpty(t, l.c.requireOneDefault("imported"))
	require.NotEmpty(t, l.c.listedDefault(embedded))
}

// A seeded random sequence of saves, deletes, set-defaults and in-place
// replacements keeps exactly one default after every step.
func TestDefaultPaymentMethodInvariantSequence(t *testing.T) {
	t.Parallel()
	for _, seed := range []uint64{1, 2, 3} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			c := w.newCustomer()
			rng := rand.New(rand.NewPCG(seed, seed))
			var methods []string
			for step := range 14 {
				tp := []topology{embedded, remote}[rng.IntN(2)]
				op := rng.IntN(4)
				if len(methods) == 0 {
					op = 0
				}
				pick := func() string { return methods[rng.IntN(len(methods))] }
				switch op {
				case 0:
					methods = append(methods, c.saveCard([]string{"nmi", "stripe"}[rng.IntN(2)], []card{visa, mastercard}[rng.IntN(2)]))
				case 1:
					victim := pick()
					c.removeCard(victim)
					for i, m := range methods {
						if m == victim {
							methods = append(methods[:i], methods[i+1:]...)
							break
						}
					}
				case 2:
					c.setDefault(tp, pick())
				case 3:
					before := c.requireOneDefault("before replace")
					target := pick()
					status, _ := c.call(http.MethodPut, "/payment-methods/"+target, "", map[string]any{"provider": "nmi", "payment_token": w.nmi.Tokenize(mastercard), "last_four": mastercard.Last4, "card_type": mastercard.Brand, "expiry_date": "12/35"})
					w.settle()
					if status < 300 {
						require.Equal(t, before, c.requireOneDefault("replace"), "step %d: a replacement never moves the default", step)
					}
				}
				got := c.requireOneDefault(fmt.Sprintf("step %d op %d", step, op))
				require.Equal(t, got, c.listedDefault(tp))
			}
		})
	}
}
