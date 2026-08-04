package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
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
// notified; nothing is terminally cancelled and nothing rail-side is deleted.
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
	svc := &basisTheoryWebhookService{d: d, accountID: msg.PspID}
	return d.DeduplicationService.ProcessWebhook(ctx, evt.ID, evt.Type, models.EventSourceBasisTheory, msg.Payload, func(ctx context.Context) error {
		return svc.apply(ctx, evt)
	})
}

type basisTheoryWebhookService struct {
	d         *WebhookDispatcher
	accountID string
}

// btClient arms the merchant's BT client from the resolved PSP state (#788).
// accountID is the routed PSP's rail account id (or#879): custody hangs off the
// NMI PSP whose gateway the proxy charges land on.
func (s *basisTheoryWebhookService) btClient(ctx context.Context) (*basistheory.Client, error) {
	if s.d.RailConfigs == nil {
		return nil, fmt.Errorf("basistheory webhook rejected: rail resolution is not configured")
	}
	rc, err := s.d.RailConfigs.RailConfig(ctx, string(models.RailNMI), s.accountID)
	if err != nil {
		return nil, err
	}
	if rc.Custody == nil || rc.Custody.Custodian != models.CustodianBasisTheory {
		return nil, fmt.Errorf("psp %s declares no basis_theory custodian", s.accountID)
	}
	return basistheory.New(basistheory.Config{
		APIKey:        rc.Custody.APIKey,
		BaseURL:       rc.Custody.APIBaseURL,
		WebhookKeyURL: rc.Custody.WebhookKeyURL,
		ReadOnly:      s.d.Config != nil && s.d.Config.IsProviderReadOnly(),
	})
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
	var data basistheory.TokenEventData
	if err := json.Unmarshal(evt.Data, &data); err != nil || strings.TrimSpace(data.Token.ID) == "" {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("basistheory %s event carries no token id", evt.Type))
	}
	reason := "bt_" + strings.ReplaceAll(evt.Type, ".", "_") // bt_token_deleted | bt_token_expired
	rows, err := s.gen(ctx).ParkPaymentMethodByMethodRef(ctx, gen.ParkPaymentMethodByMethodRefParams{
		Custodian:     models.CustodianBasisTheory,
		RailMethodRef: data.Token.ID,
		ParkReason:    reason,
	})
	if err != nil {
		return fmt.Errorf("park custodian-held instrument for %s: %w", evt.Type, err)
	}
	if rows > 0 {
		// Operator-visible: a parked instrument means renewals on it will fail
		// loudly until re-collection (#657 cutover) or vault repair.
		log.WithContext(ctx).Error("basistheory webhook: instrument PARKED — custodian token gone; operator action required (never auto-cancelled)")
	}
	return nil
}

