package nmi_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/nmimock"
)

// NMI dates its actions to the second: a since finer than that still reads
// a sale made in its second.
func TestReadsCompareWholeSeconds(t *testing.T) {
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	gateway := nmimock.NewUnstarted(nmimock.Options{Clock: func() time.Time { return at.Add(time.Hour) }})
	vault := gateway.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
	sale := gateway.AddSale(nmimock.Sale{Vault: vault, OrderID: "order-1", Amount: "10.00", At: at})
	client, err := nmi.NewAccountClient(slog.Default(), uuid.New(), uuid.New(), "gateway", &config.NMIProviderSettings{SecurityKey: "key", Environment: config.EnvironmentTest})
	if err != nil {
		t.Fatal(err)
	}
	client.UseTransport(gateway.Transport())
	since := at.Add(600 * time.Millisecond)

	probe, err := client.ProbeSalesByOrderID(t.Context(), "order-1", since)
	if err != nil || !probe.SuccessFound || probe.SuccessTransactionID != sale.TransactionID {
		t.Fatalf("probe: %+v, %v", probe, err)
	}
	cycle, err := client.ReadCycleTransactions(t.Context(), nmi.CycleScope{OrderIDs: []string{"order-1"}}, since)
	if err != nil || len(cycle) != 1 || cycle[0].TransactionID != sale.TransactionID {
		t.Fatalf("cycle: %+v, %v", cycle, err)
	}
	vaulted, err := client.ReadVaultTransactions(t.Context(), vault, since)
	if err != nil || len(vaulted) != 1 || vaulted[0].TransactionID != sale.TransactionID {
		t.Fatalf("vault: %+v, %v", vaulted, err)
	}
	if probe, err := client.ProbeSalesByOrderID(t.Context(), "order-1", at.Add(time.Second)); err != nil || probe.SuccessFound {
		t.Fatalf("a sale before since's second was read: %+v, %v", probe, err)
	}
}
