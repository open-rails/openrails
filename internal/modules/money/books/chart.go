// Package books is the chart of OpenRails' two books, kept in the same
// ledger tables: the units book (what customers hold and owe, at face, in any
// unit) and the general ledger (money, in registry currencies only), and the
// general ledger's poster. The units book's writer is package ledger.
package books

import (
	"slices"
)

// Book is one of the two books. A transfer never crosses books.
type Book string

const (
	Units   Book = "units"
	General Book = "general"
)

// Class is an account type's kind; it sets the side its balance is normally
// on. Every stored balance is credits_posted - debits_posted.
type Class string

const (
	// Units book.
	Held     Class = "held"     // units a customer holds
	Owed     Class = "owed"     // units a customer owes
	Issued   Class = "issued"   // where units come from
	Retired  Class = "retired"  // where units go
	Clearing Class = "clearing" // former refund clearing and loss; history only

	// General ledger.
	Asset         Class = "asset"
	ContraAsset   Class = "contra_asset"
	Liability     Class = "liability"
	Revenue       Class = "revenue"
	ContraRevenue Class = "contra_revenue"
	Expense       Class = "expense"
	Equity        Class = "equity"
)

// Side is a balance's normal side.
type Side string

const (
	Debit  Side = "debit"
	Credit Side = "credit"
)

// Normal is the side the class's balances are normally on; "" for clearing,
// which has none.
func (c Class) Normal() Side {
	switch c {
	case Owed, Issued, Asset, ContraRevenue, Expense:
		return Debit
	case Clearing:
		return ""
	}
	return Credit
}

// Scope is what one account of a type belongs to.
type Scope string

const (
	System   Scope = "system"   // one per merchant and currency
	Customer Scope = "customer" // one per customer and currency
	PSP      Scope = "psp"      // one per PSP and currency
)

// AccountType is an account's role in its book: ledger_accounts.account_type.
type AccountType string

// The units book's accounts.
const (
	CustomerUnits       AccountType = "customer_units"
	CustomerOwed        AccountType = "customer_owed"
	UnitsSold           AccountType = "units_sold"
	UnitsGranted        AccountType = "units_granted"
	UnitsRedeemed       AccountType = "units_redeemed"
	UnitsExpired        AccountType = "units_expired"
	UnitsRevoked        AccountType = "units_revoked"
	UnitsRefunded       AccountType = "units_refunded"
	UnitsRefundClearing AccountType = "units_refund_clearing"
	UnitsRefundLoss     AccountType = "units_refund_loss"
)

// The general ledger's accounts.
const (
	CashClearing          AccountType = "cash_clearing"
	ExternalReceipts      AccountType = "external_receipts"
	UsageReceivable       AccountType = "usage_receivable"
	AllowanceDoubtful     AccountType = "allowance_doubtful"
	UnappliedCash         AccountType = "unapplied_cash"
	DeferredRevenue       AccountType = "deferred_revenue"
	DeferredRevenueCredit AccountType = "deferred_revenue_credit"
	TaxPayable            AccountType = "tax_payable"
	SubscriptionRevenue   AccountType = "subscription_revenue"
	OneTimeRevenue        AccountType = "one_time_revenue"
	UsageRevenue          AccountType = "usage_revenue"
	BreakageRevenue       AccountType = "breakage_revenue"
	Refunds               AccountType = "refunds"
	Chargebacks           AccountType = "chargebacks"
	RevenueAdjustments    AccountType = "revenue_adjustments"
	PromotionalCredit     AccountType = "promotional_credit"
	ProcessorFees         AccountType = "processor_fees"
	ChargebackFees        AccountType = "chargeback_fees"
	BadDebt               AccountType = "bad_debt"
	OpeningBalanceEquity  AccountType = "opening_balance_equity"
)

// Account is one account type in the chart: its book, class, scope and the
// sign rules its accounts' counters enforce. A merchant-wide general-ledger
// account keeps no counters and so no sign rule: its balance is its type
// total, kept at commit.
type Account struct {
	Type  AccountType
	Book  Book
	Class Class
	Scope Scope
	// DebitsMustNotExceedCredits: the balance may not go below zero.
	DebitsMustNotExceedCredits bool
	// CreditsMustNotExceedDebits: the balance may not go above zero.
	CreditsMustNotExceedDebits bool
}

