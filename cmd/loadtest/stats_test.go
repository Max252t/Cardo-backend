package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSummarizeUsesNearestRankPercentiles(t *testing.T) {
	samples := make([]time.Duration, 0, 100)
	for i := 100; i >= 1; i-- {
		samples = append(samples, time.Duration(i)*time.Millisecond)
	}
	d := summarize(samples)
	assert.Equal(t, 100, d.Count)
	assert.InDelta(t, 1, d.MinMs, 0.001)
	assert.InDelta(t, 50, d.P50Ms, 0.001)
	assert.InDelta(t, 95, d.P95Ms, 0.001)
	assert.InDelta(t, 99, d.P99Ms, 0.001)
	assert.InDelta(t, 100, d.MaxMs, 0.001)
	assert.InDelta(t, 50.5, d.MeanMs, 0.001)
}

func TestSummarizeHandlesEmptyAndSingleSamples(t *testing.T) {
	assert.Equal(t, Dist{}, summarize(nil))
	d := summarize([]time.Duration{7 * time.Millisecond})
	assert.Equal(t, 1, d.Count)
	assert.InDelta(t, 7, d.P50Ms, 0.001)
	assert.InDelta(t, 7, d.P99Ms, 0.001)
}

func TestSummarizeDoesNotReorderTheInput(t *testing.T) {
	in := []time.Duration{3 * time.Millisecond, time.Millisecond, 2 * time.Millisecond}
	summarize(in)
	assert.Equal(t, []time.Duration{3 * time.Millisecond, time.Millisecond, 2 * time.Millisecond}, in)
}

func TestReportPassRequiresBothTargetsAndNoFailures(t *testing.T) {
	r := Report{Targets: Targets{P95Pass: true, ResyncPass: true}}
	assert.True(t, r.Passed())
	r.Targets.ResyncPass = false
	assert.False(t, r.Passed())
	r.Targets.ResyncPass = true
	r.Failures = []string{"boom"}
	assert.False(t, r.Passed())
}

func TestReportRendersTextAndJSON(t *testing.T) {
	r := Report{
		Config:  ConfigInfo{URL: "http://x", Clients: 3, RatePerSec: 10},
		Latency: Dist{Count: 4, P95Ms: 12.5},
		Resync:  []ResyncTrial{{Victim: 1, Missed: 10, CaughtUp: true, Handshake: handshakeResult{Kind: "delta", Events: 10, ReadyMs: 30}}},
		Targets: Targets{P95LimitMs: 200, ResyncLimitMs: 2000, P95Pass: true, ResyncPass: true},
	}
	var text bytes.Buffer
	r.WriteText(&text)
	assert.Contains(t, text.String(), "Cardo load test against http://x")
	assert.Contains(t, text.String(), "Result: PASS")

	data, err := json.Marshal(r)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Contains(t, decoded, "latency_all")
	assert.Contains(t, decoded, "resync")
}

func TestOptionsValidation(t *testing.T) {
	valid := options{Clients: 5, Rate: 10, Duration: time.Second, RegisterParallelism: 1}
	require.NoError(t, valid.validate())
	for name, mutate := range map[string]func(*options){
		"too few clients": func(o *options) { o.Clients = 1 },
		"zero rate":       func(o *options) { o.Rate = 0 },
		"zero duration":   func(o *options) { o.Duration = 0 },
		"negative resync": func(o *options) { o.ResyncEvents = -1 },
		"no parallelism":  func(o *options) { o.RegisterParallelism = 0 },
	} {
		o := valid
		mutate(&o)
		assert.Error(t, o.validate(), name)
	}
}
