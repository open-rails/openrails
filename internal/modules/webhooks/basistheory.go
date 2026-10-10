package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// BasisTheoryWebhookHandler folds Basis Theory custody events (#795) into
// instrument state. Signature verification (RSA-PSS vs the CDN key) happens at
// HTTP ingestion — like Stripe — so Apply trusts the ingestion-set flag.
//
// The event SOURCE is the custodian, not a rail (or#879): Basis Theory holds
// the card, NMI charges it, and only the custodian emits these events.
//
// Doctrine: custody-side instrument problems PARK the instrument
// (cancellation-last-resort) — charges fail loudly and the operator is
// notified; nothing is terminally canceled and nothing rail-side is deleted.
type BasisTheoryWebhookHandler struct{}

func (BasisTheoryWebhookHandler) Rail() string { return string(models.EventSourceBasisTheory) }

func (h BasisTheoryWebhookHandler) Verify(msg *WebhookMessage) error {
	if msg == nil {
		return fmt.Errorf("nil webhook message")
	}
	if !webhookSignatureVerified(msg) {
		return fmt.Errorf("basistheory webhook signature was not verified at ingestion")
	}
	return nil
}

func (h BasisTheoryWebhookHandler) Normalize(msg *WebhookMessage) (WebhookEvent, error) {
	if msg == nil {
		return WebhookEvent{}, fmt.Errorf("nil webhook message")
	}
	var evt basistheory.Event
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return WebhookEvent{}, fmt.Errorf("parse basistheory webhook: %w", err)
	}
	out := WebhookEvent{
		Rail:    string(models.EventSourceBasisTheory),
		Type:    mapBasisTheoryEventType(evt.Type),
		RawType: evt.Type,
		RailRef: evt.ID,
		Raw:     msg.Payload,
	}
	if evt.DeliveredAt != nil {
		out.OccurredAt = evt.DeliveredAt.UTC()
	}
	return out, nil
}

func mapBasisTheoryEventType(t string) WebhookEventType {
	switch t {
	case basistheory.EventTokenUpdated, basistheory.EventNetworkTokenUpdated,
		basistheory.EventTokenDeleted, basistheory.EventTokenExpired,
		basistheory.EventNetworkTokenDeleted, basistheory.EventAccountUpdaterJobCompleted:
		return WebhookEventCustomerUpdated
	default:
		return WebhookEventUnknown
	}
}

func (h BasisTheoryWebhookHandler) Apply(ctx context.Context, d *WebhookDispatcher, msg *WebhookMessage) error {
	if err := h.Verify(msg); err != nil {
		return MarkWebhookErrorNonRetryable(err)
	}
	var evt basistheory.Event
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("parse basistheory webhook payload: %w", err))
	}
	if strings.TrimSpace(evt.ID) == "" {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("basistheory webhook has no event id"))
	}
	svc := &basisTheoryWebhookService{d: d, accountID: msg.CustodianAccountID}
	return d.DeduplicationService.ProcessWebhook(ctx, evt.ID, evt.Type, models.EventSourceBasisTheory, func(ctx context.Context) error {
		return svc.apply(ctx, evt)
	})
}

type basisTheoryWebhookService struct {
	d         *WebhookDispatcher
	accountID string
}

// btClient arms the merchant's BT client from the resolved CUSTODIAN (or#880).
// accountID is the custodian's own tenant id — the identity the event carries.
// It deliberately does not go through a PSP: one custodian may back several,
// and a token/network-token event is about the instrument, not a gateway.
func (s *basisTheoryWebhookService) btClient(ctx context.Context) (*basistheory.Client, error) {
	if s.d.RailConfigs == nil {
		return nil, fmt.Errorf("basistheory webhook rejected: custodian resolution is not configured")
	}
	cc, err := s.d.RailConfigs.CustodianConfig(ctx, models.CustodianBasisTheory, s.accountID)
	if err != nil {
		return nil, err
	}
	if cc == nil || cc.Custodian != models.CustodianBasisTheory {
		return nil, fmt.Errorf("custodian tenant %s is not declared as basis_theory", s.accountID)
	}
	return basistheory.New(basistheory.Config{
		APIKey:        cc.APIKey,
		BaseURL:       cc.APIBaseURL,
		WebhookKeyURL: cc.WebhookKeyURL,
		ReadOnly:      s.d.Config != nil && config.IsProviderReadOnly(s.d.Config),
	})
}

func custodianScopeIDs(ctx context.Context) (uuid.UUID, uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	cid, err := db.RequireCustodianID(ctx)
	return mid.UUID(), cid, err
}

