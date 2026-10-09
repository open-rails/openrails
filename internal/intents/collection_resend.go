package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// Stripe engine charges may retry the original idempotency key after a settle
// delay and within its documented retention window. NMI submissions only read
// receipts: empty Query API results do not prove nonexecution. ClockMargin
// widens read windows for clock skew between replicas and the gateway.
const (
	LostSubmissionSettle     = 5 * time.Minute
	MaxLostSubmissionResends = 2
	ClockMargin              = 2 * time.Minute
	resendArmedKey           = "resend_armed"
	duplicateRefusedKey      = "duplicate_refused_at"
	// Stripe may prune idempotency keys after 24 hours. Include the existing
	// clock margin and provider-call hold rather than start a resend at expiry.
	stripeIdempotencyRetention = 24 * time.Hour
)

func resentKey(attempt int) string { return fmt.Sprintf("resent_%d_at", attempt) }

// SubmissionHistory is the operation's submission fences: the original and
// each resend, with the time of the latest.
type SubmissionHistory struct {
	Resends int
	Armed   int
	First   time.Time
	Latest  time.Time
}

func LoadSubmissionHistory(in gen.BillingProviderIntent) (SubmissionHistory, error) {
	raw := EvidenceString(in, "submitted_at")
	if raw == "" {
		return SubmissionHistory{}, errors.New("operation has no submission fence")
	}
	latest, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return SubmissionHistory{}, fmt.Errorf("submission fence is unreadable: %w", err)
	}
	h := SubmissionHistory{First: latest, Latest: latest, Armed: evidenceInt(in, resendArmedKey)}
	for n := 1; ; n++ {
		raw := EvidenceString(in, resentKey(n))
		if raw == "" {
			break
		}
		at, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return SubmissionHistory{}, fmt.Errorf("resend fence is unreadable: %w", err)
		}
		h.Resends, h.Latest = n, at
	}
	return h, nil
}

// Settled reports whether an unresolved submission has passed the observation
// delay. It does not make absent provider records proof of nonexecution.
func (h SubmissionHistory) Settled(now time.Time) bool {
	return !now.Before(h.Latest.Add(LostSubmissionSettle))
}

// StripeReplaySafe bounds retransmission, not receipt reads. A request whose
// original key may have expired remains unresolved instead of creating another
// PaymentIntent from an empty provider lookup.
func (h SubmissionHistory) StripeReplaySafe(now time.Time) bool {
	return !h.First.IsZero() && !now.Before(h.First) && now.Before(h.First.Add(stripeIdempotencyRetention-ClockMargin-ProviderCallHold))
}

// RecordDuplicateRefusal marks that NMI refused the charge as a duplicate of
// a recent one. Such an operation is never resent.
func (s *Store) RecordDuplicateRefusal(ctx context.Context, in gen.BillingProviderIntent, at time.Time) error {
	_, err := s.RecordProgressIfAbsent(ctx, in.ID, duplicateRefusedKey, at.UTC().Format(time.RFC3339Nano))
	return err
}

// DuplicateRefusedAt is when NMI refused the operation as a duplicate.
func DuplicateRefusedAt(in gen.BillingProviderIntent) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339Nano, EvidenceString(in, duplicateRefusedKey))
	return at, err == nil
}

// ArmLostSubmissionResend arms one Stripe replay of the original idempotency
// key after its retention and observation checks.
func (s *Store) ArmLostSubmissionResend(ctx context.Context, in gen.BillingProviderIntent, attempt int) error {
	if in.Rail != "stripe" {
		return errors.New("only Stripe renewals support idempotent resubmission")
	}
	if attempt < 1 || attempt > MaxLostSubmissionResends {
		return errors.New("resend attempt is outside the cap")
	}
	n, err := s.db.Gen(ctx).ArmProviderIntentResend(ctx, gen.ArmProviderIntentResendParams{ID: in.ID, MerchantID: in.MerchantID, Attempt: int32(attempt)})
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("resend arming rejected a changed or completed operation")
	}
	return nil
}

// BeginLostSubmissionResend is the fence for one armed resend. Only its
// writer may send; a crash after it counts toward the cap. The proof it mints
// stays bound to the operation's original fence.
func (s *Store) BeginLostSubmissionResend(ctx context.Context, in gen.BillingProviderIntent, attempt int, now time.Time) (CollectionNonexecutionProof, bool, error) {
	if in.Rail != "stripe" || in.IntentType != subscriptions.TypeSubscriptionCollection {
		return CollectionNonexecutionProof{}, false, errors.New("only Stripe engine collections resend lost submissions")
	}
	if attempt < 1 || attempt > MaxLostSubmissionResends || evidenceInt(in, resendArmedKey) != attempt {
		return CollectionNonexecutionProof{}, false, errors.New("resend is not armed")
	}
	binding, err := collectionBinding(in)
	if err != nil {
		return CollectionNonexecutionProof{}, false, err
	}
	first, err := s.RecordProgressIfAbsent(ctx, in.ID, resentKey(attempt), now.UTC().Format(time.RFC3339Nano))
	if err != nil || !first {
		return CollectionNonexecutionProof{}, false, err
	}
	return CollectionNonexecutionProof{binding: binding, submittedAt: EvidenceString(in, "submitted_at")}, true, nil
}

// ReadNMIOrderAttempts reads this attempt's transactions under the
// obligation's shared order from its accepted account.
func ReadNMIOrderAttempts(ctx context.Context, in gen.BillingProviderIntent, resolver NMIClientResolver) (nmi.OrderAttempts, error) {
	p, err := decodeCollectedTerms(in)
	if err != nil {
		return nmi.OrderAttempts{}, err
	}
	client, err := resolveReceiptNMIClient(ctx, resolver, in)
	if err != nil {
		return nmi.OrderAttempts{}, err
	}
	accepted := nmi.SaleParams{
		OrderID: p.OrderReference, OrderDescription: subscriptions.SubscriptionCollectionDescription(in.ID),
		Amount: p.AmountMinor, Currency: p.Currency,
	}
	if p.Instrument.CustodianHeld() {
		return client.ReadCustodianOrderAttempts(ctx, accepted)
	}
	accepted.CustomerVaultID, accepted.BillingID = p.Instrument.RailCustomerRef, p.Instrument.RailMethodRef
	return client.ReadRecurringOrderAttempts(ctx, accepted)
}

// ReadNMIVaultTransactions reads every transaction on the operation's frozen
// vault since since, of any order and outcome.
func ReadNMIVaultTransactions(ctx context.Context, in gen.BillingProviderIntent, resolver NMIClientResolver, since time.Time) ([]nmi.VaultTransaction, error) {
	p, err := decodeCollectedTerms(in)
	if err != nil {
		return nil, err
	}
	if p.Instrument.CustodianHeld() || p.Instrument.RailCustomerRef == "" {
		return nil, errors.New("operation has no NMI vault to read")
	}
	client, err := resolveReceiptNMIClient(ctx, resolver, in)
	if err != nil {
		return nil, err
	}
	return client.ReadVaultTransactions(ctx, p.Instrument.RailCustomerRef, since)
}

func evidenceInt(in gen.BillingProviderIntent, key string) int {
	var evidence map[string]json.RawMessage
	if json.Unmarshal(in.ResultEvidence, &evidence) != nil {
		return 0
	}
	var value int
	_ = json.Unmarshal(evidence[key], &value)
	return value
}
