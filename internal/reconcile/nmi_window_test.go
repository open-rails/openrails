package reconcile_test

import (
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/reconcile"
)

// A void is read in the window it happened in, though its sale is older.
func TestNMIWindowReadsAVoidByItsOwnDate(t *testing.T) {
	sold := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	now := sold
	gateway := nmimock.NewUnstarted(nmimock.Options{Clock: func() time.Time { return now }})
	vault := gateway.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
	sale := gateway.AddSale(nmimock.Sale{Vault: vault, Amount: "10.00", At: sold})
	now = sold.Add(48 * time.Hour)
	gateway.Void(sale.TransactionID)
	now = now.Add(time.Hour)
	client, err := nmi.NewAccountClient(slog.New(slog.DiscardHandler), uuid.New(), uuid.New(), "gateway", &config.NMIProviderSettings{SecurityKey: "key", Environment: config.EnvironmentTest})
	if err != nil {
		t.Fatal(err)
	}
	client.UseTransport(gateway.Transport())
	read := func(since, until time.Time) []reconcile.RemoteTransaction {
		t.Helper()
		snap, err := reconcile.NewNMIFetcher(client).Fetch(t.Context(), reconcile.FetchParams{Since: since, Until: until, ObservedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		return snap.Transactions
	}
	voidedIn := func(txns []reconcile.RemoteTransaction) bool {
		for _, txn := range txns {
			if txn.TransactionID == sale.TransactionID && txn.Type == reconcile.TransactionTypeVoid {
				return true
			}
		}
		return false
	}
	if !voidedIn(read(sold.Add(24*time.Hour), now)) {
		t.Fatal("the window the void happened in does not read it")
	}
	if !voidedIn(read(sold.Add(-time.Hour), sold.Add(time.Hour))) {
		t.Fatal("the sale's own window lost the voided sale")
	}
	if voidedIn(read(sold.Add(2*time.Hour), sold.Add(24*time.Hour))) {
		t.Fatal("a window holding neither the sale nor the void read it")
	}
}

// A transaction a page shift lists twice is read once.
func TestNMIPagesReadAShiftedTransactionOnce(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	gateway := nmimock.NewUnstarted(nmimock.Options{Clock: func() time.Time { return at.Add(time.Hour) }})
	vault := gateway.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
	gateway.AddSale(nmimock.Sale{Vault: vault, Amount: "1.00", At: at, Hidden: true})
	for range 1000 {
		gateway.AddSale(nmimock.Sale{Vault: vault, Amount: "1.00", At: at})
	}
	// NMI indexes the first sale between the two pages: the second page
	// starts one transaction earlier.
	gateway.Intercept(func(r *http.Request) bool {
		_ = r.ParseForm()
		return r.Form.Get("page_number") == "1"
	}, func(_ *http.Request, serve func() *http.Response) (*http.Response, error) {
		gateway.Reveal()
		return serve(), nil
	})
	client, err := nmi.NewAccountClient(slog.New(slog.DiscardHandler), uuid.New(), uuid.New(), "gateway", &config.NMIProviderSettings{SecurityKey: "key", Environment: config.EnvironmentTest})
	if err != nil {
		t.Fatal(err)
	}
	client.UseTransport(gateway.Transport())
	txns, err := reconcile.NMIChangedTransactions(t.Context(), client, at.Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, txn := range txns {
		seen[txn.TransactionID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("transaction %s read %d times", id, n)
		}
	}
	if len(seen) != 1000 {
		t.Fatalf("read %d transactions, want the 1000 the first page listed", len(seen))
	}
}
