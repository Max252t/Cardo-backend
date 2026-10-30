package main

import (
	"math"
	"sort"
	"time"
)

type Dist struct {
	Count  int     `json:"count"`
	MinMs  float64 `json:"min_ms"`
	MeanMs float64 `json:"mean_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	P99Ms  float64 `json:"p99_ms"`
	MaxMs  float64 `json:"max_ms"`
}

func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	rank = min(max(rank, 0), len(sorted)-1)
	return sorted[rank]
}

func summarize(samples []time.Duration) Dist {
	if len(samples) == 0 {
		return Dist{}
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var total time.Duration
	for _, s := range sorted {
		total += s
	}
	return Dist{
		Count:  len(sorted),
		MinMs:  ms(sorted[0]),
		MeanMs: ms(total / time.Duration(len(sorted))),
		P50Ms:  ms(percentile(sorted, 0.50)),
		P95Ms:  ms(percentile(sorted, 0.95)),
		P99Ms:  ms(percentile(sorted, 0.99)),
		MaxMs:  ms(sorted[len(sorted)-1]),
	}
}
