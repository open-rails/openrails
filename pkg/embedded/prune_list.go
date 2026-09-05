package embedded

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PruneListOptions mirrors `openrails prune list`.
type PruneListOptions struct {
	Config     *config.Config
	PGXPool    *pgxpool.Pool
	MerchantID merchant.ID
	Limit      int
	Format     string
	Out        io.Writer
}

// PruneList shows this merchant's prune runs, newest first — the ids
// `openrails undo-run --run` takes.
func PruneList(ctx context.Context, opts PruneListOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	// Host-supplied page size: clamp AT the narrowing so a caller's huge value
	// cannot wrap to a negative LIMIT.
	if limit > math.MaxInt32 {
		limit = math.MaxInt32
	}
	lim := int32(limit)
	database, err := openEmbeddedDB(ctx, opts.Config, opts.PGXPool)
	if err != nil {
		return err
	}
	defer database.Close()

	merchantID := opts.MerchantID
	if err := database.RequireMerchantID(ctx, merchantID); err != nil {
		return err
	}
	ctx = merchant.WithID(ctx, merchantID)

	var runs []gen.OpenrailsDestructiveRun
	if err := database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		runs, e = database.Gen(ctx).ListDestructiveRuns(ctx, gen.ListDestructiveRunsParams{
			MerchantID: merchantID.UUID(), Lim: lim,
		})
		return e
	}); err != nil {
		return err
	}

	if strings.EqualFold(opts.Format, "json") {
		return json.NewEncoder(opts.Out).Encode(runs)
	}
	if len(runs) == 0 {
		fmt.Fprintln(opts.Out, "no destructive runs recorded for this merchant")
		return nil
	}
	for i := range runs {
		r := &runs[i]
		fmt.Fprintf(opts.Out, "%s  %-8s  %-9s  actor=%-16s  started=%s  affected=%s\n",
			r.ID, r.Kind, r.Status, r.Actor, r.StartedAt.UTC().Format("2006-01-02T15:04:05Z"), string(r.Affected))
	}
	return nil
}
