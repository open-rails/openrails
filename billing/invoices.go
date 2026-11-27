package billing

import (
	"time"

	"github.com/open-rails/openrails/catalog"
)

// InvoiceStatus is where an invoice stands, as Stripe's Invoice: open (it takes
// payment or awaits the customer's action), processing (the provider has its
// payment), complete (paid and fulfilled), canceled or expired (a purchase
// closed unpaid), void (a renewal its subscription's end left unowed, or a
// usage invoice the merchant voided) or uncollectible (a usage invoice the
// merchant wrote off; collecting it reopens it).
type InvoiceStatus string

const (
	InvoiceOpen       InvoiceStatus = "open"
	InvoiceProcessing InvoiceStatus = "processing"
	InvoiceComplete   InvoiceStatus = "complete"
	InvoiceCanceled   InvoiceStatus = "canceled"
	InvoiceExpired    InvoiceStatus = "expired"
	InvoiceVoid       InvoiceStatus = "void"
	// InvoiceUncollectible is a usage invoice written off.
	InvoiceUncollectible InvoiceStatus = "uncollectible"
)

// InvoicePaymentStatus is where an invoice's payment stands, as a Stripe
// PaymentIntent's: a decline returns it to requires_payment_method with
// LastPaymentError.
type InvoicePaymentStatus string

const (
	InvoicePaymentRequiresPaymentMethod InvoicePaymentStatus = "requires_payment_method"
	// InvoicePaymentRequiresAction waits for the customer: NextAction says how.
	InvoicePaymentRequiresAction InvoicePaymentStatus = "requires_action"
	InvoicePaymentProcessing     InvoicePaymentStatus = "processing"
	InvoicePaymentSucceeded      InvoicePaymentStatus = "succeeded"
)

// InvoiceOrigin says who created an invoice.
type InvoiceOrigin string

const (
	InvoiceOriginCustomer InvoiceOrigin = "customer"
	InvoiceOriginMerchant InvoiceOrigin = "merchant"
	// InvoiceOriginSubscription is a renewal of an engine-owned subscription:
	// one per period, due when it starts. Its automatic collection and
	// retries are its attempts; a failed one leaves it open for the customer
	// to pay.
	InvoiceOriginSubscription InvoiceOrigin = "subscription"
	// InvoiceOriginUsage is owed usage billed in arrears: one per customer,
	// currency and period (or, past the customer's threshold, the period so
	// far), one line per accrual source, due after the customer's net terms.
	// A funded balance and remittances may pay part of it.
	InvoiceOriginUsage InvoiceOrigin = "usage"
)

// MaxInvoiceLines bounds one invoice.
const MaxInvoiceLines = 100

// InvoiceLineParams is one line to buy: a price and how many. Quantity is
// seats on a per-seat price (omitted: its minimum), units of a consumable
// product (omitted: 1) and 1 on another one-time price; any other recurring
// price takes none (quantity_not_allowed). Amount is what the customer
// chose on a price whose customer picks it (a deposit, a top-up), within
// its bounds; a fixed price takes none.
type InvoiceLineParams struct {
	PriceID  PriceID `json:"price_id"`
	Quantity *int    `json:"quantity,omitempty"`
	Amount   *int64  `json:"amount,omitempty,string"`
}

// InvoicePaymentParams names how to pay: exactly one of a saved card
// (PaymentMethodID), a card just entered (Token), a Solana wallet
// (TokenSymbol, the token it pays in) or the PSP's hosted page (ReturnURL).
type InvoicePaymentParams struct {
	PaymentMethodID *PaymentMethodID `json:"payment_method_id,omitempty"`
	// Token is the single-use output of the PSP's own card fields: a Stripe
	// pm_ from Elements, an NMI Collect.js payment_token. The card is saved and
	// charged in the same call; the charge is its storing transaction.
	Token string `json:"token,omitempty"`
	// PSPID is the PSP whose fields made Token; optional while one payment
	// option takes new cards.
	PSPID          *PSPID          `json:"psp_id,omitempty"`
	BillingDetails *BillingDetails `json:"billing_details,omitempty"`
	// TokenSymbol pays from a Solana wallet in this token (one of GET
	// /v1/me/config's payment.solana.tokens): the invoice answers requires_action
	// with a solana_pay next_action, a Solana Pay transaction request the
	// wallet completes.
	TokenSymbol string `json:"token_symbol,omitempty"`
	// ReturnURL pays on the PSP's hosted page (Stripe Checkout, an option
	// that accepts redirect): the invoice answers requires_action with a
	// redirect_to_url next action, and the customer comes back to ReturnURL.
	ReturnURL string `json:"return_url,omitempty"`
}

