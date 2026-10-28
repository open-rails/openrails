package billingimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

type (
	DeclaredPurchase   = billing.DeclaredPurchase
	DeclaredOrderLine  = billing.DeclaredOrderLine
	DeclaredSettlement = billing.DeclaredSettlement
)

// FindingPurchaseRefused is a declared purchase the import refused: its facts
// do not add up to a settled sale it can record.
const FindingPurchaseRefused = "life.import.purchase_refused"

// purchaseSource is a declared purchase's bill identity, keyed by its source id.
const purchaseSource = "import_purchase"

const (
	maxPurchaseLines    = 100
	maxPurchaseEvidence = 64 << 10
)

// plannedPurchase is a declared purchase whose facts add up.
type plannedPurchase struct {
	decl       DeclaredPurchase
	psp        uuid.UUID
	currency   string
	listAmount int64
	lines      []plannedLine
}

type plannedLine struct {
	price       gen.BillingPrice
	product     gen.BillingProduct
	quantity    *int
	unit        int64
	amount      int64
	description string
	accessEnds  *time.Time
}

// providerTransactionID: NMI and CCBill issue digits-only transaction ids, so
// anything else (a host's synthetic id) is not the provider's record.
func providerTransactionID(rail, id string) bool {
	switch strings.ToLower(strings.TrimSpace(rail)) {
	case string(models.RailNMI), string(models.RailCCBill):
	default:
		return strings.TrimSpace(id) == id && id != ""
	}
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// planPurchases checks the book's purchases inside its transaction. Every
// purchase names its PSP like any provider row (one that cannot refuses the
// import); one whose facts do not add up is refused on its own, with a finding.
func planPurchases(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, psps *pspResolver, declared []DeclaredPurchase, asOf time.Time, res *Result) ([]plannedPurchase, error) {
	var out []plannedPurchase
	sources, transactions := map[string]bool{}, map[string]bool{}
	for i, p := range declared {
		if strings.TrimSpace(p.SourceID) == "" {
			return nil, apperr.Invalidf("declared purchase %d has no source_id", i).WithParam("purchases")
		}
		psp, err := psps.resolve(p.PSP, p.Rail, fmt.Sprintf("purchase %s", p.SourceID))
		if err != nil {
			return nil, err
		}
		refuse := func(reason string) error {
			return refusePurchase(ctx, q, merchantID, psp, p, reason, res)
		}
		plan, reason, err := planPurchase(ctx, q, merchantID, psp, p, asOf)
		if err != nil {
			return nil, err
		}
		key := psp.String() + "\x1f" + p.TransactionID
		switch {
		case sources[p.SourceID]:
			reason = "duplicate source_id in batch"
		case reason == "" && transactions[key]:
			reason = "duplicate transaction_id in batch"
		}
		sources[p.SourceID], transactions[key] = true, true
		if reason != "" {
			if err := refuse(reason); err != nil {
				return nil, err
			}
			continue
		}
		if err := db.EnsureCustomerRowQ(ctx, q, merchantID, p.Customer.UUID()); err != nil {
			return nil, fmt.Errorf("ensure customer %s: %w", p.Customer, err)
		}
		out = append(out, plan)
	}
	return out, nil
}

// planPurchase returns why a purchase cannot be recorded, or its plan.
func planPurchase(ctx context.Context, q *gen.Queries, merchantID, psp uuid.UUID, p DeclaredPurchase, asOf time.Time) (plannedPurchase, string, error) {
	plan := plannedPurchase{decl: p, psp: psp, currency: moneyutil.NormalizeCurrency(p.Currency)}
	switch {
	case p.Customer.IsZero():
		return plan, "customer is required", nil
	case strings.TrimSpace(p.TransactionID) == "":
		return plan, "transaction_id is required", nil
	case !providerTransactionID(p.Rail, p.TransactionID):
		return plan, "transaction_id is not a " + p.Rail + " transaction id", nil
	case moneyutil.RequireFiatCurrency(plan.currency) != nil || p.Amount <= 0:
		return plan, "no fiat amount: the sale has no recorded USD (or other fiat) value", nil
	case p.PaidAt.IsZero() || p.PaidAt.After(asOf):
		return plan, "paid_at is required and cannot be after as_of", nil
	case len(p.Lines) == 0 || len(p.Lines) > maxPurchaseLines:
		return plan, fmt.Sprintf("a purchase has between 1 and %d lines", maxPurchaseLines), nil
	case len(p.Evidence) > maxPurchaseEvidence:
		return plan, "evidence exceeds 64 KiB", nil
	case p.Settlement != nil && (strings.TrimSpace(p.Settlement.Asset) == "" || !digits(p.Settlement.Quantity)):
		return plan, "settlement requires its asset and its quantity in base units", nil
	}
	if _, err := moneyutil.NativeToRailMinorExact(plan.currency, p.Amount); err != nil {
		return plan, "amount: " + err.Error(), nil
	}
	var total int64
	for i, l := range p.Lines {
		priceID, err := q.LookupPriceStorageID(ctx, gen.LookupPriceStorageIDParams{MerchantID: merchantID, PublicID: l.Price.String()})
		if errors.Is(err, pgx.ErrNoRows) {
			return plan, fmt.Sprintf("line %d: price %s not found", i, l.Price), nil
		} else if err != nil {
			return plan, "", err
		}
		price, err := q.GetPriceByID(ctx, gen.GetPriceByIDParams{MerchantID: merchantID, ID: priceID})
		if err != nil {
			return plan, "", err
		}
		product, err := q.GetProductByID(ctx, gen.GetProductByIDParams{MerchantID: merchantID, ID: price.ProductID})
		if err != nil {
			return plan, "", err
		}
		seats := int64(1)
		if l.Quantity != nil {
			if *l.Quantity < 1 || *l.Quantity > math.MaxInt32 {
				return plan, fmt.Sprintf("line %d: quantity must be positive", i), nil
			}
			seats = int64(*l.Quantity)
		}
		switch {
		case price.Currency != plan.currency:
			return plan, fmt.Sprintf("line %d: price %s is in %s, not %s", i, l.Price, price.Currency, plan.currency), nil
		case l.UnitAmount < 0 || l.UnitAmount > math.MaxInt64/seats:
			return plan, fmt.Sprintf("line %d: unit_amount is out of range", i), nil
		case l.AccessEndsAt != nil && !l.AccessEndsAt.After(p.PaidAt):
			return plan, fmt.Sprintf("line %d: access_ends_at must be after paid_at", i), nil
		}
		amount := l.UnitAmount * seats
		if total > math.MaxInt64-amount || plan.listAmount > math.MaxInt64-price.Amount*seats {
			return plan, "line total overflows", nil
		}
		total += amount
		plan.listAmount += price.Amount * seats
		description := strings.TrimSpace(l.Description)
		plan.lines = append(plan.lines, plannedLine{price: price, product: product, quantity: l.Quantity, unit: l.UnitAmount, amount: amount, description: description, accessEnds: l.AccessEndsAt})
	}
	if total != p.Amount {
		return plan, fmt.Sprintf("amount %s differs from the line total %s", moneyutil.FormatAmount(p.Amount, plan.currency), moneyutil.FormatAmount(total, plan.currency)), nil
	}
	return plan, "", nil
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// refusePurchase reports a refused purchase and keeps it as a finding.
func refusePurchase(ctx context.Context, q *gen.Queries, merchantID, psp uuid.UUID, p DeclaredPurchase, reason string, res *Result) error {
	res.Blocked = append(res.Blocked, p.SourceID)
	res.Reasons[p.SourceID] = reason
	evidence, _ := json.Marshal(map[string]any{"source_id": p.SourceID, "psp_id": billing.PSPID(psp).String(), "transaction_id": p.TransactionID, "reason": reason})
	action := "Declared purchase " + p.SourceID + " was not recorded (" + reason + "). Fix the declared facts and import it again."
	_, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: merchantID,
		FindingType: FindingPurchaseRefused, SubjectKey: "purchase:" + p.SourceID, Severity: "high", Status: "requires_review",
		RecommendedAction: &action, Evidence: evidence})
	return err
}

