package pspimport

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/reconcile"
)

// ccbillSource reads a CCBill account through DataLink: the ACTIVEMEMBERS
// roster (username, email, signup and rebill dates) and, a day at a time, the
// signups, rebills, cancellations and expiries of the history. A member who
// left is staged from its signup and its end; one whose signup predates the
// history stays unread. CCBill keeps no vault to stage and no catalog to
// read: its terms are the provider price the merchant declares.
type ccbillSource struct {
	dl *ccbill.DataLinkClient
}

func ccbillDate(s string) (time.Time, bool) {
	t, err := ccbill.ParseTime(s)
	return t, err == nil
}

func (s *ccbillSource) roster(ctx context.Context) ([]staged, error) {
	members, err := s.dl.FetchActiveMembers(ctx)
	if err != nil {
		return nil, fmt.Errorf("ccbill active members: %w", err)
	}
	out := make([]staged, 0, len(members))
	for _, m := range members {
		f := facts{}
		f.add(FactSubscriptionRef, m.SubscriptionID)
		f.add(FactUsername, m.Username)
		f.add(FactEmail, m.Email)
		data := subscriptionData{}
		data.Evidence, _ = json.Marshal(map[string]any{"source": "ccbill_psp_import", "record": m})
		if t, ok := ccbillDate(m.Date); ok {
			data.StartedAt = t
		}
		if t, ok := ccbillDate(m.RebillDate); ok {
			data.NextBillingAt = &t
		} else if t, ok := ccbillDate(m.ExpiryDate); ok {
			// Canceled with access left: it ends at its expiry.
			end := ccbill.DayEnd(t)
			data.NextBillingAt, data.Ended, data.EndedAt, data.EndKind = &end, true, &end, string(reconcile.DeclaredCancelUser)
		}
		remote := reconcile.CCBillRemoteSubscription(m)
		remote.Raw = nil
		data.Remote = &remote
		out = append(out, staged{kind: billing.PSPImportSubscription, ref: m.SubscriptionID, occurredAt: data.StartedAt, facts: f, data: data})
	}
	return out, nil
}

// window is one DataLink export's day: a live account answers one an hour,
// so each day commits apart and a pull waiting for its slot resumes there.
func (s *ccbillSource) window() time.Duration { return 24 * time.Hour }

// historyTypes are read together: one request a day whichever are asked.
var historyTypes = []ccbill.DataLinkTxnType{ccbill.DataLinkTxnNew, ccbill.DataLinkTxnRebill, ccbill.DataLinkTxnCancellation, ccbill.DataLinkTxnExpire}

func (s *ccbillSource) history(ctx context.Context, since, until time.Time, _ bool) ([]staged, error) {
	rows, err := s.dl.FetchTransactionExport(ctx, since, until, historyTypes)
	if err != nil {
		return nil, fmt.Errorf("ccbill history: %w", err)
	}
	var out []staged
	for _, row := range rows {
		sub := row.SubscriptionID()
		if sub == "" {
			continue
		}
		switch row.TransactionType {
		case ccbill.DataLinkTxnNew:
			out = append(out, ccbillSignup(row))
		case ccbill.DataLinkTxnCancellation, ccbill.DataLinkTxnExpire:
			if rec, ok := ccbillEnd(row); ok {
				out = append(out, rec)
			}
		case ccbill.DataLinkTxnRebill:
			if row.TransactionID() == "" {
				continue
			}
			at, _ := ccbillDate(row.Timestamp())
			f := facts{}
			f.add(FactSubscriptionRef, sub)
			// DataLink names no currency: the subscription's price prices it.
			remote := reconcile.CCBillRemoteTransaction(row)
			out = append(out, staged{kind: billing.PSPImportTransaction, ref: row.TransactionID(), subscriptionRef: sub, occurredAt: at, facts: f,
				data: transactionData{TransactionID: row.TransactionID(), Type: "sale", Success: true, Decimal: row.Amount(), OccurredAt: at, Remote: &remote}})
		}
	}
	return out, nil
}

// ccbillSignup fills a member's record from its signup: when it started,
// who it is, and the recurring terms it signed up for.
func ccbillSignup(row ccbill.DataLinkExportRow) staged {
	signup, _ := row.Signup()
	f := facts{}
	f.add(FactSubscriptionRef, row.SubscriptionID())
	f.add(FactUsername, signup.Username)
	f.add(FactEmail, signup.Email)
	data := map[string]any{}
	at, ok := ccbillDate(row.Timestamp())
	if ok {
		data["started_at"] = at
	}
	if signup.RecurringAmount != "" && signup.RecurringDays > 0 {
		data["stated"] = statedTerms{Decimal: signup.RecurringAmount, IntervalHours: signup.RecurringDays * 24}
	}
	return staged{kind: billing.PSPImportSubscription, ref: row.SubscriptionID(), occurredAt: at, facts: f, data: data, merge: true}
}

// ccbillEnd fills a member's record from a cancellation or an expiry: its
// paid time ends on the expire date, and a cancel date says the member
// canceled. False for a row that dates neither.
func ccbillEnd(row ccbill.DataLinkExportRow) (staged, bool) {
	expires, ok := ccbillDate(row.ExpireDate())
	if !ok {
		return staged{}, false
	}
	end := ccbill.DayEnd(expires)
	data := map[string]any{"ended": true, "ended_at": end}
	if canceled, ok := ccbillDate(row.CancelDate()); ok {
		data["canceled_at"] = canceled
	}
	f := facts{}
	f.add(FactSubscriptionRef, row.SubscriptionID())
	return staged{kind: billing.PSPImportSubscription, ref: row.SubscriptionID(), facts: f, data: data, merge: true}, true
}
