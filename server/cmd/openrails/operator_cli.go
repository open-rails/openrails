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
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/server"
)

// The operator's commands over the server's Go API: the merchant directory,
// worker health and administrator lockouts. None has an HTTP route.

// operatorServer is the standalone server the configuration builds, and its
// database for name lookups.
type operatorServer struct {
	*server.Server
	db *db.DB
}

// withServer runs fn against the standalone server the configuration builds.
func withServer(cmd *cobra.Command, fn func(context.Context, operatorServer) error) error {
	ctx := cmd.Context()
	srv, graph, _, err := openServer(ctx, openrails.Deps{})
	if err != nil {
		return err
	}
	defer func() { _ = srv.Close(context.WithoutCancel(ctx)) }()
	return fn(ctx, operatorServer{Server: srv, db: graph.Runtime.DB})
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// merchantArg resolves a merchant id, or a live merchant's current or former
// name.
func merchantArg(ctx context.Context, srv operatorServer, ref string) (billing.MerchantID, error) {
	if id, err := uuid.Parse(strings.TrimSpace(ref)); err == nil {
		return billing.MerchantID(id), nil
	}
	return resolveCLIMerchant(ctx, srv.db, ref)
}

func newMerchantsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "merchants", Short: "The merchant directory: list, read, create, rename, soft-delete and restore merchants"}
	cmd.AddCommand(newMerchantsListCmd(), newMerchantsGetCmd(), newMerchantsCreateCmd(), newMerchantsRenameCmd(),
		newMerchantsLifecycleCmd("delete", "Soft-delete a merchant: off the default lists, its credentials resolve nothing, its records kept", (*server.Server).DeleteMerchant),
		newMerchantsLifecycleCmd("restore", "Restore a soft-deleted merchant", (*server.Server).RestoreMerchant))
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
			return withServer(cmd, func(ctx context.Context, srv operatorServer) error {
				page, err := srv.ListMerchants(ctx, params)
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
			return withServer(cmd, func(ctx context.Context, srv operatorServer) error {
				id, err := merchantArg(ctx, srv, args[0])
				if err != nil {
					return err
				}
				m, err := srv.GetMerchant(ctx, id)
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
			return withServer(cmd, func(ctx context.Context, srv operatorServer) error {
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
			return withServer(cmd, func(ctx context.Context, srv operatorServer) error {
				id, err := merchantArg(ctx, srv, args[0])
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

func newMerchantsLifecycleCmd(use, short string, op func(*server.Server, context.Context, billing.MerchantID) (*billing.Merchant, error)) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <merchant-id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuid.Parse(strings.TrimSpace(args[0]))
			if err != nil {
				return fmt.Errorf("a merchant id: %w", err)
			}
			return withServer(cmd, func(ctx context.Context, srv operatorServer) error {
				m, err := op(srv.Server, ctx, billing.MerchantID(id))
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
			return withServer(cmd, func(ctx context.Context, srv operatorServer) error {
				health, err := srv.ListWorkerHealth(ctx)
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
		Short: "End a person's lockout and reset their operation counters, on every replica",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := uuid.Parse(strings.TrimSpace(args[0])); err != nil {
				return fmt.Errorf("a user id: %w", err)
			}
			return withServer(cmd, func(ctx context.Context, srv operatorServer) error {
				if err := srv.UnlockAdminLockout(ctx, args[0]); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "unlocked %s\n", args[0])
				return nil
			})
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
