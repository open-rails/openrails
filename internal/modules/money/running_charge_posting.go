package money

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
)

// A running charge reaches the ledger in the usage postings
// (usage_posting.go): the daily one through the accounting zone's last
// midnight, and the ones before a usage invoice bills, a credit lot it draws
// is expired, revoked or refunded, or a merchant is purged, through their
// instant. Each is a charge line of the posting's receipt, from its last
// posting to that instant or its stop. A stop posts nothing; until a posting
// takes it, a balance read adds the accrual from the charge's row.

// runningChargeOwedSource is the owed items' source of what running charges
// leave owed: a usage invoice's line.
const runningChargeOwedSource = "running_charge"

// ledgerPosting is what a posting's ledger write runs under: accrual is held
// as of accruedAsOf, and the lots live just before liveAt are drawn.
type ledgerPosting struct {
	accruedAsOf, liveAt time.Time
	// owed splits what the posting leaves owed, each share dated.
	owed []owedShare
}

// owedShare is one part of what a posting leaves owed: its owed item's
// source (metered:<meter>, running_charge), its weight, when its charges
// began (where the usage invoice that bills it starts) and when it is dated.
type owedShare struct {
	source   string
	weight   int64
	from, at time.Time
}

func (s *MoneyService) accruedAsOf() *time.Time {
	if s.posting == nil {
		return nil
	}
	return &s.posting.accruedAsOf
}

func (s *MoneyService) lotsLiveAt() *time.Time {
	if s.posting == nil {
		return nil
	}
	t := s.posting.liveAt.Add(-time.Microsecond)
	return &t
}

// settleRunningCharges posts everything the customer's usage and running
// charges cost through now, in the caller's transaction: before a credit lot
// is revoked, so the lot funds what came before. Lots that lapsed are
// expired first, each after what came before its lapse.
func (s *MoneyService) settleRunningCharges(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string) error {
	_, err := s.creditLots(q, merchantID).SettleRunningCharges(ctx, customer, currency, s.now())
	return err
}
