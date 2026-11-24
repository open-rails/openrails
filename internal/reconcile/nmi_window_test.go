package reconcile_test

import (
	"log/slog"
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