// recordPurchases records each planned purchase in its own transaction: a
// complete engine-owned order with frozen lines, its succeeded rail payment
// and the access its lines grant. No provider is called.
func recordPurchases(ctx context.Context, database *db.DB, merchantID uuid.UUID, plans []plannedPurchase, now time.Time, res *Result) error {
	for _, p := range plans {
		created := false
		err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			created, err = recordPurchase(ctx, database.NewWithPgxTx(tx), merchantID, p, now)
			return err
		})
		switch {
		case errors.Is(err, observed.ErrConflict):
			if err := refusePurchase(ctx, database.Gen(ctx), merchantID, p.psp, p.decl,
				"conflicts with a purchase already recorded under this source_id or transaction_id", res); err != nil {
				return err
			}
		case err != nil:
			return fmt.Errorf("record purchase %s: %w", p.decl.SourceID, err)
		case created:
			res.Imported = append(res.Imported, p.decl.SourceID)
		default:
			res.Skipped = append(res.Skipped, p.decl.SourceID)
		}
	}
	return nil
}

func recordPurchase(ctx context.Context, d *db.DB, merchantID uuid.UUID, p plannedPurchase, now time.Time) (bool, error) {
	q := d.Gen(ctx)
	// A replay keeps the accepted lines' frozen names.
	var accepted observed.Bill
	if prior, err := q.GetProviderBill(ctx, gen.GetProviderBillParams{MerchantID: merchantID, PspID: p.psp, SourceKind: purchaseSource, SourceKey: p.decl.SourceID}); err == nil {
		if err := json.Unmarshal(prior.Snapshot, &accepted); err != nil {
			return false, err
		}
	} else if !db.IsNotFound(err) {
		return false, err
	}
	lines := make([]observed.Line, len(p.lines))
	for i, l := range p.lines {
		description := l.description
		if description == "" {
			description = strings.TrimSpace(l.product.DisplayName)
			if i < len(accepted.Lines) && accepted.Lines[i].PriceID != nil && *accepted.Lines[i].PriceID == l.price.ID {
				description = accepted.Lines[i].Description
			}
			if description == "" {
				description = "Purchase " + l.price.PublicID
			}
		}
		lines[i] = observed.Line{PriceID: &l.price.ID, ProductID: &l.price.ProductID, Description: description, Quantity: l.quantity,
			UnitAmount: l.unit, Amount: l.amount, AccessDurationHours: models.DerefIntPtr(l.price.AccessDurationHours)}
	}
	metadata := map[string]any{"import_source_id": p.decl.SourceID}
	if s := p.decl.Settlement; s != nil {
		metadata["settlement"] = s
	}
	if len(p.decl.Evidence) > 0 {
		metadata["evidence"] = p.decl.Evidence
	}
	result, err := observed.Record(ctx, d, observed.Bill{PSPID: p.psp, CustomerID: p.decl.Customer.UUID(), Owner: observed.OwnerEngine, Kind: "purchase",
		SourceKind: purchaseSource, SourceKey: p.decl.SourceID, ProviderReference: p.decl.TransactionID, Currency: p.currency, Total: p.decl.Amount, Lines: lines,
		Payment: &models.Payment{CustomerID: p.decl.Customer.UUID(), Channel: models.ChannelRail, Rail: models.Rail(strings.ToLower(strings.TrimSpace(p.decl.Rail))),
			TransactionID: p.decl.TransactionID, Amount: p.decl.Amount, ListAmount: p.listAmount, Currency: p.currency, Status: payments.PaymentStatusSucceededValue,
			MoneyMovement: models.MoneyMovementRail, PurchasedAt: p.decl.PaidAt.UTC(), CreatedAt: now, Metadata: metadata},
		Settled: true, SettlementSource: "cash"}, now)
	if err != nil {
		return false, err
	}
	return result.Created, grantPurchaseAccess(ctx, q, merchantID, p, result.Payment.ID)
}