// PreviewMyInvoiceParams prices lines for the signed-in customer without
// creating an invoice: Lines, or the customer's cart (Cart true).
type PreviewMyInvoiceParams struct {
	Lines []InvoiceLineParams `json:"lines,omitempty"`
	Cart  bool                `json:"cart,omitempty"`
}

// CreateMyInvoiceParams creates the signed-in customer's invoice and, with
// Payment, pays it in the same call. ExpectedTotal is required with Payment:
// a different total refuses the invoice with invoice_total_changed. Reusable
// keeps a new card for one-click buys where that needs the customer's opt-in
// (billing or card country in the EEA or the UK); elsewhere every new card is
// kept. Without Payment the invoice is open for POST /v1/me/invoices/{id}/pay,
// and asking again for the same Lines answers the customer's open invoice of
// them instead of a second one. Cart true buys the customer's cart instead of
// Lines: the invoice takes the cart's lines in the transaction that creates it.
type CreateMyInvoiceParams struct {
	Lines         []InvoiceLineParams   `json:"lines,omitempty"`
	Cart          bool                  `json:"cart,omitempty"`
	ExpectedTotal *int64                `json:"expected_total,omitempty,string"`
	Payment       *InvoicePaymentParams `json:"payment,omitempty"`
	Reusable      *bool                 `json:"reusable,omitempty"`
	// PayURL asks, instead of Payment, for the invoice's embedded pay URL:
	// Invoice.PayURL on the checkout origin, fresh on every answer (the last
	// one stops working).
	PayURL *MyInvoicePayURLParams `json:"pay_url,omitempty"`
	// Attribution is where the host's page says the customer came from.
	// It comes from the browser, unverified; the customer's open invoice of
	// the same lines keeps its first.
	Attribution *InvoiceAttribution `json:"attribution,omitempty"`
}

// InvoiceAttribution is where the merchant says a checkout came from: the
// marketing source, medium, campaign, term and content (as utm_*), the
// referrer and an affiliate. Each is optional, 1 to 100 bytes of printable text without
// "@" or a card number; Referrer is a URL or host, kept as its lowercase
// host only.
type InvoiceAttribution struct {
	Source    *string `json:"source"`
	Medium    *string `json:"medium"`
	Campaign  *string `json:"campaign"`
	Term      *string `json:"term"`
	Content   *string `json:"content"`
	Referrer  *string `json:"referrer"`
	Affiliate *string `json:"affiliate"`
}

// CountrySource is which evidence an invoice's country came from.
type CountrySource string

const (
	// CountrySourceBillingAddress is the billing address the customer gave.
	CountrySourceBillingAddress CountrySource = "billing_address"
	// CountrySourceCard is the card's issuing country, as its PSP reports it.
	CountrySourceCard CountrySource = "card"
	// CountrySourceIP is the country a trusted proxy reported for the
	// customer's request; the address itself is never stored.
	CountrySourceIP CountrySource = "ip"
)

// CustomerType is who a checkout's customer was when it was created.
type CustomerType string

const (
	// CustomerTypeNew is a first purchase by a customer who never
	// subscribed.
	CustomerTypeNew CustomerType = "new"
	// CustomerTypeReturning bought before, or subscribes now.
	CustomerTypeReturning CustomerType = "returning"
	// CustomerTypeReactivated subscribed before and subscribes to nothing
	// now.
	CustomerTypeReactivated CustomerType = "reactivated"
)

