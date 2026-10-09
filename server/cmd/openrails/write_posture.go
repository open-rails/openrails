package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/writeposture"
)

// newBookCmd arms the billing book in this database. A copy of the book (a
// dump restored elsewhere, another schema) is readonly until it is armed.
func newBookCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "book", Short: "Show or arm the billing book in this database"}
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Report whether this database is the one the billing book was armed in",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			database, err := openCLIDB(cmd.Context(), cmd.Context().Value(config.ConfigContextKey).(*config.Config))
			if err != nil {
				return err
			}
			defer database.Close()
			armed, err := writeposture.BookArmed(cmd.Context(), database.GenDirectory())
			if err != nil {
				return err
			}
			if armed {
				fmt.Fprintln(cmd.OutOrStdout(), "armed: this database is the billing book's live copy")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "readonly: this database is a copy of the billing book; arm it only once every other copy is stopped")
			}
			return nil
		},
	})
	var by string
	arm := &cobra.Command{
		Use:   "arm --by NAME",
		Short: "Record this database as the billing book's one live copy",
		Long: "Provider writes stay readonly in a copy of the billing book. Arm a copy only once every other copy using " +
			"the same provider credentials is stopped for good: two live copies bill the same customers twice.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			database, err := openCLIDB(cmd.Context(), cmd.Context().Value(config.ConfigContextKey).(*config.Config))
			if err != nil {
				return err
			}
			defer database.Close()
			if err := writeposture.ArmBook(cmd.Context(), database.GenDirectory(), strings.TrimSpace(by), time.Now()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "armed by %s\n", strings.TrimSpace(by))
			return nil
		},
	}
	arm.Flags().StringVar(&by, "by", "", "who attests that every other copy is stopped (required)")
	cmd.AddCommand(arm)
	return cmd
}

// newMerchantPostureCmd sets one merchant's write posture. Export leaves its
// source readonly and restore its destination; arm restores writes.
func newMerchantPostureCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "merchant", Short: "Arm or hold one merchant's provider writes"}
	var slug, by, mode string
	set := func(cmd *cobra.Command, m writeposture.Mode) error {
		cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
		database, err := openCLIDB(cmd.Context(), cfg)
		if err != nil {
			return err
		}
		defer database.Close()
		mid, err := resolveCLIMerchant(cmd.Context(), database, slug)
		if err != nil {
			return err
		}
		if err := database.RunInMerchantScope(cmd.Context(), mid, "write posture", func(ctx context.Context) error {
			return writeposture.Set(ctx, database.Gen(ctx), mid.UUID(), m, writeposture.ReasonOperator, strings.TrimSpace(by), time.Now())
		}); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "merchant %s write posture %s, set by %s\n", mid, m, strings.TrimSpace(by))
		return nil
	}
	arm := &cobra.Command{
		Use:   "arm --merchant NAME --by NAME",
		Short: "Restore a merchant's provider writes after an export or a restore",
		Long: "An exported merchant is readonly at its source and a restored one at its destination. Arm only the copy " +
			"that stays live, once the other is stopped: both would bill the same customers.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return set(cmd, writeposture.Full) },
	}
	hold := &cobra.Command{
		Use:   "hold --merchant NAME --mode readonly|limited --by NAME",
		Short: "Hold a merchant's provider writes (readonly) or its proactive ones (limited)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, ok := writeposture.ParseMode(strings.TrimSpace(mode))
			if !ok || m == writeposture.Full {
				return fmt.Errorf("--mode must be readonly or limited")
			}
			return set(cmd, m)
		},
	}
	hold.Flags().StringVar(&mode, "mode", "", "readonly or limited (required)")
	for _, c := range []*cobra.Command{arm, hold} {
		c.Flags().StringVar(&slug, "merchant", "", "merchant public name or id:<uuid> (required)")
		c.Flags().StringVar(&by, "by", "", "who sets it (required)")
		cmd.AddCommand(c)
	}
	return cmd
}
