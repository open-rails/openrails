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
	// their rate throughout.
	Hot    MerchantPlan   `json:"hot"`
	Normal []MerchantPlan `json:"normal"`

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
		Hot:          MerchantPlan{Slug: "hot", Customers: 2500, Rate: 1000},
		Normal:       []MerchantPlan{{Slug: "n1", Customers: 500, Rate: 200}, {Slug: "n2", Customers: 500, Rate: 200}, {Slug: "n3", Customers: 500, Rate: 200}},
		Units:        [2]int64{10, 300},
		UnitPrice:    1000,
		WorkMeanMS:   300,
		LongShare:    0.05,
		ReleaseShare: 0.03,

		DrainCustomers: 20,
		DrainFunding:   20_000_000,
		DrainRate:      5,
		TopUps:         20,
		Expiring:       20,

		WarmSeconds:   15,
		SteadySeconds: 75,
		Ramp:          []int{2000, 4000, 8000, 16000},
		StepSeconds:   40,

		LostReplyShare:  0.05,
		FaultSeconds:    30,
		CrashRate:       100,
		CrashCustomers:  200,
		OutageSeconds:   30,
		RecoverySeconds: 30,

		EmbeddedSteadySeconds: 45,
		EmbeddedRamp:          []int{2000, 4000},

		PostingCustomers: 2000,
		PostingEvents:    20,
		HeavyRows:        26_000_000,

		Worlds: []string{"http", "embedded", "posting"},
	}
}

func (p Plan) merchants() []MerchantPlan { return append([]MerchantPlan{p.Hot}, p.Normal...) }

func (p Plan) maxCost() int64 { return p.Units[1] * p.UnitPrice }

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