// InvoiceCheckout is what OpenRails recorded of a checkout (an invoice it
// started for its customer to pay): which purchase it is for the customer,
// how far they got, where they are, the language they read and the
// merchant's attribution. Countries are ISO 3166-1 alpha-2; Country is the
// billing address's, else the card's, else the IP's, as CountrySource says.
type InvoiceCheckout struct {
	CustomerType CustomerType `json:"customer_type"`
	// PurchaseNumber is 1 + the customer's paid purchases with the merchant
	// when the checkout was created.
	PurchaseNumber int `json:"purchase_number"`
	// OpenedAt is when the customer first read the open checkout in person
	// (the payment form loaded); SubmittedAt when they first asked to pay it.
	OpenedAt       *time.Time     `json:"opened_at"`
	SubmittedAt    *time.Time     `json:"submitted_at"`
	Country        *string        `json:"country"`
	CountrySource  *CountrySource `json:"country_source"`
	BillingCountry *string        `json:"billing_country"`
	CardCountry    *string        `json:"card_country"`
	IPCountry      *string        `json:"ip_country"`
	// Language is what the customer's browser preferred, language[-region].
	Language    *string            `json:"language"`
	Attribution InvoiceAttribution `json:"attribution"`
}

// PreviewInvoiceParams prices lines for one customer, as a merchant invoice
// would freeze them.
type PreviewInvoiceParams struct {
	CustomerID CustomerID          `json:"customer_id"`
	Lines      []InvoiceLineParams `json:"lines"`
}

// CreateInvoiceParams creates a merchant invoice: a bill for one customer,
// which they pay on the merchant's own site (POST /v1/me/invoices/{id}/pay),
// or, with PayURL, at its pay URL. It never charges and never expires: it
// stays open until paid or canceled. A pay URL expires on its own.
type CreateInvoiceParams struct {
	CustomerID CustomerID          `json:"customer_id"`
	Lines      []InvoiceLineParams `json:"lines"`
	// PayURL asks for the invoice's pay URL (Invoice.PayURL), where the
	// customer pays. The deployment must serve a checkout origin.
	PayURL *InvoicePayURLParams `json:"pay_url,omitempty"`
	// Attribution is where the customer came from, as the host knows it.
	Attribution *InvoiceAttribution `json:"attribution,omitempty"`
}

// PayInvoiceParams pays an open invoice; Reusable as in CreateMyInvoiceParams.
type PayInvoiceParams struct {
	Payment       InvoicePaymentParams `json:"payment"`
	ExpectedTotal int64                `json:"expected_total,string"`
	Reusable      *bool                `json:"reusable,omitempty"`
}

// ConfirmInvoiceParams confirms an invoice awaiting its customer. Signature is
// the transaction a Solana wallet sent for it: OpenRails reads it at once.
type ConfirmInvoiceParams struct {
	Signature *string `json:"signature,omitempty"`
}

// InvoiceCollectionOwner names the authority allowed to collect an invoice.
// Provider and NMI schedule invoices cannot be paid through generic invoice APIs.
type InvoiceCollectionOwner string

const (
	InvoiceCollectionEngine      InvoiceCollectionOwner = "engine"
	InvoiceCollectionNMISchedule InvoiceCollectionOwner = "nmi_schedule"
	InvoiceCollectionProvider    InvoiceCollectionOwner = "provider"
)

