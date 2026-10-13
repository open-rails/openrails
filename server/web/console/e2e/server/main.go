// A loopback-only browser fixture over the actual standalone server.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/fxfake"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/controlplane"
)

const password = "Console-test-password-42!"

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "loopback listen address")
	base := flag.String("base-url", "", "browser origin")
	assets := flag.String("assets", "", "compiled console directory")
	flag.Parse()
	if !strings.HasPrefix(*addr, "127.0.0.1:") || *base == "" || *assets == "" {
		panic("fixture needs loopback address, browser origin and compiled assets")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, *addr, *base, *assets); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, addr, base, assets string) error {
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	fx := fxfake.New()
	defer fx.Close()
	outbox := &authtest.Outbox{}
	srv, err := server.New(ctx, server.Config{
		Engine:      openrails.Config{Database: openrails.DatabaseConfig{Schema: "console", RiverSchema: "console"}, ProviderWriteMode: openrails.ProviderWritesReadOnly, PublicBillingBaseURL: base, ReturnOrigins: []string{base}},
		LocalSignIn: true,
		Auth: authkit.Config{
			Registration: authkit.RegistrationConfig{PasswordlessLogin: true},
			Database:     authkit.DatabaseConfig{Schema: "console_auth"},
			Token:        authkit.TokenConfig{Issuer: base + "/auth"},
			Keys:         authkit.KeysConfig{AllowEphemeralDevKeys: true},
			TwoFactor:    authkit.TwoFactorConfig{Mode: iam.TwoFactorDisabled},
			Passkeys:     authkit.PasskeyConfig{RPID: "localhost", RPDisplayName: "Console test", Origins: []string{base}},
			HTTP:         &authkit.HTTPConfig{DirectPeerIP: true, RateLimits: map[string]authkit.RateLimit{"invite_create": {Limit: 100, Window: time.Hour}}},
		},
		RouteGroups: openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true, Programmatic: true},
	}, server.Deps{Engine: openrails.Deps{Postgres: pool, FXTransport: fx.Transport()}, Auth: authkit.Deps{Email: outbox.Email()}, ConsoleAssets: os.DirFS(assets)})
	if err != nil {
		return err
	}
	defer srv.Close(context.Background())
	if err := engine.Graph(srv.Engine()).Runtime.InitRiver(ctx); err != nil {
		return err
	}
	var merchants []billing.MerchantID
	for _, name := range []string{"alpha", "beta"} {
		created, err := srv.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: name})
		if err != nil {
			return err
		}
		merchants = append(merchants, created.MerchantID)
	}
	write := func(w http.ResponseWriter, status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__test/health", func(w http.ResponseWriter, r *http.Request) { write(w, 200, map[string]any{"merchants": merchants}) })
	mux.HandleFunc("POST /__test/staff/{role}", func(w http.ResponseWriter, r *http.Request) {
		suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		email := "staff-" + suffix + "@console.test"
		user, err := srv.AuthKit().CreateUser(r.Context(), iam.NewUser{Email: email, Username: "staff" + suffix, Password: password, EmailVerified: true})
		if err != nil {
			write(w, 500, map[string]string{"error": err.Error()})
			return
		}
		var role iam.Role
		group := iam.RootGroup()
		switch r.PathValue("role") {
		case "owner":
			role = controlplane.Roles.Root.Owner
		case "support":
			role = controlplane.MerchantSupport
			group = iam.GroupByID(merchants[0].String())
		default:
			write(w, 400, map[string]string{"error": "unknown fixture role"})
			return
		}
		if _, err = srv.AuthKit().SetGroupRole(r.Context(), iam.SystemIdentity(), group, iam.UserSubject(user.ID), role); err != nil {
			write(w, 500, map[string]string{"error": err.Error()})
			return
		}
		write(w, 201, map[string]string{"id": user.ID, "email": email})
	})
	mux.HandleFunc("GET /__test/mail", func(w http.ResponseWriter, r *http.Request) {
		messages := outbox.Messages(iam.MessageKind(r.URL.Query().Get("kind")), r.URL.Query().Get("to"))
		if len(messages) == 0 {
			write(w, 404, map[string]string{"error": "no message"})
			return
		}
		message := messages[len(messages)-1]
		write(w, 200, map[string]string{"code": message.Code, "link": message.Link})
	})
	mux.HandleFunc("POST /__test/stale-session", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			write(w, 400, map[string]string{"error": "access token required"})
			return
		}
		claims, err := srv.AuthKit().Verify(r.Context(), input.AccessToken)
		if err != nil || claims.SessionID == "" {
			write(w, 400, map[string]string{"error": "real sign-in session required"})
			return
		}
		// Match authtest.StaleSession's fixture operation: age the real
		// sign-in, then let AuthKit issue its own session assurance claims.
		// No production freshness policy or response is replaced.
		tag, err := pool.Exec(r.Context(), `UPDATE console_auth.refresh_sessions
			SET last_authenticated_at = now() - interval '1 day',
			    mfa_authenticated_at = CASE WHEN mfa_authenticated_at IS NULL THEN NULL ELSE now() - interval '1 day' END
			WHERE id = $1::uuid`, claims.SessionID)
		if err != nil || tag.RowsAffected() != 1 {
			write(w, 500, map[string]string{"error": "fixture session could not be aged"})
			return
		}
		token, err := srv.AuthKit().MintAccessToken(r.Context(), claims.UserID, iam.AccessTokenOptions{SessionID: claims.SessionID})
		if err != nil {
			write(w, 500, map[string]string{"error": err.Error()})
			return
		}
		write(w, 200, map[string]string{"access_token": token.Value})
	})
	mux.Handle("/", srv.Handler())
	httpServer := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		stop, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = httpServer.Shutdown(stop)
	}()
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
