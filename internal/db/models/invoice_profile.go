package models

// InvoiceContact is one billing contact in an invoice profile.
type InvoiceContact struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}
