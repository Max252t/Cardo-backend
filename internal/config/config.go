package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	minJWTSecretLength = 32
	minDBConns         = 2
)

type Config struct {
	HTTPAddr          string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	ShutdownDrain     time.Duration
	MaxBodyBytes      int64
	TrustedProxies    []string

	DatabaseURL        string
	DBMaxConns         int32
	DBStatementTimeout time.Duration
	DBLockTimeout      time.Duration
	MigrateOnStart     bool

	RedisURL       string
	SingleInstance bool

	JWTSecret       []byte
	JWTIssuer       string
	JWTAudience     string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration

	LoginMaxFailures   int
	LoginFailureWindow time.Duration

	HashMaxConcurrent  int
	HashAcquireTimeout time.Duration

	WSAllowedOrigins []string

	MetricsAddr string

	LogLevel slog.Level
}

func Load() (Config, error) {
	return LoadFrom(os.Getenv)
}

func LoadFrom(getenv func(string) string) (Config, error) {
	p := parser{getenv: getenv}
	cfg := Config{
		HTTPAddr:           p.str("HTTP_ADDR", ":8080"),
		ReadHeaderTimeout:  p.duration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
		ReadTimeout:        p.duration("HTTP_READ_TIMEOUT", 15*time.Second),
		WriteTimeout:       p.duration("HTTP_WRITE_TIMEOUT", 15*time.Second),
		IdleTimeout:        p.duration("HTTP_IDLE_TIMEOUT", 60*time.Second),
		ShutdownTimeout:    p.duration("SHUTDOWN_TIMEOUT", 20*time.Second),
		ShutdownDrain:      p.duration("SHUTDOWN_DRAIN_DELAY", 3*time.Second),
		MaxBodyBytes:       int64(p.integer("HTTP_MAX_BODY_BYTES", 1<<20)),
		TrustedProxies:     p.list("TRUSTED_PROXIES"),
		DatabaseURL:        p.str("DATABASE_URL", ""),
		DBMaxConns:         p.int32("DB_MAX_CONNS", 20),
		DBStatementTimeout: p.duration("DB_STATEMENT_TIMEOUT", 30*time.Second),
		DBLockTimeout:      p.duration("DB_LOCK_TIMEOUT", 5*time.Second),
		MigrateOnStart:     p.boolean("MIGRATE_ON_START", true),
		RedisURL:           p.str("REDIS_URL", ""),
		SingleInstance:     p.boolean("SINGLE_INSTANCE", false),
		JWTSecret:          []byte(p.str("JWT_SECRET", "")),
		JWTIssuer:          p.str("JWT_ISSUER", "cardo"),
		JWTAudience:        p.str("JWT_AUDIENCE", "cardo-api"),
		AccessTokenTTL:     p.duration("ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTokenTTL:    p.duration("REFRESH_TOKEN_TTL", 7*24*time.Hour),
		LoginMaxFailures:   p.integer("LOGIN_MAX_FAILURES", 5),
		LoginFailureWindow: p.duration("LOGIN_FAILURE_WINDOW", time.Minute),
		HashMaxConcurrent:  p.integer("ARGON2_MAX_CONCURRENT", 0),
		HashAcquireTimeout: p.duration("ARGON2_ACQUIRE_TIMEOUT", 2*time.Second),
		WSAllowedOrigins:   p.list("WS_ALLOWED_ORIGINS"),
		MetricsAddr:        p.str("METRICS_ADDR", ""),
		LogLevel:           p.level("LOG_LEVEL", slog.LevelInfo),
	}
	if len(p.errs) > 0 {
		return Config{}, errors.Join(p.errs...)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if len(c.JWTSecret) < minJWTSecretLength {
		errs = append(errs, fmt.Errorf("JWT_SECRET must be at least %d bytes", minJWTSecretLength))
	}
	if c.JWTIssuer == "" {
		errs = append(errs, errors.New("JWT_ISSUER must not be empty"))
	}
	if c.JWTAudience == "" {
		errs = append(errs, errors.New("JWT_AUDIENCE must not be empty"))
	}
	if c.AccessTokenTTL <= 0 {
		errs = append(errs, errors.New("ACCESS_TOKEN_TTL must be positive"))
	}
	if c.RefreshTokenTTL <= c.AccessTokenTTL {
		errs = append(errs, errors.New("REFRESH_TOKEN_TTL must be greater than ACCESS_TOKEN_TTL"))
	}
	if c.LoginMaxFailures <= 0 {
		errs = append(errs, errors.New("LOGIN_MAX_FAILURES must be positive"))
	}
	if c.LoginFailureWindow <= 0 {
		errs = append(errs, errors.New("LOGIN_FAILURE_WINDOW must be positive"))
	}
	if c.DBMaxConns < minDBConns {
		errs = append(errs, fmt.Errorf("DB_MAX_CONNS must be at least %d", minDBConns))
	}
	if c.DBStatementTimeout < 0 {
		errs = append(errs, errors.New("DB_STATEMENT_TIMEOUT must not be negative"))
	}
	if c.DBLockTimeout < 0 {
		errs = append(errs, errors.New("DB_LOCK_TIMEOUT must not be negative"))
	}
	if c.ShutdownDrain < 0 {
		errs = append(errs, errors.New("SHUTDOWN_DRAIN_DELAY must not be negative"))
	}
	if c.HashMaxConcurrent < 0 {
		errs = append(errs, errors.New("ARGON2_MAX_CONCURRENT must not be negative"))
	}
	if c.HashAcquireTimeout <= 0 {
		errs = append(errs, errors.New("ARGON2_ACQUIRE_TIMEOUT must be positive"))
	}
	errs = append(errs, c.validateRedis()...)
	errs = append(errs, c.validateMetricsAddr()...)
	if c.MaxBodyBytes <= 0 {
		errs = append(errs, errors.New("HTTP_MAX_BODY_BYTES must be positive"))
	}
	if c.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("SHUTDOWN_TIMEOUT must be positive"))
	}
	return errors.Join(errs...)
}

