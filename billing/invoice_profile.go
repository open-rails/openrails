package billing

// InvoiceCollectionMethod is how a customer's usage orders are paid:
// charged to their collection card, or sent for them to remit.
type InvoiceCollectionMethod string

const (
	CollectChargeAutomatically InvoiceCollectionMethod = "charge_automatically"
	CollectSendInvoice         InvoiceCollectionMethod = "send_invoice"
)

// InvoiceContact is one billing contact in an invoice profile.
type InvoiceContact struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// InvoiceProfile is a customer's invoice terms and document fields, copied
// onto each usage order when it is billed.
type InvoiceProfile struct {
	NetTermsDays     int                     `json:"net_terms_days"`
	CollectionMethod InvoiceCollectionMethod `json:"collection_method"`
	PONumber         string                  `json:"po_number"`
	Tax              map[string]any          `json:"tax"`
	BillingContacts  []InvoiceContact        `json:"billing_contacts"`
	Memo             string                  `json:"memo"`
}
