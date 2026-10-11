package app

import (
	"context"
	"net/url"
	"reflect"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
)

// checkoutDomain is the hosted checkout's host name; "" without one.
func (r *Runtime) checkoutDomain() string {
	if r == nil || r.HostedCheckoutOrigin == "" {
		return ""
	}
	u, err := url.Parse(r.HostedCheckoutOrigin)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// followCheckoutDomain registers the checkout host on each Stripe PSP a
// configuration change declared or changed. One declared before the host was
// served is registered by its merchant's next checkout (RegisterCheckoutDomain).
func (r *Runtime) followCheckoutDomain(mid billing.MerchantID, previous, current merchantdocs.Set) {
	if r.checkoutDomain() == "" {
		return
	}
	keys := map[string]bool{}
	for key, doc := range current.PSPs {
		if doc.Value.Rail != string(models.RailStripe) || doc.Value.Archived {
			continue
		}
		if before, ok := previous.PSPs[key]; ok && reflect.DeepEqual(before.Value, doc.Value) {
			continue
		}
		keys[strings.ToLower(key)] = true
	}
	if len(keys) == 0 {
		return
	}
	r.Go("checkout domain registration", func(ctx context.Context) {
		if err := r.RegisterCheckoutDomain(ctx, mid, keys); err != nil && ctx.Err() == nil {
			log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).Warn("checkout domain: cannot queue its registration")
		}
	})
}

// RegisterCheckoutDomain queues the checkout host's registration as a
// payment-method domain on the merchant's live Stripe PSPs (only those keys,
// when keys is not nil): once per PSP and host.
func (r *Runtime) RegisterCheckoutDomain(ctx context.Context, mid billing.MerchantID, keys map[string]bool) error {
	domain := r.checkoutDomain()
	if domain == "" || r.Merchants == nil || r.DB == nil {
		return nil
	}
	ctx = merchant.WithID(ctx, mid)
	scopes, err := r.Merchants.ActivePSPScopesForRail(ctx, mid, string(models.RailStripe), r.Merchants.Environment())
	if err != nil {
		return err
	}
	now := time.Now()
	if r.Clock != nil {
		now = r.Clock.Now()
	}
	store := intents.NewStore(r.DB)
	for _, scope := range scopes {
		if keys != nil && !keys[strings.ToLower(scope.Key)] {
			continue
		}
		key := intents.StripePaymentMethodDomainKey(scope.ID, domain)
		if _, queued := r.checkoutDomains.Load(key); queued {
			continue
		}
		_, err := store.GetByIdempotencyKey(ctx, key)
		if db.IsNotFound(err) {
			_, err = store.Enqueue(ctx, intents.EnqueueParams{
				MerchantID: mid.UUID(), Provider: string(models.RailStripe), PspID: scope.ID,
				IntentType: intents.TypeStripePaymentMethodDomain, Payload: intents.StripePaymentMethodDomainPayload{DomainName: domain},
				IdempotencyKey: key, NextAttemptAt: now, Origin: intents.OriginSystem, OriginReason: "hosted checkout domain",
			})
		}
		if err != nil {
			return err
		}
		r.checkoutDomains.Store(key, struct{}{})
	}
	return nil
}
