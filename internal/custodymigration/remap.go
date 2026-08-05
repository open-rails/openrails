package custodymigration

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// remap is the custody flip for ONE instrument.
//
// Everything happens in a single merchant-scoped transaction holding the
// instrument's row lock, so no concurrent charge site can read the instrument
// half-moved. Inside the lock the decision is re-made — a charge that went
// in-flight between the plan read and this moment must still refuse.
//
// The flip itself is a compare-and-swap on the CURRENT custodian: two runs of
// the same manifest cannot both apply.
func (p *planner) remap(ctx context.Context, tk ImportedToken, existing *gen.OpenrailsPaymentMethod, out RowResult) (RowResult, error) {
	token := strings.TrimSpace(tk.Token)

	// The plan leg stops here: it has performed every read the apply leg makes
	// its decision from, EXCEPT the in-flight check, which is the one thing
	// that can change under it. Run it too, so the plan's counts are honest.
	blocked, err := p.chargeInFlight(ctx, existing.ID)
	if err != nil {
		return out, err
	}
	if blocked {
		out.Outcome, out.Reason = OutcomeBlocked, ReasonChargeInFlight
		return out, nil
	}
	if !p.apply {
		out.Outcome = OutcomeRemapped
		return out, nil
	}

	err = p.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		locked, lerr := q.LockPaymentMethodForCustodyRemap(ctx, gen.LockPaymentMethodForCustodyRemapParams{
			MerchantID: p.merchantID.UUID(), ID: existing.ID,
		})
		if lerr != nil {
			return fmt.Errorf("lock instrument %s: %w", existing.ID, lerr)
		}
		fromPSP := locked.PspID
		// Re-decide under the lock. A concurrent run may have moved it; a
		// dunning attempt may have gone in flight since the plan read.
		if locked.Custodian == p.custodianKind() {
			if locked.RailMethodRef == token {
				out.Outcome = OutcomeAlreadyMigrated
				return nil
			}
			out.Outcome, out.Reason = OutcomeBlocked, ReasonCustodyConflict
			return nil
		}
		n, cerr := q.CountInFlightChargeIntentsForPaymentMethod(ctx, gen.CountInFlightChargeIntentsForPaymentMethodParams{
			MerchantID: p.merchantID.UUID(), PaymentMethodID: existing.ID,
		})
		if cerr != nil {
			return fmt.Errorf("count in-flight charge intents for %s: %w", existing.ID, cerr)
		}
		if n > 0 {
			out.Outcome, out.Reason = OutcomeBlocked, ReasonChargeInFlight
			return nil
		}

		rows, uerr := q.RemapPaymentMethodCustody(ctx, gen.RemapPaymentMethodCustodyParams{
			MerchantID:         p.merchantID.UUID(),
			ID:                 existing.ID,
			FromCustodian:      locked.Custodian,
			ToCustodian:        p.custodianKind(),
			ToRailMethodRef:    token,
			Fingerprint:        strings.TrimSpace(tk.Fingerprint),
			ChargeVia:          chargeViaFor(tk),
			NetworkTokenID:     strings.TrimSpace(tk.NetworkTokenID),
			NetworkTokenStatus: strings.TrimSpace(tk.NetworkTokenStatus),
			NetworkTokenPar:    strings.TrimSpace(tk.NetworkTokenPAR),
			ToPspID:            p.targetPSPID(),
			LastFour:           strings.TrimSpace(tk.LastFour),
			CardType:           strings.TrimSpace(tk.CardType),
			ExpiryDate:         strings.TrimSpace(tk.ExpiryDate),
		})
		if uerr != nil {
			return fmt.Errorf("remap instrument %s: %w", existing.ID, uerr)
		}
		if rows == 0 {
			// The CAS lost: something changed custody between the lock read and
			// the update, which the lock makes impossible — treat as a refusal
			// rather than assume.
			out.Outcome, out.Reason = OutcomeBlocked, ReasonCustodyConflict
			return nil
		}

		if _, rerr := q.RecordCustodyMigration(ctx, gen.RecordCustodyMigrationParams{
			MerchantID:          p.merchantID.UUID(),
			BatchID:             p.batchID,
			PaymentMethodID:     existing.ID,
			Rail:                locked.Rail,
			FromCustodian:       locked.Custodian,
			FromCustodianID:     nil,
			FromRailCustomerRef: locked.RailCustomerRef,
			FromRailMethodRef:   locked.RailMethodRef,
			FromPspID:           &fromPSP,
			ToCustodian:         p.custodianKind(),
			ToCustodianID:       p.custodian.ID,
			ToRailMethodRef:     token,
			ToPspID:             p.targetPSPID(),
			ExportedAt:          &p.exportedAt,
			Outcome:             string(OutcomeRemapped),
			Reason:              "",
		}); rerr != nil {
			return fmt.Errorf("record custody migration for %s: %w", existing.ID, rerr)
		}
		out.Outcome = OutcomeRemapped
		return nil
	})
	if err != nil {
		return out, err
	}
	if out.Outcome == OutcomeRemapped {
		log.WithContext(ctx).WithFields(log.Fields{
			"payment_method_id":  existing.ID,
			"from_custodian":     existing.Custodian,
			"to_custodian":       p.custodianKind(),
			"from_vault_handle":  existing.RailCustomerRef,
			"custodian_token":    token,
			"batch_id":           p.batchID,
			"target_psp":         p.targetPSPID(),
			"subscriptions_move": false,
		}).Info("or#297: instrument custody remapped; subscriptions unchanged (same payment_method_id)")
	}
	return out, nil
}

