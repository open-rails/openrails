package nmimock_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/nmimock"
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

// The Customer Vault takes a card by number (server card entry): the caller
// names the vault and billing entry, and the stored card reads back masked.
func TestVaultCardByNumber(t *testing.T) {
	ctx := context.Background()
	m := nmimock.New(nmimock.Options{})
	t.Cleanup(m.Close)
	c := client(t, m)
	typed := func(number string) *cardguard.Card {
		card, err := cardguard.NewCard(number, 10, 2027, "999")
		require.NoError(t, err)
		return card
	}

	created, err := c.CreateCustomerVaultFromCard(ctx, "700000000000000001", "900000000000000001", nmi.CreateCustomerVaultData{FirstName: "A", LastName: "B"}, typed("4111111111111111"))
	require.NoError(t, err)
	require.Equal(t, "700000000000000001", created.CustomerVaultID)
	require.NoError(t, c.AddCustomerBillingFromCard(ctx, "700000000000000001", "900000000000000002", nmi.CreateCustomerVaultData{}, typed("5431111111111111")))
	customer, found, err := c.GetCustomer(ctx, "700000000000000001")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, customer.Billing, 2)
	require.Equal(t, []string{"900000000000000001", "4xxxxxxxxxxx1111", "1027", "visa"},
		[]string{customer.Billing[0].ID, customer.Billing[0].PaymentDetails.CardNumber, customer.Billing[0].PaymentDetails.CardExp, customer.Billing[0].PaymentDetails.CardType})
	require.Equal(t, []string{"900000000000000002", "mastercard"}, []string{customer.Billing[1].ID, customer.Billing[1].PaymentDetails.CardType})
	_, err = c.RunSale(ctx, nmi.SaleParams{CustomerVaultID: "700000000000000001", BillingID: "900000000000000002", Amount: 999, Currency: "USD", OrderID: "order-card", StoredCredential: cit()})
	require.NoError(t, err)
	require.Equal(t, "mastercard", m.LastSale().Card.Brand)

	// The gateway refuses a taken vault or billing id and a card the issuer
	// will not let it store; each refusal is definite, never ambiguous.
	m.Issue("6011000991300009", nmimock.Card{Decline: "vault"})
	for name, call := range map[string]func() error{
		"taken vault id": func() error {
			_, err := c.CreateCustomerVaultFromCard(ctx, "700000000000000001", "b", nmi.CreateCustomerVaultData{}, typed("4111111111111111"))
			return err
		},
		"taken billing id": func() error {
			return c.AddCustomerBillingFromCard(ctx, "700000000000000001", "900000000000000002", nmi.CreateCustomerVaultData{}, typed("4111111111111111"))
		},
		"unknown vault": func() error {
			return c.AddCustomerBillingFromCard(ctx, "missing", "b", nmi.CreateCustomerVaultData{}, typed("4111111111111111"))
		},
		"refused card": func() error {
			_, err := c.CreateCustomerVaultFromCard(ctx, "700000000000000002", "b", nmi.CreateCustomerVaultData{}, typed("6011000991300009"))
			return err
		},
	} {
		err := call()
		var refused *nmi.CustomerVaultError
		require.ErrorAs(t, err, &refused, name)
		require.Equal(t, 300, refused.ResponseCode, name)
		require.False(t, nmi.IsTransportAmbiguous(err), name)
	}
	require.Equal(t, 1, m.RefusedSaves())
	require.Len(t, m.Vaults(), 1)
	require.Empty(t, m.Unexpected())
}
