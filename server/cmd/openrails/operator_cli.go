package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/merchants"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// The operator's commands over the server's Go API: the merchant directory,
// worker health and administrator lockouts. None has an HTTP route.

type operatorSession = commandControlPlane

// withOperator runs an operator action using only the account and merchant stores.
func withOperator(cmd *cobra.Command, fn func(context.Context, operatorSession) error) error {
	ctx := cmd.Context()
	srv, err := openCLIControlPlane(ctx)
	if err != nil {
		return err
	}
	defer srv.close()
	if err := srv.loadMerchants(ctx); err != nil {
		return err
	}
	return fn(ctx, *srv)
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// merchantArg resolves a merchant id, or a live merchant's current or former
// name.
func merchantArg(ctx context.Context, database *db.DB, ref string) (billing.MerchantID, error) {
	if id, err := uuid.Parse(strings.TrimSpace(ref)); err == nil {
		return billing.MerchantID(id), nil
	}
	return resolveCLIMerchant(ctx, database, ref)
}

func newMerchantsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "merchants", Short: "The merchant directory: list, read, create, rename, soft-delete and restore merchants"}
	cmd.AddCommand(newMerchantsListCmd(), newMerchantsGetCmd(), newMerchantsCreateCmd(), newMerchantsRenameCmd(),
		newMerchantsLifecycleCmd("delete", "Soft-delete a merchant: off the default lists, its credentials resolve nothing, its records kept", (*commandControlPlane).DeleteMerchant),
		newMerchantsLifecycleCmd("restore", "Restore a soft-deleted merchant", (*commandControlPlane).RestoreMerchant))
	return cmd
}

func newMerchantsListCmd() *cobra.Command {
	var status, query, cursor string
	var limit int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List merchants, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			params := billing.MerchantListParams{PageRequest: billing.PageRequest{Limit: limit, Cursor: cursor}, Query: query}
			switch status {
			case "active", "":
			case "deleted":
				params.Statuses = []billing.MerchantStatus{billing.MerchantDeleted}
			case "all":
				params.Statuses = []billing.MerchantStatus{billing.MerchantActive, billing.MerchantDeleted}
			default:
				return fmt.Errorf("--status is active, deleted or all")
			}
			return withCLIDirectory(cmd, func(ctx context.Context, database *db.DB, directory *merchants.Service) error {
				page, err := directory.ListDirectory(ctx, params)
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(cmd.OutOrStdout(), page)
				}
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tRAILS\tLAST PAYMENT\tCREATED")
				for _, m := range page.Items {
					rails := make([]string, len(m.RailsArmed))
					for i, r := range m.RailsArmed {
						rails[i] = string(r)
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", m.ID, m.Slug, m.Status, strings.Join(rails, ","), when(m.LastPaymentAt), m.CreatedAt.UTC().Format(time.RFC3339))
				}
				if page.Next != "" {
					fmt.Fprintf(tw, "\nmore: --cursor %s\n", page.Next)
				}
				return tw.Flush()
			})
		},
	}
	cmd.Flags().StringVar(&status, "status", "active", "active, deleted or all")
	cmd.Flags().StringVar(&query, "query", "", "part of the current name")
	cmd.Flags().IntVar(&limit, "limit", 0, "page size (default 25)")
	cmd.Flags().StringVar(&cursor, "cursor", "", "the next page's cursor")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newMerchantsGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <merchant>",
		Short: "Read one merchant, in any status (an id, or a live merchant's name)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCLIDirectory(cmd, func(ctx context.Context, database *db.DB, directory *merchants.Service) error {
				id, err := merchantArg(ctx, database, args[0])
				if err != nil {
					return err
				}
				m, err := directory.DirectoryEntry(ctx, id)
				if err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), m)
			})
		},
	}
}

func newMerchantsCreateCmd() *cobra.Command {
	var displayName, owner string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a merchant claiming a name, or report the one that holds it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withOperator(cmd, func(ctx context.Context, srv operatorSession) error {
				res, err := srv.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: args[0], DisplayName: displayName, OwnerUserID: owner})
				if err != nil {
					return err
				}
				m, err := srv.GetMerchant(ctx, res.MerchantID)
				if err != nil {
					return err
				}
				if !res.Created {
					fmt.Fprintf(cmd.ErrOrStderr(), "%s exists; unchanged\n", m.Slug)
				}
				return printJSON(cmd.OutOrStdout(), m)
			})
		},
	}
	cmd.Flags().StringVar(&displayName, "display-name", "", "the name buyers see")
	cmd.Flags().StringVar(&owner, "owner-user-id", "", "the server account that owns the new merchant's team")
	return cmd
}

func newMerchantsRenameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <merchant> <new-name>",
		Short: "Rename a merchant; its former name forwards under auth.naming",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withOperator(cmd, func(ctx context.Context, srv operatorSession) error {
				id, err := merchantArg(ctx, srv.db, args[0])
				if err != nil {
					return err
				}
				name, err := srv.RenameMerchant(ctx, id, billing.RenameMerchantParams{Name: args[1]})
				if err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), name)
			})
		},
	}
}

func newMerchantsLifecycleCmd(use, short string, op func(*commandControlPlane, context.Context, billing.MerchantID) (*billing.Merchant, error)) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <merchant-id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuid.Parse(strings.TrimSpace(args[0]))
			if err != nil {
				return fmt.Errorf("a merchant id: %w", err)
			}
			return withOperator(cmd, func(ctx context.Context, srv operatorSession) error {
				m, err := op(&srv, ctx, billing.MerchantID(id))
				if err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), m)
			})
		},
	}
}

func newWorkersCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "workers",
		Short: "Each background job kind's recent runs, with its last error",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCLIDB(cmd, func(ctx context.Context, database *db.DB) error {
				health, err := riverjobs.ListWorkerHealth(ctx, database)
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(cmd.OutOrStdout(), health)
				}
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "KIND\tLAST SUCCESS\tFAILURES\tLAST ERROR")
				for _, h := range health {
					last := ""
					if h.LastError != nil {
						last = strings.Join(strings.Fields(*h.LastError), " ")
						if len(last) > 120 {
							last = last[:117] + "..."
						}
					}
					fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", h.WorkerKind, when(h.LastSuccessAt), h.ConsecutiveFailures, last)
				}
				return tw.Flush()
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON, with the full error text")
	return cmd
}

func newAdminLockoutsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "admin-lockouts", Short: "Administrators locked out of administrative operations"}
	cmd.AddCommand(&cobra.Command{
		Use:   "unlock <user-id>",
		Short: "End a person's lockout and reset their operation counters (needs Redis: without it a lockout lives in the serving process)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := uuid.Parse(strings.TrimSpace(args[0])); err != nil {
				return fmt.Errorf("a user id: %w", err)
			}
			cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
			if cfg == nil {
				return fmt.Errorf("config not loaded")
			}
			if cfg.Redis == nil {
				return fmt.Errorf("Redis is required to unlock another process's administrator lockout")
			}
			opts, err := config.RedisOptions(cfg.Redis)
			if err != nil {
				return err
			}
			client := redis.NewClient(opts)
			defer client.Close()
			limiter := middleware.NewAdminOperationLimiter(abusestate.New(client))
			if err := limiter.Unlock(cmd.Context(), args[0], "operator"); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "unlocked %s\n", args[0])
			return err
		},
	})
	return cmd
}

func when(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}
