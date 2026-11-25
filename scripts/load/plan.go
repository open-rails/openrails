package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Plan is one qualification run. Rates are usage events a second (each a
// gate decision that is then captured or released).
type Plan struct {
	DSN                 string `json:"dsn"`
	RedisURL            string `json:"redis_url"`
	ClickHouse          string `json:"clickhouse"`
	ClickHouseContainer string `json:"clickhouse_container"`

	// Processes is how many gate processes each world runs; every process
	// has one gate per merchant and serves every customer of it.
	Processes int `json:"processes"`
	// Hot is the busy merchant whose rate is ramped; Normal merchants keep
	// their rate throughout. The hot merchant's active customers grow with
	// its rate, each making CustomerRate requests a second, up to all of
	// them; past that each customer is busier.
	Hot          MerchantPlan   `json:"hot"`
	Normal       []MerchantPlan `json:"normal"`
	CustomerRate float64        `json:"customer_rate"`

	// A request costs Units[0]..Units[1] units at UnitPrice micros a unit.
	Units     [2]int64 `json:"units"`
	UnitPrice int64    `json:"unit_price"`
	// A ticket stays open WorkMeanMS on average (exponential), LongShare of
	// them 2 to 6 seconds (agent jobs); ReleaseShare are released, not
	// captured.
	WorkMeanMS   float64 `json:"work_mean_ms"`
	LongShare    float64 `json:"long_share"`
	ReleaseShare float64 `json:"release_share"`

	// Draining customers race a small balance from every process.
	DrainCustomers int     `json:"drain_customers"`
	DrainFunding   int64   `json:"drain_funding"`
	DrainRate      float64 `json:"drain_rate"`

	TopUps   int `json:"top_ups"`
	Expiring int `json:"expiring"`

	WarmSeconds   int   `json:"warm_seconds"`
	SteadySeconds int   `json:"steady_seconds"`
	Ramp          []int `json:"ramp"`
	StepSeconds   int   `json:"step_seconds"`
	// CooldownSeconds at the base rates follow the ramp, so the faults start
	// from a settled fleet.
	CooldownSeconds int `json:"cooldown_seconds"`

	LostReplyShare  float64 `json:"lost_reply_share"`
	FaultSeconds    int     `json:"fault_seconds"`
	CrashRate       float64 `json:"crash_rate"`
	CrashCustomers  int     `json:"crash_customers"`
	OutageSeconds   int     `json:"outage_seconds"`
	RecoverySeconds int     `json:"recovery_seconds"`

	EmbeddedSteadySeconds int   `json:"embedded_steady_seconds"`
	EmbeddedRamp          []int `json:"embedded_ramp"`

	PostingCustomers int   `json:"posting_customers"`
	PostingEvents    int   `json:"posting_events"`
	HeavyRows        int64 `json:"heavy_rows"`

	// The storage world's customers each report StoragePerDay events a day
	// for StorageDays, from StorageInvokers invokers, through
	// StorageReporters concurrent reporters; it stops taking days in after
	// StorageMinutes.
	StorageCustomers int `json:"storage_customers"`
	StoragePerDay    int `json:"storage_per_day"`
	StorageDays      int `json:"storage_days"`
	StorageInvokers  int `json:"storage_invokers"`
	StorageReporters int `json:"storage_reporters"`
	StorageMinutes   int `json:"storage_minutes"`

	Worlds []string `json:"worlds"`
}

type MerchantPlan struct {
	Slug      string  `json:"slug"`
	Customers int     `json:"customers"`
	Rate      float64 `json:"rate"`
}

// defaultPlan is #1367's workload scaled to one 4-vCPU runner: an agent
// customer makes about 24 requests a minute.
func defaultPlan() Plan {
	return Plan{
		Processes:    4,
		Hot:          MerchantPlan{Slug: "hot", Customers: 2500, Rate: 200},
		Normal:       []MerchantPlan{{Slug: "n1", Customers: 250, Rate: 100}, {Slug: "n2", Customers: 250, Rate: 100}, {Slug: "n3", Customers: 250, Rate: 100}},
		CustomerRate: 0.4,
		Units:        [2]int64{10, 300},
		UnitPrice:    1000,
		WorkMeanMS:   300,
		LongShare:    0.05,
		ReleaseShare: 0.03,

		DrainCustomers: 10,
		DrainFunding:   10_000_000,
		DrainRate:      4,
		TopUps:         20,
		Expiring:       20,

		WarmSeconds:     15,
		SteadySeconds:   75,
		Ramp:            []int{400, 1000, 2000, 4000, 8000, 16000},
		StepSeconds:     40,
		CooldownSeconds: 20,

		LostReplyShare:  0.05,
		FaultSeconds:    30,
		CrashRate:       25,
		CrashCustomers:  100,
		OutageSeconds:   30,
		RecoverySeconds: 30,

		EmbeddedSteadySeconds: 45,
		EmbeddedRamp:          []int{400, 1000, 2000, 4000, 8000},

		PostingCustomers: 2000,
		PostingEvents:    10,
		HeavyRows:        26_000_000,

		StorageCustomers: 2000,
		StoragePerDay:    31,
		StorageDays:      24,
		StorageInvokers:  3,
		StorageReporters: 4,
		StorageMinutes:   16,

		Worlds: []string{"http", "embedded", "posting"},
	}
}

func (p Plan) merchants() []MerchantPlan { return append([]MerchantPlan{p.Hot}, p.Normal...) }

func (p Plan) maxCost() int64 { return p.Units[1] * p.UnitPrice }

// active is how many of m's customers a rate keeps busy: the hot
// merchant's grow with its rate; every other merchant's are all of them.
func (p Plan) active(m MerchantPlan, rate float64) int {
	if m.Slug != p.Hot.Slug || p.CustomerRate <= 0 {
		return m.Customers
	}
	return max(1, min(m.Customers, int(rate/p.CustomerRate+0.5)))
}

func (p Plan) validate() error {
	switch {
	case p.DSN == "" || p.ClickHouse == "":
		return fmt.Errorf("plan: dsn and clickhouse are required")
	case p.Processes < 1 || p.Hot.Customers < 1 || p.Units[0] < 1 || p.Units[1] < p.Units[0] || p.UnitPrice < 1:
		return fmt.Errorf("plan: processes, customers, units and unit price must be positive")
	}
	return nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// parseInts reads "1000,2000"; empty is nil.
func parseInts(s string) ([]int, error) {
	var out []int
	for f := range strings.SplitSeq(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}
