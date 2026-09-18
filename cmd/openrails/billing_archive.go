package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/inprocess"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
)

type billingArchiveOptions struct {
	merchant  string
	url       string
	tokenFile string
	timeout   time.Duration
}

func newBillingCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "billing",
		Short: "Move a merchant's billing snapshot between OpenRails deployments",
	}
	// Remote archive operations need only their URL and bearer file. Do not let
	// local server/database configuration become an accidental remote dependency.
	cmd.PersistentPreRunE = func(leaf *cobra.Command, args []string) error {
		if remoteURL, err := leaf.Flags().GetString("url"); err == nil && strings.TrimSpace(remoteURL) != "" {
			return nil
		}
		root := leaf.Root()
		if root != cmd && root.PersistentPreRunE != nil {
			return root.PersistentPreRunE(leaf, args)
		}
		return nil
	}
	cmd.AddCommand(newBillingExportCmd(), newBillingImportCmd(), newBillingPrepareTargetCmd())
	return cmd
}

func addBillingArchiveFlags(cmd *cobra.Command, opts *billingArchiveOptions) {
	cmd.Flags().StringVar(&opts.merchant, "merchant", "", "Exact merchant UUID (optionally prefixed with id:); preserved on import")
	cmd.Flags().StringVar(&opts.url, "url", "", "Remote OpenRails base URL; omit to use the local database configuration")
	cmd.Flags().StringVar(&opts.tokenFile, "token-file", "", "File containing a remote bearer credential; required with --url")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 0, "Optional archive operation deadline (0 uses caller cancellation)")
}

func newBillingExportCmd() *cobra.Command {
	opts := billingArchiveOptions{}
	var out string
	var overwrite, sourceStopped bool
	cmd := &cobra.Command{
		Use:   "export --merchant UUID --out PATH",
		Short: "Export a complete billing snapshot to a private local file",
		Long: "Export a versioned billing snapshot. Quiesce all source writers before a final cutover. " +
			"The destination file appears only after the complete export succeeds; existing files require --overwrite.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mid, err := opts.validate()
			if err != nil {
				return err
			}
			if err := validateArchivePath(out, "out"); err != nil {
				return err
			}
			if !sourceStopped {
				return fmt.Errorf("--source-stopped is required: attest that all source billing writers are stopped for final cutover")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
			var bytes int64
			err = writeBillingArchive(out, overwrite, func(w io.Writer) error {
				client, close, err := openBillingArchiveClient(ctx, cfg, opts, mid)
				if err != nil {
					return err
				}
				defer close()
				counter := &archiveCountingWriter{Writer: w}
				if err := client.ExportMerchantBilling(ctx, counter); err != nil {
					return err
				}
				bytes = counter.n
				return nil
			})
			if err != nil {
				return fmt.Errorf("export billing: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "exported merchant=%s bytes=%d file=%s\n", mid, bytes, out)
			return err
		},
	}
	addBillingArchiveFlags(cmd, &opts)
	cmd.Flags().StringVar(&out, "out", "", "Destination snapshot file (required; stdout is not supported)")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "Atomically replace an existing destination file after a successful export")
	cmd.Flags().BoolVar(&sourceStopped, "source-stopped", false, "Attest that all source billing writers are stopped; the exporter cannot verify this operational condition")
	return cmd
}

func newBillingImportCmd() *cobra.Command {
	opts := billingArchiveOptions{}
	var in string
	cmd := &cobra.Command{
		Use:   "import --merchant UUID --in PATH",
		Short: "Restore a billing snapshot into an empty, separately provisioned merchant",
		Long: "Restore a complete snapshot atomically, preserving the merchant UUID and billing identities. " +
			"Provision the destination separately and keep its writers and workers stopped until cutover. " +
			"Import does not transfer credentials or replay financial operations.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mid, err := opts.validate()
			if err != nil {
				return err
			}
			f, err := openBillingArchiveInput(in)
			if err != nil {
				return err
			}
			defer f.Close()
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
			client, close, err := openBillingArchiveClient(ctx, cfg, opts, mid)
			if err != nil {
				return err
			}
			defer close()
			result, err := client.ImportMerchantBilling(ctx, f)
			if err != nil {
				return fmt.Errorf("import billing: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "imported merchant=%s digest=%s rows=%d already_imported=%t\n",
				result.MerchantID, result.Digest, result.Rows, result.AlreadyImported)
			return err
		},
	}
	addBillingArchiveFlags(cmd, &opts)
	cmd.Flags().StringVar(&in, "in", "", "Snapshot file to restore (required; stdin is not supported)")
	return cmd
}

func (o billingArchiveOptions) validate() (merchant.ID, error) {
	mid, err := parseBillingArchiveMerchant(o.merchant)
	if err != nil {
		return merchant.ID{}, err
	}
	if (strings.TrimSpace(o.url) == "") != (strings.TrimSpace(o.tokenFile) == "") {
		return merchant.ID{}, fmt.Errorf("--url and --token-file must be supplied together")
	}
	if o.timeout < 0 {
		return merchant.ID{}, fmt.Errorf("--timeout cannot be negative")
	}
	return mid, nil
}

func parseBillingArchiveMerchant(value string) (merchant.ID, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "id:")
	mid, err := merchant.ParseID(value)
	if err != nil || mid.IsZero() {
		return merchant.ID{}, fmt.Errorf("--merchant requires a nonzero exact UUID (optionally prefixed with id:)")
	}
	return mid, nil
}

