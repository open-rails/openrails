package contract

import "fmt"

// preBooksAccounts are the units book's accounts as an archive from before
// the books migration names them: their names and classes now.
var preBooksAccounts = map[string][2]string{
	"customer_balance":       {"customer_units", "held"},
	"arrears_liability":      {"customer_owed", "owed"},
	"processor_clearing":     {"units_sold", "issued"},
	"promotional_funding":    {"units_granted", "issued"},
	"platform_revenue":       {"units_redeemed", "retired"},
	"expired_credits":        {"units_expired", "retired"},
	"revoked_credits":        {"units_revoked", "retired"},
	"credit_refund_clearing": {"units_refund_clearing", "clearing"},
	"credit_refund_loss":     {"units_refund_loss", "clearing"},
}

// normalizePreBooks reads an account or transfer exported before the books
// as the units book's, as the migration moved the database: an account by its
// new name and class, a transfer effective when it posted. A transfer
// exported before revenue schedules names none.
func normalizePreBooks(p Profile, values *[]*string) error {
	switch {
	case p.Name == "ledger_accounts" && len(*values) == len(p.Columns)-3:
		typ := value(p, *values, "account_type")
		if typ == nil {
			return fmt.Errorf("archive ledger account without a type")
		}
		now, ok := preBooksAccounts[*typ]
		if !ok {
			return fmt.Errorf("archive ledger account of unknown type %q", *typ)
		}
		setValue(p, *values, "account_type", now[0])
		*values = append(*values, new("units"), new(now[1]), nil)
	case p.Name == "ledger_transfers" && len(*values) == len(p.Columns)-5:
		created := value(p, *values, "created_at")
		if created == nil {
			return fmt.Errorf("archive ledger transfer without created_at")
		}
		*values = append(*values, new("units"), new(*created), nil, nil, nil)
	case p.Name == "ledger_transfers" && len(*values) == len(p.Columns)-1:
		*values = append(*values, nil)
	case p.Name == "credit_grants" && len(*values) == len(p.Columns)-2:
		// Before the credit books no lot's money was deferred.
		*values = append(*values, new("0"), new("0"))
	}
	return nil
}
