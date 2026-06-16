package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
)

// newIntentsCmd wires the read-only provider-intent-ledger (#358) CLI:
//
//	billing intents [--status=pending|all|...] [--provider=...] [--type=...] [--merchant=...]
//
// Under mode=limited/readonly this is the dry-run view: the pending rows are
// exactly the provider writes the executor will drain when the mode lifts —
// user/admin-origin intents under mode=limited, system-origin only under
// mode=full, nothing under readonly.
func newIntentsCmd() *cobra.Command {
	var (
		status       string
		provider     string
		intentType   string
		format       string
		merchantSlug string
		limit        int
	)
	cmd := &cobra.Command{
		Use:   "intents",
		Short: "List the provider intent ledger (#358): queued outbound provider mutations and which mode executes them (read-only)",
		RunE: func(c *cobra.Command, _ []string) error {
			return runIntentsList(c, status, provider, intentType, format, merchantSlug, limit)
		},
	}
	cmd.Flags().StringVar(&status, "status", "pending", "Status filter: pending, in_flight, succeeded, failed_retryable, failed (terminal), unknown, superseded, expired, all")
	cmd.Flags().StringVar(&provider, "provider", "", "Provider filter (e.g. mobius, stripe)")
	cmd.Flags().StringVar(&intentType, "type", "", "Intent type filter (e.g. nmi_delete_subscription, manual_rebill, stripe_refund)")
	cmd.Flags().StringVar(&format, "format", "table", "Output format: table, json")
	cmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant slug or id (default: the default merchant)")
	cmd.Flags().IntVar(&limit, "limit", 500, "Maximum rows to list")

	var (
		rfProvider string
		rfMerchant string
		rfYes      bool
	)
	refingerprintCmd := &cobra.Command{
		Use:   "refingerprint",
		Short: "Re-stamp LIVE intents of a provider to the CURRENT account fingerprint (#365 escape hatch after a confirmed same-account credential change)",
		RunE: func(c *cobra.Command, _ []string) error {
			return runIntentsRefingerprint(c, rfProvider, rfMerchant, rfYes)
		},
	}
	refingerprintCmd.Flags().StringVar(&rfProvider, "provider", "", "Provider whose live intents to re-stamp (required)")
	refingerprintCmd.Flags().StringVar(&rfMerchant, "merchant", "", "Merchant slug or id (default: the default merchant)")
	refingerprintCmd.Flags().BoolVar(&rfYes, "yes", false, "Confirm: the current credentials point at the same (or deliberately adopted) provider account")
	cmd.AddCommand(refingerprintCmd)
	return cmd
}

func runIntentsRefingerprint(cmd *cobra.Command, provider, merchantSlug string, yes bool) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return fmt.Errorf("--provider is required")
	}
	if !yes {
		return fmt.Errorf("refingerprint re-stamps every live %s intent to the CURRENT account fingerprint; intents will then execute against the CURRENT credentials. Confirm with --yes after verifying the credential change does not point at a different provider account", provider)
	}
	return withReconcileApp(cmd, merchantSlug, func(ctx context.Context, application *app.App) error {
		rt := application.Runtime
		fp, ok := rt.AccountFingerprints().AccountFingerprint(ctx, provider)
		if !ok || fp == "" {
			return fmt.Errorf("no account fingerprint resolvable for provider %q (unconfigured client, or an exempt provider like solana)", provider)
		}
		n, err := intents.NewStore(rt.DB).Refingerprint(ctx, provider, fp)
		if err != nil {
			return err
		}
		fmt.Printf("Re-stamped %d live intent(s) of provider %q to %s\n", n, provider, fp)
		return nil
	})
}

// intentsStatusFilters maps CLI vocabulary onto ledger statuses.
var intentsStatusFilters = map[string]string{
	"pending":                        intents.StatusPending,
	"succeeded":                      intents.StatusSucceeded,
	"failed":                         intents.StatusFailedTerminal,
	"unknown":                        intents.StatusUnknownNeedsVerify,
	intents.StatusInFlight:           intents.StatusInFlight,
	intents.StatusFailedRetryable:    intents.StatusFailedRetryable,
	intents.StatusFailedTerminal:     intents.StatusFailedTerminal,
	intents.StatusUnknownNeedsVerify: intents.StatusUnknownNeedsVerify,
	intents.StatusSuperseded:         intents.StatusSuperseded,
	intents.StatusExpired:            intents.StatusExpired,
}

