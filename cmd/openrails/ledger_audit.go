package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/pkg/merchant"
)

// newLedgerAuditCmd wires the #833 ledger-integrity diagnostics as an operator
// command:
//
//	openrails ledger-audit [--merchant=<slug|id>] [--format=table|json]
//
// Read-only. Exits non-zero on any finding so it can be wired to alerting.
func newLedgerAuditCmd() *cobra.Command {
	var (
		merchantSlug string
		format       string
	)
	cmd := &cobra.Command{
		Use:   "ledger-audit",
		Short: "Verify double-entry ledger integrity (#833): per-(merchant,currency) conservation and account counters vs the transfer log (read-only)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runLedgerAudit(c, merchantSlug, format)
		},
	}
	cmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant slug or id (default: every merchant)")
	cmd.Flags().StringVar(&format, "format", "table", "Output format: table, json")
	return cmd
}

type ledgerAuditResult struct {
	MerchantID   uuid.UUID              `json:"merchant_id"`
	MerchantSlug string                 `json:"merchant_slug"`
	Report       ledger.IntegrityReport `json:"report"`
	Err          string                 `json:"error,omitempty"`
}

func runLedgerAudit(cmd *cobra.Command, merchantSlug, format string) error {
	cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	if cfg == nil || cfg.DB == nil {
		return fmt.Errorf("config not loaded")
	}
	database, err := db.NewDB(cfg.DB)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer func() { _ = database.Close() }()

	targets, err := ledgerAuditTargets(cmd.Context(), database, merchantSlug)
	if err != nil {
		return err
	}

	results := make([]ledgerAuditResult, 0, len(targets))
	breaches := 0
	for _, target := range targets {
		res := ledgerAuditResult{MerchantID: target.id.UUID(), MerchantSlug: target.slug}
		ctx := merchant.WithID(cmd.Context(), target.id)
		runErr := database.RunInMerchantConn(ctx, func(ctx context.Context) error {
			// The connection is already RLS-scoped to this merchant; the explicit
			// id keeps the predicate honest on a privileged connection too.
			rep, err := ledger.CheckIntegrity(ctx, database.Qx(ctx), target.id.UUID())
			res.Report = rep
			return err
		})
		if runErr != nil {
			res.Err = runErr.Error()
		}
		if !res.Report.OK() || res.Err != "" {
			breaches++
		}
		results = append(results, res)
	}

	if strings.EqualFold(strings.TrimSpace(format), "json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"merchants": results, "unhealthy": breaches}); err != nil {
			return err
		}
	} else {
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "MERCHANT\tCONSERVATION\tCOUNTERS\tDETAIL")
		for _, res := range results {
			detail := ""
			switch {
			case res.Err != "":
				detail = res.Err
			case !res.Report.OK():
				parts := make([]string, 0, len(res.Report.Conservation)+len(res.Report.Counters))
				for _, b := range res.Report.Conservation {
					parts = append(parts, b.String())
				}
				for _, d := range res.Report.Counters {
					parts = append(parts, d.String())
				}
				detail = strings.Join(parts, "; ")
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				ledgerAuditLabel(res),
				ledgerAuditVerdict(len(res.Report.Conservation)),
				ledgerAuditVerdict(len(res.Report.Counters)),
				detail)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	if breaches > 0 {
		return fmt.Errorf("ledger integrity FAILED for %d of %d merchant(s)", breaches, len(results))
	}
	fmt.Printf("\nLedger integrity OK across %d merchant(s).\n", len(results))
	return nil
}

func ledgerAuditVerdict(findings int) string {
	if findings == 0 {
		return "ok"
	}
	return fmt.Sprintf("FAIL(%d)", findings)
}

func ledgerAuditLabel(res ledgerAuditResult) string {
	if res.MerchantSlug != "" {
		return res.MerchantSlug
	}
	return res.MerchantID.String()
}

type ledgerAuditTarget struct {
	id   merchant.ID
	slug string
}

// ledgerAuditTargets resolves the merchants to audit: one when --merchant is
// given, otherwise every merchant on file (this is an on-demand operator sweep,
// not a routine per-record job).
func ledgerAuditTargets(ctx context.Context, database *db.DB, merchantSlug string) ([]ledgerAuditTarget, error) {
	if strings.TrimSpace(merchantSlug) != "" {
		id, err := resolveCLIMerchant(ctx, database, merchantSlug)
		if err != nil {
			return nil, err
		}
		return []ledgerAuditTarget{{id: id, slug: strings.TrimSpace(merchantSlug)}}, nil
	}
	rows, err := database.DataPool().Query(ctx,
		`SELECT id::text, slug FROM openrails.merchants ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("list merchants: %w", err)
	}
	defer rows.Close()

	var out []ledgerAuditTarget
	for rows.Next() {
		var rawID, slug string
		if err := rows.Scan(&rawID, &slug); err != nil {
			return nil, fmt.Errorf("list merchants: %w", err)
		}
		id, err := merchant.ParseID(rawID)
		if err != nil {
			return nil, fmt.Errorf("parse merchant id %q: %w", rawID, err)
		}
		out = append(out, ledgerAuditTarget{id: id, slug: slug})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list merchants: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no merchants on file")
	}
	return out, nil
}