// chart is every account type, mirrored by ledger_accounts_type_check and
// ledger_accounts_scope_check.
var chart = []Account{
	{Type: CustomerUnits, Book: Units, Class: Held, Scope: Customer, DebitsMustNotExceedCredits: true},
	{Type: CustomerOwed, Book: Units, Class: Owed, Scope: Customer},
	{Type: UnitsSold, Book: Units, Class: Issued, Scope: System},
	{Type: UnitsGranted, Book: Units, Class: Issued, Scope: System},
	{Type: UnitsRedeemed, Book: Units, Class: Retired, Scope: System},
	{Type: UnitsExpired, Book: Units, Class: Retired, Scope: System},
	{Type: UnitsRevoked, Book: Units, Class: Retired, Scope: System},
	{Type: UnitsRefunded, Book: Units, Class: Retired, Scope: System},
	{Type: UnitsRefundClearing, Book: Units, Class: Clearing, Scope: System},
	{Type: UnitsRefundLoss, Book: Units, Class: Clearing, Scope: System},

	{Type: CashClearing, Book: General, Class: Asset, Scope: PSP},
	{Type: ExternalReceipts, Book: General, Class: Asset, Scope: System},
	{Type: UsageReceivable, Book: General, Class: Asset, Scope: Customer, CreditsMustNotExceedDebits: true},
	{Type: AllowanceDoubtful, Book: General, Class: ContraAsset, Scope: System},
	{Type: UnappliedCash, Book: General, Class: Liability, Scope: System},
	{Type: DeferredRevenue, Book: General, Class: Liability, Scope: System},
	{Type: DeferredRevenueCredit, Book: General, Class: Liability, Scope: Customer, DebitsMustNotExceedCredits: true},
	{Type: TaxPayable, Book: General, Class: Liability, Scope: System},
	{Type: SubscriptionRevenue, Book: General, Class: Revenue, Scope: System},
	{Type: OneTimeRevenue, Book: General, Class: Revenue, Scope: System},
	{Type: UsageRevenue, Book: General, Class: Revenue, Scope: System},
	{Type: BreakageRevenue, Book: General, Class: Revenue, Scope: System},
	{Type: Refunds, Book: General, Class: ContraRevenue, Scope: System},
	{Type: Chargebacks, Book: General, Class: ContraRevenue, Scope: System},
	{Type: RevenueAdjustments, Book: General, Class: ContraRevenue, Scope: System},
	{Type: PromotionalCredit, Book: General, Class: ContraRevenue, Scope: System},
	{Type: ProcessorFees, Book: General, Class: Expense, Scope: System},
	{Type: ChargebackFees, Book: General, Class: Expense, Scope: System},
	{Type: BadDebt, Book: General, Class: Expense, Scope: System},
	{Type: OpeningBalanceEquity, Book: General, Class: Equity, Scope: System},
}

// Chart is every account type of both books.
func Chart() []Account { return slices.Clone(chart) }

// Lookup is one account type's entry.
func Lookup(t AccountType) (Account, bool) {
	i := slices.IndexFunc(chart, func(a Account) bool { return a.Type == t })
	if i < 0 {
		return Account{}, false
	}
	return chart[i], true
}

// TransferType is ledger_transfers.transfer_type. The units book's types are
// package ledger's; the general ledger's are here. Each belongs to one book
// (ledger_transfers_type_check), except UsageReversed.
type TransferType string

// UsageReversed is the one type in both books: a period whose usage came to
// less than was posted returns units to the balance or reduces owed, and the
// general ledger reverses the receivable or the credit it drew. book is part
// of the once key, so its two legs share a coordinate.
const UsageReversed TransferType = "usage_reversed"

// The general ledger's transfer types.
const (
	CashReceived TransferType = "cash_received"
	CashApplied  TransferType = "cash_applied"
	CashReturned TransferType = "cash_returned"
	// RevenueAdjusted settles a line with value that is not cash, such as a
	// provider's customer balance.
	RevenueAdjusted          TransferType = "revenue_adjusted"
	RevenueRecognized        TransferType = "revenue_recognized"
	RefundDeferred           TransferType = "refund_deferred"
	RefundRecognized         TransferType = "refund_recognized"
	ChargebackDeferred       TransferType = "chargeback_deferred"
	ChargebackRecognized     TransferType = "chargeback_recognized"
	DisputeWon               TransferType = "dispute_won"
	CreditReleased           TransferType = "credit_released"
	CreditBreakage           TransferType = "credit_breakage"
	CreditForfeited          TransferType = "credit_forfeited"
	CreditForfeitReversed    TransferType = "credit_forfeit_reversed"
	PromotionalCreditApplied TransferType = "promotional_credit_applied"
	UsageAccrued             TransferType = "usage_accrued"
	UsageCollected           TransferType = "usage_collected"
	UsageRepaid              TransferType = "usage_repaid"
	UsageWaived              TransferType = "usage_waived"
	UsageReopened            TransferType = "usage_reopened"
	BadDebtWrittenOff        TransferType = "bad_debt"
	BadDebtRecovered         TransferType = "bad_debt_recovery"
	ProcessorFee             TransferType = "processor_fee"
	ChargebackFee            TransferType = "chargeback_fee"
	OpeningBalance           TransferType = "opening_balance"
)

// GeneralTransferTypes is the general ledger's whole vocabulary.
var GeneralTransferTypes = []TransferType{
	CashReceived, CashApplied, CashReturned, RevenueAdjusted, RevenueRecognized,
	RefundDeferred, RefundRecognized, ChargebackDeferred, ChargebackRecognized, DisputeWon,
	CreditReleased, CreditBreakage, CreditForfeited, CreditForfeitReversed, PromotionalCreditApplied,
	UsageAccrued, UsageCollected, UsageRepaid, UsageWaived, UsageReopened, UsageReversed,
	BadDebtWrittenOff, BadDebtRecovered, ProcessorFee, ChargebackFee, OpeningBalance,
}