func (s *basisTheoryWebhookService) now() time.Time {
	if s.d.Clock != nil {
		return s.d.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *basisTheoryWebhookService) gen(ctx context.Context) *gen.Queries {
	return s.d.DB.Gen(ctx)
}

func (s *basisTheoryWebhookService) apply(ctx context.Context, evt basistheory.Event) error {
	switch evt.Type {
	case basistheory.EventTokenDeleted, basistheory.EventTokenExpired:
		return s.parkInstrumentFromTokenEvent(ctx, evt)
	case basistheory.EventTokenUpdated:
		return s.refreshInstrumentFromToken(ctx, evt)
	case basistheory.EventTokenPropertyExpired:
		// Expected post-checkout: the intent/token CVC retention lapse.
		return nil
	case basistheory.EventTokenIntentConverted:
		return s.reconcileIntentConversion(ctx, evt)
	case basistheory.EventNetworkTokenUpdated:
		return s.foldNetworkTokenStatus(ctx, evt, "")
	case basistheory.EventNetworkTokenDeleted:
		return s.foldNetworkTokenStatus(ctx, evt, basistheory.NetworkTokenDeleted)
	case basistheory.EventAccountUpdaterJobCompleted:
		return s.foldAccountUpdaterJob(ctx, evt)
	default:
		log.WithContext(ctx).Debug("basistheory webhook: unhandled event type")
		return nil
	}
}

// parkInstrumentFromTokenEvent: the custody-side credential is GONE (deleted or
// expired). Park — never terminal-cancel, never delete (cancellation-last-
// resort): subscriptions keep their access posture and the operator decides.
func (s *basisTheoryWebhookService) parkInstrumentFromTokenEvent(ctx context.Context, evt basistheory.Event) error {
	mid, cid, err := custodianScopeIDs(ctx)
	if err != nil {
		return err
	}
	var data basistheory.TokenEventData
	if err := json.Unmarshal(evt.Data, &data); err != nil || strings.TrimSpace(data.Token.ID) == "" {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("basistheory %s event carries no token id", evt.Type))
	}
	reason := "bt_" + strings.ReplaceAll(evt.Type, ".", "_") // bt_token_deleted | bt_token_expired
	rows, err := s.gen(ctx).ParkPaymentMethodByMethodRef(ctx, gen.ParkPaymentMethodByMethodRefParams{MerchantID: mid, CustodianID: cid,
		Custodian:     models.CustodianBasisTheory,
		RailMethodRef: data.Token.ID,
		ParkReason:    reason,
	})
	if err != nil {
		return fmt.Errorf("park custodian-held instrument for %s: %w", evt.Type, err)
	}
	if len(rows) > 0 {
		// Operator-visible: a parked instrument means renewals on it will fail
		// loudly until re-collection (#657 cutover) or vault repair.
		log.WithContext(ctx).Error("basistheory webhook: instrument PARKED — custodian token gone; operator action required (never auto-canceled)")
	}
	return nil
}

// refreshInstrumentFromToken re-reads the token and applies the card it now
// holds to every method on it.
func (s *basisTheoryWebhookService) refreshInstrumentFromToken(ctx context.Context, evt basistheory.Event) error {
	mid, cid, err := custodianScopeIDs(ctx)
	if err != nil {
		return err
	}
	var data basistheory.TokenEventData
	if err := json.Unmarshal(evt.Data, &data); err != nil || strings.TrimSpace(data.Token.ID) == "" {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("basistheory token.updated event carries no token id"))
	}
	client, err := s.btClient(ctx)
	if err != nil {
		return err
	}
	token, err := client.GetToken(ctx, data.Token.ID)
	if err != nil {
		if basistheory.IsNotFound(err) {
			// Updated-then-deleted race; the delete event parks it.
			return nil
		}
		return fmt.Errorf("basistheory token.updated: fetch token: %w", err)
	}
	methods, err := s.gen(ctx).ListPaymentMethodsByCustodianRef(ctx, gen.ListPaymentMethodsByCustodianRefParams{MerchantID: mid, CustodianID: cid, Custodian: models.CustodianBasisTheory, RailMethodRef: token.ID})
	if err != nil {
		return fmt.Errorf("basistheory token.updated: find instruments: %w", err)
	}
	card := paymentmethods.Card{Card: btCard(token.Card), Fingerprint: token.Fingerprint}
	for _, id := range methods {
		if _, err := applyCardEvent(ctx, s.d.DB, s.d.SubscriptionLifecycleService, paymentmethods.CardEvent{MerchantID: mid, PaymentMethodID: id,
			Source: paymentmethods.SourceProviderRead, EventRef: evt.ID, Card: card, At: s.now()}); err != nil {
			return fmt.Errorf("basistheory token.updated: %w", err)
		}
	}
	return nil
}

