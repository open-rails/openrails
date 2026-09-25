package nmimock

import (
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Direct Post answers are urlencoded: response 1 approved, 2 declined,
// 3 error; response_code 100 approved, 300 gateway rejection.
func rejected(text string) string {
	return "response=3&responsetext=" + url.QueryEscape(text) + "&authcode=&transactionid=&avsresponse=&cvvresponse=&orderid=&type=&response_code=300"
}

func answer(fields ...string) string {
	v := make([]string, 0, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		v = append(v, fields[i]+"="+url.QueryEscape(fields[i+1]))
	}
	return strings.Join(v, "&")
}

func (m *Mock) directPost(form url.Values) string {
	switch {
	case form.Get("type") == "validate":
		return m.validate(form)
	case form.Get("type") == "sale":
		return m.sale(form)
	case form.Get("type") == "refund":
		return m.directRefund(form)
	case form.Get("type") == "void":
		return m.directVoid(form)
	case form.Get("recurring") == "update_subscription":
		return m.updateSubscription(form)
	case form.Get("recurring") == "add_subscription":
		id, err := m.addSubscription(form)
		if err != "" {
			return rejected(err)
		}
		return answer("response", "1", "responsetext", "Subscription added", "subscription_id", id, "orderid", form.Get("orderid"), "response_code", "100")
	}
	m.odd = append(m.odd, "POST transact.php type="+form.Get("type")+" recurring="+form.Get("recurring"))
	return rejected("Invalid Transaction Type")
}

// resolveCard is the vault and card a request names.
func (m *Mock) resolveCard(form url.Values) (*Vault, *Card, string) {
	v := m.vaults[form.Get("customer_vault_id")]
	if v == nil {
		return nil, nil, "Invalid Customer Vault Id"
	}
	c, ok := v.cardFor(form.Get("billing_id"))
	if !ok {
		return nil, nil, "Invalid Billing Id"
	}
	return v, c, ""
}

func (m *Mock) sale(form url.Values) string {
	m.attempts = append(m.attempts, form)
	v, charged, bad := m.resolveCard(form)
	if bad != "" {
		return rejected(bad)
	}
	order := form.Get("orderid")
	if code := charged.Decline; code != "" {
		d := &Sale{TransactionID: m.next("tx"), OrderID: order, Vault: v.ID, BillingID: v.BillingID, Amount: form.Get("amount"),
			Currency: strings.ToUpper(form.Get("currency")), Card: *charged, Declined: code, At: m.now()}
		m.sales = append(m.sales, d)
		return answer("response", "2", "responsetext", "DECLINE", "authcode", "", "transactionid", d.TransactionID, "avsresponse", "", "cvvresponse", "",
			"orderid", order, "type", "sale", "response_code", code, "customer_vault_id", v.ID)
	}
	if m.duplicate > 0 {
		m.duplicate--
		return rejected("Duplicate transaction REFID:3187654321")
	}
	if m.duplicateOf(*charged, form.Get("amount")) || m.withinDupSeconds(*charged, form) {
		return rejected("Duplicate transaction REFID:3187654322")
	}
	amount, currency, scheduleID := form.Get("amount"), strings.ToUpper(form.Get("currency")), ""
	if form.Get("recurring") == "rebill_subscription" {
		s := m.schedules[form.Get("subscription_id")]
		if s == nil || s.Deleted || s.Vault != v.ID {
			return rejected("Invalid subscription")
		}
		// NMI charges the saved schedule amount and leaves its next date.
		amount, currency, scheduleID = s.Amount, "USD", s.ID
	}
	s := &Sale{TransactionID: m.next("tx"), OrderID: order, Vault: v.ID, BillingID: form.Get("billing_id"), Amount: amount, ScheduleID: scheduleID,
		Currency: currency, InitiatedBy: form.Get("initiated_by"), Indicator: form.Get("stored_credential_indicator"),
		Initial: form.Get("initial_transaction_id"), Card: *charged, At: m.now()}
	if s.BillingID == "" {
		s.BillingID = v.BillingID
	}
	if m.hide > 0 {
		m.hide--
		s.Hidden = true
	}
	m.sales = append(m.sales, s)
	m.remember(*charged, form.Get("amount"))
	fields := []string{"response", "1", "responsetext", "SUCCESS", "authcode", "123456", "transactionid", s.TransactionID, "avsresponse", "", "cvvresponse", "",
		"orderid", order, "type", "sale", "response_code", "100", "customer_vault_id", v.ID}
	if form.Get("recurring") == "add_subscription" {
		id, bad := m.addSubscription(form)
		if bad != "" {
			return rejected(bad)
		}
		s.ScheduleID = id
		fields = append(fields, "subscription_id", id)
	}
	return answer(fields...)
}

// addSubscription enrolls a vault on a stored plan; start_date (YYYYMMDD)
// is the first charge, else one cadence from now.
func (m *Mock) addSubscription(form url.Values) (string, string) {
	plan, ok := m.plan(form.Get("plan_id"))
	v := m.vaults[form.Get("customer_vault_id")]
	if !ok || v == nil {
		return "", "Invalid plan or vault"
	}
	s := &Schedule{ID: m.next("rsub"), Vault: v.ID, Plan: plan.ID, Amount: plan.Amount, Order: form.Get("orderid"), Days: plan.Days, Months: plan.Months}
	if next, err := time.Parse("20060102", form.Get("start_date")); err == nil {
		s.NextBilling = next
	} else {
		s.NextBilling = s.advance(m.now().Truncate(24 * time.Hour))
	}
	m.schedules[s.ID] = s
	return s.ID, ""
}

// updateSubscription moves a schedule to a vault, a named plan, or (custom
// schedules only) a new amount or cadence. The next billing date never moves.
func (m *Mock) updateSubscription(form url.Values) string {
	s := m.schedules[form.Get("subscription_id")]
	if s == nil || s.Deleted {
		return rejected("Invalid subscription")
	}
	if m.failUpdates > 0 {
		m.failUpdates--
		return rejected("Subscription update unavailable")
	}
	if vault := form.Get("customer_vault_id"); vault != "" {
		if m.vaults[vault] == nil {
			return rejected("Invalid Customer Vault Id")
		}
		s.Vault = vault
	}
	if planID := form.Get("plan_id"); planID != "" {
		plan, ok := m.plan(planID)
		if !ok {
			return rejected("Invalid plan")
		}
		s.Plan, s.Amount, s.Custom, s.Days, s.Months = planID, plan.Amount, false, plan.Days, plan.Months
	} else if s.Custom {
		if amount := form.Get("plan_amount"); amount != "" {
			s.Amount = amount
		}
		if days, err := strconv.Atoi(form.Get("day_frequency")); err == nil {
			s.Days, s.Months = days, 0
		}
		if months, err := strconv.Atoi(form.Get("month_frequency")); err == nil {
			s.Months, s.Days = months, 0
		}
	}
	return answer("response", "1", "responsetext", "Subscription updated", "subscription_id", s.ID, "response_code", "100")
}

// validate is a no-funds verification: the issuer refuses a card it would
// never honor; a funds decline (202/203) still verifies.
func (m *Mock) validate(form url.Values) string {
	v, c, bad := m.resolveCard(form)
	if bad != "" {
		return rejected(bad)
	}
	if m.duplicateOf(*c, "0.00") {
		return rejected("Duplicate transaction REFID:3187654323")
	}
	id := m.next("validate")
	approved := c.Decline == "" || c.Decline == "202" || c.Decline == "203"
	if approved {
		m.remember(*c, "0.00")
	}
	m.validations = append(m.validations, &Validation{TransactionID: id, Vault: v.ID, BillingID: form.Get("billing_id"), Card: *c, Approved: approved, Form: form, At: m.now()})
	if !approved {
		return answer("response", "2", "responsetext", "DECLINE", "authcode", "", "transactionid", id, "orderid", form.Get("orderid"), "type", "validate", "response_code", c.Decline)
	}
	return answer("response", "1", "responsetext", "VALIDATED", "authcode", "", "transactionid", id, "orderid", form.Get("orderid"), "type", "validate", "response_code", "100")
}

func (m *Mock) directRefund(form url.Values) string {
	id, err := m.refund(form.Get("transactionid"), form.Get("amount"))
	if err != "" {
		return rejected(err)
	}
	return answer("response", "1", "responsetext", "SUCCESS", "transactionid", id, "type", "refund", "response_code", "100")
}

func (m *Mock) directVoid(form url.Values) string {
	if bad := m.void(form.Get("transactionid")); bad != "" {
		return rejected(bad)
	}
	return answer("response", "1", "responsetext", "Transaction Void Successful", "transactionid", form.Get("transactionid"), "type", "void", "response_code", "100")
}

// void cancels an unsettled approved sale or an auth probe.
func (m *Mock) void(txID string) string {
	if m.probes[txID] {
		delete(m.probes, txID)
		return ""
	}
	s := m.saleByID(txID)
	if s == nil || !s.Approved() || s.Voided || s.RefundedCents > 0 {
		return "Transaction not found or not voidable"
	}
	s.Voided = true
	return ""
}

// refund returns up to the unrefunded amount of an approved sale; "" is all.
func (m *Mock) refund(txID, amount string) (string, string) {
	s := m.saleByID(txID)
	if s == nil || !s.Approved() {
		return "", "Transaction not found"
	}
	left := centsOf(s.Amount) - s.RefundedCents
	cents := left
	if amount != "" {
		if _, err := strconv.ParseFloat(amount, 64); err != nil {
			return "", "Invalid amount"
		}
		cents = centsOf(amount)
	}
	if cents <= 0 || cents > left {
		return "", "Refund amount may not exceed the transaction balance"
	}
	return m.recordRefund(s, cents), ""
}

func (m *Mock) recordRefund(s *Sale, cents int64) string {
	id := m.next("rf")
	s.RefundedCents += cents
	s.RefundIDs = append(s.RefundIDs, id)
	m.refunds = append(m.refunds, &refund{ID: id, Sale: s, Cents: cents, At: m.now()})
	return id
}

func (m *Mock) duplicateOf(c Card, amount string) bool {
	if m.opts.DuplicateWindow <= 0 {
		return false
	}
	now := m.now()
	for _, r := range m.recent {
		if r.card.Brand == c.Brand && r.card.Last4 == c.Last4 && r.amount == amount && now.Sub(r.at) < m.opts.DuplicateWindow {
			return true
		}
	}
	return false
}

func (m *Mock) remember(c Card, amount string) {
	if m.opts.DuplicateWindow > 0 {
		m.recent = append(m.recent, recentCharge{card: c, amount: amount, at: m.now()})
	}
}

// withinDupSeconds is the per-request dup_seconds check against approved
// sales of the vault's same card and amount, indexed or not.
func (m *Mock) withinDupSeconds(c Card, form url.Values) bool {
	window, _ := strconv.Atoi(form.Get("dup_seconds"))
	if window <= 0 {
		return false
	}
	for _, s := range m.sales {
		if s.Approved() && s.Vault == form.Get("customer_vault_id") && s.Card.Brand == c.Brand && s.Card.Last4 == c.Last4 &&
			s.Amount == form.Get("amount") && m.now().Sub(s.At) <= time.Duration(window)*time.Second {
			return true
		}
	}
	return false
}

func (m *Mock) saleByID(id string) *Sale {
	for _, s := range m.sales {
		if s.TransactionID == id {
			return s
		}
	}
	return nil
}

func (m *Mock) plan(id string) (Plan, bool) {
	if p := m.plans[id]; p != nil {
		return *p, true
	}
	if m.opts.PlanFallback != nil {
		return m.opts.PlanFallback(id)
	}
	return Plan{}, false
}
