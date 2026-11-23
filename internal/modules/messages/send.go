package messages

import (
	"context"
	"encoding/json"
	"errors"
	"net/mail"
	"net/textproto"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

// Send settles args' message on the job's attempt. A message already settled
// or erased is done. Each attempt resolves, in order, whether the merchant
// sends its event type, whether the fact's PSP moves real money, the
// customer's address and the hourly cap; each that stops it is where it
// ends. Otherwise it renders and sends it. On the last attempt an error
// fails it.
func (s *Service) Send(ctx context.Context, args SendArgs, last bool) error {
	ctx = merchant.WithID(ctx, billing.MerchantID(args.MerchantID))
	m, err := s.DB.Gen(ctx).GetCustomerMessage(ctx, gen.GetCustomerMessageParams{MerchantID: args.MerchantID, ID: args.MessageID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if m.Status != statusQueued && m.Status != statusSending {
		return nil
	}
	err = s.send(ctx, m, last)
	if err != nil && last {
		var failed sendError
		if !errors.As(err, &failed) {
			_ = s.settle(ctx, m, settled{status: statusFailed, attempts: m.Attempts, code: errUnavailable})
		}
	}
	return err
}

// maxRecipient is the longest address the log holds.
const maxRecipient = 320

// sendError is a send the sender refused, its message settled already.
type sendError struct{ error }

func (s *Service) send(ctx context.Context, m gen.GetCustomerMessageRow, last bool) error {
	end := func(status, code string) error {
		return s.settle(ctx, m, settled{status: status, attempts: m.Attempts, code: code})
	}
	if !Enabled(m.EventType) {
		return end(statusDisabled, "")
	}
	if m.Data == nil || m.OccurredAt == nil {
		return end(statusFailed, errEventMissing)
	}
	var data billing.NotificationData
	if err := json.Unmarshal(m.Data, &data); err != nil {
		return end(statusFailed, errEventUnreadable)
	}
	test, err := s.DB.Gen(ctx).CustomerMessageOnTestPSP(ctx, gen.CustomerMessageOnTestPSPParams{
		MerchantID: m.MerchantID, PaymentID: optional(data.PaymentID.UUID()),
		OrderID: optional(data.OrderID.UUID()), SubscriptionID: optional(data.SubscriptionID.UUID()),
	})
	if err != nil {
		return err
	}
	if test {
		return end(statusTestEnvironment, "")
	}
	if s.Email == nil {
		return end(statusFailed, errNoSender)
	}
	found, err := s.Contacts.Contacts(ctx, billing.MerchantID(m.MerchantID), []uuid.UUID{m.CustomerID})
	if err != nil {
		return err
	}
	contact := found[m.CustomerID]
	if contact.Email == "" {
		return end(statusNoContact, "")
	}
	to, err := mail.ParseAddress(contact.Email)
	if err != nil || len(to.Address) > maxRecipient {
		return end(statusFailed, errInvalidRecipient)
	}
	profile, err := s.profile(ctx)
	if err != nil {
		return err
	}
	greet := contact.Name
	if greet == "" {
		greet = contact.Username
	}
	content, ok, err := s.Renderer.Render(ctx, Message{
		EventType: m.EventType, Data: data, OccurredAt: *m.OccurredAt, Customer: greet,
		Merchant: Merchant{Name: profile.DisplayName, SupportURL: profile.SupportURL, BillingURL: profile.BillingURL, SubscribeURL: profile.SubscribeURL},
	})
	if err != nil || !ok {
		return end(statusFailed, errRenderFailed)
	}
	claimed, err := s.claim(ctx, m, to.Address)
	if err != nil || !claimed {
		return err
	}
	attempts := m.Attempts + 1
	sendErr := s.Email.Send(ctx, config.Email{
		From: config.EmailAddress{Name: profile.DisplayName, Address: profile.FromEmail}, To: to.Address,
		Subject: content.Subject, Text: content.Text, HTML: content.HTML,
	})
	now := s.Clock.Now().UTC()
	switch {
	case sendErr == nil:
		return s.settle(ctx, m, settled{status: statusSent, attempts: attempts, sentAt: &now})
	case permanent(sendErr):
		return s.settle(ctx, m, settled{status: statusFailed, attempts: attempts, code: errRefused})
	case last:
		if err := s.settle(ctx, m, settled{status: statusFailed, attempts: attempts, code: errSendFailed}); err != nil {
			return errors.Join(sendErr, err)
		}
	default:
		if err := s.settle(ctx, m, settled{status: statusQueued, attempts: attempts, code: errSendFailed}); err != nil {
			return errors.Join(sendErr, err)
		}
	}
	return sendError{sendErr}
}

// claim marks the message sending to recipient unless the customer was sent
// RateCap messages in the last RateWindow; then it is rate_limited, and the
// merchant has a finding. False when it does not send.
func (s *Service) claim(ctx context.Context, m gen.GetCustomerMessageRow, recipient string) (bool, error) {
	now := s.Clock.Now().UTC()
	claimed := false
	err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := q.LockCustomerMessages(ctx, gen.LockCustomerMessagesParams{MerchantID: m.MerchantID, CustomerID: m.CustomerID}); err != nil {
			return err
		}
		sent, err := q.CountCustomerMessagesSince(ctx, gen.CountCustomerMessagesSinceParams{
			MerchantID: m.MerchantID, CustomerID: m.CustomerID, ID: m.ID, Since: now.Add(-RateWindow),
		})
		if err != nil {
			return err
		}
		if sent >= RateCap {
			n, err := q.SettleCustomerMessage(ctx, settled{status: statusRateLimited, attempts: m.Attempts}.params(m, now))
			if err != nil || n == 0 {
				return err
			}
			return alerting.RecordMessageRateLimit(ctx, s.DB.NewWithPgxTx(tx), now, alerting.MessageRateLimit{
				CustomerID: m.CustomerID, MessageID: m.ID, EventType: m.EventType, Cap: RateCap, Window: RateWindow,
			})
		}
		n, err := q.SettleCustomerMessage(ctx, settled{status: statusSending, attempts: m.Attempts + 1, recipient: recipient}.params(m, now))
		claimed = n == 1
		return err
	})
	return claimed, err
}

// settled is where a message moves.
type settled struct {
	status    string
	attempts  int32
	code      string
	recipient string
	sentAt    *time.Time
}

func (v settled) params(m gen.GetCustomerMessageRow, now time.Time) gen.SettleCustomerMessageParams {
	p := gen.SettleCustomerMessageParams{Status: v.status, Attempts: v.attempts, SentAt: v.sentAt, Now: now, MerchantID: m.MerchantID, ID: m.ID}
	if v.code != "" {
		p.ErrorCode = &v.code
	}
	if v.recipient != "" {
		p.Recipient = &v.recipient
	}
	return p
}

func (s *Service) settle(ctx context.Context, m gen.GetCustomerMessageRow, v settled) error {
	_, err := s.DB.Gen(ctx).SettleCustomerMessage(ctx, v.params(m, s.Clock.Now().UTC()))
	return err
}

// profile is the merchant's profile: its name, links and from address.
func (s *Service) profile(ctx context.Context) (models.MerchantProfileConfiguration, error) {
	cfg, _, err := s.Profiles.Get(ctx)
	if errors.Is(err, merchantconfig.ErrNoSettings) {
		return models.MerchantProfileConfiguration{}, nil
	}
	return cfg.Profile, err
}

// permanent is a refusal another attempt cannot change: an SMTP 5xx reply.
func permanent(err error) bool {
	var reply *textproto.Error
	return errors.As(err, &reply) && reply.Code >= 500
}

func optional(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
