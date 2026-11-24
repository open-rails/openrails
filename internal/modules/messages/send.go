package messages

import (
	"context"
	"encoding/json"
	"errors"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/logx"
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
	settings, err := s.Profiles.Messages(ctx)
	if err != nil {
		return err
	}
	var data billing.NotificationData
	unreadable := m.Data != nil && json.Unmarshal(m.Data, &data) != nil
	if !Enabled(settings, m.EventType, data) {
		return end(statusDisabled, "")
	}
	if m.Data == nil || m.OccurredAt == nil {
		return end(statusFailed, errEventMissing)
	}
	if unreadable {
		return end(statusFailed, errEventUnreadable)
	}
	test, err := s.DB.Gen(ctx).CustomerMessageOnTestPSP(ctx, gen.CustomerMessageOnTestPSPParams{
		MerchantID: m.MerchantID, PaymentID: optional(data.PaymentID.UUID()),
		OrderID: optional(data.OrderID.UUID()), SubscriptionID: optional(data.SubscriptionID.UUID()), PspID: optional(data.PSPID.UUID()),
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
	units, err := s.Profiles.Registry(ctx)
	if err != nil {
		return err
	}
	greet := contact.Name
	if greet == "" {
		greet = contact.Username
	}
	product, err := s.productName(ctx, m.MerchantID, data)
	if err != nil {
		return err
	}
	content, err := s.render(ctx, settings, messageData(fact{
		EventType: m.EventType, Data: data, OccurredAt: *m.OccurredAt, CustomerID: m.CustomerID,
		Customer: greet, ProductName: product, Profile: profile, Units: units,
	}))
	var retry hostError
	switch {
	case errors.As(err, &retry) && !last:
		return err
	case err != nil:
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
	refusal, reply := refusalOf(sendErr)
	var settleErr error
	switch {
	case sendErr == nil:
		if err := s.settle(ctx, m, settled{status: statusSent, attempts: attempts, sentAt: &now}); err != nil {
			return err
		}
		s.senderAccepted(ctx, profile.FromEmail)
		return nil
	case refusal == recipientRefused:
		return s.settle(ctx, m, settled{status: statusFailed, attempts: attempts, code: errRefused})
	case last:
		settleErr = s.settle(ctx, m, settled{status: statusFailed, attempts: attempts, code: errSendFailed})
	default:
		settleErr = s.settle(ctx, m, settled{status: statusQueued, attempts: attempts, code: errSendFailed})
	}
	if refusal == senderRefused {
		settleErr = errors.Join(settleErr, s.senderRefused(ctx, m, profile.FromEmail, sendErr, reply, now))
	}
	if settleErr != nil {
		return errors.Join(sendErr, settleErr)
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

// settleTimeout bounds a settle. It runs whatever the job's context says, so
// a message the sender took, or its last attempt's failure, is recorded
// after a timeout too.
const settleTimeout = 10 * time.Second

func (s *Service) settle(ctx context.Context, m gen.GetCustomerMessageRow, v settled) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	_, err := s.DB.Gen(ctx).SettleCustomerMessage(ctx, v.params(m, s.Clock.Now().UTC()))
	return err
}

// senderAccepted resolves the merchant's sender-refused finding: the sender
// took a message from from. A failure waits for the next message.
func (s *Service) senderAccepted(ctx context.Context, from string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	if err := alerting.ResolveMessageSenderRefused(ctx, s.DB, from); err != nil {
		s.Log.WarnContext(ctx, "customer messages: the sender-refused finding was not resolved", logx.Err(err))
	}
}

// senderRefused raises the merchant's finding for a mail server that refused
// its sender; the message is retried.
func (s *Service) senderRefused(ctx context.Context, m gen.GetCustomerMessageRow, from string, err error, reply *textproto.Error, now time.Time) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	step := "MAIL FROM"
	if strings.Contains(err.Error(), "AUTH: ") {
		step = "AUTH"
	}
	return alerting.RecordMessageSenderRefused(ctx, s.DB, now, alerting.MessageSenderRefusal{
		From: from, Step: step, Code: reply.Code, Reply: reply.Msg, MessageID: m.ID, EventType: m.EventType,
	})
}

// hostError is a failure of the host's renderer: retried like a send.
type hostError struct{ error }

// render renders data's message: the host's renderer's when it takes it, else
// the merchant's templates over the built-in ones. Contacts carry no
// language, so it is written in the merchant's default.
func (s *Service) render(ctx context.Context, settings billing.MessageSettings, data billing.MessageData) (billing.MessageContent, error) {
	if s.Renderer != nil {
		data.Language = languages(settings, "")[0]
		content, ok, err := s.Renderer(ctx, data)
		if err != nil {
			return content, hostError{err}
		}
		if ok {
			if strings.TrimSpace(content.Subject) == "" || strings.TrimSpace(content.Text) == "" && strings.TrimSpace(content.HTML) == "" {
				return content, errors.New("the host's renderer returned no subject or body")
			}
			return content, nil
		}
	}
	return Render(settings, "", data)
}

// productName is what a subscription fact is about when it names nothing:
// its product's display name, else its key.
func (s *Service) productName(ctx context.Context, merchantID uuid.UUID, data billing.NotificationData) (string, error) {
	if data.ProductName != "" {
		return "", nil
	}
	q := s.DB.Gen(ctx)
	product := data.ProductID.UUID()
	if product == uuid.Nil && !data.SubscriptionID.IsZero() {
		sub, err := q.GetSubscriptionByID(ctx, gen.GetSubscriptionByIDParams{MerchantID: merchantID, ID: data.SubscriptionID.UUID()})
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		product = sub.ProductID
	}
	if product == uuid.Nil {
		return "", nil
	}
	p, err := q.GetProductByID(ctx, gen.GetProductByIDParams{MerchantID: merchantID, ID: product})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if name := strings.TrimSpace(p.DisplayName); name != "" {
		return name, nil
	}
	return p.Key, nil
}

// profile is the merchant's profile: its name, links and from address.
func (s *Service) profile(ctx context.Context) (models.MerchantProfileConfiguration, error) {
	cfg, _, err := s.Profiles.Get(ctx)
	if errors.Is(err, merchantconfig.ErrNoSettings) {
		return models.MerchantProfileConfiguration{}, nil
	}
	return cfg.Profile, err
}

// refusal is what a sender's error says of the message.
type refusal int

const (
	// retryable: another attempt may send it.
	retryable refusal = iota
	// recipientRefused: the mail server refused the recipient for good.
	recipientRefused
	// senderRefused: the mail server refused the sender (AUTH or MAIL
	// FROM); another attempt may send it once the sender is fixed.
	senderRefused
)

// refusalOf reads a sender's error. helpers/smtp names the step a reply
// answered ("smtp: RCPT TO: 550 …") and exposes nothing else, so the step is
// read from its text: only a 5xx to RCPT TO refuses this recipient for good.
func refusalOf(err error) (refusal, *textproto.Error) {
	var reply *textproto.Error
	if !errors.As(err, &reply) || reply.Code < 500 || reply.Code > 599 {
		return retryable, nil
	}
	switch text := err.Error(); {
	case strings.Contains(text, "RCPT TO: "):
		return recipientRefused, reply
	case strings.Contains(text, "MAIL FROM: "), strings.Contains(text, "AUTH: "):
		return senderRefused, reply
	}
	return retryable, reply
}

func optional(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
