package nmimock

import (
	"sort"
	"time"
)

// Tokenize is Collect.js: a payment token for c.
func (m *Mock) Tokenize(c Card) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.next("tok-")
	m.tokens[t] = c
	return t
}

// AddVault stores c in a new vault, as a legacy system did, and returns its id.
func (m *Mock) AddVault(c Card) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := &Vault{ID: m.next("vault"), BillingID: m.next("bill"), Card: c}
	m.vaults[v.ID] = v
	return v.ID
}

// PutVault stores v as given, replacing any vault with its id.
func (m *Mock) PutVault(v Vault) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := v.clone()
	m.vaults[v.ID] = &c
}

// AddCard adds a billing entry to a vault and returns its billing id.
func (m *Mock) AddCard(vault string, c Card) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.mustVault(vault)
	b := Billing{ID: m.next("bill"), Card: c}
	v.Extra = append(v.Extra, b)
	return b.ID
}

// RemoveVault deletes a vault outside OpenRails.
func (m *Mock) RemoveVault(vault string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.vaults, vault)
}

// EditVault changes a vault outside OpenRails.
func (m *Mock) EditVault(vault string, edit func(*Vault)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	edit(m.mustVault(vault))
}

// AddPlan stores a Recurring Plan the account already has.
func (m *Mock) AddPlan(p Plan) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plans[p.ID] = &p
}

// AddSchedule stores a recurring subscription NMI bills, and returns its
// id (s.ID when set).
func (m *Mock) AddSchedule(s Schedule) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.ID == "" {
		s.ID = m.next("rsub")
	}
	m.schedules[s.ID] = &s
	return s.ID
}

// EditSchedule changes a schedule outside OpenRails.
func (m *Mock) EditSchedule(id string, edit func(*Schedule)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	edit(m.mustSchedule(id))
}

// DeleteSchedule cancels a schedule outside OpenRails.
func (m *Mock) DeleteSchedule(id string) {
	m.EditSchedule(id, func(s *Schedule) { s.Deleted = true })
}

// AddSale records an approved sale made outside OpenRails (history, or a
// dashboard charge). Empty fields default to: a new transaction id, USD, the
// vault's primary card and billing id, and now.
func (m *Mock) AddSale(s Sale) Sale {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addSale(s)
}

func (m *Mock) addSale(s Sale) Sale {
	v := m.mustVault(s.Vault)
	if s.TransactionID == "" {
		s.TransactionID = m.next("tx")
	}
	if s.Currency == "" {
		s.Currency = "USD"
	}
	if s.BillingID == "" {
		s.BillingID = v.BillingID
	}
	if s.Card == (Card{}) {
		c, _ := v.cardFor(s.BillingID)
		s.Card = *c
	}
	if s.At.IsZero() {
		s.At = m.now()
	}
	stored := s
	m.sales = append(m.sales, &stored)
	return stored.clone()
}

// AddScheduleSale records an approved charge NMI's recurring engine made
// for a schedule at a time, without advancing it (backdated history).
func (m *Mock) AddScheduleSale(id string, at time.Time) Sale {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.mustSchedule(id)
	return m.addSale(Sale{Vault: s.Vault, Amount: s.Amount, OrderID: s.Order, ScheduleID: id, At: at})
}

// AddRecentCharge is a charge of amount on c just processed for another
// merchant customer, for the duplicate window.
func (m *Mock) AddRecentCharge(c Card, amount string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recent = append(m.recent, recentCharge{card: c, amount: amount, at: m.now()})
}

// Refund refunds cents of an approved sale outside OpenRails (a dashboard
// refund) and returns the refund's transaction id.
func (m *Mock) Refund(txID string, cents int64) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.saleByID(txID)
	if s == nil {
		panic("nmimock: refund of unknown sale " + txID)
	}
	return m.recordRefund(s, cents)
}

// RenewSchedule is NMI's recurring engine billing a schedule, dated its next
// billing time whatever the clock: approve false declines it with 202. Either way the schedule
// advances to its next regular date; NMI never retries a failed period.
func (m *Mock) RenewSchedule(id string, approve bool) Sale {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.renew(m.mustSchedule(id), approve)
}

func (m *Mock) renew(s *Schedule, approve bool) Sale {
	sale := Sale{Vault: s.Vault, Amount: s.Amount, OrderID: s.Order, ScheduleID: s.ID, At: s.NextBilling}
	if !approve {
		v := m.mustVault(s.Vault)
		sale.TransactionID, sale.BillingID, sale.Card, sale.Currency, sale.Declined = m.next("declined"), v.BillingID, v.Card, "USD", "202"
		m.sales = append(m.sales, &sale)
		s.NextBilling = s.advance(s.NextBilling)
		return sale.clone()
	}
	out := m.addSale(sale)
	s.NextBilling = s.advance(s.NextBilling)
	return out
}

// RunDue plays NMI's recurring engine up to now: every live, unpaused
// schedule bills each date due, approved or declined by its vault's card.
func (m *Mock) RunDue() []Sale {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var out []Sale
	for {
		var due []*Schedule
		for _, s := range m.schedules {
			if !s.Deleted && !s.Paused && !s.NextBilling.IsZero() && !s.NextBilling.After(now) {
				due = append(due, s)
			}
		}
		if len(due) == 0 {
			return out
		}
		sort.Slice(due, func(i, j int) bool {
			if !due[i].NextBilling.Equal(due[j].NextBilling) {
				return due[i].NextBilling.Before(due[j].NextBilling)
			}
			return due[i].ID < due[j].ID
		})
		for _, s := range due {
			v := m.vaults[s.Vault]
			out = append(out, m.renew(s, v != nil && v.Card.Decline == ""))
		}
	}
}

func (m *Mock) mustVault(id string) *Vault {
	v := m.vaults[id]
	if v == nil {
		panic("nmimock: unknown vault " + id)
	}
	return v
}

func (m *Mock) mustSchedule(id string) *Schedule {
	s := m.schedules[id]
	if s == nil {
		panic("nmimock: unknown schedule " + id)
	}
	return s
}
