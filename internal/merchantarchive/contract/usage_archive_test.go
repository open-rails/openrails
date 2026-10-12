package contract

import (
	"bytes"
	"slices"
	"testing"

	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/stretchr/testify/require"
)

// An archive exported before usage orders names invoices and the tables
// usage orders renamed, and has rows of the earlier shapes. Without invoice
// data it reads as the migration moved the database; with any it is refused.
func TestReadArchiveBeforeUsageOrders(t *testing.T) {
	const order = "10000000-0000-0000-0000-000000000009"
	ordersProfile, orderRow := row(t, "orders", map[string]string{"merchant_id": testMerchant, "id": order, "customer_id": testMerchant, "origin": "customer",
		"status": "open", "payment_status": "requires_payment_method", "currency": "USD", "total": "100", "created_at": "2026-09-01 00:00:00+00", "updated_at": "2026-09-01 00:00:00+00"})
	orderRow = orderRow[:len(ordersProfile.Columns)-2-orderUsageColumns]
	paymentsProfile, paymentRow := row(t, "payments", map[string]string{"merchant_id": testMerchant, "id": order, "order_id": order})
	at := slices.IndexFunc(paymentsProfile.Columns, func(c Column) bool { return c.Name == "ledger_transfer_id" })
	paymentRow = slices.Insert(paymentRow, at, nil) // invoice_id
	_, item := row(t, "owed_items", map[string]string{"merchant_id": testMerchant, "id": order, "customer_id": testMerchant, "currency": "USD", "amount": "1", "status": "pending"})

	archive := func(invoice []*string, itemInvoice *string) []byte {
		var artifact bytes.Buffer
		writer, err := archivewire.NewWriter(&artifact, testMerchant)
		require.NoError(t, err)
		for _, p := range Profiles {
			switch p.Name {
			case "usage_collection_cadence":
				require.NoError(t, writer.Table("invoice_collection_cadence"))
				require.NoError(t, writer.Table("invoices"))
				if invoice != nil {
					require.NoError(t, writer.Row(invoice))
				}
			case "owed_items":
				require.NoError(t, writer.Table("invoice_items"))
				values := slices.Clone(item)
				values[slices.IndexFunc(p.Columns, func(c Column) bool { return c.Name == "order_id" })] = itemInvoice
				require.NoError(t, writer.Row(values))
			default:
				require.NoError(t, writer.Table(p.Name))
			}
			switch p.Name {
			case "orders":
				require.NoError(t, writer.Row(orderRow))
			case "payments":
				require.NoError(t, writer.Row(paymentRow))
			}
		}
		require.NoError(t, writer.Close())
		return artifact.Bytes()
	}

	read := map[string]int{}
	_, err := Read(bytes.NewReader(archive(nil, nil)), nil, func(p Profile, values []*string) error {
		require.Len(t, values, len(p.Columns), p.Name)
		read[p.Name]++
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, map[string]int{"orders": 1, "payments": 1, "owed_items": 1}, read)

	merchant, invoiceID := testMerchant, order
	_, err = Read(bytes.NewReader(archive([]*string{&merchant, &invoiceID}, nil)), nil, nil)
	require.ErrorIs(t, err, errArchivedInvoices)
	_, err = Read(bytes.NewReader(archive(nil, &invoiceID)), nil, nil)
	require.ErrorIs(t, err, errArchivedInvoices, "an owed item an invoice billed")
}
