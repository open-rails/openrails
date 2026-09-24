// Command server runs real AuthKit + embedded OpenRails on Postgres for
// billing-ui's Playwright suite. Test-only: /__test/* creates and seeds users.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/open-rails/billing-ui/e2e/server/harness"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:4790", "listen address")
	baseURL := flag.String("base-url", "", "public origin (default http://localhost:<port>)")
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "Postgres DSN")
	static := flag.String("static", "", "directory served at /")
	lifetime := flag.Duration("lifetime", 30*time.Minute, "exit after this long")
	flag.Parse()
	if *baseURL == "" {
		_, port, err := net.SplitHostPort(*addr)
		if err != nil {
			log.Fatal(err)
		}
		*baseURL = "http://localhost:" + port
	}
	if err := run(*addr, *baseURL, *dsn, *static, *lifetime); err != nil {
		log.Fatal(err)
	}
}

func run(addr, baseURL, dsn, static string, lifetime time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, lifetime)
	defer cancel()

	pool, err := harness.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	rt, err := harness.New(ctx, baseURL, dsn, pool, true)
	if err != nil {
		return err
	}
	defer rt.Close()
	if err := rt.Auth.Start(ctx); err != nil {
		return err
	}

	mux := http.NewServeMux()
	if err := rt.Mount(mux); err != nil {
		return err
	}
	mux.HandleFunc("GET /__test/health", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		reply(w, http.StatusOK, rt.Catalog)
	})
	mux.HandleFunc("POST /__test/users", func(w http.ResponseWriter, r *http.Request) {
		u, err := rt.CreateUser(r.Context())
		if err != nil {
			reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		reply(w, http.StatusCreated, u)
	})
	mux.HandleFunc("POST /__test/users/{id}/billing", func(w http.ResponseWriter, r *http.Request) {
		s, err := rt.SeedBilling(r.Context(), r.PathValue("id"))
		if err != nil {
			reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		reply(w, http.StatusCreated, s)
	})
	if static != "" {
		mux.Handle("GET /", http.FileServer(http.Dir(static)))
	}

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("billing-ui e2e server on %s (%s)", addr, baseURL)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	return srv.Shutdown(shutdown)
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
