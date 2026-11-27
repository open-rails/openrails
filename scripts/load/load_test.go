package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
)

func TestHistQuantilesWithinABucket(t *testing.T) {
	for _, us := range []int64{0, 1, 15, 16, 17, 31, 32, 33, 1000, 123_456, 9_999_999} {
		b := bucketOf(us)
		if lo, hi := bucketLow(b), bucketLow(b+1); us < lo || us >= hi {
			t.Fatalf("%d is in bucket %d [%d, %d)", us, b, lo, hi)
		}
	}
	h := newHist()
	for i := range int64(1000) {
		h.AddUS(i + 1)
	}
	for _, c := range []struct {
		q    float64
		want int64
	}{{0.5, 500}, {0.99, 990}, {1, 1000}} {
		if got := h.Q(c.q); got < c.want*15/16 || got > c.want*17/16 {
			t.Errorf("q%.2f = %d, want about %d", c.q, got, c.want)
		}
	}
	other := newHist()
	other.Add(5 * time.Second)
	h.Merge(other)
	if h.N != 1001 || h.Max != 5_000_000 || h.Q(1) != 5_000_000 {
		t.Fatalf("merged: n %d max %d q1 %d", h.N, h.Max, h.Q(1))
	}
}

func TestOvershootBoundCountsOtherGatesInTheWindow(t *testing.T) {
	s := int64(time.Second)
	adms := []admission{
		{Proc: 0, At: 0, Cost: 100},      // outside the window
		{Proc: 0, At: 7 * s, Cost: 300},  // gate 0
		{Proc: 1, At: 8 * s, Cost: 200},  // gate 1
		{Proc: 2, At: 9 * s, Cost: 50},   // gate 2
		{Proc: 1, At: 10 * s, Cost: 100}, // last
	}
	bound, gates := overshootBound(adms, 4*time.Second, 1000)
	// In [6s, 10s]: gate 0 300, gate 1 300, gate 2 50. The most another
	// gate's view misses is everything but gate 2's own: 600.
	if gates != 3 || bound != 600+3*1000 {
		t.Fatalf("bound %d over %d gates", bound, gates)
	}
	if b, g := overshootBound(nil, time.Second, 1); b != 0 || g != 0 {
		t.Fatalf("no admissions: %d %d", b, g)
	}
}

func TestCrashRequestKinds(t *testing.T) {
	for id, want := range map[string]string{"c0-1": "outbox", "c1-9": "plain", "c2-3": "outbox", "c3-4": "plain"} {
		if got := kindOfPrefix(id); got != want {
			t.Errorf("%s: %s, want %s", id, got, want)
		}
	}
}

func TestFileOutboxReplaysWhatWasNotAcked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.jsonl")
	ctx := context.Background()
	o, err := openOutbox(path)
	if err != nil {
		t.Fatal(err)
	}
	lease := billing.LeaseID(uuid.Must(uuid.NewV7()))
	items := []billing.OutboxItem{{LeaseID: lease, Usage: billing.LeaseUsage{N: 1, SourceID: "a"}}, {LeaseID: lease, Usage: billing.LeaseUsage{N: 2, SourceID: "b"}}}
	if err := o.Put(ctx, items); err != nil {
		t.Fatal(err)
	}
	if err := o.Ack(ctx, []billing.LeaseUsageRef{{LeaseID: lease, N: 1}}); err != nil {
		t.Fatal(err)
	}
	o.close()
	reopened, err := openOutbox(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	pending, _ := reopened.Pending(ctx)
	if len(pending) != 1 || pending[0].Usage.SourceID != "b" {
		t.Fatalf("pending after restart: %+v", pending)
	}
}

func TestParseMB(t *testing.T) {
	for in, want := range map[string]float64{"1.5GiB": 1536, "512MiB": 512, "2048KiB": 2, "0B": 0} {
		if got := parseMB(in); got != want {
			t.Errorf("%s: %v, want %v", in, got, want)
		}
	}
}

func TestSummaryRendersAWorld(t *testing.T) {
	h := newHist()
	h.AddUS(40)
	w := &WorldResult{Name: "http", Mode: "remote",
		Decisions: map[string]*Hist{"steady|hot|local|allowed": h, "steady|hot|server|allowed": h},
		Syncs:     map[string]*Hist{"steady|hot": h}, Counters: map[string]int64{"waited|steady|hot": 1},
		Steps:     []Step{{Rate: 2000, Offered: 2600, Achieved: 2590, Cores: map[string]float64{"server": 1.2}}},
		Checks:    []Check{{Name: "stored-once/hot", OK: true, Detail: "fine"}},
		Overshoot: &Overshoot{Customers: 2, Drained: 2, Max: 300_000, MaxBound: 9_000_000},
		Storage:   []TableStorage{{Table: "usage_events", Rows: 10, Bytes: 400}}}
	r := &Results{Plan: defaultPlan(), Host: map[string]string{"cpus": "4"}, Worlds: map[string]*WorldResult{"http": w},
		Posting: &PostingResult{Syncs: newHist(), TopUpLight: newHist(), DailyPosting: &PassResult{State: "completed", Seconds: 12}},
		Storage: &StorageResult{Shape: "shape", Worlds: []*StorageWorld{{Name: "aged", Events: 4, Syncs: h,
			Tables:  []TableStorage{{Table: "usage_events", Rows: 4, Compressed: 100, Uncompressed: 400, Bytes: 200}},
			Columns: []ColumnStorage{{Column: "event_id", Compressed: 64, Uncompressed: 64}}, History: &HistoryRows{Rows: 8, DayRows: 2}}}}}
	out := summarize(r)
	for _, want := range []string{"| http | stored-once/hot | yes |", "| hot | local | allowed | 1 | 50.00%", "| 2000 | 0 | 2600 | 2590 |", "Daily posting at the month's end", "| usage_events | 10 |",
		"| usage_events | 4 | 0 | 0.0 MB | 0.0 MB | 0.0 MB | 25.0 | 100.0 | 50.0 |", "| event_id | 0.0 MB | 0.0 MB | 16.00 |", "one a UTC day would be 2 rows."} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
}