// reconcileIntentConversion is the crash-window signal for B5.5: conversion
// happened at BT but the instrument row may be missing (crash between charge
// approval and the local write). An existing row = no-op; a missing row is
// surfaced loudly for repair.
func (s *basisTheoryWebhookService) reconcileIntentConversion(ctx context.Context, evt basistheory.Event) error {
	var data basistheory.TokenIntentConvertedData
	if err := json.Unmarshal(evt.Data, &data); err != nil || strings.TrimSpace(data.Token.ID) == "" {
		return nil // conversion events without a token id carry nothing to reconcile
	}
	mid, cid, err := custodianScopeIDs(ctx)
	if err != nil {
		return err
	}
	_, err = s.gen(ctx).GetPaymentMethodForCustodianToken(ctx, gen.GetPaymentMethodForCustodianTokenParams{
		MerchantID: mid, CustodianID: cid, Custodian: models.CustodianBasisTheory,
		RailMethodRef: data.Token.ID,
	})
	if err == nil {
		return nil
	}
	if !db.IsNotFound(err) {
		return fmt.Errorf("basistheory token-intent.converted: lookup instrument: %w", err)
	}
	log.WithContext(ctx).Warn("basistheory webhook: converted token has no instrument row (checkout crash window); repair via checkout replay or manual re-link")
	return nil
}

// foldNetworkTokenStatus records a network token's status as a card version;
// the card itself is the Account Updater's.
func (s *basisTheoryWebhookService) foldNetworkTokenStatus(ctx context.Context, evt basistheory.Event, forcedStatus string) error {
	mid, cid, err := custodianScopeIDs(ctx)
	if err != nil {
		return err
	}
	var data basistheory.NetworkTokenEventData
	if err := json.Unmarshal(evt.Data, &data); err != nil || strings.TrimSpace(data.NetworkToken.ID) == "" {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("basistheory %s event carries no network token id", evt.Type))
	}
	status := forcedStatus
	if status == "" {
		status = strings.TrimSpace(data.NetworkToken.Status)
		if status == "" {
			client, err := s.btClient(ctx)
			if err != nil {
				return err
			}
			nt, err := client.GetNetworkToken(ctx, data.NetworkToken.ID)
			if err != nil {
				return fmt.Errorf("basistheory %s: fetch network token: %w", evt.Type, err)
			}
			status = nt.Status
		}
	}
	if status == "" {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("basistheory %s: no status resolvable for network token %s", evt.Type, data.NetworkToken.ID))
	}
	methods, err := s.gen(ctx).ListPaymentMethodsByNetworkToken(ctx, gen.ListPaymentMethodsByNetworkTokenParams{MerchantID: mid, CustodianID: cid,
		Custodian: models.CustodianBasisTheory, NetworkTokenID: data.NetworkToken.ID})
	if err != nil {
		return fmt.Errorf("basistheory %s: find instruments: %w", evt.Type, err)
	}
	for _, id := range methods {
		if _, err := applyCardEvent(ctx, s.d.DB, s.d.SubscriptionLifecycleService, paymentmethods.CardEvent{MerchantID: mid, PaymentMethodID: id,
			Source: paymentmethods.SourceNetworkToken, EventRef: evt.ID, Card: paymentmethods.Card{NetworkTokenStatus: status}, At: s.now()}); err != nil {
			return fmt.Errorf("basistheory %s: fold network token status: %w", evt.Type, err)
		}
	}
	return nil
}