// grantPurchaseAccess grants each product its lines bought, from paid_at to
// the latest of its lines' ends (a line without access_ends_at takes its
// price's access duration). A credit pack's credits are not deposited: an
// imported sale does not know what was spent.
func grantPurchaseAccess(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, p plannedPurchase, paymentID uuid.UUID) error {
	start := p.decl.PaidAt.UTC().Truncate(time.Microsecond)
	type window struct {
		end        *time.Time
		indefinite bool
	}
	ends := map[uuid.UUID]*window{}
	var order []uuid.UUID
	for _, l := range p.lines {
		if len(l.product.CreditGrant) > 0 && string(l.product.CreditGrant) != "null" {
			continue
		}
		end := l.accessEnds
		if end == nil {
			end = grants.AccessWindow(models.DerefIntPtr(l.price.AccessDurationHours), start).End
		}
		w, ok := ends[l.product.ID]
		if !ok {
			w = &window{}
			ends[l.product.ID] = w
			order = append(order, l.product.ID)
		}
		switch {
		case end == nil:
			w.indefinite, w.end = true, nil
		case !w.indefinite && (w.end == nil || end.After(*w.end)):
			e := end.UTC().Truncate(time.Microsecond)
			w.end = &e
		}
	}
	ledger := grants.New(q, merchantID)
	for _, product := range order {
		w := ends[product]
		g, created, err := ledger.GrantAccessOnce(ctx, grants.GrantInput{Customer: p.decl.Customer.UUID(), Product: &product, Kind: grants.Access,
			Source: grants.Purchase, SourceID: paymentID.String(), Payment: &paymentID, StartsAt: start, EndsAt: w.end})
		if err != nil {
			return err
		}
		if created {
			if err := ledger.MaterializeGrant(ctx, g); err != nil {
				return err
			}
		}
	}
	return nil
}