type parser struct {
	getenv func(string) string
	errs   []error
}

func (p *parser) raw(key string) (string, bool) {
	v := strings.TrimSpace(p.getenv(key))
	return v, v != ""
}

func (p *parser) str(key, def string) string {
	if v, ok := p.raw(key); ok {
		return v
	}
	return def
}

func (p *parser) duration(key string, def time.Duration) time.Duration {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: invalid duration %q", key, v))
		return def
	}
	return d
}

func (p *parser) integer(key string, def int) int {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: invalid integer %q", key, v))
		return def
	}
	return int(n)
}

func (p *parser) int32(key string, def int32) int32 {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < math.MinInt32 || n > math.MaxInt32 {
		p.errs = append(p.errs, fmt.Errorf("%s: invalid integer %q", key, v))
		return def
	}
	return int32(n)
}

func (p *parser) boolean(key string, def bool) bool {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: invalid boolean %q", key, v))
		return def
	}
	return b
}

func (p *parser) list(key string) []string {
	v, ok := p.raw(key)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func (p *parser) level(key string, def slog.Level) slog.Level {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: invalid log level %q", key, v))
		return def
	}
	return l
}

func (c Config) validateRedis() []error {
	if !c.SingleInstance && c.RedisURL == "" {
		return []error{errors.New("REDIS_URL is required unless SINGLE_INSTANCE=true")}
	}
	return nil
}

func (c Config) validateMetricsAddr() []error {
	if c.MetricsAddr == "" {
		return nil
	}
	_, port, err := net.SplitHostPort(c.MetricsAddr)
	if err != nil {
		return []error{fmt.Errorf("METRICS_ADDR: invalid address %q", c.MetricsAddr)}
	}
	if n, convErr := strconv.Atoi(port); convErr != nil || n < 1 || n > 65535 {
		return []error{fmt.Errorf("METRICS_ADDR: invalid port %q", port)}
	}
	if c.MetricsAddr == c.HTTPAddr {
		return []error{errors.New("METRICS_ADDR must differ from HTTP_ADDR")}
	}
	return nil
}
