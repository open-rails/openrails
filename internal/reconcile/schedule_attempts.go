package reconcile

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/billing/decline"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// recordScheduleAttempts records NMI's own scheduled charges of an NMI-owned
// subscription as rebill attempts (#1111): the first charge at or after the
// paid period's end is that cycle's rebill, and each approval moves on to the
// next cycle. Older history has no cycle here. OpenRails' own retries carry
// the same transaction ids and were recorded when they completed.
func recordScheduleAttempts(ctx context.Context, q *gen.Queries, sub *models.Subscription, txns []RemoteTransaction, now time.Time) error {
	if sub.CollectionPolicy != models.CollectionPolicyNMISchedule || sub.CurrentPeriodStartsAt == nil || sub.CurrentPeriodEndsAt == nil || !sub.CurrentPeriodEndsAt.After(*sub.CurrentPeriodStartsAt) {
		return nil
	}
	due := sub.CurrentPeriodEndsAt.UTC()
	period := due.Sub(sub.CurrentPeriodStartsAt.UTC())
	cutoff := due.Add(-AlignmentSlack(sub.CurrentPeriodStartsAt, sub.CurrentPeriodEndsAt))
	var charges []RemoteTransaction
	for _, t := range txns {
		if t.TransactionID != "" && chargeAttempt(t.Type) && !t.OccurredAt.Before(cutoff) {
			charges = append(charges, t)
		}
	}
	sort.SliceStable(charges, func(i, j int) bool { return charges[i].OccurredAt.Before(charges[j].OccurredAt) })
	via := attempts.ObservedFrom(ctx, "pull")
	for _, t := range charges {
		currency, amount := money.NormalizeCurrency(t.Currency), int64(moneyutil.CentsToMicros(moneyutil.Cents(t.AmountCents)))
		if sub.Price != nil {
			if currency == "" {
				currency = money.NormalizeCurrency(sub.Price.Currency)
			}
			if amount == 0 {
				amount = sub.Price.Amount
			}
		}
		if currency == "" {
			continue
		}
		answer := t.Answer
		if answer.Rail == "" {
			answer = decline.Evidence{Code: strings.TrimSpace(t.DeclineCode), Text: t.DeclineReason}
		}
		a := attempts.Attempt{
			MerchantID: sub.MerchantID, CustomerID: sub.CustomerID, PSPID: sub.PspID, Rail: string(sub.Rail),
			Kind: attempts.Rebill, Owner: attempts.OwnerNMISchedule, ProviderSchedule: true, ObservedVia: via,
			Approved: t.Success, Answer: answer,
			TransactionID: t.TransactionID, Amount: amount, Currency: currency, At: t.OccurredAt,
			Cycle: &attempts.Cycle{SubscriptionID: sub.ID, DueAt: due}, PaymentMethodID: sub.PaymentMethodID, TokenType: charge.TokenTypePSPToken,
		}
		if t.Answer.Rail != "" {
			a.EnrichedAt = now // the transaction report's full answer
		}
		if err := attempts.Record(ctx, q, a); err != nil {
			return err
		}
		if t.Success {
			due = due.Add(period)
		}
	}
	return nil
}
