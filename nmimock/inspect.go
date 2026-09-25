package nmimock

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Sales is every sale NMI processed, approved or declined, in order.
func (m *Mock) Sales() []Sale {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Sale, 0, len(m.sales))
	for _, s := range m.sales {
		out = append(out, s.clone())
	}
	return out
}

// Sale is one sale by transaction id.
func (m *Mock) Sale(txID string) (Sale, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.saleByID(txID); s != nil {
		return s.clone(), true
	}
	return Sale{}, false
}

// LastSale is the latest approved sale, or nil.
func (m *Mock) LastSale() *Sale { return m.last(true) }

// LastDecline is the latest declined sale, or nil.
func (m *Mock) LastDecline() *Sale { return m.last(false) }

func (m *Mock) last(approved bool) *Sale {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.sales) - 1; i >= 0; i-- {
		if m.sales[i].Approved() == approved {
			s := m.sales[i].clone()
			return &s
		}
	}
	return nil
}

// Ledger is the money approved, unvoided sales moved, for one vault or ("")
// all: the gateway's side of a money invariant.
func (m *Mock) Ledger(vault string) []LedgerEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []LedgerEntry
	for _, s := range m.sales {
		if s.Approved() && !s.Voided && (vault == "" || s.Vault == vault) {
			out = append(out, LedgerEntry{TransactionID: s.TransactionID, OrderID: s.OrderID, Vault: s.Vault, Last4: s.Card.Last4,
				Cents: centsOf(s.Amount), RefundedCents: s.RefundedCents})
		}
	}
	return out
}

// Charged is the net cents moved: per vault ("" = all) and optionally one order.
func (m *Mock) Charged(vault, order string) int64 {
	var n int64
	for _, e := range m.Ledger(vault) {
		if order == "" || e.OrderID == order {
			n += e.Cents - e.RefundedCents
		}
	}
	return n
}

// Attempts is the form of every Direct Post sale request, in order,
// including refused and declined ones.
func (m *Mock) Attempts() []url.Values {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]url.Values(nil), m.attempts...)
}

// Validations is every card verification, for one vault or ("") all.
func (m *Mock) Validations(vault string) []Validation {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Validation
	for _, v := range m.validations {
		if vault == "" || v.Vault == vault {
			out = append(out, *v)
		}
	}
	return out
}

// Vault is a copy of one vault, or nil once removed.
func (m *Mock) Vault(id string) *Vault {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v := m.vaults[id]; v != nil {
		c := v.clone()
		return &c
	}
	return nil
}

// Vaults is every stored vault, by id.
func (m *Mock) Vaults() []Vault {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Vault, 0, len(m.vaults))
	for _, id := range sortedKeys(m.vaults) {
		out = append(out, m.vaults[id].clone())
	}
	return out
}

// Schedule is a copy of one schedule, deleted or not.
func (m *Mock) Schedule(id string) Schedule {
	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.mustSchedule(id)
}

// ScheduleLive reports whether a schedule exists and is not deleted.
func (m *Mock) ScheduleLive(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.schedules[id]
	return s != nil && !s.Deleted
}

// Schedules is every schedule, deleted or not, by id.
func (m *Mock) Schedules() []Schedule {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Schedule, 0, len(m.schedules))
	for _, id := range sortedKeys(m.schedules) {
		out = append(out, *m.schedules[id])
	}
	return out
}

// Calls is every mutation the gateway received, in order.
func (m *Mock) Calls() []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Call(nil), m.calls...)
}

// CallsTo is the mutations of method whose path starts with prefix and
// whose form (Direct Post) satisfies form, when given.
func (m *Mock) CallsTo(method, prefix string, form func(url.Values) bool) []Call {
	var out []Call
	for _, c := range m.Calls() {
		if c.Method == method && strings.HasPrefix(c.Path, prefix) && (form == nil || form(c.Form)) {
			out = append(out, c)
		}
	}
	return out
}

// ScheduleDeletes counts DELETE requests for one schedule.
func (m *Mock) ScheduleDeletes(id string) int {
	return len(m.CallsTo(http.MethodDelete, "/subscriptions/"+id, nil))
}

// ScheduleUpdates is the update_subscription requests for one schedule.
func (m *Mock) ScheduleUpdates(id string) []Call {
	return m.CallsTo(http.MethodPost, "transact.php", func(v url.Values) bool {
		return v.Get("recurring") == "update_subscription" && v.Get("subscription_id") == id
	})
}

// Reads counts reads served, by kind ("query:<report_type>",
// "v5:<resource>" or "v5:<resource>/{id}").
func (m *Mock) Reads() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.reads))
	for k, v := range m.reads {
		out[k] = v
	}
	return out
}

// Unexpected lists requests the mock does not model; tests assert it empty.
func (m *Mock) Unexpected() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]string(nil), m.odd...)
	sort.Strings(out)
	return out
}

// Lost counts requests LoseSales made fail before reaching the gateway.
func (m *Mock) Lost() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lost
}

// RefusedSaves counts vault creations refused for a card declined "vault".
func (m *Mock) RefusedSaves() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refusedSaves
}
