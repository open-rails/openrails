package merchants

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/pagination"
)

// The operator's merchant directory (#721, #1173). Soft delete is directory
// state only: the merchant leaves the default lists and its credentials
// resolve nothing, and every record stays. The gated purge (Delete) is the
// only path that destroys rows.

// ListDirectory pages the directory, newest first: active merchants unless
// params name statuses, Query matching part of the current name.
func (s *Service) ListDirectory(ctx context.Context, params billing.MerchantListParams) (*billing.ListPage[billing.Merchant], error) {
	limit, err := pagination.Limit(params.PageRequest)
	if err != nil {
		return nil, err
	}
	var status *string
	switch statuses := slices.Compact(slices.Sorted(slices.Values(params.Statuses))); {
	case len(statuses) == 0:
		active := string(billing.MerchantActive)
		status = &active
	case len(statuses) == 1:
		one := string(statuses[0])
		status = &one
	}
	for _, st := range params.Statuses {
		if st != billing.MerchantActive && st != billing.MerchantDeleted {
			return nil, fmt.Errorf("%w: merchant status %q", billing.ErrInvalid, st)
		}
	}
	afterAt, afterID, err := pagination.After(params.Cursor)
	if err != nil {
		return nil, err
	}
	var query *string
	if q := strings.TrimSpace(params.Query); q != "" {
		query = &q
	}
	rows, err := s.database.GenDirectory().ListPlatformMerchants(ctx, gen.ListPlatformMerchantsParams{
		Status: status, Query: query, AfterAt: afterAt, AfterID: afterID, RowLimit: pagination.Fetch(limit),
	})
	if err != nil {
		return nil, err
	}
	cut := pagination.Cut(rows, limit, func(row gen.ListPlatformMerchantsRow) any { return pagination.TimeID{At: row.CreatedAt, ID: row.ID} })
	out := &billing.ListPage[billing.Merchant]{Items: make([]billing.Merchant, 0, len(cut.Items)), Next: cut.Next}
	for _, row := range cut.Items {
		m := s.directoryMerchant(ctx, row.ID, row.Slug, row.Status)
		m.CreatedAt, m.UpdatedAt, m.DeletedAt = row.CreatedAt, row.UpdatedAt, row.DeletedAt
		if err := s.withActivity(ctx, &m); err != nil {
			return nil, err
		}
		out.Items = append(out.Items, m)
	}
	return out, nil
}

// DirectoryEntry is one merchant in any status; ErrMerchantNotFound, which
// is also billing.ErrNotFound, when none.
func (s *Service) DirectoryEntry(ctx context.Context, id billing.MerchantID) (*billing.Merchant, error) {
	row, err := s.database.GenDirectory().GetPlatformMerchant(ctx, id.UUID())
	if err != nil {
		return nil, directoryError(id, err)
	}
	m := s.directoryMerchant(ctx, row.ID, row.Slug, row.Status)
	m.CreatedAt, m.UpdatedAt, m.DeletedAt = row.CreatedAt, row.UpdatedAt, row.DeletedAt
	if err := s.withActivity(ctx, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// SoftDelete tombstones a merchant; deleting a deleted one changes nothing.
func (s *Service) SoftDelete(ctx context.Context, id billing.MerchantID) (*billing.Merchant, error) {
	row, err := s.database.GenDirectory().SoftDeletePlatformMerchant(ctx, id.UUID())
	if err != nil {
		return nil, directoryError(id, err)
	}
	m := s.directoryMerchant(ctx, row.ID, row.Slug, row.Status)
	m.CreatedAt, m.UpdatedAt, m.DeletedAt = row.CreatedAt, row.UpdatedAt, row.DeletedAt
	return &m, nil
}

// Restore clears a merchant's tombstone; restoring an active one changes
// nothing. A retired merchant is billing.ErrConflict, and a name another
// merchant took meanwhile billing.ErrMerchantNameTaken.
func (s *Service) Restore(ctx context.Context, id billing.MerchantID) (*billing.Merchant, error) {
	row, err := s.database.GenDirectory().RestorePlatformMerchant(ctx, id.UUID())
	if err != nil {
		return nil, directoryError(id, err)
	}
	m := s.directoryMerchant(ctx, row.ID, row.Slug, row.Status)
	m.CreatedAt, m.UpdatedAt, m.DeletedAt = row.CreatedAt, row.UpdatedAt, row.DeletedAt
	return &m, nil
}

// directoryMerchant is a directory row with the display name its
// configuration sets.
func (s *Service) directoryMerchant(ctx context.Context, id uuid.UUID, slug, status string) billing.Merchant {
	m := billing.Merchant{ID: billing.MerchantID(id), Slug: slug, Status: billing.MerchantStatus(status), RailsArmed: []billing.Rail{}}
	if name := s.DisplayName(ctx, m.ID); name != "" {
		m.DisplayName = &name
	}
	return m
}

// withActivity adds the rails of the merchant's live PSPs and its latest
// payment: merchant-owned rows, one indexed probe each.
func (s *Service) withActivity(ctx context.Context, m *billing.Merchant) error {
	return s.pool.MerchantTx(ctx, m.ID, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		rails, err := q.ListPlatformMerchantRailsArmed(ctx, m.ID.UUID())
		if err != nil {
			return err
		}
		for _, rail := range rails {
			m.RailsArmed = append(m.RailsArmed, billing.Rail(rail))
		}
		last, err := q.GetPlatformMerchantLastPayment(ctx, m.ID.UUID())
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		m.LastPaymentAt = &last
		return nil
	})
}

func directoryError(id billing.MerchantID, err error) error {
	var pg *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: %s (%w)", ErrMerchantNotFound, id, billing.ErrNotFound)
	case errors.As(err, &pg) && pg.Code == "23514":
		return fmt.Errorf("%w: a retired or purged merchant cannot be restored", billing.ErrConflict)
	case errors.As(err, &pg) && pg.Code == "23505":
		return fmt.Errorf("%w: %w", billing.ErrMerchantNameTaken, err)
	}
	return err
}