// Invoice is one bill with frozen lines and total. Its collection owner decides
// how it is paid; its id is not a credential. Payments record money that
// moved and always name their invoice; a refund or dispute names the original
// payment and invoice.
//
// Provider-run bills are invoices too: an NMI schedule's renewal
// (CollectionOwner InvoiceCollectionNMISchedule) or a Stripe or CCBill bill
// (InvoiceCollectionProvider), with CollectionPSPID when known and
// ProviderBills holding the provider's document references and verified
// settlement facts. Such an invoice takes no saved-card collection from
// OpenRails; the server refuses pay, confirm, remittance and retries on it.
// The provider's confirmed amount, currency, service dates and terms are
// frozen: catalog edits never reprice them. A renewal, an enrollment and an
// adjustment are distinct bills; a decline then a successful retry is one.
// Replayed or reordered webhooks reuse the accepted bill and payment, and a
// financial identity reused with another amount, currency, customer or invoice
// is refused. NMI and CCBill receipts match the accepted schedule version in
// force; an extra capture is kept as money for review and grants nothing. A
// charge before the first accepted cycle is settled history
// (provider_pre_schedule): a complete invoice that renews and grants nothing.
type Invoice struct {
	ID         InvoiceID  `json:"id"`
	CustomerID CustomerID `json:"customer_id"`
	// CustomerContact is the customer's directory contact on a staff list;
	// absent when the directory holds none or could not answer.
	CustomerContact *CustomerContact `json:"customer_contact,omitempty"`
	Origin          InvoiceOrigin    `json:"origin"`
	Status          InvoiceStatus    `json:"status"`
	// CollectionOwner is frozen when the bill is accepted. Only engine
	// invoices accept generic pay, confirm and remittance commands.
	CollectionOwner InvoiceCollectionOwner `json:"collection_owner"`
	// CollectionPSPID is the account responsible for collection, when known.
	CollectionPSPID *PSPID `json:"collection_psp_id"`
	// ProviderBills are verified provider document references and settlement
	// facts. They never authorize local collection.
	ProviderBills []InvoiceProviderBill `json:"provider_bills"`
	// Number is the invoice number: taken when the invoice completes, unique per merchant, not consecutive.
	Number   *int64           `json:"number"`
	Currency catalog.Currency `json:"currency"`
	Total    int64            `json:"total,string"`
	Lines    []InvoiceLine    `json:"lines"`
	Payment  InvoicePayment   `json:"payment"`
	// PaymentOptions are the PSPs that can take this invoice while it is open.
	PaymentOptions []InvoicePaymentOption `json:"payment_options"`
	// PayURL is where the customer pays the invoice on the checkout origin,
	// /pay/<invoice>#<secret>; the secret stays in the fragment and is stored
	// hashed. Creation and a re-ask while the invoice is open answer it;
	// other reads are null.
	PayURL *string `json:"pay_url"`
	// PayURLExpiresAt is when the pay URL stops working; null without one.
	PayURLExpiresAt *time.Time `json:"pay_url_expires_at"`
	// Embedded: the merchant's page frames the pay URL.
	Embedded bool `json:"embedded"`
	// SuccessURL and CancelURL are where a merchant invoice's pay URL returns
	// and its back link; a customer's embedded one has neither.
	SuccessURL          *string `json:"success_url"`
	CancelURL           *string `json:"cancel_url"`
	SavedPaymentMethods bool    `json:"saved_payment_methods"`
	// Renewal is the subscription period a renewal invoice (origin
	// subscription) bills; null on any other.
	Renewal *InvoiceRenewal `json:"renewal"`
	// Usage is a usage invoice's period, collection and document fields; null
	// on any other.
	Usage *InvoiceUsage `json:"usage"`
	// ExpiresAt is when a customer's unpaid purchase (origin customer)
	// expires, a day after it was created; any other invoice has none.
	ExpiresAt   *time.Time `json:"expires_at"`
	CompletedAt *time.Time `json:"completed_at"`
	CanceledAt  *time.Time `json:"canceled_at"`
	ExpiredAt   *time.Time `json:"expired_at"`
	VoidedAt    *time.Time `json:"voided_at"`
	// UncollectibleAt is when a usage invoice was written off.
	UncollectibleAt *time.Time `json:"uncollectible_at"`
	CreatedAt       time.Time  `json:"created_at"`
	// Checkout is what OpenRails recorded of a checkout, on the merchant's
	// reads; null on the customer's own and on invoices that are not
	// checkouts.
	Checkout *InvoiceCheckout `json:"checkout"`
}

