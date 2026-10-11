package hosttools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/proposals"
	"github.com/open-rails/openrails/internal/reconcile"
)

// ProposalsOptions mirrors `openrails proposals`: which merchant, who acts,
// and how to print.
type ProposalsOptions struct {
	Config     *config.Config
	PGXPool    *pgxpool.Pool
	MerchantID billing.MerchantID
	// Actor is the operator, recorded as cli:<actor> on what they resolve.
	Actor  string
	Format string
	Out    io.Writer
	// Clock is the approval's clock; nil is the wall clock.
	Clock clockwork.Clock
}

// ProposalSet names proposals: listed ids, or the open proposals a filter
// selects. A filtered set applies only when Expect equals its size.
type ProposalSet struct {
	IDs    []string
	PSP    string // a PSP key
	Kind   string
	Source string
	Expect int
}

func (s ProposalSet) filtered() bool { return len(s.IDs) == 0 }

func (o ProposalsOptions) json() bool { return strings.EqualFold(strings.TrimSpace(o.Format), "json") }

// withProposals runs fn on a merchant-scoped connection with the executor.
func withProposals(ctx context.Context, opts ProposalsOptions, fn func(ctx context.Context, database *db.DB, x *proposals.Executor) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	database, err := openEmbeddedDB(ctx, opts.Config, opts.PGXPool)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.RequireMerchantID(ctx, opts.MerchantID); err != nil {
		return err
	}
	// Approved refunds and provider cancels queue their jobs in the change's
	// transaction.
	if err := bindRiverProducer(ctx, opts.Config, database); err != nil {
		return err
	}
	ctx = merchant.WithID(ctx, opts.MerchantID)
	x := proposals.New(database, opts.Config, opts.Clock)
	return database.RunInMerchantConn(ctx, func(ctx context.Context) error { return fn(ctx, database, x) })
}

// bindRiverProducer binds an insert-only River client in the configured River
// schema, as the runtime does.
func bindRiverProducer(ctx context.Context, cfg *config.Config, database *db.DB) error {
	schema := config.RiverSchemaName(cfg)
	producer, err := river.NewClient(riverpgxv5.New(database.Pool()), &river.Config{Schema: schema, SkipUnknownJobCheck: true})
	if err != nil {
		return fmt.Errorf("River producer: %w", err)
	}
	if err := database.ValidateRiverJobBinding(ctx, database.Pool(), schema); err != nil {
		return fmt.Errorf("River producer: %w", err)
	}
	database.SetRiverJobInserter(producer)
	return nil
}

// ProposalsList prints the open proposals a set's filter selects (ids are
// ignored).
func ProposalsList(ctx context.Context, opts ProposalsOptions, set ProposalSet) error {
	return withProposals(ctx, opts, func(ctx context.Context, database *db.DB, x *proposals.Executor) error {
		items, more, err := selectProposals(ctx, database, x, set, 500)
		if err != nil {
			return err
		}
		if opts.json() {
			return encodeJSON(opts.Out, items)
		}
		printProposals(opts.Out, items)
		if more {
			fmt.Fprintln(opts.Out, "(more not shown: narrow with --psp, --kind or --source)")
		}
		return nil
	})
}

// ProposalsShow prints one proposal with its change.
func ProposalsShow(ctx context.Context, opts ProposalsOptions, id string) error {
	fid, err := billing.ParseFindingID(strings.TrimSpace(id))
	if err != nil || fid.IsZero() {
		return fmt.Errorf("proposals show: %q is not a finding id (fnd_…)", id)
	}
	return withProposals(ctx, opts, func(ctx context.Context, _ *db.DB, x *proposals.Executor) error {
		page, err := x.List(ctx, billing.ProposalListParams{IDs: []billing.FindingID{fid}})
		if err != nil {
			return err
		}
		if len(page.Items) == 0 {
			return fmt.Errorf("proposals show: %s is not a proposal of this merchant", fid)
		}
		return encodeJSON(opts.Out, page.Items[0])
	})
}

// ProposalsApprove approves a set: listed ids, or a filtered set whose size
// the operator typed back (--expect). Nothing applies on a mismatch.
func ProposalsApprove(ctx context.Context, opts ProposalsOptions, set ProposalSet, notes string) error {
	return resolveProposals(ctx, opts, set, notes, (*proposals.Executor).Approve)
}

// ProposalsDismiss dismisses a set; notes are required.
func ProposalsDismiss(ctx context.Context, opts ProposalsOptions, set ProposalSet, notes string) error {
	return resolveProposals(ctx, opts, set, notes, (*proposals.Executor).Dismiss)
}

func resolveProposals(ctx context.Context, opts ProposalsOptions, set ProposalSet, notes string,
	do func(*proposals.Executor, context.Context, proposals.Request) (billing.ProposalResults, error)) error {
	return withProposals(ctx, opts, func(ctx context.Context, database *db.DB, x *proposals.Executor) error {
		listing := opts.Out
		if opts.json() {
			listing = io.Discard
		}
		ids, err := setIDs(ctx, database, x, set, listing)
		if err != nil {
			return err
		}
		res, err := do(x, ctx, proposals.Request{IDs: ids, Notes: notes, Actor: "cli:" + opts.Actor})
		if err != nil {
			return err
		}
		if opts.json() {
			return encodeJSON(opts.Out, res)
		}
		printResults(opts.Out, res)
		return nil
	})
}

