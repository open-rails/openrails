package main

import (
	"math"
	"math/bits"
	"sort"
	"sync"
	"time"
)

// Hist is a log-linear histogram of microseconds: 16 linear steps per power
// of two, so a quantile is within 1/16 of its value. Buckets are sparse.
type Hist struct {
	B   map[int]uint64 `json:"b"`
	N   uint64         `json:"n"`
	Sum float64        `json:"sum_us"`
	Max int64          `json:"max_us"`
}

func newHist() *Hist { return &Hist{B: map[int]uint64{}} }

func bucketOf(us int64) int {
	if us < 16 {
		return int(max(us, 0))
	}
	e := bits.Len64(uint64(us)) - 1
	return (e-3)*16 + int((us>>(e-4))&15)
}

// bucketLow is the smallest value of bucket i; bucketLow(i+1) bounds it above.
func bucketLow(i int) int64 {
	if i < 16 {
		return int64(i)
	}
	e := i/16 + 3
	return (16 + int64(i%16)) << (e - 4)
}

func (h *Hist) Add(d time.Duration) { h.AddUS(d.Microseconds()) }

func (h *Hist) AddUS(us int64) {
	h.B[bucketOf(us)]++
	h.N++
	h.Sum += float64(us)
	h.Max = max(h.Max, us)
}

func (h *Hist) Merge(o *Hist) {
	if o == nil {
		return
	}
	for b, n := range o.B {
		h.B[b] += n
	}
	h.N += o.N
	h.Sum += o.Sum
	h.Max = max(h.Max, o.Max)
}

// Q is quantile q in microseconds: the middle of its bucket, at most Max.
func (h *Hist) Q(q float64) int64 {
	if h == nil || h.N == 0 {
		return 0
	}
	keys := make([]int, 0, len(h.B))
	for b := range h.B {
		keys = append(keys, b)
	}
	sort.Ints(keys)
	target := uint64(math.Ceil(q * float64(h.N)))
	target = max(target, 1)
	var seen uint64
	for _, b := range keys {
		seen += h.B[b]
		if seen >= target {
			lo, hi := bucketLow(b), bucketLow(b+1)
			return min(lo+(hi-lo)/2, h.Max)
		}
	}
	return h.Max
}

// hists is a set of histograms by key, safe for concurrent use.
type hists struct {
	mu sync.Mutex
	m  map[string]*Hist
}

func newHists() *hists { return &hists{m: map[string]*Hist{}} }

func (s *hists) add(key string, d time.Duration) {
	s.mu.Lock()
	h := s.m[key]
	if h == nil {
		h = newHist()
		s.m[key] = h
	}
	h.Add(d)
	s.mu.Unlock()
}

func (s *hists) snapshot() map[string]*Hist {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*Hist, len(s.m))
	for k, h := range s.m {
		c := newHist()
		c.Merge(h)
		out[k] = c
	}
	return out
}
