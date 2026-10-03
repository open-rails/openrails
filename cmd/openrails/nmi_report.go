package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hosttools"
)

// newNMICmd groups read-only NMI tools.
func newNMICmd() *cobra.Command {
	cmd := &cobra.Command{Use: "nmi", Short: "Read-only NMI account tools"}
	var merchantSlug, psp, since, until, format, manifestPath string
	report := &cobra.Command{
		Use:   "decline-report",
		Short: "Approval and refusal rates from an NMI account's history (#1114); writes nothing",
		Long: "Reads the NMI account's transaction history through the Query API and reports, by month, the approval and " +
			"refusal rates of card verifications, one-off sales and NMI-scheduled rebills, and each refusal's reason and " +
			"category from OpenRails' decline classifier. Nothing is written.",
		RunE: func(c *cobra.Command, _ []string) error {
			cfg := c.Context().Value(config.ConfigContextKey).(*config.Config)
			mid, err := resolveConfiguredCLIMerchant(c.Context(), cfg, merchantSlug)
			if err != nil {
				return err
			}
			return hosttools.NMIDeclineReport(c.Context(), hosttools.NMIDeclineReportOptions{
				Config: cfg, MerchantID: mid, PSP: psp, Since: since, Until: until, Format: format,
				MerchantManifestPath: manifestPath, Out: os.Stdout,
			})
		},
	}
	report.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant public name or id:<uuid> (required)")
	report.Flags().StringVar(&psp, "provider-account", "", "NMI PSP UUID (default: the merchant's armed NMI account)")
	report.Flags().StringVar(&since, "since", "", "Window start, RFC3339 or YYYY-MM-DD (required)")
	report.Flags().StringVar(&until, "until", "", "Window end (default: now)")
	report.Flags().StringVar(&format, "format", "table", "Output format: table, json")
	report.Flags().StringVar(&manifestPath, "manifest", "", "MODE-1 merchant manifest to arm credentials from")
	cmd.AddCommand(report)
	return cmd
}