// InvoiceProviderBill is a verified provider document attached to an invoice.
// SettlementSource distinguishes gateway cash, credit, out_of_band or mixed
// settlement. A null source/settled_at means settlement is not yet verified.
type InvoiceProviderBill struct {
	PSPID            PSPID      `json:"psp_id"`
	Kind             string     `json:"kind"`
	Reference        string     `json:"reference"`
	SettlementSource *string    `json:"settlement_source"`
	NonCashAmount    int64      `json:"non_cash_amount,string"`
	SettledAt        *time.Time `json:"settled_at"`
}

// InvoiceRenewal is the period of a subscription a renewal invoice bills. It is
// due when the period starts; open past DueAt, it is overdue.
type InvoiceRenewal struct {
	SubscriptionID SubscriptionID `json:"subscription_id"`
	PeriodStartsAt time.Time      `json:"period_starts_at"`
	PeriodEndsAt   time.Time      `json:"period_ends_at"`
	DueAt          time.Time      `json:"due_at"`
	// CheckoutInvoiceID is the checkout that bought the subscription; null
	// when none did (imported, started at its provider, or bought before
	// checkouts were recorded).
	CheckoutInvoiceID *InvoiceID `json:"checkout_invoice_id"`
}

// InvoiceUsage is what a usage invoice bills and how it is collected: the
// period of its charges, when it is due, how much of its total is paid
// (a funded balance and remittances pay part of it), and the document fields
// of the customer's invoice profile when it was billed.
type InvoiceUsage struct {
	PeriodStartsAt time.Time `json:"period_starts_at"`
	PeriodEndsAt   time.Time `json:"period_ends_at"`
	DueAt          time.Time `json:"due_at"`
	AmountPaid     int64     `json:"amount_paid,string"`
	AmountDue      int64     `json:"amount_due,string"`
	// CollectionMethod is charge_automatically (the currency's collection
	// card is charged) or send_invoice (the customer remits).
	CollectionMethod        InvoiceCollectionMethod `json:"collection_method"`
	NextCollectionAttemptAt *time.Time              `json:"next_collection_attempt_at"`
	PONumber                *string                 `json:"po_number"`
	Memo                    *string                 `json:"memo"`
	Tax                     map[string]any          `json:"tax"`
	BillingContacts         []InvoiceContact        `json:"billing_contacts"`
	// Delinquent: the invoice is overdue and its customer's grace has run out,
	// so their new usage in its currency is refused.
	Delinquent bool `json:"delinquent"`
}

// InvoicePayment is an invoice's payment, as a Stripe PaymentIntent reads.
type InvoicePayment struct {
	Status InvoicePaymentStatus `json:"status"`
	// NextAction is what the customer completes while Status is
	// requires_action.
	NextAction *NextAction `json:"next_action"`
	// LastPaymentError is why the latest attempt failed.
	LastPaymentError *PaymentFailure `json:"last_payment_error"`
	// PaymentMethodID is the card the latest attempt charged.
	PaymentMethodID *PaymentMethodID `json:"payment_method_id"`
	// PaymentID is the money that moved.
	PaymentID *PaymentID `json:"payment_id"`
}

// InvoiceLine is one line of an invoice, and what paying it produced.
type InvoiceLine struct {
	ID InvoiceLineID `json:"id"`
	// PriceID, ProductID and Ownership are what a purchased line sold; null
	// on a usage line, whose Source names its accrual source instead.
	PriceID     *PriceID   `json:"price_id"`
	ProductID   *ProductID `json:"product_id"`
	Source      *string    `json:"source"`
	Description string     `json:"description"`
	// Quantity is null on a recurring line of a price without seats.
	Quantity   *int               `json:"quantity"`
	UnitAmount int64              `json:"unit_amount,string"`
	Amount     int64              `json:"amount,string"`
	Ownership  *catalog.Ownership `json:"ownership"`
	// BillingIntervalHours is set on a recurring line, whose subscription
	// renews every interval (for Quantity seats on a per-seat price).
	BillingIntervalHours  *int             `json:"billing_interval_hours"`
	BillingIntervalMonths *int             `json:"billing_interval_months"`
	AccessDurationHours   *int             `json:"access_duration_hours"`
	SubscriptionID        *SubscriptionID  `json:"subscription_id"`
	ProductAccessID       *ProductAccessID `json:"product_access_id"`
}

