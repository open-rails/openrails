// Command gate is an API whose requests count toward usage limits: each
// request is admitted in process by an openrails.Gate against holds of the
// customer's capacity, its work runs, and the units it actually used are
// captured and sent on the gate's next sync. A warm decision makes no
// network call, so the API keeps deciding while OpenRails is briefly
// unreachable.
//
// Run it with OPENRAILS_URL, the merchant's API URL, OPENRAILS_MERCHANT_ID,
// its ID, and OPENRAILS_API_KEY, a merchant API key holding
// merchant:usage:manage and merchant:usage:read (README.md). REDIS_URL makes the API's replicas one gate. ADDR is the
// listen address (default :8080). The catalog declares the limits
// (catalog.yaml).
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	merchant, err := billing.ParseMerchantID(os.Getenv("OPENRAILS_MERCHANT_ID"))
	if err != nil {
		log.Fatal(err)
	}
	remote, err := openrails.NewRemote(os.Getenv("OPENRAILS_URL"), openrails.RemoteOptions{
		Credentials: map[billing.MerchantID]openrails.RemoteCredential{merchant: {APIKey: os.Getenv("OPENRAILS_API_KEY")}},
		Timeout:     5 * time.Second,
	})
	if err != nil {
		log.Fatal(err)
	}
	client, err := remote.Merchant(merchant)
	if err != nil {
		log.Fatal(err)
	}
	// guide:gate-new
	var deps openrails.GateDeps
	if url := os.Getenv("REDIS_URL"); url != "" {
		opts, err := redis.ParseURL(url)
		if err != nil {
			log.Fatal(err)
		}
		rdb := redis.NewClient(opts)
		defer rdb.Close()
		deps.Redis = rdb
	}
	gate, err := openrails.NewGate(client, deps)
	if err != nil {
		log.Fatal(err)
	}
	// guide:end

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/work", func(w http.ResponseWriter, r *http.Request) { work(w, r, gate) })
	mux.HandleFunc("GET /v1/limits", func(w http.ResponseWriter, r *http.Request) {
		customer, ok := caller(w, r)
		if !ok {
			return
		}
		lines, err := gate.ListCustomerLimits(r.Context(), customer)
		if err != nil {
			http.Error(w, err.Error(), status(err))
			return
		}
		_ = json.NewEncoder(w).Encode(lines)
	})
	server := &http.Server{Addr: cmp.Or(os.Getenv("ADDR"), ":8080"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	// In-flight requests have captured or released their tickets. Close
	// sends the captured usage and closes the lease, giving its holds back,
	// before main returns and Redis closes.
	<-drained
	closing, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := gate.Close(closing); err != nil {
		log.Print(err)
	}
}

// caller is the customer the request acts for. A real API takes it from
// its own authentication; this one reads a header.
func caller(w http.ResponseWriter, r *http.Request) (billing.CustomerID, bool) {
	id, err := uuid.Parse(r.Header.Get("X-Customer-ID"))
	if err != nil {
		http.Error(w, "X-Customer-ID names the customer", http.StatusUnauthorized)
		return billing.CustomerID{}, false
	}
	return billing.CustomerID(id), true
}

// work admits the request's estimate, does the work, and captures what it
// used. A refusal is a verdict: 429 with when to retry.
func work(w http.ResponseWriter, r *http.Request, gate *openrails.Gate) {
	customer, ok := caller(w, r)
	if !ok {
		return
	}
	estimate, err := strconv.ParseInt(r.URL.Query().Get("units"), 10, 64)
	if err != nil || estimate < 1 {
		http.Error(w, "units is a positive estimate", http.StatusBadRequest)
		return
	}
	// guide:gate-admit
	ticket, err := gate.Admit(r.Context(), billing.AdmitParams{
		RequestID: uuid.NewString(), CustomerID: customer, Invoker: customer.String(), Currency: "USD",
		Usage: &billing.UsageEstimate{EventType: "work", Dimensions: map[string]int64{"units": estimate}},
	})
	if err != nil {
		http.Error(w, err.Error(), status(err))
		return
	}
	if refusal := ticket.Refusal(); refusal != nil {
		if refusal.RetryAfterSeconds != nil {
			w.Header().Set("Retry-After", strconv.FormatInt(*refusal.RetryAfterSeconds, 10))
		} else if refusal.ResetsAt != nil {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(time.Until(*refusal.ResetsAt).Seconds())+1, 10))
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(refusal)
		return
	}
	used, err := doWork(r.Context(), estimate)
	if err != nil {
		_ = ticket.Release(r.Context())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := ticket.Capture(r.Context(), billing.CaptureUsage{Dimensions: map[string]int64{"units": used}}); err != nil {
		log.Print(err)
	}
	// guide:end
	_ = json.NewEncoder(w).Encode(map[string]int64{"units": used})
}

// doWork stands for the metered work: it uses up to its estimate.
func doWork(ctx context.Context, estimate int64) (int64, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(10 * time.Millisecond):
		return max(1, estimate*3/4), nil
	}
}

func status(err error) int {
	switch {
	case errors.Is(err, openrails.ErrUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, billing.ErrInvalid):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}
