package pspimport

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// ccbillSource reads a CCBill account through DataLink: the ACTIVEMEMBERS
// roster (username, email, signup and rebill dates) and the rebills of each
// history window. CCBill keeps no vault to stage and no catalog to read: its
// terms are the provider price the merchant declares.
type ccbillSource struct {
	dl *ccbill.DataLinkClient
}

// dataLinkPeriod is the longest window one DataLink export answers.
const dataLinkPeriod = 24 * time.Hour

func ccbillDate(s string) (time.Time, bool) {
	t, err := timeutil.ParseFirstUTC(strings.TrimSpace(s), "2006-01-02 15:04:05", "2006-01-02", "20060102150405", "01/02/2006", time.RFC3339)
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
			data.NextBillingAt, data.Ended, data.UserCanceled = &t, true, true
		}
		out = append(out, staged{kind: billing.PSPImportSubscription, ref: m.SubscriptionID, occurredAt: data.StartedAt, facts: f, data: data})
	}
	return out, nil
}

func (s *ccbillSource) history(ctx context.Context, since, until time.Time) ([]staged, error) {
	var out []staged
	for from := since; from.Before(until); from = from.Add(dataLinkPeriod) {
		to := from.Add(dataLinkPeriod)
		if to.After(until) {
			to = until
		}
		to = to.Add(-time.Second) // DataLink's end is inclusive
		rows, err := s.dl.FetchTransactionExport(ctx, from, to, []ccbill.DataLinkTxnType{ccbill.DataLinkTxnRebill})
		if err != nil {
			return nil, fmt.Errorf("ccbill rebills: %w", err)
		}
		for _, row := range rows {
			if row.TransactionType != ccbill.DataLinkTxnRebill || row.TransactionID() == "" {
				continue
			}
			at, _ := ccbillDate(row.Timestamp())
			f := facts{}
			f.add(FactSubscriptionRef, row.SubscriptionID())
			// DataLink names no currency: the subscription's price prices it.
			out = append(out, staged{kind: billing.PSPImportTransaction, ref: row.TransactionID(), subscriptionRef: row.SubscriptionID(), occurredAt: at, facts: f,
				data: transactionData{TransactionID: row.TransactionID(), Type: "sale", Success: true, Decimal: row.Amount(), OccurredAt: at}})
		}
	}
	return out, nil
}