// attributeCharges keeps the book's charges it cannot record as payments: a
// synthetic id as a finding, and an approved charge without an amount as an
// attribution of its provider transaction to its subscription, read from the
// provider after the import. It returns the attributions it declared.
func attributeCharges(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, facts []reconcile.DeclaredSubscriptionFact, attributed, unverified []DeclaredTransaction, now time.Time) ([]reconcile.DeclaredAttributions, error) {
	if len(attributed) == 0 && len(unverified) == 0 {
		return nil, nil
	}
	byRail := map[string]reconcile.DeclaredSubscriptionFact{}
	for _, f := range facts {
		byRail[f.RailSubscriptionID] = f
	}
	subscription := func(t DeclaredTransaction) (reconcile.DeclaredSubscriptionFact, *gen.BillingSubscription, error) {
		f := byRail[t.RailSubscriptionID]
		sub, err := q.GetSubscriptionByPSPSubID(ctx, gen.GetSubscriptionByPSPSubIDParams{MerchantID: merchantID, PspID: f.PspID, Rail: f.Rail, RailSubscriptionID: f.RailSubscriptionID})
		if errors.Is(err, pgx.ErrNoRows) {
			return f, nil, nil
		}
		return f, &sub, err
	}
	for _, t := range unverified {
		f, sub, err := subscription(t)
		if err != nil {
			return nil, err
		}
		var id *uuid.UUID
		if sub != nil {
			id = &sub.ID
		}
		if err := reconcile.RecordUnverifiedTransaction(ctx, q, merchantID, f.PspID, t.TransactionID, id, "the transaction id is not the provider's: NMI and CCBill ids are digits"); err != nil {
			return nil, err
		}
	}
	ids := map[uuid.UUID][]string{}
	for _, t := range attributed {
		f, sub, err := subscription(t)
		if err != nil {
			return nil, err
		}
		if sub == nil {
			if err := reconcile.RecordUnverifiedTransaction(ctx, q, merchantID, f.PspID, t.TransactionID, nil, "its subscription was not imported"); err != nil {
				return nil, err
			}
			continue
		}
		rail := sub.Rail
		if _, err := q.GetPaymentByPSPTransactionID(ctx, gen.GetPaymentByPSPTransactionIDParams{MerchantID: merchantID, PspID: &sub.PspID,
			Channel: string(models.ChannelRail), Rail: &rail, TransactionID: t.TransactionID}); err == nil {
			continue // already recorded
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		var declared *time.Time
		if !t.OccurredAt.IsZero() {
			at := t.OccurredAt.UTC()
			declared = &at
		}
		created, err := q.InsertChargeAttribution(ctx, gen.InsertChargeAttributionParams{MerchantID: merchantID, PspID: sub.PspID, TransactionID: t.TransactionID,
			CustomerID: sub.CustomerID, SubscriptionID: sub.ID, DeclaredAt: declared, Now: now})
		if err != nil {
			return nil, err
		}
		if created == 1 {
			ids[sub.PspID] = append(ids[sub.PspID], t.TransactionID)
		}
		existing, err := q.GetChargeAttribution(ctx, gen.GetChargeAttributionParams{MerchantID: merchantID, PspID: sub.PspID, TransactionID: t.TransactionID})
		if err != nil {
			return nil, err
		}
		if existing.SubscriptionID != sub.ID {
			if err := reconcile.RecordUnverifiedTransaction(ctx, q, merchantID, sub.PspID, t.TransactionID, &sub.ID, "the transaction is already attributed to another subscription"); err != nil {
				return nil, err
			}
		}
	}
	out := make([]reconcile.DeclaredAttributions, 0, len(ids))
	for _, psp := range slices.SortedFunc(maps.Keys(ids), func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) }) {
		out = append(out, reconcile.DeclaredAttributions{PSPID: psp, TransactionIDs: ids[psp]})
	}
	return out, nil
}
