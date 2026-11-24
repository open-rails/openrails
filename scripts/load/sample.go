package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Sample is the machine, the processes and the services every few seconds.
type Sample struct {
	At          time.Time          `json:"at"`
	Phase       string             `json:"phase"`
	MachineBusy float64            `json:"machine_busy"`
	Cores       map[string]float64 `json:"cores"`
	MemoryMB    map[string]float64 `json:"memory_mb"`
	CHParts     uint64             `json:"ch_active_parts"`
	CHMaxParts  uint64             `json:"ch_max_parts_per_partition"`
	CHMerges    uint64             `json:"ch_merges"`
	CHMemoryMB  float64            `json:"ch_memory_tracked_mb"`
	CHAsync     int64              `json:"ch_pending_async_inserts"`
	PGActive    int64              `json:"pg_active"`
	PGLockWaits int64              `json:"pg_lock_waits"`
}

const sampleEvery = 5 * time.Second

func (f *fleet) currentPhase() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.phase
}

// sample records a Sample every sampleEvery until the returned func is
// called. Cores are per process (from /proc) and per container (docker
// stats).
func (e *env) sample(ctx context.Context, w *WorldResult, database string, phase func() string, pids func() map[string]int) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		prevBusy, prevTotal := machineTicks()
		prevProc := map[int]int64{}
		prevAt := time.Now()
		dockerOK := true
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(sampleEvery):
			}
			now := time.Now()
			el := now.Sub(prevAt).Seconds()
			s := Sample{At: now.UTC(), Phase: phase(), Cores: map[string]float64{}, MemoryMB: map[string]float64{}}
			busy, total := machineTicks()
			if total > prevTotal {
				s.MachineBusy = float64(busy-prevBusy) / float64(total-prevTotal)
			}
			prevBusy, prevTotal = busy, total
			for name, pid := range pids() {
				ticks, rss := procStat(pid)
				if last, ok := prevProc[pid]; ok && ticks >= last {
					s.Cores[name] = float64(ticks-last) / 100 / el
				}
				prevProc[pid] = ticks
				s.MemoryMB[name] = rss
			}
			prevAt = now
			if dockerOK {
				dockerOK = dockerStats(ctx, &s)
			}
			e.chSample(ctx, &s, database)
			pgSample(ctx, e.pool, &s)
			w.smu.Lock()
			w.Samples = append(w.Samples, s)
			w.smu.Unlock()
		}
	}()
	return func() { cancel(); <-done }
}

// cpuBetween is the mean machine load and cores per process or container
// over the samples taken between from and to.
func (w *WorldResult) cpuBetween(from, to time.Time) (float64, map[string]float64) {
	w.smu.Lock()
	defer w.smu.Unlock()
	cores := map[string]float64{}
	var busy float64
	n := 0
	for _, s := range w.Samples {
		if s.At.Before(from) || s.At.After(to.Add(sampleEvery)) {
			continue
		}
		n++
		busy += s.MachineBusy
		for k, v := range s.Cores {
			name := k
			if strings.Contains(k, "gates") {
				name = "gate processes"
			}
			cores[name] += v
		}
	}
	if n == 0 {
		return 0, cores
	}
	for k := range cores {
		cores[k] /= float64(n)
	}
	return busy / float64(n), cores
}

// machineTicks is /proc/stat's busy and total CPU ticks.
func machineTicks() (busy, total int64) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	fields := strings.Fields(splitLines(string(data))[0])
	for i, f := range fields[1:] {
		v, _ := strconv.ParseInt(f, 10, 64)
		total += v
		if i != 3 && i != 4 {
			busy += v
		}
	}
	return busy, total
}

// procStat is a process's user and system ticks and its resident MB.
func procStat(pid int) (int64, float64) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0
	}
	s := string(data)
	fields := strings.Fields(s[strings.LastIndexByte(s, ')')+2:])
	if len(fields) < 22 {
		return 0, 0
	}
	ut, _ := strconv.ParseInt(fields[11], 10, 64)
	st, _ := strconv.ParseInt(fields[12], 10, 64)
	rss, _ := strconv.ParseInt(fields[21], 10, 64)
	return ut + st, float64(rss*int64(os.Getpagesize())) / (1 << 20)
}

// dockerStats adds each container's CPU and memory; false when docker is
// not there to ask.
func dockerStats(ctx context.Context, s *Sample) bool {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}").Output()
	if err != nil {
		return ctx.Err() != nil
	}
	for _, line := range splitLines(string(out)) {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			continue
		}
		cpu, _ := strconv.ParseFloat(strings.TrimSuffix(parts[1], "%"), 64)
		s.Cores[parts[0]] = cpu / 100
		used, _, _ := strings.Cut(parts[2], " / ")
		s.MemoryMB[parts[0]] = parseMB(used)
	}
	return true
}

func parseMB(v string) float64 {
	v = strings.TrimSpace(v)
	for _, u := range []struct {
		suffix string
		mb     float64
	}{{"GiB", 1024}, {"MiB", 1}, {"KiB", 1.0 / 1024}, {"GB", 1000}, {"MB", 1}, {"kB", 1.0 / 1000}, {"B", 1.0 / (1 << 20)}} {
		if strings.HasSuffix(v, u.suffix) {
			n, _ := strconv.ParseFloat(strings.TrimSuffix(v, u.suffix), 64)
			return n * u.mb
		}
	}
	return 0
}

func (e *env) chSample(ctx context.Context, s *Sample, database string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = e.ch.QueryRow(ctx, "SELECT count(), max(c) FROM (SELECT table, partition, count() c FROM system.parts WHERE database = ? AND active GROUP BY table, partition)", database).Scan(&s.CHParts, &s.CHMaxParts)
	_ = e.ch.QueryRow(ctx, "SELECT count() FROM system.merges WHERE database = ?", database).Scan(&s.CHMerges)
	var mem, async int64
	_ = e.ch.QueryRow(ctx, "SELECT sumIf(value, metric = 'MemoryTracking'), sumIf(value, metric = 'PendingAsyncInsert') FROM system.metrics").Scan(&mem, &async)
	s.CHMemoryMB, s.CHAsync = float64(mem)/(1<<20), async
}

func pgSample(ctx context.Context, pool *pgxpool.Pool, s *Sample) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE state = 'active'), count(*) FILTER (WHERE wait_event_type = 'Lock') FROM pg_stat_activity").Scan(&s.PGActive, &s.PGLockWaits)
}

// postgresSettings records the server's settings that bear on the numbers.
func postgresSettings(ctx context.Context, pool *pgxpool.Pool) map[string]string {
	out := map[string]string{}
	rows, err := pool.Query(ctx, `SELECT name, setting || coalesce(unit, '') FROM pg_settings WHERE name IN
 ('server_version','max_connections','shared_buffers','effective_cache_size','work_mem','synchronous_commit','max_wal_size','checkpoint_timeout','wal_level','fsync')`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			out[k] = v
		}
	}
	return out
}

func splitLines(s string) []string {
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}
