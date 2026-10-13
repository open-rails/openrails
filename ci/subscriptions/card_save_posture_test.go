//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/writeposture"
)

// Change the same settings an operator uses, without mutating a running config.
func setCardSavePosture(t *testing.T, w *world, mode string) {
	t.Helper()
	switch mode {
	case "process readonly", "process limited":
		value := config.ProviderWriteModeReadOnly
		if mode == "process limited" {
			value = config.ProviderWriteModeLimited
		}
		w.cfg = func(c *config.Config) { c.ProviderWriteMode = value }
		w.restart()
	case "merchant readonly", "merchant limited":
		value := writeposture.ReadOnly
		if mode == "merchant limited" {
			value = writeposture.Limited
		}
		rt := engine.Graph(w.rt).Runtime
		ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
		require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
			return writeposture.Set(ctx, rt.DB.Gen(ctx), w.client[embedded].MerchantID().UUID(), value, writeposture.ReasonOperator, "card save test", w.clock.Now())
		}))
	case "copied database":
		_, err := w.pool.Exec(t.Context(), w.q(`UPDATE billing.book_identity SET system_identifier = '0'`))
		require.NoError(t, err)
	}
}

func TestCardSaveHonorsWritePosture(t *testing.T) {
	for _, mode := range []string{"full", "process limited", "merchant limited", "process readonly", "merchant readonly", "copied database"} {
		t.Run(mode, func(t *testing.T) {
			w := newWorld(t)
			setCardSavePosture(t, w, mode)
			allowed := mode == "full" || mode == "process limited" || mode == "merchant limited"
			for _, rail := range []string{"nmi", "stripe"} {
				for _, entry := range []string{"http", "service"} {
					t.Run(rail+"/"+entry, func(t *testing.T) {
						c := w.newCustomer()
						token := w.newCardToken(rail, visa)
						nmiWrites := len(w.nmi.Calls())
						stripeWrites := len(w.stripe.Mutations(""))
						if entry == "http" {
							status, body := c.call(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp[rail], "token": token})
							if allowed {
								require.Equal(t, http.StatusCreated, status, "%v", body)
							} else {
								require.GreaterOrEqual(t, status, 400, "%v", body)
							}
						} else {
							rt := engine.Graph(w.rt).Runtime
							ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
							err := rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
								if rail == "nmi" {
									_, err := rt.RailPaymentMethodService.CreatePaymentMethod(ctx, c.id, &paymentmethods.CreatePaymentMethodRequest{PSPID: w.psp[rail].UUID(), PaymentToken: token})
									return err
								}
								_, err := rt.PurchaseService.SaveStripeCard(ctx, w.psp[rail].UUID(), token, &payment.UserIdentity{ID: c.id}, rt.CollectionResolver.(intents.StripeEngineServiceResolver))
								return err
							})
							if allowed {
								require.NoError(t, err)
							} else {
								require.Error(t, err)
							}
						}
						if allowed {
							if rail == "nmi" {
								require.Greater(t, len(w.nmi.Calls()), nmiWrites)
							} else {
								require.Greater(t, len(w.stripe.Mutations("")), stripeWrites)
							}
						} else {
							require.Len(t, w.nmi.Calls(), nmiWrites, "no NMI vault or verification was submitted")
							require.Len(t, w.stripe.Mutations(""), stripeWrites, "no Stripe customer or setup was created")
							require.Empty(t, c.paymentMethods())
						}
					})
				}
			}
		})
	}
}

