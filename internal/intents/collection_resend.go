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

// A submitted engine charge the provider has no record of is resolved from the
// provider's authoritative reads: after LostSubmissionSettle with no
// transaction under the obligation's order and none at all on the vault since
// the fence, the same operation is sent again under the same order, at most
// MaxLostSubmissionResends times. An inconclusive or contradictory read never
// arms a resend. ClockMargin widens every read window for clock skew between
// replicas and the gateway.
const (
	LostSubmissionSettle     = 5 * time.Minute
	MaxLostSubmissionResends = 2
	ClockMargin              = 2 * time.Minute
	resendArmedKey           = "resend_armed"
	duplicateRefusedKey      = "duplicate_refused_at"
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

func LoadSubmissionHistory(in gen.OpenrailsRailIntent) (SubmissionHistory, error) {
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

// Settled reports whether the latest submission is old enough for absence at
// the provider to be its answer rather than lag.
func (h SubmissionHistory) Settled(now time.Time) bool {
	return !now.Before(h.Latest.Add(LostSubmissionSettle))
}

// Window is the start of every provider read for this operation's charge.
func (h SubmissionHistory) Window() time.Time { return h.First.Add(-ClockMargin) }

// DupSeconds is the NMI duplicate-check window a resend sends: from the
// original fence to now, so any charge the original made is refused.
func (h SubmissionHistory) DupSeconds(now time.Time) int {
	return int(now.Sub(h.Window()) / time.Second)
}

// RecordDuplicateRefusal marks that NMI refused the charge as a duplicate of
// a recent one. Such an operation is never resent.
func (s *Store) RecordDuplicateRefusal(ctx context.Context, in gen.OpenrailsRailIntent, at time.Time) error {
	_, err := s.RecordProgressIfAbsent(ctx, in.ID, duplicateRefusedKey, at.UTC().Format(time.RFC3339Nano))
	return err
}

// DuplicateRefusedAt is when NMI refused the operation as a duplicate.
func DuplicateRefusedAt(in gen.OpenrailsRailIntent) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339Nano, EvidenceString(in, duplicateRefusedKey))
	return at, err == nil
}

// ArmLostSubmissionResend records that an authoritative read found nothing
// under the operation after the settle delay. It opens the retry transition
// for exactly the next resend.
func (s *Store) ArmLostSubmissionResend(ctx context.Context, in gen.OpenrailsRailIntent, attempt int) error {
	if attempt < 1 || attempt > MaxLostSubmissionResends {
		return errors.New("resend attempt is outside the cap")
	}
	n, err := s.db.Gen(ctx).ArmRailIntentResend(ctx, gen.ArmRailIntentResendParams{ID: in.ID, MerchantID: in.MerchantID, Attempt: int32(attempt)})
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
func (s *Store) BeginLostSubmissionResend(ctx context.Context, in gen.OpenrailsRailIntent, attempt int, now time.Time) (CollectionNonexecutionProof, bool, error) {
	if in.IntentType != subscriptions.TypeSubscriptionCollection {
		return CollectionNonexecutionProof{}, false, errors.New("only engine collections resend lost submissions")
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
func ReadNMIOrderAttempts(ctx context.Context, in gen.OpenrailsRailIntent, resolver NMIClientResolver) (nmi.OrderAttempts, error) {
	history, err := LoadSubmissionHistory(in)
	if err != nil {
		return nmi.OrderAttempts{}, err
	}
	p, err := decodeCollectedTerms(in)
	if err != nil {
		return nmi.OrderAttempts{}, err
	}
	client, err := resolveReceiptNMIClient(ctx, resolver, in)
	if err != nil {
		return nmi.OrderAttempts{}, err
	}
	return client.ReadOrderAttemptsSince(ctx, p.OrderReference, history.Window())
}

// ReadNMIVaultTransactions reads every transaction on the operation's frozen
// vault since since, of any order and outcome.
func ReadNMIVaultTransactions(ctx context.Context, in gen.OpenrailsRailIntent, resolver NMIClientResolver, since time.Time) ([]nmi.VaultTransaction, error) {
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

func evidenceInt(in gen.OpenrailsRailIntent, key string) int {
	var evidence map[string]json.RawMessage
	if json.Unmarshal(in.ResultEvidence, &evidence) != nil {
		return 0
	}
	var value int
	_ = json.Unmarshal(evidence[key], &value)
	return value
}
