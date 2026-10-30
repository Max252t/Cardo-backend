package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runExecute(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := execute(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func smallRun(url string, extra ...string) []string {
	args := []string{
		"-url", url,
		"-clients", "3",
		"-rate", "30",
		"-duration", "1s",
		"-warmup", "0s",
		"-resync-trials", "0",
		"-register-parallelism", "3",
		"-seed", "5",
	}
	return append(args, extra...)
}

func TestExecuteRejectsUnknownFlagsAndBadValues(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown flag":  {"-nope"},
		"bad int":       {"-clients", "many"},
		"bad duration":  {"-duration", "soon"},
		"bad float":     {"-rate", "fast"},
		"missing value": {"-url"},
	} {
		code, stdout, stderr := runExecute(args...)
		assert.Equal(t, exitUsage, code, name)
		assert.Empty(t, stdout, name)
		assert.NotEmpty(t, stderr, name)
	}
}

func TestExecuteHelpExitsSuccessfullyAndListsFlags(t *testing.T) {
	code, _, stderr := runExecute("-h")
	assert.Equal(t, exitOK, code)
	for _, flagName := range []string{"-url", "-clients", "-rate", "-duration", "-warmup", "-resync-events", "-resync-trials", "-p95-target", "-resync-target", "-json", "-enforce", "-seed", "-password", "-refresh-interval"} {
		assert.Contains(t, stderr, flagName)
	}
}

func TestExecuteReportsInvalidOptionsAsAnError(t *testing.T) {
	for name, args := range map[string][]string{
		"one client":      {"-clients", "1"},
		"zero rate":       {"-rate", "0"},
		"negative rate":   {"-rate", "-3"},
		"zero duration":   {"-duration", "0s"},
		"negative resync": {"-resync-events", "-1"},
		"no parallelism":  {"-register-parallelism", "0"},
	} {
		code, stdout, stderr := runExecute(append([]string{"-url", "http://127.0.0.1:1"}, args...)...)
		assert.Equal(t, exitError, code, name)
		assert.Empty(t, stdout, name)
		assert.Contains(t, stderr, "load test failed", name)
	}
}

func TestExecuteFailsFastWhenTheServerIsUnreachable(t *testing.T) {
	started := time.Now()
	code, stdout, stderr := runExecute("-url", "http://127.0.0.1:1", "-clients", "2", "-duration", "1s", "-warmup", "0s")
	assert.Equal(t, exitError, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "load test failed")
	assert.Less(t, time.Since(started), 15*time.Second)
}

func TestExecuteFailsWhenRegistrationIsRejected(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":"nope"}`)
	}))
	t.Cleanup(srv.Close)
	code, _, stderr := runExecute("-url", srv.URL, "-clients", "2", "-duration", "1s", "-warmup", "0s")
	assert.Equal(t, exitError, code)
	assert.Contains(t, stderr, "status 422")
	assert.GreaterOrEqual(t, calls.Load(), int32(1))
}

func TestExecuteRetriesRateLimitedRequestsHonouringRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	t.Cleanup(srv.Close)
	api := newAPIClient(srv.URL)
	_, err := api.register(context.Background(), "a@example.com", "A", "pw")
	require.Error(t, err)
	assert.GreaterOrEqual(t, calls.Load(), int32(2))
	assert.Contains(t, err.Error(), "status 422")
}

func TestExecutePrintsATextReportAndExitsZeroWithoutEnforce(t *testing.T) {
	url := startAPI(t)
	code, stdout, stderr := runExecute(smallRun(url, "-p95-target", "1ns")...)
	assert.Equal(t, exitOK, code, stderr)
	assert.Contains(t, stdout, "Cardo load test against "+url)
	assert.Contains(t, stdout, "Result: FAIL")
	assert.NotContains(t, stdout, `"config"`)
}

func TestExecuteEnforceReturnsMissedWhenTheP95TargetIsNotMet(t *testing.T) {
	url := startAPI(t)
	code, stdout, stderr := runExecute(smallRun(url, "-json", "-enforce", "-p95-target", "1ns")...)
	assert.Equal(t, exitMissed, code, stderr)
	var report Report
	require.NoError(t, json.Unmarshal([]byte(stdout), &report))
	assert.False(t, report.Targets.P95Pass)
	assert.False(t, report.Passed())
}

