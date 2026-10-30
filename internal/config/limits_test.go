package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/config"
)

func TestLoadFromRetentionBoundaries(t *testing.T) {
	t.Parallel()
	valid := map[string]map[string]string{
		"equal retentions":         {"EVENTS_RETENTION": "24h", "PROCESSED_OPS_RETENTION": "24h"},
		"events just longer":       {"EVENTS_RETENTION": "24h1s", "PROCESSED_OPS_RETENTION": "24h"},
		"batch one":                {"RETENTION_BATCH_SIZE": "1"},
		"keep min zero":            {"EVENTS_KEEP_MIN": "0"},
		"tiny interval":            {"RETENTION_INTERVAL": "1ms"},
		"disabled with defaults":   {"RETENTION_ENABLED": "false"},
		"very long retention":      {"EVENTS_RETENTION": "8760h", "PROCESSED_OPS_RETENTION": "8760h"},
		"minutes and seconds mix":  {"RETENTION_INTERVAL": "1h30m15s"},
		"disabled with bad values": {"RETENTION_ENABLED": "false", "RETENTION_BATCH_SIZE": "0", "DB_MAX_CONNS": "2"},
		"batch at the cap":         {"RETENTION_BATCH_SIZE": "50000"},
		"three connections":        {"DB_MAX_CONNS": "3"},
	}
	for name, extra := range valid {
		t.Run("valid "+name, func(t *testing.T) {
			t.Parallel()
			_, err := config.LoadFrom(env(baseEnv(extra)))
			assert.NoError(t, err)
		})
	}
	invalid := map[string]map[string]string{
		"events just shorter":           {"EVENTS_RETENTION": "23h59m59s"},
		"negative events":               {"EVENTS_RETENTION": "-1h"},
		"negative ops retention":        {"PROCESSED_OPS_RETENTION": "-1h"},
		"negative interval":             {"RETENTION_INTERVAL": "-1m"},
		"negative batch":                {"RETENTION_BATCH_SIZE": "-5"},
		"fractional batch":              {"RETENTION_BATCH_SIZE": "1.5"},
		"non numeric keep min":          {"EVENTS_KEEP_MIN": "many"},
		"invalid enabled flag":          {"RETENTION_ENABLED": "maybe"},
		"disabled but windows inverted": {"RETENTION_ENABLED": "false", "EVENTS_RETENTION": "1h"},
		"batch over the cap":            {"RETENTION_BATCH_SIZE": "50001"},
		"too few connections":           {"DB_MAX_CONNS": "2"},
	}
	for name, extra := range invalid {
		t.Run("invalid "+name, func(t *testing.T) {
			t.Parallel()
			_, err := config.LoadFrom(env(baseEnv(extra)))
			assert.Error(t, err)
		})
	}
}

func TestLoadFromRetentionReportsAllProblemsTogether(t *testing.T) {
	t.Parallel()
	_, err := config.LoadFrom(env(baseEnv(map[string]string{
		"RETENTION_INTERVAL":   "0s",
		"EVENTS_KEEP_MIN":      "-3",
		"RETENTION_BATCH_SIZE": "0",
	})))
	require.Error(t, err)
	for _, key := range []string{"RETENTION_INTERVAL", "EVENTS_KEEP_MIN", "RETENTION_BATCH_SIZE"} {
		assert.Contains(t, err.Error(), key)
	}
}

func TestLoadFromRESTRateLimitBoundaries(t *testing.T) {
	t.Parallel()
	valid := map[string]map[string]string{
		"max rate":           {"REST_RATE_LIMIT": "1000000"},
		"fractional rate":    {"REST_RATE_LIMIT": "0.001"},
		"burst one":          {"REST_RATE_LIMIT": "5", "REST_RATE_BURST": "1"},
		"disabled any burst": {"REST_RATE_LIMIT": "0", "REST_RATE_BURST": "-4"},
		"exponent":           {"REST_RATE_LIMIT": "2e1"},
	}
	for name, extra := range valid {
		t.Run("valid "+name, func(t *testing.T) {
			t.Parallel()
			_, err := config.LoadFrom(env(baseEnv(extra)))
			assert.NoError(t, err)
		})
	}
	invalid := map[string]map[string]string{
		"above max":      {"REST_RATE_LIMIT": "1000001"},
		"inf":            {"REST_RATE_LIMIT": "Inf"},
		"negative inf":   {"REST_RATE_LIMIT": "-Inf"},
		"negative tiny":  {"REST_RATE_LIMIT": "-0.001"},
		"negative burst": {"REST_RATE_LIMIT": "5", "REST_RATE_BURST": "-1"},
		"empty rate":     {"REST_RATE_LIMIT": "  "},
	}
	for name, extra := range invalid {
		t.Run("invalid "+name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.LoadFrom(env(baseEnv(extra)))
			if name == "empty rate" {
				require.NoError(t, err)
				assert.InDelta(t, 20.0, cfg.RESTRateLimit, 0)
				return
			}
			assert.Error(t, err)
		})
	}
}

func TestLoadFromRetentionDurationsAreExact(t *testing.T) {
	t.Parallel()
	cfg, err := config.LoadFrom(env(baseEnv(map[string]string{"RETENTION_INTERVAL": "90s", "EVENTS_RETENTION": "168h", "PROCESSED_OPS_RETENTION": "36h"})))
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, cfg.RetentionInterval)
	assert.Equal(t, 168*time.Hour, cfg.EventsRetention)
	assert.Equal(t, 36*time.Hour, cfg.ProcessedOpsRetention)
}
