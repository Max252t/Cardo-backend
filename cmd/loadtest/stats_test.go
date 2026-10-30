package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
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

func ranks(n int) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = time.Duration(i + 1)
	}
	return out
}

func TestPercentileMatchesIntegerNearestRankForEverySampleCount(t *testing.T) {
	for _, pct := range []int{50, 95, 99} {
		p := float64(pct) / 100
		for n := 1; n <= 3000; n++ {
			want := time.Duration((pct*n + 99) / 100)
			require.Equal(t, want, percentile(ranks(n), p), "p%d of %d samples", pct, n)
		}
	}
}

func TestPercentileBoundaries(t *testing.T) {
	assert.Zero(t, percentile(nil, 0.95))
	assert.Equal(t, time.Duration(1), percentile(ranks(10), 0))
	assert.Equal(t, time.Duration(10), percentile(ranks(10), 1))
	assert.Equal(t, time.Duration(10), percentile(ranks(10), 7))
	assert.Equal(t, time.Duration(1), percentile(ranks(10), -1))
}

func TestSummarizeTwoSamples(t *testing.T) {
	d := summarize([]time.Duration{20 * time.Millisecond, 10 * time.Millisecond})
	assert.Equal(t, 2, d.Count)
	assert.InDelta(t, 10, d.MinMs, 0.001)
	assert.InDelta(t, 10, d.P50Ms, 0.001)
	assert.InDelta(t, 20, d.P95Ms, 0.001)
	assert.InDelta(t, 20, d.P99Ms, 0.001)
	assert.InDelta(t, 20, d.MaxMs, 0.001)
	assert.InDelta(t, 15, d.MeanMs, 0.001)
}

func TestSummarizeKeepsSubMillisecondPrecisionAndDuplicates(t *testing.T) {
	d := summarize([]time.Duration{1500 * time.Microsecond, 1500 * time.Microsecond, 1500 * time.Microsecond})
	assert.InDelta(t, 1.5, d.MinMs, 0.0001)
	assert.InDelta(t, 1.5, d.P99Ms, 0.0001)
	assert.InDelta(t, 1.5, d.MeanMs, 0.0001)
}

func TestPercentilesAreMonotonic(t *testing.T) {
	samples := []time.Duration{9, 1, 7, 3, 5, 3, 8, 2, 2, 100}
	d := summarize(samples)
	assert.LessOrEqual(t, d.MinMs, d.P50Ms)
	assert.LessOrEqual(t, d.P50Ms, d.P95Ms)
	assert.LessOrEqual(t, d.P95Ms, d.P99Ms)
	assert.LessOrEqual(t, d.P99Ms, d.MaxMs)
}

func TestReportWithoutLatencySamplesNeverPassesTheP95Target(t *testing.T) {
	r := &runner{opts: options{P95Target: time.Second, ResyncTarget: time.Second}, col: newCollector(), report: Report{}}
	r.finish()
	assert.False(t, r.report.Targets.P95Pass)
	assert.True(t, r.report.Targets.ResyncPass)
	assert.False(t, r.report.Passed())
}

func TestReportFlagsMissingEventsAndResyncsThatDidNotCatchUp(t *testing.T) {
	col := newCollector()
	col.recording.Store(true)
	id := uuid.New()
	col.opSent(id, 0, time.Now())
	col.eventReceived(id, time.Now().Add(time.Millisecond), 0)
	r := &runner{
		opts:    options{P95Target: time.Second, ResyncTarget: time.Second},
		col:     col,
		clients: make([]*wsClient, 2),
		report:  Report{Resync: []ResyncTrial{{Victim: 1, TargetSeq: 9, CaughtUp: false, CaughtUpMs: 5000, Handshake: handshakeResult{ReadyMs: 10}}}},
	}
	r.finish()
	assert.Equal(t, 1, r.report.Ops.MissingEvents)
	assert.False(t, r.report.Targets.ResyncPass)
	assert.InDelta(t, 5000, r.report.ResyncMaxMs, 0.001)
	require.Len(t, r.report.Failures, 2)
	assert.Contains(t, r.report.Failures[0], "did not catch up to seq 9")
	assert.Contains(t, r.report.Failures[1], "never arrived")
	assert.False(t, r.report.Passed())
}
