package hosttools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/merchant"
)

// NMIDeclineReportOptions mirrors `openrails nmi decline-report`.
type NMIDeclineReportOptions struct {
	Config     *config.Config
	PGXPool    *pgxpool.Pool
	MerchantID merchant.ID
	// PSP pins the NMI account; empty reads the merchant's armed one.
	PSP string
	// Since is required; Until defaults to now (RFC3339 or YYYY-MM-DD).
	Since, Until string
	// Format is table (default) or json.
	Format               string
	MerchantManifest     *BillingConfig
	MerchantManifestPath string
	// NMITransport replaces the NMI wire (a test seam); nil is the gateway.
	NMITransport http.RoundTripper
	Out          io.Writer
}

// NMIDeclineReport reads an NMI account's authorization history through the
// Query API and reports approval and refusal rates by month and kind, and
// each refusal's reason and category from the one decline classifier
// (#1114). It is read-only: nothing is written anywhere.
func NMIDeclineReport(ctx context.Context, opts NMIDeclineReportOptions) error {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	since, err := parsePullProviderTime(opts.Since, "since")
	if err != nil {
		return err
	}
	if since.IsZero() {
		return errors.New("--since is required")
	}
	until, err := parsePullProviderTime(opts.Until, "until")
	if err != nil {
		return err
	}
	if until.IsZero() {
		until = time.Now().UTC()
	}
	rt, cleanup, err := newPullProviderRuntime(ctx, PullProviderOptions{Config: opts.Config, PGXPool: opts.PGXPool, MerchantID: opts.MerchantID,
		MerchantManifest: opts.MerchantManifest, MerchantManifestPath: opts.MerchantManifestPath})
	if err != nil {
		return err
	}
	defer cleanup()
	ctx = merchant.WithID(ctx, opts.MerchantID)
	return rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		pins := map[reconcile.Provider]string{}
		if strings.TrimSpace(opts.PSP) != "" {
			provider, binding, err := resolvePullPSPTarget(ctx, rt, opts.PSP)
			if err != nil {
				return err
			}
			if provider != reconcile.ProviderNMI {
				return fmt.Errorf("--provider-account %s is a %s account, not NMI", opts.PSP, provider)
			}
			pins[provider] = binding.AccountID
		}
		armed := reconcile.MerchantFetcherBuilder{StripeClients: rt.StripeClients, Config: rt.Config, Merchants: rt.Merchants, DB: rt.DB,
			AccountIDs: pins, NMIClients: nmiClients(opts.NMITransport)}.Build(ctx, opts.MerchantID)
		client, psp := reconcile.NMIReader(armed)
		if client == nil {
			return errors.New("no armed NMI account for this merchant")
		}
		history, err := client.DeclineHistory(ctx, since, until)
		if err != nil {
			return err
		}
		report := newDeclineReport(history)
		report.PSP = psp
		return report.render(opts.Out, opts.Format)
	})
}

type declineReport struct {
	PSP      uuid.UUID        `json:"psp_id"`
	Since    time.Time        `json:"since"`
	Until    time.Time        `json:"until"`
	Months   []declineMonth   `json:"months"`
	Refusals []declineRefusal `json:"refusals"`
	// Undated authorizations carry a date NMI garbled.
	Undated int      `json:"undated,omitempty"`
	Notes   []string `json:"notes"`
}

type declineMonth struct {
	Month    string `json:"month"`
	Kind     string `json:"kind"`
	Attempts int    `json:"attempts"`
	Approved int    `json:"approved"`
	Refused  int    `json:"refused"`
	// RefusalRate is refused / attempts, in percent.
	RefusalRate float64 `json:"refusal_rate"`
}

type declineRefusal struct {
	Kind     string `json:"kind"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
	Count    int    `json:"count"`
}

// newDeclineReport lays the history out as the report prints it: rates by
// month and kind, then refusals by reason over the whole window.
func newDeclineReport(h nmi.DeclineHistory) declineReport {
	report := declineReport{Since: h.Since, Until: h.Until, Undated: h.Undated, Notes: nmi.HistoryNotes}
	months := map[[2]string]*declineMonth{}
	refusals := map[declineRefusal]int{}
	for _, c := range h.Counts {
		key := [2]string{c.Month.Format("2006-01"), c.Kind}
		m := months[key]
		if m == nil {
			m = &declineMonth{Month: key[0], Kind: c.Kind}
			months[key] = m
		}
		m.Attempts += c.Count
		if c.Category == string(decline.Approved) {
			m.Approved += c.Count
			continue
		}
		m.Refused += c.Count
		refusals[declineRefusal{Kind: c.Kind, Category: c.Category, Reason: c.Reason}] += c.Count
	}
	order := map[string]int{nmi.HistoryVerification: 0, nmi.HistoryOneOffSale: 1, nmi.HistoryScheduledRebill: 2}
	for _, m := range months {
		m.RefusalRate = float64(m.Refused*1000/m.Attempts) / 10
		report.Months = append(report.Months, *m)
	}
	slices.SortFunc(report.Months, func(a, b declineMonth) int {
		return cmp.Or(cmp.Compare(a.Month, b.Month), cmp.Compare(order[a.Kind], order[b.Kind]))
	})
	for r, n := range refusals {
		r.Count = n
		report.Refusals = append(report.Refusals, r)
	}
	slices.SortFunc(report.Refusals, func(a, b declineRefusal) int {
		return cmp.Or(cmp.Compare(order[a.Kind], order[b.Kind]), cmp.Compare(b.Count, a.Count), cmp.Compare(a.Reason, b.Reason))
	})
	return report
}

func (r declineReport) render(out io.Writer, format string) error {
	if format == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	fmt.Fprintf(out, "NMI decline baseline: PSP %s, %s to %s\n\n", r.PSP, r.Since.Format(time.DateOnly), r.Until.Format(time.DateOnly))
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "month\tkind\tattempts\tapproved\trefused\trefusal rate\t")
	for _, m := range r.Months {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%.1f%%\t\n", m.Month, m.Kind, m.Attempts, m.Approved, m.Refused, m.RefusalRate)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out, "\nRefusals by reason")
	tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "kind\tcategory\treason\tcount")
	for _, x := range r.Refusals {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", x.Kind, x.Category, x.Reason, x.Count)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if r.Undated > 0 {
		fmt.Fprintf(out, "\n%d authorizations carry no readable date and are in no month.\n", r.Undated)
	}
	fmt.Fprintln(out, "\nNotes")
	for _, n := range r.Notes {
		fmt.Fprintf(out, "- %s\n", n)
	}
	return nil
}