func TestStripeCardConfirmationReadsWhileWritesHeld(t *testing.T) {
	for _, mode := range []string{"process readonly", "merchant readonly", "copied database"} {
		t.Run(mode, func(t *testing.T) {
			w := newWorld(t)
			c := w.newCustomer()
			token := w.newCardToken("stripe", card{Brand: "visa", Last4: "3155", Decline: "auth"})
			pending := unwrap(c.must(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp["stripe"], "token": token}))
			id := pending["id"].(string)
			setup := pending["next_action"].(map[string]any)["payload"].(map[string]any)["setup_intent_id"].(string)
			// The method pins the provider customer, so recovery must not need
			// the general mapping or try to create a replacement customer.
			_, err := w.pool.Exec(t.Context(), w.q(`DELETE FROM billing.psp_customers WHERE customer_id = $1`), c.id)
			require.NoError(t, err)
			setCardSavePosture(t, w, mode)
			before := len(w.stripe.Mutations(""))
			setups := len(w.stripe.Submitted("/v1/setup_intents"))
			customers := len(w.stripe.Submitted("/v1/customers"))
			still := unwrap(c.must(http.MethodPost, "/payment-methods/"+id+"/confirm", "", nil))
			require.Equal(t, "requires_action", still["status"])
			require.True(t, w.stripe.AuthenticateSetup(setup))
			saved := unwrap(c.must(http.MethodPost, "/payment-methods/"+id+"/confirm", "", nil))
			require.Equal(t, "active", saved["status"])
			require.Equal(t, true, saved["reusable"], "the completed agreement is recorded locally")
			replay := unwrap(c.must(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp["stripe"], "token": token}))
			require.Equal(t, id, replay["id"])
			require.Equal(t, "active", replay["status"])
			require.Len(t, w.stripe.Mutations(""), before, "confirmation and replay only read the provider")
			require.Len(t, w.stripe.Submitted("/v1/setup_intents"), setups)
			require.Len(t, w.stripe.Submitted("/v1/customers"), customers)
			var mappings int
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.psp_customers WHERE customer_id = $1`), c.id).Scan(&mappings))
			require.Zero(t, mappings, "confirmation does not resolve or recreate the customer mapping")
		})
	}
}

func TestNMICardSaveRecoveryHonorsWritePosture(t *testing.T) {
	for _, mode := range []string{"process readonly", "merchant readonly", "copied database"} {
		for _, lateVault := range []bool{false, true} {
			name := "not received"
			if lateVault {
				name = "orphan vault"
			}
			t.Run(mode+"/"+name, func(t *testing.T) {
				w := serverEntryWorld(t)
				c := w.newCustomer()
				late := make(chan func(), 1)
				var once sync.Once
				w.nmi.Intercept(func(r *http.Request) bool {
					return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "transact.php")
				}, func(r *http.Request, serve func() *http.Response) (*http.Response, error) {
					held := false
					once.Do(func() { held = true; late <- func() { serve() } })
					if held {
						return nil, io.ErrUnexpectedEOF
					}
					return serve(), nil
				})
				status, body := c.call(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp["nmi"], "card": entryCard(entryVisa), "billing_details": map[string]any{"name": "Card Holder"}})
				require.Equal(t, http.StatusConflict, status, "%v", body)
				require.Equal(t, []string{"unknown_needs_verify"}, w.intentStatuses("nmi_card_vault"))
				deliver := <-late
				if lateVault {
					deliver()
				}
				setCardSavePosture(t, w, mode)
				writes := len(w.nmi.Calls())
				w.advance(5 * time.Minute)
				w.wake()
				require.Len(t, w.nmi.Calls(), writes, "recovery may read but cannot delete a vault while writes are held")
				if !lateVault {
					require.Equal(t, []string{"failed_terminal"}, w.intentStatuses("nmi_card_vault"), "a read can settle a save that never reached NMI")
					return
				}
				require.Len(t, w.nmi.Vaults(), 1, "cleanup waits for provider writes")
				require.Equal(t, []string{"unknown_needs_verify"}, w.intentStatuses("nmi_card_vault"))

				rt := engine.Graph(w.rt).Runtime
				require.NoError(t, writeposture.ArmBook(t.Context(), rt.DB.GenDirectory(), "card save test", w.clock.Now()))
				armMerchant(t, w, w.client[embedded].MerchantID())
				w.cfg = func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeFull }
				w.restart()
				w.advance(5 * time.Minute)
				w.wake()
				require.Empty(t, w.nmi.Vaults(), "cleanup resumes once writes are allowed")
				require.Equal(t, []string{"failed_terminal"}, w.intentStatuses("nmi_card_vault"))
				require.Len(t, w.nmi.CallsTo(http.MethodDelete, "/customers/", nil), 1)
			})
		}
	}
}