// executesUnder reports the weakest operating mode that lets an intent of
// this origin attempt its provider write (the GateExecution matrix).
func executesUnder(origin string) string {
	if intents.Origin(origin) == intents.OriginSystem {
		return "full"
	}
	return "limited"
}

func runIntentsList(cmd *cobra.Command, status, provider, intentType, format, merchantSlug string, limit int) error {
	var statusFilter *string
	status = strings.ToLower(strings.TrimSpace(status))
	if status != "all" {
		mapped, ok := intentsStatusFilters[status]
		if !ok {
			return fmt.Errorf("invalid --status %q", status)
		}
		statusFilter = &mapped
	}
	var providerFilter *string
	if p := strings.ToLower(strings.TrimSpace(provider)); p != "" {
		providerFilter = &p
	}
	var typeFilter *string
	if t := strings.ToLower(strings.TrimSpace(intentType)); t != "" {
		typeFilter = &t
	}
	if limit <= 0 {
		limit = 500
	}

	return withReconcileApp(cmd, merchantSlug, func(ctx context.Context, application *app.App) error {
		q := application.Runtime.DB.Gen(ctx)
		total, err := q.CountProviderIntents(ctx, gen.CountProviderIntentsParams{
			Status: statusFilter, Provider: providerFilter, IntentType: typeFilter,
		})
		if err != nil {
			return fmt.Errorf("count provider intents: %w", err)
		}
		rows, err := q.ListProviderIntents(ctx, gen.ListProviderIntentsParams{
			Status: statusFilter, Provider: providerFilter, IntentType: typeFilter,
			PageLimit: int64(limit), PageOffset: 0,
		})
		if err != nil {
			return fmt.Errorf("list provider intents: %w", err)
		}

		if format == "json" {
			items := make([]map[string]any, 0, len(rows))
			for _, row := range rows {
				item := map[string]any{
					"id":                  row.ID,
					"type":                row.IntentType,
					"origin":              row.Origin,
					"executes_under":      executesUnder(row.Origin),
					"processor":           row.Provider,
					"subscription_id":     row.SubscriptionID,
					"payment_id":          row.PaymentID,
					"status":              row.Status,
					"attempts":            row.Attempts,
					"next_attempt_at":     row.NextAttemptAt,
					"failure_reason":      row.LastFailureReason,
					"origin_reason":       row.OriginReason,
					"expires_at":          row.ExpiresAt,
					"executed_at":         row.ExecutedAt,
					"created_at":          row.CreatedAt,
					"account_fingerprint": row.AccountFingerprint,
				}
				if len(row.Payload) > 0 {
					item["payload"] = json.RawMessage(row.Payload)
				}
				if len(row.ResultEvidence) > 0 {
					item["result_evidence"] = json.RawMessage(row.ResultEvidence)
				}
				items = append(items, item)
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(map[string]any{"total": total, "intents": items})
		}

		fmt.Printf("Provider intents (status=%s): %d total, showing %d\n\n", status, total, len(rows))
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "CREATED\tTYPE\tORIGIN\tEXECUTES UNDER\tPROVIDER\tACCOUNT\tSTATUS\tATTEMPTS\tSUBSCRIPTION\tLAST FAILURE")
		byMode := map[string]int{}
		for _, row := range rows {
			sub := ""
			if row.SubscriptionID != nil {
				sub = row.SubscriptionID.String()
			}
			reason := ""
			if row.LastFailureReason != nil {
				reason = *row.LastFailureReason
			}
			account := ""
			if row.AccountFingerprint != nil {
				account = *row.AccountFingerprint
			}
			if row.Status == intents.StatusPending {
				byMode[executesUnder(row.Origin)]++
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				row.CreatedAt.Format("2006-01-02 15:04"),
				row.IntentType, row.Origin, executesUnder(row.Origin),
				row.Provider, account, row.Status, row.Attempts, sub, reason)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if byMode["limited"] > 0 || byMode["full"] > 0 {
			fmt.Printf("\nPending drain forecast: %d execute under mode=limited (or full), %d require mode=full; nothing executes under readonly.\n",
				byMode["limited"], byMode["full"])
		}
		return nil
	})
}