// setIDs resolves a set to ids. A filtered set prints itself and is refused
// unless --expect matches its size, which may not exceed MaxBatchItems.
func setIDs(ctx context.Context, database *db.DB, x *proposals.Executor, set ProposalSet, out io.Writer) ([]billing.FindingID, error) {
	if !set.filtered() {
		ids := make([]billing.FindingID, 0, len(set.IDs))
		for _, raw := range set.IDs {
			id, err := billing.ParseFindingID(strings.TrimSpace(raw))
			if err != nil || id.IsZero() {
				return nil, fmt.Errorf("%q is not a finding id (fnd_…)", raw)
			}
			ids = append(ids, id)
		}
		return ids, nil
	}
	if set.PSP == "" && set.Kind == "" && set.Source == "" {
		return nil, fmt.Errorf("name proposal ids, or a set with --psp, --kind or --source")
	}
	items, more, err := selectProposals(ctx, database, x, set, billing.MaxBatchItems)
	if err != nil {
		return nil, err
	}
	printProposals(out, items)
	if more {
		return nil, fmt.Errorf("the set holds more than %d proposals: narrow it with --psp, --kind or --source", billing.MaxBatchItems)
	}
	if set.Expect != len(items) {
		return nil, fmt.Errorf("refused: --expect %d does not match the %d proposal(s) listed above; nothing was applied", set.Expect, len(items))
	}
	ids := make([]billing.FindingID, len(items))
	for i, f := range items {
		ids[i] = f.ID
	}
	return ids, nil
}

// selectProposals reads up to limit open proposals the filter selects, and
// whether more remain.
func selectProposals(ctx context.Context, database *db.DB, x *proposals.Executor, set ProposalSet, limit int) ([]billing.Finding, bool, error) {
	params := billing.ProposalListParams{Kind: billing.ChangeKind(set.Kind), Source: billing.ProposalSource(set.Source)}
	if set.PSP != "" {
		psp, err := pspByKey(ctx, database, set.PSP)
		if err != nil {
			return nil, false, err
		}
		params.PSPID = billing.PSPID(psp)
	}
	var items []billing.Finding
	for {
		params.Limit = 100
		page, err := x.List(ctx, params)
		if err != nil {
			return nil, false, err
		}
		for _, f := range page.Items {
			if len(items) == limit {
				return items, true, nil
			}
			items = append(items, f)
		}
		if page.Next == "" {
			return items, false, nil
		}
		params.Cursor = page.Next
	}
}

func pspByKey(ctx context.Context, database *db.DB, key string) (uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	psps, err := database.Gen(ctx).ListPSPsForMerchant(ctx, mid.UUID())
	if err != nil {
		return uuid.Nil, err
	}
	for _, p := range psps {
		if p.Key == strings.TrimSpace(key) {
			return p.ID, nil
		}
	}
	return uuid.Nil, fmt.Errorf("no PSP %q for this merchant", key)
}

func printProposals(w io.Writer, items []billing.Finding) {
	if len(items) == 0 {
		fmt.Fprintln(w, "no open proposals")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTYPE\tKIND\tSUBJECT\tEFFECTS\tSOURCE\tOBSERVED")
	for _, f := range items {
		kind, subject, effects, observed := "-", f.SubjectKey, "-", "-"
		if c := f.Change; c != nil {
			kind, subject, observed = string(c.Kind), c.Subject.Type+" "+c.Subject.ID, c.ObservedAt.UTC().Format("2006-01-02T15:04:05Z")
			var e []string
			if c.Effects.EndsAccess {
				e = append(e, "ends_access")
			}
			if c.Effects.MovesMoney {
				e = append(e, "moves_money")
			}
			if c.Effects.ProviderWrite {
				e = append(e, "provider_write")
			}
			if len(e) > 0 {
				effects = strings.Join(e, ",")
			}
		}
		source := "-"
		if f.Source != nil {
			source = string(*f.Source)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", f.ID, f.Type, kind, subject, effects, source, observed)
	}
	_ = tw.Flush()
}

func printResults(w io.Writer, res billing.ProposalResults) {
	if res.RunID != nil {
		fmt.Fprintf(w, "approval run %s (reverse it with `openrails undo-run --run %s`)\n", res.RunID, res.RunID)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tOUTCOME\tDETAIL")
	for _, r := range res.Data {
		detail := ""
		switch {
		case r.Error != nil:
			detail = r.Error.Code + ": " + r.Error.Message
		case len(r.Execution) > 0:
			b, _ := json.Marshal(r.Execution)
			detail = string(b)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.ID, r.Outcome, detail)
	}
	_ = tw.Flush()
}

// ProposalsRuns lists the merchant's approval runs, newest first.
func ProposalsRuns(ctx context.Context, opts ProposalsOptions, limit int) error {
	if limit <= 0 {
		limit = 20
	}
	if limit > math.MaxInt32 {
		limit = math.MaxInt32
	}
	lim := int32(limit)
	return withProposals(ctx, opts, func(ctx context.Context, database *db.DB, _ *proposals.Executor) error {
		kind := reconcile.DestructiveRunKindApproval
		runs, err := database.Gen(ctx).ListDestructiveRuns(ctx, gen.ListDestructiveRunsParams{
			MerchantID: opts.MerchantID.UUID(), Kind: &kind, Lim: lim,
		})
		if err != nil {
			return err
		}
		if opts.json() {
			return encodeJSON(opts.Out, runs)
		}
		if len(runs) == 0 {
			fmt.Fprintln(opts.Out, "no approval runs recorded for this merchant")
			return nil
		}
		tw := tabwriter.NewWriter(opts.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "RUN\tSTATUS\tACTOR\tSTARTED\tAFFECTED")
		for i := range runs {
			r := &runs[i]
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", billing.RunID(r.ID), r.Status, models.DerefStr(r.Actor),
				r.StartedAt.UTC().Format("2006-01-02T15:04:05Z"), string(r.Affected))
		}
		return tw.Flush()
	})
}