// InvoicePaymentOption is one PSP that can take an invoice's lines, and what it
// accepts: saved_card and new_card on a card PSP; redirect (its hosted page)
// on Stripe for one-time lines and on CCBill for one subscription;
// solana_pay on Solana. FailedAttempts counts the invoice's failed payments
// through the PSP; 0 before the invoice exists.
type InvoicePaymentOption struct {
	// SavedPaymentMethodIDs are platform cards this customer may reuse through
	// this PSP. PSP-held cards remain identified by their own psp_id.
	SavedPaymentMethodIDs []PaymentMethodID `json:"saved_payment_method_ids,omitempty"`
	PSPID                 PSPID             `json:"psp_id"`
	Rail                  Rail              `json:"rail"`
	Accepts               []string          `json:"accepts"`
	FailedAttempts        int               `json:"failed_attempts"`
	// Requires is what the customer must add before this PSP can take the
	// invoice: "verified_email" and "username" for a CCBill page. Omitted when
	// nothing is missing, and on the merchant's reads.
	Requires []string `json:"requires,omitempty"`
}

// InvoicePreview prices lines as an invoice would freeze them.
type InvoicePreview struct {
	Currency       catalog.Currency       `json:"currency"`
	Total          int64                  `json:"total,string"`
	Lines          []InvoicePreviewLine   `json:"lines"`
	PaymentOptions []InvoicePaymentOption `json:"payment_options"`
}

// InvoicePreviewLine is one priced line, or why it cannot be bought.
type InvoicePreviewLine struct {
	PriceID               PriceID             `json:"price_id"`
	ProductID             ProductID           `json:"product_id"`
	Description           string              `json:"description"`
	Quantity              *int                `json:"quantity"`
	UnitAmount            int64               `json:"unit_amount,string"`
	Amount                int64               `json:"amount,string"`
	Ownership             catalog.Ownership   `json:"ownership"`
	BillingIntervalHours  *int                `json:"billing_interval_hours"`
	BillingIntervalMonths *int                `json:"billing_interval_months"`
	AccessDurationHours   *int                `json:"access_duration_hours"`
	Refusal               *InvoiceLineRefusal `json:"refusal"`
}

// InvoiceLineRefusal is why a line cannot be bought. Code is already_owned,
// entitlement_required, unavailable, currency_mismatch, quantity_invalid,
// quantity_not_allowed, amount_not_allowed, amount_required or amount_invalid.
// An already_owned line names its owner (a subscription, product access or an
// unpaid invoice) and a hint: change (change that subscription instead) or
// resume (pay that invoice, or resume that subscription). An
// entitlement_required line lists the product's required entitlements the
// customer lacks in Missing.
type InvoiceLineRefusal struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	OwnedBy *string  `json:"owned_by"`
	Hint    *string  `json:"hint"`
	Missing []string `json:"missing,omitempty"`
}

// InvoiceListParams selects invoices, newest first; every filter is optional.
// PriceID names invoices with a line on that price; SubscriptionID a
// subscription's renewals; Overdue the open invoices past their due date. A
// customer's own list ignores CustomerID. IDs instead reads 1 to
// MaxBatchItems named invoices in one page; unknown ones are absent.
type InvoiceListParams struct {
	IDs            []InvoiceID    `form:"ids,comma"`
	CustomerID     CustomerID     `form:"customer_id"`
	PriceID        PriceID        `form:"price_id"`
	SubscriptionID SubscriptionID `form:"subscription_id"`
	Status         InvoiceStatus  `form:"status"`
	Origin         InvoiceOrigin  `form:"origin"`
	Overdue        bool           `form:"overdue"`
	PageRequest
}
