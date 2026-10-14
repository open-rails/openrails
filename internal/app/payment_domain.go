package app

import (
	"context"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

// paymentDomain is the payment page's host name; "" without one.
func (r *Runtime) paymentDomain() string {
	if r == nil || r.CheckoutOrigin() == "" {
		return ""
	}
	u, err := url.Parse(r.CheckoutOrigin())
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// followPaymentDomain registers the checkout origin on each Stripe PSP a
// configuration change declared or changed, and the sites a settings change
// registered for embedded payment pages on every Stripe PSP. It reads the sync's
// own set (the cache holds it only once the sync returns); PSP ids follow
// from their documents. One declared before the host was served is
// registered by its merchant's next purchase (RegisterPaymentDomain).
func (r *Runtime) followPaymentDomain(mid billing.MerchantID, previous, current merchantdocs.Set) {
	if r.paymentDomain() == "" || r.Merchants == nil {
		return
	}
	domains := append([]string{r.paymentDomain()}, embedDomains(current)...)
	every := !slices.Equal(embedDomains(previous), embedDomains(current))
	var psps []uuid.UUID
	for key, doc := range current.PSPs {
		p := doc.Value
		if p.Rail != string(models.RailStripe) || p.Archived {
			continue
		}
		if _, rejected := current.Rejected[merchantdocs.PSPDoc(key)]; rejected {
			continue
		}
		if before, ok := previous.PSPs[key]; !every && ok && reflect.DeepEqual(before.Value, p) {
			continue
		}
		id, _, _, _ := merchants.PSPNaturalKey(p.Rail, p.Environment, p.AccountID)
		psps = append(psps, id)
	}
	if len(psps) == 0 {
		return
	}
	r.Go("purchase domain registration", func(ctx context.Context) {
		if err := r.registerPaymentDomains(ctx, mid, psps, domains); err != nil && ctx.Err() == nil {
			log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).Warn("purchase domain: cannot queue its registration")
		}
	})
}

// embedDomains are the hosts of the https sites a merchant's settings
// register for its embedded payment pages: Stripe shows a wallet in a frame only
// when the framing page's domain is registered too.
func embedDomains(set merchantdocs.Set) []string {
	if !set.HasMerchant {
		return nil
	}
	var out []string
	for _, origin := range merchantconfig.EmbedOrigins(set.Merchant.Value.MerchantSettings) {
		if u, err := url.Parse(origin); err == nil && u.Scheme == "https" {
			out = append(out, strings.ToLower(u.Hostname()))
		}
	}
	return out
}

// RegisterPaymentDomain queues the registration of the checkout origin, and
// of the sites the merchant registered for embedded payment pages, as
// payment-method domains on the merchant's live Stripe PSPs: once per PSP
// and host.
func (r *Runtime) RegisterPaymentDomain(ctx context.Context, mid billing.MerchantID) error {
	purchase := r.paymentDomain()
	if purchase == "" || r.Merchants == nil || r.MerchantConfig == nil {
		return nil
	}
	ctx = merchant.WithID(ctx, mid)
	set, err := r.MerchantConfig.Get(ctx, mid)
	if err != nil {
		return err
	}
	scopes, err := r.Merchants.ActivePSPScopesForRail(ctx, mid, string(models.RailStripe))
	if err != nil {
		return err
	}
	psps := make([]uuid.UUID, 0, len(scopes))
	for _, scope := range scopes {
		psps = append(psps, scope.ID)
	}
	return r.registerPaymentDomains(ctx, mid, psps, append([]string{purchase}, embedDomains(set)...))
}

// registerPaymentDomains queues each domain's registration on each PSP.
func (r *Runtime) registerPaymentDomains(ctx context.Context, mid billing.MerchantID, psps []uuid.UUID, domains []string) error {
	if r.DB == nil {
		return nil
	}
	ctx = merchant.WithID(ctx, mid)
	now := time.Now()
	if r.Clock != nil {
		now = r.Clock.Now()
	}
	store := intents.NewStore(r.DB)
	for _, psp := range psps {
		for _, domain := range domains {
			if err := r.queuePaymentDomain(ctx, store, mid, psp, domain, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Runtime) queuePaymentDomain(ctx context.Context, store *intents.Store, mid billing.MerchantID, psp uuid.UUID, domain string, now time.Time) error {
	key := intents.StripePaymentMethodDomainKey(psp, domain)
	if _, queued := r.paymentDomains.Load(key); queued {
		return nil
	}
	_, err := store.GetByIdempotencyKey(ctx, key)
	if db.IsNotFound(err) {
		_, err = store.Enqueue(ctx, intents.EnqueueParams{
			MerchantID: mid.UUID(), Provider: string(models.RailStripe), PspID: psp,
			IntentType: intents.TypeStripePaymentMethodDomain, Payload: intents.StripePaymentMethodDomainPayload{DomainName: domain},
			IdempotencyKey: key, NextAttemptAt: now, Origin: intents.OriginSystem, OriginReason: "payment page domain",
		})
	}
	if err != nil {
		return err
	}
	r.paymentDomains.Store(key, struct{}{})
	return nil
}
