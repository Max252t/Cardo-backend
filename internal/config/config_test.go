package config_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/config"
)

const validSecret = "0123456789abcdef0123456789abcdef"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func baseEnv(extra map[string]string) map[string]string {
	m := map[string]string{
		"DATABASE_URL": "postgres://u:p@localhost:5432/db",
		"JWT_SECRET":   validSecret,
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestLoadFromDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := config.LoadFrom(env(baseEnv(nil)))
	require.NoError(t, err)

	assert.Equal(t, ":8080", cfg.HTTPAddr)
	assert.Equal(t, 5*time.Second, cfg.ReadHeaderTimeout)
	assert.Equal(t, 15*time.Second, cfg.ReadTimeout)
	assert.Equal(t, 15*time.Second, cfg.WriteTimeout)
	assert.Equal(t, 60*time.Second, cfg.IdleTimeout)
	assert.Equal(t, 20*time.Second, cfg.ShutdownTimeout)
	assert.Equal(t, int64(1<<20), cfg.MaxBodyBytes)
	assert.Empty(t, cfg.TrustedProxies)
	assert.Equal(t, int32(20), cfg.DBMaxConns)
	assert.True(t, cfg.MigrateOnStart)
	assert.Empty(t, cfg.RedisURL)
	assert.Equal(t, []byte(validSecret), cfg.JWTSecret)
	assert.Equal(t, "cardo", cfg.JWTIssuer)
	assert.Equal(t, "cardo-api", cfg.JWTAudience)
	assert.Equal(t, 15*time.Minute, cfg.AccessTokenTTL)
	assert.Equal(t, 7*24*time.Hour, cfg.RefreshTokenTTL)
	assert.Equal(t, 5, cfg.LoginMaxFailures)
	assert.Equal(t, time.Minute, cfg.LoginFailureWindow)
	assert.Empty(t, cfg.WSAllowedOrigins)
	assert.Equal(t, slog.LevelInfo, cfg.LogLevel)
}

func TestLoadFromOverrides(t *testing.T) {
	t.Parallel()
	cfg, err := config.LoadFrom(env(baseEnv(map[string]string{
		"HTTP_ADDR":                "127.0.0.1:9000",
		"HTTP_READ_HEADER_TIMEOUT": "1s",
		"HTTP_READ_TIMEOUT":        "2s",
		"HTTP_WRITE_TIMEOUT":       "3s",
		"HTTP_IDLE_TIMEOUT":        "4s",
		"SHUTDOWN_TIMEOUT":         "5s",
		"HTTP_MAX_BODY_BYTES":      "2048",
		"TRUSTED_PROXIES":          " 10.0.0.1 , 10.0.0.2,,",
		"DB_MAX_CONNS":             "7",
		"MIGRATE_ON_START":         "false",
		"REDIS_URL":                "redis://localhost:6379/0",
		"JWT_ISSUER":               "iss",
		"JWT_AUDIENCE":             "aud",
		"ACCESS_TOKEN_TTL":         "5m",
		"REFRESH_TOKEN_TTL":        "24h",
		"LOGIN_MAX_FAILURES":       "3",
		"LOGIN_FAILURE_WINDOW":     "30s",
		"WS_ALLOWED_ORIGINS":       "https://a.example, https://b.example",
		"LOG_LEVEL":                "debug",
	})))
	require.NoError(t, err)

	assert.Equal(t, "127.0.0.1:9000", cfg.HTTPAddr)
	assert.Equal(t, time.Second, cfg.ReadHeaderTimeout)
	assert.Equal(t, 2*time.Second, cfg.ReadTimeout)
	assert.Equal(t, 3*time.Second, cfg.WriteTimeout)
	assert.Equal(t, 4*time.Second, cfg.IdleTimeout)
	assert.Equal(t, 5*time.Second, cfg.ShutdownTimeout)
	assert.Equal(t, int64(2048), cfg.MaxBodyBytes)
	assert.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, cfg.TrustedProxies)
	assert.Equal(t, int32(7), cfg.DBMaxConns)
	assert.False(t, cfg.MigrateOnStart)
	assert.Equal(t, "redis://localhost:6379/0", cfg.RedisURL)
	assert.Equal(t, "iss", cfg.JWTIssuer)
	assert.Equal(t, "aud", cfg.JWTAudience)
	assert.Equal(t, 5*time.Minute, cfg.AccessTokenTTL)
	assert.Equal(t, 24*time.Hour, cfg.RefreshTokenTTL)
	assert.Equal(t, 3, cfg.LoginMaxFailures)
	assert.Equal(t, 30*time.Second, cfg.LoginFailureWindow)
	assert.Equal(t, []string{"https://a.example", "https://b.example"}, cfg.WSAllowedOrigins)
	assert.Equal(t, slog.LevelDebug, cfg.LogLevel)
}

func TestLoadFromErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		env     map[string]string
		wantErr []string
	}{
		{
			name:    "missing everything",
			env:     map[string]string{},
			wantErr: []string{"DATABASE_URL is required", "JWT_SECRET must be at least 32 bytes"},
		},
		{
			name:    "short secret",
			env:     baseEnv(map[string]string{"JWT_SECRET": "short"}),
			wantErr: []string{"JWT_SECRET must be at least 32 bytes"},
		},
		{
			name:    "whitespace secret",
			env:     baseEnv(map[string]string{"JWT_SECRET": "   "}),
			wantErr: []string{"JWT_SECRET must be at least 32 bytes"},
		},
		{
			name:    "invalid duration",
			env:     baseEnv(map[string]string{"ACCESS_TOKEN_TTL": "fifteen"}),
			wantErr: []string{`ACCESS_TOKEN_TTL: invalid duration "fifteen"`},
		},
		{
			name:    "invalid integer",
			env:     baseEnv(map[string]string{"LOGIN_MAX_FAILURES": "x"}),
			wantErr: []string{`LOGIN_MAX_FAILURES: invalid integer "x"`},
		},
		{
			name:    "integer overflow",
			env:     baseEnv(map[string]string{"DB_MAX_CONNS": "99999999999"}),
			wantErr: []string{`DB_MAX_CONNS: invalid integer`},
		},
		{
			name:    "invalid bool",
			env:     baseEnv(map[string]string{"MIGRATE_ON_START": "maybe"}),
			wantErr: []string{`MIGRATE_ON_START: invalid boolean "maybe"`},
		},
		{
			name:    "invalid log level",
			env:     baseEnv(map[string]string{"LOG_LEVEL": "loud"}),
			wantErr: []string{`LOG_LEVEL: invalid log level "loud"`},
		},
		{
			name:    "zero access ttl",
			env:     baseEnv(map[string]string{"ACCESS_TOKEN_TTL": "0s"}),
			wantErr: []string{"ACCESS_TOKEN_TTL must be positive"},
		},
		{
			name:    "refresh not greater than access",
			env:     baseEnv(map[string]string{"ACCESS_TOKEN_TTL": "1h", "REFRESH_TOKEN_TTL": "1h"}),
			wantErr: []string{"REFRESH_TOKEN_TTL must be greater than ACCESS_TOKEN_TTL"},
		},
		{
			name:    "non positive login failures",
			env:     baseEnv(map[string]string{"LOGIN_MAX_FAILURES": "0"}),
			wantErr: []string{"LOGIN_MAX_FAILURES must be positive"},
		},
		{
			name:    "non positive login window",
			env:     baseEnv(map[string]string{"LOGIN_FAILURE_WINDOW": "-1s"}),
			wantErr: []string{"LOGIN_FAILURE_WINDOW must be positive"},
		},
		{
			name:    "non positive db conns",
			env:     baseEnv(map[string]string{"DB_MAX_CONNS": "0"}),
			wantErr: []string{"DB_MAX_CONNS must be positive"},
		},
		{
			name:    "non positive body limit",
			env:     baseEnv(map[string]string{"HTTP_MAX_BODY_BYTES": "-5"}),
			wantErr: []string{"HTTP_MAX_BODY_BYTES must be positive"},
		},
		{
			name:    "non positive shutdown timeout",
			env:     baseEnv(map[string]string{"SHUTDOWN_TIMEOUT": "0s"}),
			wantErr: []string{"SHUTDOWN_TIMEOUT must be positive"},
		},
		{
			name: "multiple parse errors are joined",
			env: baseEnv(map[string]string{
				"HTTP_IDLE_TIMEOUT": "x",
				"DB_MAX_CONNS":      "y",
			}),
			wantErr: []string{"HTTP_IDLE_TIMEOUT", "DB_MAX_CONNS"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.LoadFrom(env(tt.env))
			require.Error(t, err)
			assert.Equal(t, config.Config{}, cfg)
			for _, want := range tt.wantErr {
				assert.True(t, strings.Contains(err.Error(), want), "error %q should contain %q", err.Error(), want)
			}
		})
	}
}

func TestLoadUsesProcessEnvironment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("JWT_SECRET", validSecret)
	t.Setenv("HTTP_ADDR", ":9999")
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, ":9999", cfg.HTTPAddr)
}
