package stripemock

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BankAccount is the external account every payout goes to.
var BankAccount = Object{"id": "ba_gf_payouts", "object": "bank_account", "bank_name": "STRIPE TEST BANK", "last4": "6789", "currency": "usd", "country": "US"}

// Payout pays amount (minor units) out of the balance to BankAccount,
// arriving on arrival's UTC date. Its balance transaction takes it from the
// balance now. An automatic one pays out every balance transaction of the
// payments balance not yet paid out, and lists them once it is paid, failed
// or canceled.
func (m *Mock) Payout(amount int64, currency string, arrival time.Time, automatic bool) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	currency = strings.ToLower(currency)
	y, mo, d := arrival.UTC().Date()
	po := Object{"object": "payout", "id": m.id("po"), "amount": amount, "currency": currency, "arrival_date": time.Date(y, mo, d, 0, 0, 0, 0, time.UTC).Unix(),
		"automatic": automatic, "created": m.now().Unix(), "destination": BankAccount["id"], "failure_balance_transaction": nil, "method": "standard",
		"status": "pending", "reconciliation_status": "not_applicable", "type": "bank_account", "source_type": "card"}
	if automatic {
		po["reconciliation_status"] = "in_progress"
		for id, bt := range m.balances {
			if m.paidOut[id] == "" && bt["balance_type"] != "risk_reserved" {
				m.paidOut[id] = po["id"].(string)
			}
		}
	}
	bt := m.addBalanceLocked("payout", "payout", po["id"].(string), -amount, currency, "payments", m.now())
	m.paidOut[bt["id"].(string)] = po["id"].(string)
	po["balance_transaction"] = bt["id"]
	m.payouts[po["id"].(string)] = po
	return clone(po)
}

// SetPayoutStatus moves a payout to in_transit, paid, failed or canceled; a
// failed or canceled one returns its amount to the balance. An automatic
// payout's balance transactions are listed from then on.
func (m *Mock) SetPayoutStatus(id, status string) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	po := m.payouts[id]
	po["status"] = status
	if po["automatic"] == true && status != "in_transit" {
		po["reconciliation_status"] = "completed"
	}
	if status == "failed" || status == "canceled" {
		typ := "payout_failure"
		if status == "canceled" {
			typ = "payout_cancel"
		}
		bt := m.addBalanceLocked(typ, "payout_reversal", id, po["amount"].(int64), po["currency"].(string), "payments", m.now())
		po["failure_balance_transaction"], po["failure_code"] = bt["id"], "account_closed"
	}
	return clone(po)
}

// ReserveHold holds amount (minor units) of the payments balance in reserve:
// reserved_funds takes it from the payments balance, reserve_hold puts it in
// the risk reserved balance.
func (m *Mock) ReserveHold(amount int64, currency string) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addBalanceLocked("reserve_hold", "risk_reserved_funds", "", amount, currency, "risk_reserved", m.now())
	return clone(m.addBalanceLocked("reserved_funds", "risk_reserved_funds", "", -amount, currency, "payments", m.now()))
}

// ReserveRelease releases amount (minor units) from the reserve to the
// payments balance, available on available.
func (m *Mock) ReserveRelease(amount int64, currency string, available time.Time) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addBalanceLocked("reserve_release", "risk_reserved_funds", "", -amount, currency, "risk_reserved", m.now())
	return clone(m.addBalanceLocked("reserved_funds", "risk_reserved_funds", "", amount, currency, "payments", available))
}

// addBalanceLocked records a fee-free balance transaction of one balance.
func (m *Mock) addBalanceLocked(typ, category, source string, amount int64, currency, balance string, available time.Time) Object {
	bt := Object{"object": "balance_transaction", "id": m.id("txn"), "amount": amount, "currency": strings.ToLower(currency), "fee": int64(0), "fee_details": []Object{}, "net": amount,
		"type": typ, "reporting_category": category, "created": m.now().Unix(), "available_on": available.Unix(), "status": "available", "balance_type": balance, "exchange_rate": nil}
	if source != "" {
		bt["source"] = source
	}
	m.balances[bt["id"].(string)] = bt
	return bt
}

// payoutLocked serves payouts and the balance transactions list.
func (m *Mock) payoutLocked(r *http.Request, seg []string, q url.Values) (int, any, bool) {
	switch {
	case r.Method == http.MethodGet && seg[0] == "payouts" && len(seg) == 2:
		po, ok := m.payouts[seg[1]]
		if !ok {
			return 404, stripeErr("resource_missing"), true
		}
		out := clone(po)
		for _, field := range q["expand[]"] {
			switch field {
			case "balance_transaction", "failure_balance_transaction":
				if bt, ok := m.balances[stringOf(po[field])]; ok {
					out[field] = clone(bt)
				}
			case "destination":
				out[field] = clone(BankAccount)
			}
		}
		return 200, out, true
	case r.Method == http.MethodGet && seg[0] == "balance_transactions" && len(seg) == 1:
		payout := q.Get("payout")
		if po, ok := m.payouts[payout]; payout != "" && (!ok || po["automatic"] != true) {
			return 400, stripeErr("invalid_request_error"), true
		}
		if po := m.payouts[payout]; payout != "" && po["reconciliation_status"] != "completed" {
			return 200, Object{"object": "list", "data": []any{}, "has_more": false}, true
		}
		ids := make([]string, 0, len(m.balances))
		for id, bt := range m.balances {
			if (payout == "" || m.paidOut[id] == payout && id != stringOf(m.payouts[payout]["balance_transaction"])) &&
				(q.Get("type") == "" || bt["type"] == q.Get("type")) {
				ids = append(ids, id)
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(ids)))
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 || limit > 100 {
			limit = 10
		}
		if after := q.Get("starting_after"); after != "" {
			i := sort.Search(len(ids), func(i int) bool { return ids[i] < after })
			ids = ids[i:]
		}
		data := []any{}
		for _, id := range ids[:min(limit, len(ids))] {
			data = append(data, clone(m.balances[id]))
		}
		return 200, Object{"object": "list", "url": "/v1/balance_transactions", "data": data, "has_more": len(ids) > limit}, true
	}
	return 0, nil, false
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}
