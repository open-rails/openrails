package solana

import (
	"context"
	"fmt"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// SolanaPayService puts a transaction request's reference under the poller's
// watch.
type SolanaPayService struct {
	db    *db.DB
	clock clockwork.Clock
}

// MintInfoSource reads a mint's current token program, fee and hook.
type MintInfoSource interface {
	MintInfo(ctx context.Context, mint solanago.PublicKey) (*solanarpc.MintInfo, error)
}

// NewSolanaPayService creates a new SolanaPayService.
func NewSolanaPayService(db *db.DB, clocks ...clockwork.Clock) *SolanaPayService {
	return &SolanaPayService{db: db, clock: timeutil.FirstClock(clocks...)}
}

func (s *SolanaPayService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// RequireMintInfo reads a mint's current program, fee and hook through an
// armed reader.
func RequireMintInfo(ctx context.Context, chain MintInfoSource, mint string) (*solanarpc.MintInfo, error) {
	if chain == nil {
		return nil, fmt.Errorf("solana: no mint reader armed (mint %s)", mint)
	}
	key, err := solanago.PublicKeyFromBase58(strings.TrimSpace(mint))
	if err != nil {
		return nil, fmt.Errorf("solana: invalid mint %q: %w", mint, err)
	}
	return chain.MintInfo(ctx, key)
}

// RegisterReference gives a transaction-request order attempt its one
// reference and puts it under the poller's watch. Idempotent per attempt.
func (s *SolanaPayService) RegisterReference(ctx context.Context, kind ReferenceKind, sessionID uuid.UUID, reference string, quoteExpiresAt time.Time) (gen.BillingSolanaPayReference, error) {
	return NewPayLedger(s.db).Register(ctx, kind, sessionID, strings.TrimSpace(reference), quoteExpiresAt, s.now())
}