func TestExecuteEnforceReturnsZeroWhenTargetsAreMetAndEmitsStableJSON(t *testing.T) {
	url := startAPI(t)
	code, stdout, stderr := runExecute(smallRun(url, "-json", "-enforce", "-p95-target", "10s")...)
	require.Equal(t, exitOK, code, stderr+stdout)

	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &raw))
	for _, key := range []string{
		"started_at", "config", "setup", "connect_handshake", "latency_all", "latency_sender", "latency_peers",
		"ops", "resync", "snapshot_resync", "resync_max_ms", "failures", "targets", "board_final_seq",
	} {
		assert.Contains(t, raw, key)
	}
	config := raw["config"].(map[string]any)
	for _, key := range []string{"url", "clients", "rate_ops_per_sec", "duration_sec", "warmup_sec", "resync_events", "resync_trials", "seed"} {
		assert.Contains(t, config, key)
	}
	assert.Equal(t, float64(3), config["clients"])
	assert.Equal(t, float64(5), config["seed"])
	for _, key := range []string{"count", "min_ms", "mean_ms", "p50_ms", "p95_ms", "p99_ms", "max_ms"} {
		assert.Contains(t, raw["latency_all"], key)
	}
	for _, key := range []string{"sent", "nacked", "fully_delivered", "missing_receiver_events", "nacks_by_reason", "send_errors"} {
		assert.Contains(t, raw["ops"], key)
	}
	for _, key := range []string{"p95_limit_ms", "resync_limit_ms", "p95_pass", "resync_pass"} {
		assert.Contains(t, raw["targets"], key)
	}
	assert.Equal(t, true, raw["targets"].(map[string]any)["p95_pass"])
	assert.Equal(t, float64(10000), raw["targets"].(map[string]any)["p95_limit_ms"])
}

func TestExecuteReportsFailuresAndMissedResyncTarget(t *testing.T) {
	url := startAPI(t)
	args := []string{
		"-url", url, "-clients", "3", "-rate", "30", "-duration", "1s", "-warmup", "0s",
		"-resync-events", "30", "-resync-trials", "1", "-register-parallelism", "3", "-seed", "9",
		"-p95-target", "10s", "-resync-target", "1ns", "-json", "-enforce",
	}
	code, stdout, stderr := runExecute(args...)
	assert.Equal(t, exitMissed, code, stderr)
	var report Report
	require.NoError(t, json.Unmarshal([]byte(stdout), &report))
	assert.True(t, report.Targets.P95Pass)
	assert.False(t, report.Targets.ResyncPass)
	require.Len(t, report.Resync, 1)
	assert.True(t, report.Resync[0].CaughtUp)
	assert.Equal(t, "snapshot", report.SnapshotSync.Kind)
}

func TestRunLongerThanTheAccessTokenLifetimeIsReportedAsFailureNotAsPass(t *testing.T) {
	url := startAPIWithTTL(t, 3*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	report, err := run(ctx, options{
		URL:                 url,
		Clients:             3,
		Rate:                20,
		Duration:            5 * time.Second,
		RegisterParallelism: 3,
		Seed:                1,
		P95Target:           10 * time.Second,
		ResyncTarget:        10 * time.Second,
		Password:            defaultPassword,
	}, io.Discard)
	require.NoError(t, err)
	assert.Contains(t, report.Failures[0], "token expired")
	assert.False(t, report.Passed())
}

func TestRunSurvivesTheAccessTokenLifetimeWhenTokensAreRefreshed(t *testing.T) {
	url := startAPIWithTTL(t, 4*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	report, err := run(ctx, options{
		URL:                 url,
		Clients:             3,
		Rate:                20,
		Duration:            7 * time.Second,
		ResyncEvents:        20,
		ResyncTrials:        1,
		RegisterParallelism: 3,
		RefreshInterval:     time.Second,
		Seed:                1,
		P95Target:           10 * time.Second,
		ResyncTarget:        10 * time.Second,
		Password:            defaultPassword,
	}, io.Discard)
	require.NoError(t, err)
	assert.Empty(t, report.Failures)
	assert.True(t, report.Passed())
}

func TestOptionsValidationBoundsTheRateAndRefreshInterval(t *testing.T) {
	valid := options{Clients: 5, Rate: maxRate, Duration: time.Second, RegisterParallelism: 1}
	assert.NoError(t, valid.validate())
	valid.Rate = maxRate + 1
	assert.Error(t, valid.validate())
	valid.Rate = 10
	valid.RefreshInterval = -time.Second
	assert.Error(t, valid.validate())
}

func TestEmptyReportListsSerialiseAsArrays(t *testing.T) {
	data, err := json.Marshal(Report{Resync: []ResyncTrial{}, Failures: []string{}})
	require.NoError(t, err)
	assert.Contains(t, string(data), `"resync":[]`)
	assert.Contains(t, string(data), `"failures":[]`)
}
