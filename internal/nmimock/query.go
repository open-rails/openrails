package nmimock

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const queryTime = "20060102150405"

func (m *Mock) query(rec *httptest.ResponseRecorder, form url.Values) {
	rec.Header().Set("Content-Type", "text/xml; charset=UTF-8")
	switch form.Get("report_type") {
	case "recurring":
		m.recurringReport(rec, form)
	case "transaction", "":
		if m.queryDown {
			rec.Header().Del("Content-Type")
			rec.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = rec.WriteString(m.search(form))
	case "test_mode_status":
		_, _ = rec.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<nm_response><test_mode_status>enabled</test_mode_status></nm_response>")
	default:
		m.odd = append(m.odd, "POST query.php report_type="+form.Get("report_type"))
		_, _ = rec.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<nm_response><error_response>Invalid report_type</error_response></nm_response>")
	}
}

type xmlAction struct {
	Amount                string `xml:"amount"`
	ActionType            string `xml:"action_type"`
	Date                  string `xml:"date"`
	Success               string `xml:"success"`
	Source                string `xml:"source"`
	ResponseText          string `xml:"response_text"`
	ResponseCode          string `xml:"response_code"`
	ProcessorResponseCode string `xml:"processor_response_code"`
	ProcessorResponseText string `xml:"processor_response_text"`
	NetworkTokenUsed      string `xml:"network_token_used"`
}

type xmlTransaction struct {
	XMLName          xml.Name  `xml:"transaction"`
	TransactionID    string    `xml:"transaction_id"`
	TransactionType  string    `xml:"transaction_type"`
	Condition        string    `xml:"condition"`
	OrderID          string    `xml:"order_id"`
	OrderDescription string    `xml:"order_description"`
	CCNumber         string    `xml:"cc_number"`
	CCBin            string    `xml:"cc_bin"`
	CCType           string    `xml:"cc_type"`
	AVSResponse      string    `xml:"avs_response"`
	CSCResponse      string    `xml:"csc_response"`
	CustomerVaultID  string    `xml:"customer_vault_id"`
	Currency         string    `xml:"currency"`
	Action           xmlAction `xml:"action"`
}

// cardTransaction is a transaction on card c with the card fields and the
// issuer's answer the Query API reports: the processor code is "00" on an
// approval, else the issuer code behind NMI's response code.
func cardTransaction(id, condition, order, vault, currency string, c Card, a xmlAction) xmlTransaction {
	a.ProcessorResponseCode, a.ProcessorResponseText = "00", "Approved"
	if a.Success != "1" {
		a.ProcessorResponseCode, a.ProcessorResponseText = "05", "Do not honor"
		if a.ResponseCode == "202" {
			a.ProcessorResponseCode, a.ProcessorResponseText = "51", "Insufficient funds"
		}
	}
	if c.NetworkToken {
		a.NetworkTokenUsed = "1"
	}
	return xmlTransaction{TransactionID: id, TransactionType: "cc", Condition: condition, OrderID: order, CCNumber: maskedNumber(c), CCBin: binFor(c), CCType: c.Brand,
		AVSResponse: c.AVS, CSCResponse: c.CVV, CustomerVaultID: vault, Currency: currency, Action: a}
}

func binFor(c Card) string {
	if c.BIN != "" {
		return c.BIN
	}
	switch c.Brand {
	case "mastercard":
		return "555555"
	case "amex":
		return "378282"
	case "discover":
		return "601111"
	}
	return "411111"
}

// search is the transaction report: every sale, validation and refund in
// arrival order, filtered as NMI filters and paged by result_limit and
// zero-based page_number. Like NMI's, it names no schedule.
func (m *Mock) search(form url.Values) string {
	match := func(field string, value string) bool {
		want := form.Get(field)
		if want == "" {
			return true
		}
		for _, w := range strings.Split(want, ",") {
			if w == value {
				return true
			}
		}
		return false
	}
	start, _ := time.Parse(queryTime, form.Get("start_date"))
	end, _ := time.Parse(queryTime, form.Get("end_date"))
	inRange := func(at time.Time) bool {
		return (start.IsZero() || !at.Before(start)) && (end.IsZero() || !at.After(end))
	}
	schedules := form.Get("subscription_id") != ""
	now := m.now()
	var rows []xmlTransaction
	add := func(t xmlTransaction, at time.Time, schedule string) {
		if !match("order_id", t.OrderID) || !match("order_description", t.OrderDescription) || !match("transaction_id", t.TransactionID) || !match("customer_vault_id", t.CustomerVaultID) ||
			!match("action_type", t.Action.ActionType) || !match("subscription_id", schedule) || !inRange(at) {
			return
		}
		rows = append(rows, t)
	}
	for _, s := range m.sales {
		if s.Hidden || now.Sub(s.At) < m.opts.IndexLag {
			continue
		}
		success, code, text, condition := "1", "100", "SUCCESS", "pendingsettlement"
		if !s.Approved() {
			success, code, text, condition = "0", s.Declined, "DECLINE", "failed"
		}
		if s.Voided {
			condition = "canceled"
		}
		if s.Condition != "" {
			condition = s.Condition
		}
		source := "api"
		if s.ScheduleID != "" {
			source = "recurring"
		}
		txn := cardTransaction(s.TransactionID, condition, s.OrderID, s.Vault, s.Currency, s.Card, xmlAction{Amount: s.Amount, ActionType: "sale", Date: s.At.UTC().Format(queryTime),
			Success: success, Source: source, ResponseText: text, ResponseCode: code})
		txn.OrderDescription = s.OrderDescription
		add(txn, s.At, s.ScheduleID)
	}
	for _, r := range m.refunds {
		s := r.Sale
		add(cardTransaction(r.ID, "pendingsettlement", s.OrderID, s.Vault, s.Currency, s.Card, xmlAction{Amount: decimalCents(r.Cents), ActionType: "refund", Date: r.At.UTC().Format(queryTime),
			Success: "1", Source: "api", ResponseText: "SUCCESS", ResponseCode: "100"}), r.At, s.ScheduleID)
	}
	if !schedules {
		for _, v := range m.validations {
			success, code, text, condition := "1", "100", "VALIDATED", "complete"
			if !v.Approved {
				success, code, text, condition = "0", v.Card.Decline, "DECLINE", "failed"
			}
			add(cardTransaction(v.TransactionID, condition, v.Form.Get("orderid"), v.Vault, "USD", v.Card, xmlAction{Amount: "0.00", ActionType: "validate", Date: v.At.UTC().Format(queryTime),
				Success: success, Source: "api", ResponseText: text, ResponseCode: code}), v.At, "")
		}
	}
	limit, _ := strconv.Atoi(form.Get("result_limit"))
	page, _ := strconv.Atoi(form.Get("page_number"))
	if limit > 0 {
		from := min(max(page, 0)*limit, len(rows))
		rows = rows[from:min(from+limit, len(rows))]
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<nm_response>")
	for _, r := range rows {
		raw, _ := xml.Marshal(r)
		b.Write(raw)
	}
	b.WriteString("</nm_response>")
	return b.String()
}

// recurringReport lists live schedules by subscription_id (comma list);
// NMI no longer reports a deleted one.
func (m *Mock) recurringReport(rec *httptest.ResponseRecorder, form url.Values) {
	type plan struct {
		ID string `xml:"plan_id"`
	}
	type row struct {
		XMLName        xml.Name `xml:"subscription"`
		SubscriptionID string   `xml:"subscription_id"`
		OrderID        string   `xml:"orderid"`
		PONumber       string   `xml:"ponumber"`
		NextChargeDate string   `xml:"next_charge_date"`
		Plan           plan     `xml:"plan"`
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<nm_response>")
	for _, id := range strings.Split(form.Get("subscription_id"), ",") {
		if s := m.schedules[id]; s != nil && !s.Deleted {
			raw, _ := xml.Marshal(row{SubscriptionID: s.ID, OrderID: s.Order, PONumber: s.Order, NextChargeDate: s.NextBilling.Format("2006-01-02"), Plan: plan{ID: s.Plan}})
			b.Write(raw)
		}
	}
	b.WriteString("</nm_response>")
	_, _ = rec.WriteString(b.String())
}

func maskedNumber(c Card) string {
	prefix := map[string]string{"mastercard": "5", "amex": "3", "discover": "6"}[c.Brand]
	if prefix == "" {
		prefix = "4"
	}
	return prefix + "xxxxxxxxxxx" + c.Last4
}
