package nmimock_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/nmimock"
)

// client is OpenRails' own NMI client on the mock's loopback listener, the
// path a sandbox stack takes.
func client(t *testing.T, m *nmimock.Mock) *nmi.NMIClient {
	t.Helper()
	c, err := nmi.NewAccountClient(uuid.New(), uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: "mock-key"}, true)
	require.NoError(t, err)
	c.DirectPostURL, c.QueryURL, c.V5BaseURL = m.URL()+"/api/transact.php", m.URL()+"/api/query.php", m.URL()+"/api/v5"
	c.LoopbackFixture = true
	return c
}

func cit() *nmi.StoredCredential {
	return &nmi.StoredCredential{InitiatedBy: nmi.InitiatedByCustomer, Indicator: nmi.IndicatorStored}
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func TestVaultSaleDeclineRefund(t *testing.T) {
	ctx := context.Background()
	clk := &clock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	m := nmimock.New(nmimock.Options{Clock: clk.Now})
	t.Cleanup(m.Close)
	c := client(t, m)

	vault, err := c.CreateCustomerVault(ctx, nmi.CreateCustomerVaultData{PaymentToken: m.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}), FirstName: "A", LastName: "B"})
	require.NoError(t, err)
	require.Equal(t, "4242", vault.Card.CardNumber[len(vault.Card.CardNumber)-4:])

	sale, err := c.RunSale(ctx, nmi.SaleParams{CustomerVaultID: vault.CustomerVaultID, Amount: 999, Currency: "USD", OrderID: "order-1", StoredCredential: cit()})
	require.NoError(t, err)
	attempts, err := c.ReadOrderAttempts(ctx, "order-1")
	require.NoError(t, err)
	require.Equal(t, 1, attempts.Transactions)
	require.False(t, attempts.Declined)

	m.SetDecline("4242", "202")
	_, err = c.RunSale(ctx, nmi.SaleParams{CustomerVaultID: vault.CustomerVaultID, Amount: 999, Currency: "USD", OrderID: "order-2", StoredCredential: cit()})
	require.Error(t, err)
	declined, err := c.ReadOrderAttempts(ctx, "order-2")
	require.NoError(t, err)
	require.True(t, declined.Declined)
	require.Equal(t, 202, declined.DeclineCode)

	refund, err := c.Refund(ctx, nmi.RefundParams{TransactionID: sale.TransactionID, Amount: 400, Currency: "USD"})
	require.NoError(t, err)
	require.NoError(t, c.ConfirmRefund(ctx, sale.TransactionID, refund.TransactionID, 400, "USD"))
	require.Equal(t, int64(599), m.Charged(vault.CustomerVaultID, ""))
	require.Empty(t, m.Unexpected())
}

func TestUnregisteredTokenAndDeclineLast4(t *testing.T) {
	ctx := context.Background()
	m := nmimock.New(nmimock.Options{})
	t.Cleanup(m.Close)
	c := client(t, m)
	vault, err := c.CreateCustomerVault(ctx, nmi.CreateCustomerVaultData{PaymentToken: "collect-js-" + nmimock.DeclineLast4, FirstName: "A", LastName: "B"})
	require.NoError(t, err)
	_, err = c.RunSale(ctx, nmi.SaleParams{CustomerVaultID: vault.CustomerVaultID, Amount: 500, Currency: "USD", OrderID: "o", StoredCredential: cit()})
	require.Error(t, err)
	require.Equal(t, "202", m.LastDecline().Declined)
	require.Empty(t, m.Ledger(""))
}

