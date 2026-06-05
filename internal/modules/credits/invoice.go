package credits

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

// FinalizeInvoice builds and finalizes the monthly itemized invoice for
// (owner, credit_type) over [from, to) (issue #303). Line items are rolled up
// from billing.usage_events; money movements + totals from the credit ledger;
// both snapshotted on the immutable finalized invoice. Idempotent: re-finalizing
// the same period returns the existing invoice.
//
// The invoice is a STATEMENT. For prepaid it is informational (usage was drawn
// from balance); for arrears the owed_accrued total is what the #301 sweep
// settles via the existing charge path. No charge is initiated here.
func (s *CreditsService) FinalizeInvoice(ctx context.Context, owner identity.OwnerOrgID, creditType string, from, to time.Time) (*models.Invoice, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	if owner.IsZero() {
		return nil, fmt.Errorf("owner required")
	}
	if !to.After(from) {
		return nil, fmt.Errorf("invalid period: to must be after from")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTenantTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	ownerID := owner.UUID()
	from, to = from.UTC(), to.UTC()

	// Idempotency: one invoice per (owner, credit_type, period).
	existing := new(models.Invoice)
	err = tx.NewSelect().Model(existing).
		Where("tenant_id = ? AND owner_id = ? AND credit_type_id = ?", tenantID, ownerID, ct.ID).
		Where("period_from = ? AND period_to = ?", from, to).
		Limit(1).Scan(ctx)
	if err == nil {
		return existing, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// --- usage line items (per event_type) ---
	type totalRow struct {
		EventType   string `bun:"event_type"`
		TotalAmount int64  `bun:"total_amount"`
		EventCount  int64  `bun:"event_count"`
	}
	var totals []totalRow
	if err := tx.NewSelect().
		Model((*models.UsageEvent)(nil)).
		ColumnExpr("event_type").
		ColumnExpr("COALESCE(SUM(amount), 0) AS total_amount").
		ColumnExpr("COUNT(*) AS event_count").
		Where("tenant_id = ? AND owner_id = ? AND credit_type_id = ?", tenantID, ownerID, ct.ID).
		Where("occurred_at >= ? AND occurred_at < ?", from, to).
		GroupExpr("event_type").
		Scan(ctx, &totals); err != nil {
		return nil, err
	}
	items := map[string]*models.InvoiceLineItem{}
	for _, t := range totals {
		items[t.EventType] = &models.InvoiceLineItem{EventType: t.EventType, Amount: t.TotalAmount, Count: t.EventCount, Dimensions: map[string]int64{}}
	}

	type dimRow struct {
		EventType string `bun:"event_type"`
		Key       string `bun:"key"`
		Total     int64  `bun:"total"`
	}
	var dims []dimRow
	if err := tx.NewSelect().
		TableExpr("billing.usage_events AS ue").
		ColumnExpr("ue.event_type AS event_type").
		ColumnExpr("d.key AS key").
		ColumnExpr("COALESCE(SUM((d.value)::bigint), 0) AS total").
		Join("CROSS JOIN LATERAL jsonb_each_text(ue.dimensions) AS d").
		Where("ue.tenant_id = ? AND ue.owner_id = ? AND ue.credit_type_id = ?", tenantID, ownerID, ct.ID).
		Where("ue.occurred_at >= ? AND ue.occurred_at < ?", from, to).
		GroupExpr("ue.event_type, d.key").
		Scan(ctx, &dims); err != nil {
		return nil, err
	}
	for _, d := range dims {
		if it, ok := items[d.EventType]; ok {
			it.Dimensions[d.Key] = d.Total
		}
	}

	lineItems := make([]models.InvoiceLineItem, 0, len(items))
	var usageTotal int64
	for _, it := range items {
		usageTotal += it.Amount
		lineItems = append(lineItems, *it)
	}

	// --- money movements (ledger, by transaction_type) ---
	type movRow struct {
		Type  string `bun:"transaction_type"`
		Total int64  `bun:"total"`
	}
	var movs []movRow
	if err := tx.NewSelect().
		Model((*models.CreditTransaction)(nil)).
		ColumnExpr("transaction_type").
		ColumnExpr("COALESCE(SUM(amount), 0) AS total").
		Where("tenant_id = ? AND owner_id = ? AND credit_type_id = ?", tenantID, ownerID, ct.ID).
		Where("created_at >= ? AND created_at < ?", from, to).
		GroupExpr("transaction_type").
		Scan(ctx, &movs); err != nil {
		return nil, err
	}
	movements := map[string]int64{}
	for _, m := range movs {
		movements[m.Type] = m.Total
	}

	// --- closing balance snapshot ---
	bal := new(models.UserCreditBalance)
	balErr := tx.NewSelect().Model(bal).
		Where("tenant_id = ? AND owner_id = ? AND credit_type_id = ?", tenantID, ownerID, ct.ID).
		Limit(1).Scan(ctx)
	if balErr != nil && !errors.Is(balErr, sql.ErrNoRows) {
		return nil, balErr
	}

	now := s.now()
	inv := &models.Invoice{
		ID:             uuidutil.NewV7(),
		TenantID:       tenantID,
		OwnerID:        ownerID,
		CreditTypeID:   ct.ID,
		Currency:       ct.Unit,
		PeriodFrom:     from,
		PeriodTo:       to,
		UsageTotal:     usageTotal,
		DepositsTotal:  movements["deposit"],
		OwedAccrued:    movements[txOwedAccrual],
		OwedPaid:       -movements[txOwedPayment], // owed_payment amounts are negative
		ClosingBalance: bal.Balance,
		LineItems:      lineItems,
		MoneyMovements: movements,
		Status:         "finalized",
		FinalizedAt:    &now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if _, err := tx.NewInsert().Model(inv).Exec(ctx); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return inv, nil
}

// ListInvoices lists an owner org's finalized invoices, newest period first,
// paginated (issue #303). It filters owner_id directly (the payer) and is
// RLS-scoped to the request tenant via Q(ctx), mirroring GetTransactionsByOwner.
// Returns the page plus the total count for pagination.
func (s *CreditsService) ListInvoices(ctx context.Context, owner identity.OwnerOrgID, limit, offset int) ([]models.Invoice, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("credits service not initialized")
	}
	if owner.IsZero() {
		return nil, 0, fmt.Errorf("owner required")
	}
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var items []models.Invoice
	var total int
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		q := s.db.Q(ctx).NewSelect().Model(&items).
			Where("tenant_id = ?", tenant.FromContextOrDefault(ctx).UUID()).
			Where("owner_id = ?", owner.UUID())
		var e error
		total, e = q.Count(ctx)
		if e != nil {
			return e
		}
		return q.OrderExpr("period_from DESC").Limit(limit).Offset(offset).Scan(ctx)
	})
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// GetInvoiceByID returns one finalized invoice (with its snapshotted line items)
// for an owner org by id (issue #303). It filters tenant + owner + id and is
// RLS-scoped via Q(ctx); an invoice belonging to another owner/tenant is
// unreachable (fail closed, sql.ErrNoRows).
func (s *CreditsService) GetInvoiceByID(ctx context.Context, owner identity.OwnerOrgID, id uuid.UUID) (*models.Invoice, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	if owner.IsZero() {
		return nil, fmt.Errorf("owner required")
	}
	inv := new(models.Invoice)
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		return s.db.Q(ctx).NewSelect().Model(inv).
			Where("tenant_id = ?", tenant.FromContextOrDefault(ctx).UUID()).
			Where("owner_id = ? AND id = ?", owner.UUID(), id).
			Limit(1).Scan(ctx)
	})
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// FinalizeDueInvoices finalizes the [from, to) invoice for every known account
// (every owner+credit_type with a balance row) in the request tenant. Idempotent
// per account. Returns the number of invoices finalized/returned.
func (s *CreditsService) FinalizeDueInvoices(ctx context.Context, from, to time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("credits service not initialized")
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	type acct struct {
		OwnerID    string `bun:"owner_id"`
		CreditType string `bun:"credit_type_name"`
	}
	var accts []acct
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		return s.db.Q(ctx).NewSelect().
			TableExpr("billing.user_credit_balances AS b").
			ColumnExpr("b.owner_id::text AS owner_id").
			ColumnExpr("ct.name AS credit_type_name").
			Join("JOIN billing.credit_types AS ct ON ct.id = b.credit_type_id").
			Where("b.tenant_id = ?", tenantID).
			GroupExpr("b.owner_id, ct.name").
			Scan(ctx, &accts)
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, a := range accts {
		owner := identity.OwnerOrgIDFromString(a.OwnerID)
		if owner.IsZero() {
			continue
		}
		if _, err := s.FinalizeInvoice(ctx, owner, a.CreditType, from, to); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