// refreshInstrumentFromToken re-reads the token and refreshes masked metadata.
func (s *basisTheoryWebhookService) refreshInstrumentFromToken(ctx context.Context, evt basistheory.Event) error {
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
	_, err = s.gen(ctx).RefreshCustodianCardMetadata(ctx, gen.RefreshCustodianCardMetadataParams{
		Custodian:     models.CustodianBasisTheory,
		RailMethodRef: token.ID,
		LastFour:      cardLast4(token.Card),
		CardType:      cardBrand(token.Card),
		ExpiryDate:    cardExpiry(token.Card),
		Fingerprint:   token.Fingerprint,
	})
	if err != nil {
		return fmt.Errorf("basistheory token.updated: refresh instrument: %w", err)
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
	_, err := s.gen(ctx).GetPaymentMethodByRailMethodRef(ctx, gen.GetPaymentMethodByRailMethodRefParams{
		Rail:          string(models.RailNMI),
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

// foldNetworkTokenStatus updates NT status/enrichment ONLY — the PAN-side
// expiry is deliberately never touched (BT never rewrites the card token's
// FPAN; that is the Account Updater's job).
func (s *basisTheoryWebhookService) foldNetworkTokenStatus(ctx context.Context, evt basistheory.Event, forcedStatus string) error {
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
	if _, err := s.gen(ctx).SetNetworkTokenStatusByNetworkTokenID(ctx, gen.SetNetworkTokenStatusByNetworkTokenIDParams{
		Custodian:          models.CustodianBasisTheory,
		NetworkTokenID:     data.NetworkToken.ID,
		NetworkTokenStatus: status,
	}); err != nil {
		return fmt.Errorf("basistheory %s: fold network token status: %w", evt.Type, err)
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
	return s.FoldAccountUpdaterRows(ctx, rows)
}

// FoldAccountUpdaterRows applies parsed AU result rows. Exported for the
// integration test to drive the fold without a live BT job.
func (s *basisTheoryWebhookService) FoldAccountUpdaterRows(ctx context.Context, rows []basistheory.AccountUpdaterResultRow) error {
	q := s.gen(ctx)
	for _, row := range rows {
		switch row.ResultCode {
		case basistheory.AUUpdatedPAN, basistheory.AUUpdatedExpDate:
			newRef := strings.TrimSpace(row.NewToken)
			if newRef == "" {
				// In-place update (dedup): metadata refresh only, same token id.
				newRef = row.Token
			}
			n, err := q.RotateCustodianMethodRef(ctx, gen.RotateCustodianMethodRefParams{
				Custodian:      models.CustodianBasisTheory,
				OldMethodRef:   row.Token,
				NewMethodRef:   newRef,
				NewFingerprint: row.NewFingerprint,
				NewLastFour:    row.NewLast4,
				NewCardType:    row.NewBrand,
				NewExpiryDate:  auExpiry(row.NewExpirationMonth, row.NewExpirationYear),
			})
			if err != nil {
				return fmt.Errorf("account updater: rotate %s -> %s: %w", row.Token, newRef, err)
			}
			if n > 0 && newRef != row.Token {
				log.WithContext(ctx).WithFields(log.Fields{
					"old_bt_token_id": row.Token, "new_bt_token_id": newRef, "result_code": row.ResultCode,
				}).Info("basistheory account updater: instrument rail_method_ref rotated")
			}
		case basistheory.AUClosedAccount:
			n, err := q.ParkPaymentMethodByMethodRef(ctx, gen.ParkPaymentMethodByMethodRefParams{
				Custodian:     models.CustodianBasisTheory,
				RailMethodRef: row.Token,
				ParkReason:    "bt_au_closed_account",
			})
			if err != nil {
				return fmt.Errorf("account updater: park %s: %w", row.Token, err)
			}
			if n > 0 {
				log.WithContext(ctx).WithFields(log.Fields{
					"bt_token_id": row.Token,
				}).Error("basistheory account updater: instrument PARKED — closed account; operator action required (never auto-cancelled)")
			}
		case basistheory.AUNoUpdate, "":
			// Recorded verbatim by the vault; nothing to fold.
		default:
			log.WithContext(ctx).WithFields(log.Fields{
				"bt_token_id": row.Token, "result_code": row.ResultCode,
			}).Warn("basistheory account updater: unrecognized result code recorded verbatim; no fold")
		}
	}
	return nil
}

func cardLast4(c *basistheory.CardDetails) string {
	if c == nil {
		return ""
	}
	return c.Last4
}

func cardBrand(c *basistheory.CardDetails) string {
	if c == nil {
		return ""
	}
	return c.Brand
}

func cardExpiry(c *basistheory.CardDetails) string {
	if c == nil || c.ExpirationMonth == 0 || c.ExpirationYear == 0 {
		return ""
	}
	return fmt.Sprintf("%02d/%02d", c.ExpirationMonth, c.ExpirationYear%100)
}

// auExpiry renders the AU CSV month/year strings as the instrument MM/YY shape.
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
	if cfg != nil && cfg.IsTestMode() {
		return basistheory.TestWebhookKeyURL
	}
	return basistheory.ProdWebhookKeyURL
}