func TestIndexLagAndDuplicateWindowFollowTheClock(t *testing.T) {
	ctx := context.Background()
	clk := &clock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	m := nmimock.New(nmimock.Options{Clock: clk.Now, IndexLag: time.Minute, DuplicateWindow: 20 * time.Second})
	t.Cleanup(m.Close)
	c := client(t, m)
	vault := m.AddVault(nmimock.Card{Brand: "visa", Last4: "1111"})

	_, err := c.RunSale(ctx, nmi.SaleParams{CustomerVaultID: vault, Amount: 100, Currency: "USD", OrderID: "o1", StoredCredential: cit()})
	require.NoError(t, err)
	_, err = c.RunSale(ctx, nmi.SaleParams{CustomerVaultID: vault, Amount: 100, Currency: "USD", OrderID: "o2", StoredCredential: cit()})
	require.Error(t, err, "same card and amount inside the window")
	attempts, err := c.ReadOrderAttempts(ctx, "o1")
	require.NoError(t, err)
	require.Zero(t, attempts.Transactions, "not indexed yet")

	clk.now = clk.now.Add(time.Minute)
	attempts, err = c.ReadOrderAttempts(ctx, "o1")
	require.NoError(t, err)
	require.Equal(t, 1, attempts.Transactions)
	_, err = c.RunSale(ctx, nmi.SaleParams{CustomerVaultID: vault, Amount: 100, Currency: "USD", OrderID: "o3", StoredCredential: cit()})
	require.NoError(t, err)
	require.Len(t, m.Attempts(), 3)
	require.Len(t, m.Ledger(vault), 2)
}

func TestRecurringEngineBillsDueSchedules(t *testing.T) {
	ctx := context.Background()
	clk := &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	m := nmimock.New(nmimock.Options{Clock: clk.Now})
	t.Cleanup(m.Close)
	c := client(t, m)
	m.AddPlan(nmimock.Plan{ID: "monthly", Name: "Monthly", Amount: "9.99", Months: 1})
	vault := m.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
	id := m.AddSchedule(nmimock.Schedule{Vault: vault, Plan: "monthly", Amount: "9.99", Months: 1, NextBilling: clk.now.AddDate(0, 0, 10)})

	require.Empty(t, m.RunDue())
	clk.now = clk.now.AddDate(0, 1, 15)
	sales := m.RunDue()
	require.Len(t, sales, 2, "both due dates bill")
	require.True(t, sales[0].Approved())
	m.SetDecline("4242", "202")
	clk.now = clk.now.AddDate(0, 1, 0)
	require.Equal(t, "202", m.RunDue()[0].Declined)
	require.Equal(t, time.Date(2026, 4, 11, 0, 0, 0, 0, time.UTC), m.Schedule(id).NextBilling)

	found, err := c.SalesForSchedules(ctx, []string{id}, time.Time{})
	require.NoError(t, err)
	require.Len(t, found, 3)
	records, err := c.ReadSchedules(ctx, []string{id})
	require.NoError(t, err)
	require.Contains(t, records, id)
	m.DeleteSchedule(id)
	records, err = c.ReadSchedules(ctx, []string{id})
	require.NoError(t, err)
	require.NotContains(t, records, id)
}

func TestLostAndHeldRequests(t *testing.T) {
	ctx := context.Background()
	m := nmimock.New(nmimock.Options{})
	t.Cleanup(m.Close)
	c := client(t, m)
	vault := m.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})

	m.LoseSales(1)
	_, err := c.RunSale(ctx, nmi.SaleParams{CustomerVaultID: vault, Amount: 100, Currency: "USD", OrderID: "lost", StoredCredential: cit()})
	require.Error(t, err)
	require.Equal(t, 1, m.Lost())
	require.Empty(t, m.Ledger(vault))

	m.DropSaleResponses(1)
	_, err = c.RunSale(ctx, nmi.SaleParams{CustomerVaultID: vault, Amount: 200, Currency: "USD", OrderID: "dropped", StoredCredential: cit()})
	require.Error(t, err)
	require.Len(t, m.Ledger(vault), 1, "the gateway charged; only the answer was lost")

	m.FailRequests(func(r *http.Request) bool { return true }, http.StatusBadGateway, 1)
	_, err = c.ReadOrderAttempts(ctx, "dropped")
	require.Error(t, err)

	held := m.Hold(func(r *http.Request) bool { return r.Method == http.MethodPost }, nmimock.HoldCommit)
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	go func() { <-held.Arrived() }()
	_, err = c.RunSale(short, nmi.SaleParams{CustomerVaultID: vault, Amount: 300, Currency: "USD", OrderID: "timeout", StoredCredential: cit()})
	require.Error(t, err)
	require.Eventually(t, func() bool { return len(m.Ledger(vault)) == 2 }, 5*time.Second, 10*time.Millisecond, "a committed request charged though its caller gave up")
	held.Release()
	m.ClearIntercepts()
}