// foldAccountUpdaterJob fetches the completed job's result CSV and folds it:
// UPD_* rows rotate rail_method_ref to new_token + refresh metadata;
// WRN_CLOSED_ACCOUNT parks the instrument. Work scales with the job's rows
// (the request CSV the runner uploaded), never all instruments.
func (s *basisTheoryWebhookService) foldAccountUpdaterJob(ctx context.Context, evt basistheory.Event) error {
	var data basistheory.AccountUpdaterJobEventData
	if err := json.Unmarshal(evt.Data, &data); err != nil {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("basistheory account-updater event unparseable: %w", err))
	}
	jobID := strings.TrimSpace(data.Job.ID)
	if jobID == "" {
		jobID = strings.TrimSpace(data.ID)
	}
	if jobID == "" {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("basistheory account-updater.job.completed carries no job id"))
	}
	client, err := s.btClient(ctx)
	if err != nil {
		return err
	}
	job, err := client.GetAccountUpdaterJob(ctx, jobID)
	if err != nil {
		return fmt.Errorf("basistheory account updater: fetch job %s: %w", jobID, err)
	}
	if strings.TrimSpace(job.DownloadURL) == "" {
		return fmt.Errorf("basistheory account updater job %s completed without a download_url", jobID)
	}
	rows, err := client.DownloadAccountUpdaterResults(ctx, job.DownloadURL)
	if err != nil {
		return fmt.Errorf("basistheory account updater: results for job %s: %w", jobID, err)
	}
	stats, err := FoldAccountUpdaterResults(ctx, s.d.DB, s.d.SubscriptionLifecycleService, jobID, rows, s.now())
	if err != nil {
		return err
	}
	// or#795: the same job may have been submitted by the batch runner, which
	// holds a durable row for it. Whoever ingests first closes it; the other
	// side then finds nothing open and re-submits nothing.
	return CloseAccountUpdaterBatch(ctx, s.gen(ctx), jobID, stats)
}

// FoldAccountUpdaterRows applies parsed AU result rows. Exported for the
// integration test to drive the fold without a live BT job.
func (s *basisTheoryWebhookService) FoldAccountUpdaterRows(ctx context.Context, jobRef string, rows []basistheory.AccountUpdaterResultRow) error {
	_, err := FoldAccountUpdaterResults(ctx, s.d.DB, s.d.SubscriptionLifecycleService, jobRef, rows, s.now())
	return err
}

// AccountUpdaterFoldStats reports what one fold did. ResultCounts holds the
// VERBATIM wire codes (#651) — including ones this build does not recognize —
// and is what the durable batch row records.
type AccountUpdaterFoldStats struct {
	Rows int
	// Adopted: instruments the network refreshed. or#872: this is the number
	// that proves the updater pays for itself — cards recovered before dunning.
	Adopted int
	Rotated int
	// Closed and Contacted: cards the issuer closed, and cards whose issuer
	// asks for the cardholder; both wait on the customer.
	Closed, Contacted int
	ResultCounts      map[string]int
}

// FoldAccountUpdaterResults applies parsed account-updater result rows, one
// card version per instrument and row (#1168): a reissue adopts the custodian's
// new token on the same method; a closed account closes the method; a
// contact-cardholder answer prompts the customer, or closes a Mastercard card.
// The batch runner (or#795) and the account-updater.job.completed webhook
// both land here. Nothing is deleted and nothing is canceled.
func FoldAccountUpdaterResults(ctx context.Context, database *db.DB, life *subscriptions.SubscriptionLifecycleService, jobRef string, rows []basistheory.AccountUpdaterResultRow, now time.Time) (AccountUpdaterFoldStats, error) {
	stats := AccountUpdaterFoldStats{Rows: len(rows), ResultCounts: map[string]int{}}
	mid, cid, err := custodianScopeIDs(ctx)
	if err != nil {
		return stats, err
	}
	for _, row := range rows {
		code := strings.TrimSpace(row.ResultCode)
		stats.ResultCounts[code]++
		ev := paymentmethods.CardEvent{MerchantID: mid, Source: paymentmethods.SourceBasisTheoryUpdater, EventRef: strings.TrimSpace(jobRef) + ":" + row.Token, At: now}
		switch basistheory.ClassifyAccountUpdaterResult(code) {
		case basistheory.AUOutcomeUpdated:
			ev.Card = paymentmethods.Card{Card: models.ParseCard(row.NewBrand, row.NewLast4, auExpiry(row.NewExpirationMonth, row.NewExpirationYear)),
				Fingerprint: row.NewFingerprint, RailMethodRef: row.NewToken}
		case basistheory.AUOutcomeClosed:
			ev.Advice = paymentmethods.AdviceClosed
		case basistheory.AUOutcomeContactCardholder:
			ev.Advice = paymentmethods.AdviceContactCardholder
		case basistheory.AUOutcomeNoChange:
			// Recorded verbatim above; no evidence, no action.
			continue
		default:
			log.WithContext(ctx).WithFields(log.Fields{
				"bt_token_id": row.Token, "result_code": code,
			}).Warn("basistheory account updater: unrecognized result code recorded verbatim; no fold")
			continue
		}
		methods, err := database.Gen(ctx).ListPaymentMethodsByCustodianRef(ctx, gen.ListPaymentMethodsByCustodianRefParams{MerchantID: mid, CustodianID: cid, Custodian: models.CustodianBasisTheory, RailMethodRef: row.Token})
		if err != nil {
			return stats, fmt.Errorf("account updater: find %s: %w", row.Token, err)
		}
		for _, id := range methods {
			ev.PaymentMethodID = id
			applied, err := applyCardEvent(ctx, database, life, ev)
			if err != nil {
				return stats, fmt.Errorf("account updater: %s: %w", row.Token, err)
			}
			switch applied.Change {
			case paymentmethods.CardUpdated, paymentmethods.CardBrandChanged:
				stats.Adopted++
				if newRef := strings.TrimSpace(row.NewToken); newRef != "" && newRef != row.Token {
					stats.Rotated++
				}
			case paymentmethods.CardClosed:
				stats.Closed++
				log.WithContext(ctx).WithFields(log.Fields{"bt_token_id": row.Token, "result_code": code}).
					Warn("basistheory account updater: card closed; the customer is asked for another (never auto-canceled)")
			case paymentmethods.CardContactCardholder:
				stats.Contacted++
			}
		}
	}
	if stats.Adopted > 0 || stats.Closed > 0 || stats.Contacted > 0 {
		log.WithContext(ctx).WithFields(log.Fields{
			"rows": stats.Rows, "adopted": stats.Adopted, "rotated": stats.Rotated, "closed": stats.Closed, "contacted": stats.Contacted,
		}).Info("basistheory account updater: fold complete")
	}
	return stats, nil
}

