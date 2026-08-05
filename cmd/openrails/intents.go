package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/pkg/merchant"
)

// newIntentsCmd wires the read-only provider-intent-ledger (#358) CLI:
//
//	openrails intents [--status=pending|all|...] [--rail=...] [--type=...] [--merchant=...]
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
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runIntentsList(c, status, provider, intentType, format, merchantSlug, limit)
		},
	}
	cmd.Flags().StringVar(&status, "status", "active", "Status filter: active (the live working set: pending, in_flight, failed_retryable, unknown), pending, in_flight, succeeded, failed_retryable, failed (terminal), unknown, superseded, expired, all")
	cmd.Flags().StringVar(&provider, "rail", "", "Rail filter (e.g. nmi, stripe)")
	cmd.Flags().StringVar(&intentType, "type", "", "Intent type filter (e.g. nmi_delete_subscription, manual_rebill, stripe_refund)")
	cmd.Flags().StringVar(&format, "format", "table", "Output format: table, json")
	cmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant slug or id (default: the default merchant)")
	cmd.Flags().IntVar(&limit, "limit", 500, "Maximum rows to list")

	return cmd
}

func newIntentsLogCmd() *cobra.Command {
	var (
		provider string
		intent   string
		psp      string
		phase    string
		format   string
		merchant string
		limit    int
	)
	cmd := &cobra.Command{
		Use:   "intents-log",
		Short: "List provider mutation attempts/results (#533)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runIntentsMutationLog(c, provider, intent, psp, phase, format, merchant, limit)
		},
	}
	cmd.Flags().StringVar(&provider, "rail", "", "Rail filter (e.g. nmi, stripe)")
	cmd.Flags().StringVar(&intent, "intent", "", "Provider intent id filter")
	cmd.Flags().StringVar(&psp, "provider-account", "", "PSP id filter")
	cmd.Flags().StringVar(&phase, "phase", "", "Phase filter: attempting, succeeded, failed, unknown, parked")
	cmd.Flags().StringVar(&format, "format", "table", "Output format: table, json")
	cmd.Flags().StringVar(&merchant, "merchant", "", "Merchant slug or id (default: the default merchant)")
	cmd.Flags().IntVar(&limit, "limit", 500, "Maximum rows to list")
	return cmd
}