func openBillingArchiveClient(ctx context.Context, cfg *config.Config, opts billingArchiveOptions, mid merchant.ID) (*openrails.Client, func(), error) {
	clientOpts := []openrails.ClientOption{openrails.WithMerchantID(mid), openrails.WithTimeout(opts.timeout)}
	if strings.TrimSpace(opts.url) != "" {
		token, err := readBillingArchiveToken(opts.tokenFile)
		if err != nil {
			return nil, nil, err
		}
		client, err := openrails.NewRemote(opts.url, append(clientOpts, openrails.WithAPIKey(token))...)
		return client, func() {}, err
	}
	database, err := openCLIDB(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	close := func() { _ = database.Close() }
	// A minimal runtime shares the real handlers and merchant gate without
	// initializing credentials, providers, workers, FX refresh or control plane.
	rt := &app.Runtime{DB: database, Config: cfg, Clock: clockwork.NewRealClock()}
	rt.SetConfiguredMerchant(mid)
	mux := http.NewServeMux()
	httproutes.RegisterMerchantArchiveRoutes(router.NewMux(mux, "/v1/merchant", rt), rt,
		httproutes.Options{Gate: httproutes.NewGate(httproutes.GateOptions{})})
	handler := middleware.BodyLimitHTTP(middleware.DefaultMaxBodyBytes)(mux)
	clientOpts = append(clientOpts,
		openrails.WithHTTPClient(&http.Client{Transport: inprocess.NewTransport(handler, rt.ConfiguredMerchant)}),
		openrails.WithTokenProvider(func(context.Context) (string, error) { return "in-process-host", nil }))
	client, err := openrails.NewRemote("http://openrails.invalid", clientOpts...)
	if err != nil {
		close()
		return nil, nil, err
	}
	return client, close, nil
}

func openBillingTargetRuntime(ctx context.Context, cfg *config.Config) (*embed.Runtime, func(), error) {
	database, err := openCLIDB(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	// Hosted identity preparation needs the control plane to verify live group ownership.
	opts := embed.Options{}
	opts.Config = cfg
	opts.PGXPool = database.Pool()
	opts.River = embedded.RiverManagedByOpenRails()
	rt, err := embed.New(ctx, opts)
	if err != nil {
		_ = database.Close()
		return nil, nil, err
	}
	return rt, func() {
		_ = rt.Close(context.WithoutCancel(ctx))
		_ = database.Close()
	}, nil
}

func readBillingArchiveToken(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open bearer credential file: %w", err)
	}
	defer f.Close()
	const maxTokenBytes = 64 << 10
	raw, err := io.ReadAll(io.LimitReader(f, maxTokenBytes+1))
	if err != nil {
		return "", fmt.Errorf("read bearer credential file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if len(raw) > maxTokenBytes || token == "" || strings.ContainsAny(token, "\r\n\t ") {
		return "", fmt.Errorf("bearer credential file must contain one nonempty token of at most 64 KiB")
	}
	return token, nil
}

func validateArchivePath(path, flag string) error {
	if strings.TrimSpace(path) == "" || path == "-" {
		return fmt.Errorf("--%s requires a file path; standard streams are not supported", flag)
	}
	return nil
}

func openBillingArchiveInput(path string) (*os.File, error) {
	if err := validateArchivePath(path, "in"); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open billing snapshot: %w", err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("billing snapshot must be a readable regular file")
	}
	return f, nil
}

// writeBillingArchive publishes only a completed stream. Linking the temporary
// file is the atomic no-replace operation: a destination created concurrently
// cannot be clobbered between an existence check and publication.
func writeBillingArchive(path string, overwrite bool, export func(io.Writer) error) error {
	if err := validateArchivePath(path, "out"); err != nil {
		return err
	}
	if !overwrite {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("destination exists; choose another --out path or use --overwrite")
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect destination: %w", err)
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".openrails-billing-*")
	if err != nil {
		return fmt.Errorf("create private snapshot file: %w", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	// CreateTemp creates mode 0600 regardless of permissions on an old destination.
	if err := export(f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync snapshot: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close snapshot: %w", err)
	}
	if overwrite {
		err = os.Rename(f.Name(), path)
	} else {
		err = os.Link(f.Name(), path)
	}
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("destination appeared during export; choose another --out path or use --overwrite")
	}
	if err != nil {
		return fmt.Errorf("publish snapshot: %w", err)
	}
	return nil
}

type archiveCountingWriter struct {
	io.Writer
	n int64
}

func (w *archiveCountingWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.n += int64(n)
	return n, err
}

func newBillingPrepareTargetCmd() *cobra.Command {
	var rawMerchant, slug, groupID, ownerID string
	var unbound bool
	cmd := &cobra.Command{
		Use:   "prepare-target --merchant UUID",
		Short: "Provision the exact merchant identity in the local destination database",
		Long: "Create the destination merchant identity before import. Use an existing destination AuthKit " +
			"group and its owner, or explicitly select an unbound host-local merchant. This does not import billing data.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mid, err := parseBillingArchiveMerchant(rawMerchant)
			if err != nil {
				return err
			}
			if err := validateBillingPrepareTarget(unbound, slug, groupID, ownerID); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
			if unbound {
				database, err := openCLIDB(ctx, cfg)
				if err != nil {
					return err
				}
				defer database.Close()
				directory, err := merchants.NewDirectoryService(database.DataPool())
				if err != nil {
					return err
				}
				if _, _, err := directory.RegisterForRestore(ctx, mid, strings.TrimSpace(slug)); err != nil {
					return err
				}
			} else {
				rt, close, err := openBillingTargetRuntime(ctx, cfg)
				if err != nil {
					return err
				}
				defer close()
				cp, err := controlplane.Attach(ctx, rt, controlplane.Options{})
				if err != nil {
					return err
				}
				if _, err := cp.ProvisionMerchantForRestore(ctx, controlplane.ProvisionMerchantForRestoreRequest{
					MerchantID: mid, ExistingGroupID: strings.TrimSpace(groupID), OwnerUserID: strings.TrimSpace(ownerID),
				}); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "prepared merchant=%s\n", mid)
			return err
		},
	}
	cmd.Flags().StringVar(&rawMerchant, "merchant", "", "Exact source merchant UUID to preserve (required)")
	cmd.Flags().BoolVar(&unbound, "unbound-merchants", false, "Create a host-local merchant without an OpenRails control-plane AuthKit group")
	cmd.Flags().StringVar(&slug, "slug", "", "Host-local merchant slug (required with --unbound-merchants)")
	cmd.Flags().StringVar(&groupID, "authkit-group-id", "", "Existing destination AuthKit merchant group UUID")
	cmd.Flags().StringVar(&ownerID, "owner-user-id", "", "Destination AuthKit user UUID holding live ownership of that group")
	return cmd
}

func validateBillingPrepareTarget(unbound bool, slug, groupID, ownerID string) error {
	slug, groupID, ownerID = strings.TrimSpace(slug), strings.TrimSpace(groupID), strings.TrimSpace(ownerID)
	if unbound {
		if slug == "" || groupID != "" || ownerID != "" {
			return fmt.Errorf("--unbound-merchants requires --slug and cannot use --authkit-group-id or --owner-user-id")
		}
	} else if slug != "" || groupID == "" || ownerID == "" {
		return fmt.Errorf("provide --authkit-group-id and --owner-user-id without --slug, or use --unbound-merchants --slug")
	}
	return nil
}

// Export/import and host-local provisioning need only an RLS-enforcing pool.
// Hosted provisioning still validates the full AuthKit runtime configuration.
func isDatabaseOnlyBillingCommand(cmd *cobra.Command) bool {
	if cmd.Parent() == nil || cmd.Parent().Name() != "billing" {
		return false
	}
	switch cmd.Name() {
	case "export", "import":
		return true
	case "prepare-target":
		unbound, _ := cmd.Flags().GetBool("unbound-merchants")
		return unbound
	}
	return false
}