// applyCardEvent applies one card event in its own transaction and delivers
// the notices it queued once it commits.
func applyCardEvent(ctx context.Context, database *db.DB, life *subscriptions.SubscriptionLifecycleService, ev paymentmethods.CardEvent) (paymentmethods.CardLifecycle, error) {
	if life == nil {
		return paymentmethods.CardLifecycle{}, errors.New("card lifecycle: no lifecycle service wired")
	}
	var applied paymentmethods.CardLifecycle
	var notices []*models.NotificationQueue
	err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		applied, notices, err = life.ApplyCardLifecycle(ctx, database.NewWithPgxTx(tx), ev)
		return err
	})
	if err != nil {
		return applied, err
	}
	life.DispatchNotifications(ctx, notices)
	return applied, nil
}

// CloseAccountUpdaterBatch marks the durable batch (or#795) that carried this
// job as completed and records the verbatim result tally. A job with no local
// batch row — an operator-created job, or one whose row was already closed by
// the other ingestion path — is a no-op, never an error.
func CloseAccountUpdaterBatch(ctx context.Context, q *gen.Queries, jobRef string, stats AccountUpdaterFoldStats) error {
	jobRef = strings.TrimSpace(jobRef)
	if jobRef == "" {
		return nil
	}
	mid, cid, err := custodianScopeIDs(ctx)
	if err != nil {
		return err
	}
	counts, err := json.Marshal(stats.ResultCounts)
	if err != nil {
		return fmt.Errorf("account updater: encode result counts: %w", err)
	}
	if _, err := q.CompleteAccountUpdaterBatchByJobRef(ctx, gen.CompleteAccountUpdaterBatchByJobRefParams{
		MerchantID:   mid,
		CustodianID:  cid,
		JobRef:       jobRef,
		ResultCounts: counts,
		CompletedAt:  time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("account updater: close batch for job %s: %w", jobRef, err)
	}
	return nil
}

// btCard is a Basis Theory token's card as a stored method's display facts.
func btCard(c *basistheory.CardDetails) models.Card {
	if c == nil {
		return models.Card{}
	}
	card := models.ParseCard(c.Brand, c.Last4, "")
	if c.ExpirationMonth >= 1 && c.ExpirationMonth <= 12 && c.ExpirationYear >= 2000 {
		card.ExpMonth, card.ExpYear = c.ExpirationMonth, c.ExpirationYear
	}
	return card
}

// auExpiry renders the AU CSV month/year strings as MM/YY.
func auExpiry(month, year string) string {
	month, year = strings.TrimSpace(month), strings.TrimSpace(year)
	if month == "" || year == "" {
		return ""
	}
	if len(month) == 1 {
		month = "0" + month
	}
	if len(year) == 4 {
		year = year[2:]
	}
	return month + "/" + year
}

// BasisTheoryWebhookKeyURL selects the CDN public key for a deployment posture
// (config-level helper; tests override the URL through the rail settings).
func BasisTheoryWebhookKeyURL(cfg *config.Config) string {
	if cfg != nil && config.IsTestMode(cfg) {
		return basistheory.TestWebhookKeyURL
	}
	return basistheory.ProdWebhookKeyURL
}
