package handlers

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/internal/shared/webhookutil"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

func Webhook(r *httprequest.Request) {
	provider := webhookutil.CanonicalProvider(r.Param("provider"))
	clientIP := r.GetClientIP()
	log.WithFields(log.Fields{"provider": provider, "client_ip": clientIP}).Debug("Received webhook")
	isTestMode := r.State.Config.IsTestMode()
	if processors.IsNMIBacked(provider) {
		if enqueueNMIWebhook(r, provider, clientIP) {
			r.SuccessJSON(map[string]string{"status": "accepted"})
		}
		return
	}
	switch provider {
	case subscriptions.ProcessorCCBill:
		if !isTestMode {
			if !iputil.IsValidCCBillIP(clientIP) {
				log.WithFields(log.Fields{"client_ip": clientIP, "processor": "ccbill", "event_type": r.Query("eventType")}).Warn("CCBill webhook rejected - unauthorized IP address")
				r.ErrorJSON(http.StatusForbidden, "Unauthorized webhook source")
				return
			}
			log.WithField("client_ip", clientIP).Debug("CCBill webhook authenticated - valid IP range")
		} else {
			log.WithField("client_ip", clientIP).Debug("CCBill webhook authentication bypassed - test mode enabled")
		}
		if enqueueCCBillWebhook(r, clientIP) {
			r.SuccessJSON(map[string]string{"status": "accepted"})
		}
		return
	case subscriptions.ProcessorStripe:
		if enqueueStripeWebhook(r, clientIP) {
			r.SuccessJSON(map[string]string{"status": "accepted"})
		}
		return
	default:
		r.ErrorJSON(http.StatusBadRequest, "Invalid provider")
		return
	}
}

func enqueueCCBillWebhook(r *httprequest.Request, clientIP string) bool {
	body, err := readRequestBody(r.Request.Body)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "Failed to read request body")
		return false
	}
	prepared, err := webhookutil.PrepareCCBill(body, r.Query("eventType"))
	if err != nil {
		switch {
		case errors.Is(err, webhookutil.ErrWebhookPayloadInvalid):
			r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		case errors.Is(err, webhookutil.ErrWebhookEventTypeMissing):
			r.ErrorJSON(http.StatusBadRequest, "Missing eventType parameter")
		default:
			r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		}
		return false
	}
	args := prepared.QueueArgs(clientIP)
	if err := enqueueWebhookJob(r, args); err != nil {
		log.WithError(err).Error("failed to enqueue CCBill webhook")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to enqueue webhook")
		return false
	}
	return true
}

func enqueueStripeWebhook(r *httprequest.Request, clientIP string) bool {
	body, err := readRequestBody(r.Request.Body)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "Failed to read request body")
		return false
	}
	secret := ""
	if stripeProc := r.State.Config.GetStripeProcessor(); stripeProc != nil {
		secret = stripeProc.WebhookSecret
	}
	prepared, err := webhookutil.PrepareStripe(body, secret, r.Request.Header.Get("Stripe-Signature"), 5*time.Minute)
	if err != nil {
		switch {
		case errors.Is(err, webhookutil.ErrWebhookSignatureRequired):
			r.ErrorJSON(http.StatusUnauthorized, "Webhook signature required")
		case errors.Is(err, webhookutil.ErrWebhookSignatureMissing):
			r.ErrorJSON(http.StatusUnauthorized, "Missing webhook signature")
		case errors.Is(err, webhookutil.ErrWebhookSignatureInvalid):
			r.ErrorJSON(http.StatusUnauthorized, "Invalid webhook signature")
		case errors.Is(err, webhookutil.ErrWebhookPayloadInvalid):
			r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		default:
			r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		}
		return false
	}
	args := prepared.QueueArgs(clientIP)
	if err := enqueueWebhookJob(r, args); err != nil {
		log.WithError(err).Error("failed to enqueue Stripe webhook")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to enqueue webhook")
		return false
	}
	return true
}

func enqueueWebhookJob(r *httprequest.Request, args riverjobs.WebhookProcessArgs) error {
	opts := &river.InsertOpts{Queue: riverjobs.QueueWebhooks, UniqueOpts: river.UniqueOpts{ByArgs: true, ByQueue: true}}
	_, err := r.State.RiverProducer.Insert(r.Request.Context(), args, opts)
	return err
}

func enqueueNMIWebhook(r *httprequest.Request, provider string, clientIP string) bool {
	body, err := readRequestBody(r.Request.Body)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "Failed to read request body")
		return false
	}
	providerKey := webhookutil.CanonicalProvider(provider)
	client, ok := r.State.NMIClients[providerKey]
	if !ok || client == nil {
		r.ErrorJSON(http.StatusNotFound, fmt.Sprintf("unknown nmi provider '%s'", providerKey))
		return false
	}
	signingKey := client.GetWebhookSecret()
	prepared, err := webhookutil.PrepareNMI(providerKey, body, signingKey, firstPresentHeader(r.Request.Header, "Webhook-Signature", "X-Signature", "X-NMI-Signature", "X-Mobius-Signature"))
	if err != nil {
		if errors.Is(err, webhookutil.ErrNMIWebhookSecretMissing) || errors.Is(err, webhookutil.ErrNMIWebhookSignatureMissing) {
			log.WithError(err).Error("Missing webhook signature for NMI webhook")
			r.ErrorJSON(http.StatusUnauthorized, "Missing webhook signature")
			return false
		}
		if errors.Is(err, webhookutil.ErrNMIWebhookSignatureInvalid) {
			log.WithError(err).Error("NMI webhook signature verification failed")
			r.ErrorJSON(http.StatusUnauthorized, "Invalid webhook signature")
			return false
		}
		if errors.Is(err, webhookutil.ErrWebhookPayloadInvalid) {
			log.WithError(err).Error("failed to parse NMI webhook JSON")
			r.ErrorJSON(http.StatusBadRequest, "Invalid JSON data")
			return false
		}
		if errors.Is(err, webhookutil.ErrWebhookEventIDMissing) {
			r.ErrorJSON(http.StatusBadRequest, "Missing event_id in payload")
			return false
		}
		log.WithError(err).Error("failed to prepare NMI webhook")
		r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		return false
	}
	args := prepared.QueueArgs(clientIP)
	if err := enqueueWebhookJob(r, args); err != nil {
		log.WithError(err).Error("failed to enqueue NMI webhook")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to enqueue webhook")
		return false
	}
	return true
}

func firstPresentHeader(header http.Header, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func readRequestBody(body io.ReadCloser) ([]byte, error) {
	if body == nil {
		return []byte{}, nil
	}
	defer body.Close()
	return io.ReadAll(body)
}
