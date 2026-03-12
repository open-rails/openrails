package riverjobs

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	KindWebhookProcess = "billing.webhook_process"
	// QueueWebhooks could be separated later; for now reuse billing queue.
	QueueWebhooks = QueueBilling
)

// WebhookProcessArgs carries all data needed to process a webhook asynchronously.
type WebhookProcessArgs struct {
	Provider       string `json:"provider"`
	EventID        string `json:"event_id,omitempty"`
	EventType      string `json:"event_type,omitempty"`
	Body           []byte `json:"body"`
	ClientIP       string `json:"client_ip,omitempty"`
	Signature      string `json:"signature,omitempty"`
	SignatureValid *bool  `json:"signature_valid,omitempty"`
	// UniqueKey is a deterministic dedupe key (provider + event ID or hash) set at enqueue time.
	UniqueKey string `json:"unique_key,omitempty" river:"unique"`
	// ReceivedAt is set in the worker to avoid uniqueness jitter.
	ReceivedAt time.Time `json:"received_at,omitempty"`
}

func (WebhookProcessArgs) Kind() string { return KindWebhookProcess }

type WebhookProcessWorker struct {
	river.WorkerDefaults[WebhookProcessArgs]
	Dispatcher *webhooks.WebhookDispatcher
}

func (WebhookProcessWorker) Kind() string { return KindWebhookProcess }

func (w WebhookProcessWorker) Work(ctx context.Context, job *river.Job[WebhookProcessArgs]) error {
	args := job.Args
	if args.ReceivedAt.IsZero() {
		args.ReceivedAt = time.Now()
	}
	provider := strings.TrimSpace(strings.ToLower(args.Provider))
	if provider == "" {
		return fmt.Errorf("webhook provider required")
	}

	msg := &webhooks.WebhookMessage{
		Processor:      provider,
		EventID:        strings.TrimSpace(args.EventID),
		EventType:      strings.TrimSpace(args.EventType),
		Payload:        args.Body,
		IPAddress:      strings.TrimSpace(args.ClientIP),
		Signature:      args.Signature,
		SignatureValid: args.SignatureValid,
		ReceivedAt:     args.ReceivedAt,
	}

	if err := w.Dispatcher.Process(ctx, msg); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"provider": provider,
			"event_id": msg.EventID,
			"kind":     KindWebhookProcess,
		}).Error("webhook processing failed")
		return err
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"provider": provider,
		"event_id": msg.EventID,
	}).Info("webhook processed")
	return nil
}