// create mints a custodian-held instrument for a card the export carried and
// our book has no row for. It is the IMPORT half: the original vault handle is
// still recorded on the row (rail_customer_ref) and in the migration record, so
// a card that arrives this way is as auditable as one that was remapped.
func (p *planner) create(ctx context.Context, tk ImportedToken, out RowResult) (RowResult, error) {
	if !p.apply {
		out.Outcome = OutcomeCreated
		return out, nil
	}
	token := strings.TrimSpace(tk.Token)
	newID := uuid.New()
	err := p.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, cerr := q.CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
			ID:         newID,
			MerchantID: p.merchantID.UUID(),
			CustomerID: *tk.Customer,
			Rail:       p.sourceRail,
			// The card's provenance: it came out of THIS PSP vault entry. The
			// handle is dead as an address the moment the custodian holds the
			// card, and it is the only link back to charges made before.
			RailCustomerRef:    strings.TrimSpace(tk.SourceRailCustomerRef),
			RailMethodRef:      token,
			PspID:              p.targetPSP.ID,
			RebillDriver:       models.RebillDriverOpenRails,
			Custodian:          p.custodianKind(),
			Fingerprint:        strings.TrimSpace(tk.Fingerprint),
			NetworkTokenID:     strings.TrimSpace(tk.NetworkTokenID),
			NetworkTokenStatus: strings.TrimSpace(tk.NetworkTokenStatus),
			NetworkTokenPar:    strings.TrimSpace(tk.NetworkTokenPAR),
			ChargeVia:          chargeViaFor(tk),
			LastFour:           nilIfEmpty(tk.LastFour),
			CardType:           nilIfEmpty(tk.CardType),
			ExpiryDate:         nilIfEmpty(tk.ExpiryDate),
			CreatedAt:          p.exportedAt,
			UpdatedAt:          p.exportedAt,
		}); cerr != nil {
			return fmt.Errorf("create custodian-held instrument for token %s: %w", token, cerr)
		}
		if _, rerr := q.RecordCustodyMigration(ctx, gen.RecordCustodyMigrationParams{
			MerchantID:          p.merchantID.UUID(),
			BatchID:             p.batchID,
			PaymentMethodID:     newID,
			Rail:                p.sourceRail,
			FromCustodian:       models.CustodianPSP,
			FromRailCustomerRef: strings.TrimSpace(tk.SourceRailCustomerRef),
			FromRailMethodRef:   strings.TrimSpace(tk.SourceRailMethodRef),
			FromPspID:           nil,
			ToCustodian:         p.custodianKind(),
			ToCustodianID:       p.custodian.ID,
			ToRailMethodRef:     token,
			ToPspID:             p.targetPSPID(),
			ExportedAt:          &p.exportedAt,
			Outcome:             string(OutcomeCreated),
			Reason:              "",
		}); rerr != nil {
			return fmt.Errorf("record custody migration for new instrument %s: %w", newID, rerr)
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	out.PaymentMethodID = &newID
	out.Outcome = OutcomeCreated
	return out, nil
}

// chargeInFlight answers the refusal predicate: is a charge on this
// instrument's book mid-attempt right now? in_flight = being executed;
// unknown_needs_verify = SENT, outcome unknown. Moving custody underneath
// either one leaves the verifier resolving an attempt whose instrument no
// longer describes how the charge was made.
func (p *planner) chargeInFlight(ctx context.Context, methodID uuid.UUID) (bool, error) {
	var n int64
	err := p.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var cerr error
		n, cerr = p.db.Gen(ctx).CountInFlightChargeIntentsForPaymentMethod(ctx, gen.CountInFlightChargeIntentsForPaymentMethodParams{
			MerchantID: p.merchantID.UUID(), PaymentMethodID: methodID,
		})
		return cerr
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("count in-flight charge intents for %s: %w", methodID, err)
	}
	return n > 0, nil
}