// activeStatuses is the live working set — the statuses the executor/verifier
// can still act on (mirrors the idx_provider_intents_due partial index). This
// is the default `openrails intents` view (#607): succeeded tombstones,
// terminal/superseded/expired rows are queryable via --status but stay out of
// the everyday "what's queued" picture.
var activeStatuses = []string{
	intents.StatusPending,
	intents.StatusInFlight,
	intents.StatusFailedRetryable,
	intents.StatusUnknownNeedsVerify,
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
	// statusFilters is the set of status predicates to union over. A single nil
	// entry means "no status predicate" (--status=all). The default "active"
	// view unions the live working set; any concrete status is a single filter.
	var statusFilters []*string
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "all":
		statusFilters = []*string{nil}
	case "active":
		for _, s := range activeStatuses {
			s := s
			statusFilters = append(statusFilters, &s)
		}
	default:
		mapped, ok := intentsStatusFilters[status]
		if !ok {
			return fmt.Errorf("invalid --status %q", status)
		}
		statusFilters = []*string{&mapped}
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

	return withIntentsListDB(cmd, merchantSlug, func(ctx context.Context, database *db.DB) error {
		q := database.Gen(ctx)
		// Union over statusFilters: one count+list per status predicate (the
		// generated query takes a single optional status), then merge, sort by
		// created_at DESC and cap at limit. For a single concrete status (or
		// --status=all) this is exactly one round trip.
		var total int64
		var rows []gen.OpenrailsRailIntent
		for _, statusFilter := range statusFilters {
			n, err := q.CountRailIntents(ctx, gen.CountRailIntentsParams{
				Status: statusFilter, Rail: providerFilter, IntentType: typeFilter,
			})
			if err != nil {
				return fmt.Errorf("count provider intents: %w", err)
			}
			total += n
			part, err := q.ListRailIntents(ctx, gen.ListRailIntentsParams{
				Status: statusFilter, Rail: providerFilter, IntentType: typeFilter,
				PageLimit: int64(limit), PageOffset: 0,
			})
			if err != nil {
				return fmt.Errorf("list provider intents: %w", err)
			}
			rows = append(rows, part...)
		}
		if len(statusFilters) > 1 {
			sort.SliceStable(rows, func(i, j int) bool {
				return rows[i].CreatedAt.After(rows[j].CreatedAt)
			})
			if len(rows) > limit {
				rows = rows[:limit]
			}
		}

		if format == "json" {
			items := make([]map[string]any, 0, len(rows))
			for _, row := range rows {
				item := map[string]any{
					"id":              row.ID,
					"type":            row.IntentType,
					"origin":          row.Origin,
					"executes_under":  executesUnder(row.Origin),
					"rail":            row.Rail,
					"subscription_id": row.SubscriptionID,
					"payment_id":      row.PaymentID,
					"status":          row.Status,
					"attempts":        row.Attempts,
					"next_attempt_at": row.NextAttemptAt,
					"failure_reason":  row.LastFailureReason,
					"origin_reason":   row.OriginReason,
					"expires_at":      row.ExpiresAt,
					"executed_at":     row.ExecutedAt,
					"created_at":      row.CreatedAt,
					"psp_id":          row.PspID,
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
			// or#893/or#795: an intent is addressed to a PSP or to a custodian.
			account := intentAddress(row.PspID, row.CustodianID)
			if row.Status == intents.StatusPending {
				byMode[executesUnder(row.Origin)]++
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				row.CreatedAt.Format("2006-01-02 15:04"),
				row.IntentType, row.Origin, executesUnder(row.Origin),
				row.Rail, account, row.Status, row.Attempts, sub, reason)
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

func runIntentsMutationLog(cmd *cobra.Command, provider, intentID, pspID, phase, format, merchantSlug string, limit int) error {
	var providerFilter *string
	if p := strings.ToLower(strings.TrimSpace(provider)); p != "" {
		providerFilter = &p
	}
	var intentFilter *uuid.UUID
	if raw := strings.TrimSpace(intentID); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return fmt.Errorf("invalid --intent %q: %w", raw, err)
		}
		intentFilter = &id
	}
	var accountFilter *uuid.UUID
	if raw := strings.TrimSpace(pspID); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return fmt.Errorf("invalid --provider-account %q: %w", raw, err)
		}
		accountFilter = &id
	}
	var phaseFilter *string
	if p := strings.ToLower(strings.TrimSpace(phase)); p != "" {
		switch p {
		case string(intents.MutationLogPhaseAttempting), string(intents.MutationLogPhaseSucceeded), string(intents.MutationLogPhaseFailed), string(intents.MutationLogPhaseUnknown), string(intents.MutationLogPhaseParked):
			phaseFilter = &p
		default:
			return fmt.Errorf("invalid --phase %q", phase)
		}
	}
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		format = "table"
	}
	if format != "table" && format != "json" {
		return fmt.Errorf("invalid --format %q", format)
	}
	if limit <= 0 {
		limit = 500
	}

	return withIntentsListDB(cmd, merchantSlug, func(ctx context.Context, database *db.DB) error {
		merchantID, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		total, err := database.Gen(ctx).CountRailMutationLogs(ctx, gen.CountRailMutationLogsParams{
			MerchantID:   merchantID.UUID(),
			Rail:         providerFilter,
			RailIntentID: intentFilter,
			PspID:        accountFilter,
			Phase:        phaseFilter,
		})
		if err != nil {
			return fmt.Errorf("count rail mutation logs: %w", err)
		}
		rows, err := database.Gen(ctx).ListRailMutationLogs(ctx, gen.ListRailMutationLogsParams{
			MerchantID:   merchantID.UUID(),
			Rail:         providerFilter,
			RailIntentID: intentFilter,
			PspID:        accountFilter,
			Phase:        phaseFilter,
			LimitRows:    int64(limit),
		})
		if err != nil {
			return fmt.Errorf("list rail mutation logs: %w", err)
		}

		if format == "json" {
			items := make([]map[string]any, 0, len(rows))
			for _, row := range rows {
				item := map[string]any{
					"id":                 row.ID,
					"merchant_id":        row.MerchantID,
					"provider":           row.Rail,
					"psp_id":             row.PspID,
					"provider_intent_id": row.RailIntentID,
					"intent_type":        row.IntentType,
					"idempotency_key":    row.IdempotencyKey,
					"attempt":            row.Attempt,
					"phase":              row.Phase,
					"reason":             row.Reason,
					"created_at":         row.CreatedAt,
				}
				if len(row.Evidence) > 0 {
					item["evidence"] = json.RawMessage(row.Evidence)
				}
				items = append(items, item)
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(map[string]any{"total": total, "provider_mutation_events": items})
		}

		fmt.Printf("Provider mutation events: %d total, showing %d\n\n", total, len(rows))
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "CREATED\tPROVIDER\tACCOUNT\tINTENT\tTYPE\tATTEMPT\tPHASE\tREASON")
		for _, row := range rows {
			// or#893/or#795: an intent is addressed to a PSP or to a custodian.
			account := intentAddress(row.PspID, row.CustodianID)
			intent := ""
			if row.RailIntentID != nil {
				intent = row.RailIntentID.String()
			}
			intentType := ""
			if row.IntentType != nil {
				intentType = *row.IntentType
			}
			reason := ""
			if row.Reason != nil {
				reason = *row.Reason
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				row.CreatedAt.Format("2006-01-02 15:04"),
				row.Rail, account, intent, intentType, row.Attempt, row.Phase, reason)
		}
		return w.Flush()
	})
}

func withIntentsListDB(cmd *cobra.Command, merchantSlug string, fn func(ctx context.Context, database *db.DB) error) error {
	cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	database, err := openCLIDB(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	defer func() {
		_ = database.Close()
	}()

	tid, err := resolveCLIMerchant(cmd.Context(), database, merchantSlug)
	if err != nil {
		return err
	}
	ctx := merchant.WithID(cmd.Context(), tid)
	return database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return fn(ctx, database)
	})
}

// intentAddress renders whichever account an intent or mutation log names.
func intentAddress(psp, custodian *uuid.UUID) string {
	if psp != nil {
		return psp.String()
	}
	if custodian != nil {
		return "custodian:" + custodian.String()
	}
	return ""
}
