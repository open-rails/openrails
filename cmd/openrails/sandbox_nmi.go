package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/internal/nmifake"
)

// newSandboxCmd serves loopback provider fakes for disposable stacks that
// arm a PSP without real sandbox credentials (provider_sandbox.*).
func newSandboxCmd() *cobra.Command {
	// No config: the fakes need no database, posture or credentials.
	cmd := &cobra.Command{Use: "sandbox", Short: "Loopback provider fakes for disposable test stacks",
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil }}
	var listen string
	var idle time.Duration
	var plans []string
	nmi := &cobra.Command{
		Use:   "nmi-gateway",
		Short: "Serve a fake NMI gateway for provider_sandbox.nmi_gateway_url",
		Long: `Serves the NMI Customer Vault, Direct Post and Query surface OpenRails uses,
accepting any Collect.js token (card ending 0002 declines). --plan seeds the
Recurring Plans a catalog links to (id=amount:days, e.g. premium_new=23.00:30).
Loopback only; point provider_sandbox.nmi_gateway_url
(PROVIDER_SANDBOX_NMI_GATEWAY_URL) at it.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			host, _, err := net.SplitHostPort(listen)
			if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
				return fmt.Errorf("--listen must be a loopback IP literal and port, got %q", listen)
			}
			gateway := nmifake.NewUnstarted()
			for _, p := range plans {
				id, terms, ok := strings.Cut(p, "=")
				amount, days, ok2 := strings.Cut(terms, ":")
				n, err := strconv.Atoi(days)
				if !ok || !ok2 || id == "" || err != nil || n <= 0 {
					return fmt.Errorf("--plan %q: want id=amount:days", p)
				}
				if _, err := strconv.ParseFloat(amount, 64); err != nil {
					return fmt.Errorf("--plan %q: amount must be decimal", p)
				}
				gateway.AddPlan(id, amount, n)
			}
			ln, err := (&net.ListenConfig{}).Listen(cmd.Context(), "tcp", listen)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			if idle > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, idle)
				defer cancel()
			}
			srv := &http.Server{Handler: gateway, ReadHeaderTimeout: 10 * time.Second}
			go func() {
				<-ctx.Done()
				shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(shut)
			}()
			fmt.Fprintf(cmd.OutOrStdout(), "nmi-gateway: http://%s\n", ln.Addr())
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	nmi.Flags().StringVar(&listen, "listen", "127.0.0.1:0", "Loopback address to serve on")
	nmi.Flags().StringArrayVar(&plans, "plan", nil, "Seed a Recurring Plan: id=amount:days (repeatable)")
	nmi.Flags().DurationVar(&idle, "max-lifetime", 2*time.Hour, "Exit after this long (0 = until signalled)")
	cmd.AddCommand(nmi)
	return cmd
}
