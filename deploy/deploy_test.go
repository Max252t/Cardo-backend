package deploy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/config"
	"github.com/Max252t/Cardo-backend/internal/metrics"
	"github.com/Max252t/Cardo-backend/internal/realtime"
)

func read(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(rel)))
	require.NoError(t, err)
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

func envFile(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(read(t, ".env.example"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		require.True(t, ok, line)
		out[key] = value
	}
	return out
}

func block(t *testing.T, conf, header string) string {
	t.Helper()
	start := strings.Index(conf, header+" {")
	require.GreaterOrEqual(t, start, 0, "block %q not found", header)
	depth := 0
	for i := start; i < len(conf); i++ {
		switch conf[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return conf[start : i+1]
			}
		}
	}
	t.Fatalf("block %q is not closed", header)
	return ""
}

func directive(t *testing.T, body, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `\s+([^;]+);`).FindStringSubmatch(body)
	require.NotNil(t, m, "directive %q not found", name)
	return strings.TrimSpace(m[1])
}

func nginxDuration(t *testing.T, v string) time.Duration {
	t.Helper()
	m := regexp.MustCompile(`^(\d+)(ms|s|m|h)?$`).FindStringSubmatch(v)
	require.NotNil(t, m, v)
	n, err := strconv.Atoi(m[1])
	require.NoError(t, err)
	unit := map[string]time.Duration{"": time.Second, "ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour}[m[2]]
	return time.Duration(n) * unit
}

func nginxSize(t *testing.T, v string) int64 {
	t.Helper()
	m := regexp.MustCompile(`^(\d+)([kKmMgG]?)$`).FindStringSubmatch(v)
	require.NotNil(t, m, v)
	n, err := strconv.ParseInt(m[1], 10, 64)
	require.NoError(t, err)
	unit := map[string]int64{"": 1, "k": 1 << 10, "m": 1 << 20, "g": 1 << 30}[strings.ToLower(m[2])]
	return n * unit
}

func TestNginxSilencesEveryInvitePath(t *testing.T) {
	t.Parallel()
	conf := read(t, "deploy/nginx.conf")
	loc := block(t, conf, "location ^~ /api/invites/")

	assert.Equal(t, "off", directive(t, loc, "access_log"))
	assert.Equal(t, "/dev/null crit", directive(t, loc, "error_log"))
	assert.Equal(t, "http://api", directive(t, loc, "proxy_pass"))
	assert.NotContains(t, loc, "proxy_set_header", "proxy settings must be inherited from the server block")
}

func TestNginxInvitePrefixCoversAcceptPathsAndTheirVariants(t *testing.T) {
	t.Parallel()
	conf := read(t, "deploy/nginx.conf")
	m := regexp.MustCompile(`location \^~ (\S+) \{`).FindStringSubmatch(conf)
	require.NotNil(t, m)

	for _, path := range []string{
		"/api/invites/abc123/accept",
		"/api/invites/abc123/accept/",
		"/api/invites/Zm9v-_ba/accept",
		"/api/invites/abc123",
		"/api/invites/abc123/anything/else",
	} {
		assert.True(t, strings.HasPrefix(path, m[1]), path)
	}
	other := "/api/boards/abc/invites/def"
	assert.False(t, strings.HasPrefix(other, m[1]))
	assert.Empty(t, regexp.MustCompile(`location ~`).FindAllString(conf, -1), "regex locations would be skipped by ^~ but could leak tokens elsewhere")
}

func TestNginxInviteLocationAlsoCoversTrailingSlashRedirect(t *testing.T) {
	t.Parallel()
	conf := read(t, "deploy/nginx.conf")
	m := regexp.MustCompile(`location \^~ (\S+) \{`).FindStringSubmatch(conf)
	require.NotNil(t, m)

	slash := "/api/invites/abc123/accept/"
	assert.True(t, strings.HasPrefix(slash, m[1]))
}

func TestNginxAccessLogFormatNeverContainsTheRawRequestLine(t *testing.T) {
	t.Parallel()
	conf := read(t, "deploy/nginx.conf")
	format := regexp.MustCompile(`log_format\s+main\s+([^;]+);`).FindStringSubmatch(conf)
	require.NotNil(t, format)

	for _, forbidden := range []string{"$request ", "$request\"", "$request_uri", "$args", "$query_string", "$http_authorization", "$http_cookie", "$request_body"} {
		assert.NotContains(t, format[1]+" ", forbidden)
	}
	assert.Contains(t, format[1], "$uri")
}

func TestNginxDoesNotExposeMetrics(t *testing.T) {
	t.Parallel()
	conf := read(t, "deploy/nginx.conf")
	loc := block(t, conf, "location = /metrics")

	assert.Equal(t, "404", directive(t, loc, "return"))
	assert.NotContains(t, loc, "proxy_pass")
}

func TestNginxForwardsWebSocketUpgradeHeaders(t *testing.T) {
	t.Parallel()
	conf := read(t, "deploy/nginx.conf")

	assert.Contains(t, conf, "proxy_http_version 1.1;")
	assert.Contains(t, conf, "proxy_set_header Upgrade $http_upgrade;")
	assert.Contains(t, conf, "proxy_set_header Connection $connection_upgrade;")
	mapping := block(t, conf, "map $http_upgrade $connection_upgrade")
	assert.Contains(t, mapping, "default upgrade;")
	assert.Regexp(t, `''\s+'';`, mapping)
}

func TestNginxWebSocketTimeoutsExceedHeartbeatWindow(t *testing.T) {
	t.Parallel()
	conf := read(t, "deploy/nginx.conf")
	loc := block(t, conf, "location /ws/")

	read := nginxDuration(t, directive(t, loc, "proxy_read_timeout"))
	send := nginxDuration(t, directive(t, loc, "proxy_send_timeout"))

	assert.Greater(t, read, 60*time.Second)
	assert.Greater(t, read, realtime.DefaultPongWait)
	assert.Greater(t, read, realtime.DefaultPingInterval)
	assert.Greater(t, send, 60*time.Second)
	assert.Equal(t, "off", directive(t, loc, "proxy_buffering"))
	assert.Equal(t, "http://api", directive(t, loc, "proxy_pass"))
}

func TestNginxForwardsClientAddress(t *testing.T) {
	t.Parallel()
	conf := read(t, "deploy/nginx.conf")

	assert.Contains(t, conf, "proxy_set_header X-Real-IP $remote_addr;")
	assert.Contains(t, conf, "proxy_set_header X-Forwarded-For $remote_addr;")
	assert.Contains(t, conf, "proxy_set_header X-Forwarded-Proto $scheme;")
	assert.Contains(t, conf, "proxy_set_header Host $http_host;")
}

func TestNginxBodyLimitMatchesAPIConfiguration(t *testing.T) {
	t.Parallel()
	conf := read(t, "deploy/nginx.conf")
	env := envFile(t)

	limit := nginxSize(t, directive(t, conf, "client_max_body_size"))
	apiLimit, err := strconv.ParseInt(env["HTTP_MAX_BODY_BYTES"], 10, 64)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, limit, apiLimit, "nginx must not reject bodies the API would accept")
	assert.Equal(t, apiLimit, limit)
}

func TestNginxBalancesRoundRobinAcrossBothReplicas(t *testing.T) {
	t.Parallel()
	conf := read(t, "deploy/nginx.conf")
	up := block(t, conf, "upstream api")

	compose := read(t, "docker-compose.yml")
	for _, svc := range []string{"api-1", "api-2"} {
		ip := regexp.MustCompile(`(?s)
  ` + svc + `:
.*?ipv4_address:\s*(\S+)`).FindStringSubmatch(compose)
		require.NotNil(t, ip, svc)
		assert.Contains(t, up, "server "+ip[1]+":8080", svc)
	}
	assert.Equal(t, 2, strings.Count(up, "server "))
	assert.NotRegexp(t, `server [a-z]`, up, "upstream hosts must be IPs so nginx starts while a replica is down")
	for _, sticky := range []string{"ip_hash", "hash ", "least_conn", "sticky", "random", "least_time"} {
		assert.NotContains(t, up, sticky)
	}
}

func TestNginxRetriesFailedUpstreamsOnTheOtherReplica(t *testing.T) {
	t.Parallel()
	conf := read(t, "deploy/nginx.conf")

	assert.Contains(t, directive(t, conf, "proxy_next_upstream"), "error")
	assert.Contains(t, directive(t, conf, "proxy_next_upstream"), "timeout")
	tries, err := strconv.Atoi(directive(t, conf, "proxy_next_upstream_tries"))
	require.NoError(t, err)
	assert.Equal(t, 2, tries)
	assert.LessOrEqual(t, nginxDuration(t, directive(t, conf, "proxy_connect_timeout")), time.Second)
	assert.Contains(t, directive(t, conf, "proxy_next_upstream"), "http_503")
}

func TestNginxHidesVersionAndRoutesEverythingElseToTheAPI(t *testing.T) {
	t.Parallel()
	conf := read(t, "deploy/nginx.conf")

	assert.Equal(t, "off", directive(t, conf, "server_tokens"))
	assert.Equal(t, "http://api", directive(t, block(t, conf, "location /"), "proxy_pass"))
}

func TestComposeRunsReplicasInMultiInstanceMode(t *testing.T) {
	t.Parallel()
	compose := read(t, "docker-compose.yml")
	api := regexp.MustCompile(`(?s)x-api: &api\n(.*?)\nservices:`).FindStringSubmatch(compose)
	require.NotNil(t, api)

	assert.Contains(t, api[1], `SINGLE_INSTANCE: "false"`)
	assert.Contains(t, api[1], "REDIS_URL: redis://redis:6379/0")
	assert.Regexp(t, `env_file:
\s+- path: \.env
\s+required: false`, api[1])
	assert.Contains(t, api[1], "JWT_SECRET: ${JWT_SECRET:?")
	assert.Contains(t, api[1], `METRICS_ADDR: ":9091"`)
	assert.NotContains(t, compose, `SINGLE_INSTANCE: "true"`)
}

func TestComposeDefinesTheExpectedServices(t *testing.T) {
	t.Parallel()
	compose := read(t, "docker-compose.yml")

	for _, svc := range []string{"api-1", "api-2", "nginx", "postgres", "redis", "prometheus", "grafana"} {
		assert.Regexp(t, `(?m)^  `+svc+`:\n`, compose, svc)
	}
	assert.Contains(t, compose, "image: postgres:16-alpine")
	assert.Contains(t, compose, "image: redis:7-alpine")
	assert.Contains(t, compose, "postgres-data:/var/lib/postgresql/data")
}

func TestComposeAPIReplicasWaitForHealthyDependenciesAndSelfCheck(t *testing.T) {
	t.Parallel()
	compose := read(t, "docker-compose.yml")
	api := regexp.MustCompile(`(?s)x-api: &api\n(.*?)\nservices:`).FindStringSubmatch(compose)
	require.NotNil(t, api)

	assert.Regexp(t, `postgres:\n\s+condition: service_healthy`, api[1])
	assert.Regexp(t, `redis:\n\s+condition: service_healthy`, api[1])
	assert.Contains(t, api[1], `test: ["CMD", "/api", "healthcheck"]`)
	assert.Contains(t, read(t, "Dockerfile"), `ENTRYPOINT ["/api"]`)
	assert.Contains(t, read(t, "Dockerfile"), "-o /out/api ./cmd/api")
}

func TestComposeNginxWaitsForBothReplicasAndOnlyPublishesTheProxyPort(t *testing.T) {
	t.Parallel()
	compose := read(t, "docker-compose.yml")
	svc := regexp.MustCompile(`(?s)\n  nginx:\n(.*?)\n  postgres:`).FindStringSubmatch(compose)
	require.NotNil(t, svc)

	assert.Regexp(t, `api-1:\n\s+condition: service_healthy`, svc[1])
	assert.Regexp(t, `api-2:\n\s+condition: service_healthy`, svc[1])
	assert.Contains(t, svc[1], "./deploy/nginx.conf:/etc/nginx/nginx.conf:ro")
	assert.Equal(t, 1, strings.Count(svc[1], `- "`), "only one port may be published")
	assert.Contains(t, svc[1], `"8080:80"`)
}

func TestComposeKeepsMonitoringOptionalAndLocal(t *testing.T) {
	t.Parallel()
	compose := read(t, "docker-compose.yml")

	for _, svc := range []string{"prometheus", "grafana"} {
		body := regexp.MustCompile(`(?s)\n  ` + svc + `:\n(.*?)(\n  [a-z-]+:\n|\nnetworks:)`).FindStringSubmatch(compose)
		require.NotNil(t, body, svc)
		assert.Contains(t, body[1], `profiles: ["monitoring"]`, svc)
		assert.Regexp(t, `"127\.0\.0\.1:\d+:\d+"`, body[1], svc)
	}
}

func TestComposeTrustsOnlyTheNginxContainer(t *testing.T) {
	t.Parallel()
	compose := read(t, "docker-compose.yml")
	subnet := regexp.MustCompile(`subnet:\s*(\S+)`).FindStringSubmatch(compose)
	require.NotNil(t, subnet)
	network, err := netip.ParsePrefix(subnet[1])
	require.NoError(t, err)
	nginx := regexp.MustCompile(`(?s)
  nginx:
.*?ipv4_address:\s*(\S+)`).FindStringSubmatch(compose)
	require.NotNil(t, nginx)
	nginxIP, err := netip.ParseAddr(nginx[1])
	require.NoError(t, err)

	composeProxies := regexp.MustCompile(`TRUSTED_PROXIES:\s*(\S+)`).FindStringSubmatch(compose)
	require.NotNil(t, composeProxies)
	for _, raw := range []string{composeProxies[1], envFile(t)["TRUSTED_PROXIES"]} {
		trusted, err := netip.ParsePrefix(raw)
		require.NoError(t, err)
		assert.True(t, trusted.Contains(nginxIP), raw)
		assert.Equal(t, 32, trusted.Bits(), raw)
	}
	for _, ip := range regexp.MustCompile(`ipv4_address:\s*(\S+)`).FindAllStringSubmatch(compose, -1) {
		addr, err := netip.ParseAddr(ip[1])
		require.NoError(t, err)
		assert.True(t, network.Contains(addr), ip[1])
	}
	rng := regexp.MustCompile(`ip_range:\s*(\S+)`).FindStringSubmatch(compose)
	require.NotNil(t, rng)
	pool, err := netip.ParsePrefix(rng[1])
	require.NoError(t, err)
	assert.False(t, pool.Contains(nginxIP), "dynamic pool must not overlap the static addresses")
}

func TestEnvExampleLoadsAsAValidConfiguration(t *testing.T) {
	t.Parallel()
	env := envFile(t)

	cfg, err := config.LoadFrom(func(key string) string { return env[key] })

	require.NoError(t, err)
	assert.Equal(t, ":9091", cfg.MetricsAddr)
	assert.False(t, cfg.SingleInstance)
	assert.Equal(t, int64(1048576), cfg.MaxBodyBytes)
	assert.NotEmpty(t, cfg.RedisURL)
	assert.NotEmpty(t, cfg.TrustedProxies)
	assert.Contains(t, cfg.DatabaseURL, "@postgres:5432/")
	assert.Contains(t, cfg.RedisURL, "//redis:6379")
}

func TestPrometheusScrapesEachReplicaDirectly(t *testing.T) {
	t.Parallel()
	prom := read(t, "deploy/prometheus.yml")

	assert.Contains(t, prom, "metrics_path: /metrics")
	assert.Contains(t, prom, `targets: ["api-1:9091", "api-2:9091"]`)
	assert.NotContains(t, prom, "nginx")
}

func collectorMetricNames(t *testing.T) map[string]struct{} {
	t.Helper()
	c := metrics.New()
	c.ObserveOp("op.card.create", "applied", time.Millisecond)
	c.ConnectionOpened()
	c.ConnectionClosed(1001)
	c.Resync("delta")
	c.ObserveRequest(http.MethodGet, "/healthz", http.StatusOK, time.Millisecond)
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	names := map[string]struct{}{}
	for _, m := range regexp.MustCompile(`(?m)^# TYPE (\S+) (\w+)$`).FindAllStringSubmatch(rec.Body.String(), -1) {
		names[m[1]] = struct{}{}
		if m[2] == "histogram" {
			names[m[1]+"_bucket"] = struct{}{}
			names[m[1]+"_sum"] = struct{}{}
			names[m[1]+"_count"] = struct{}{}
		}
	}
	return names
}

func dashboardExprs(t *testing.T) []string {
	t.Helper()
	var doc any
	require.NoError(t, json.Unmarshal([]byte(read(t, "deploy/grafana/dashboards/cardo.json")), &doc))
	var exprs []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if expr, ok := x["expr"].(string); ok {
				exprs = append(exprs, expr)
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(doc)
	return exprs
}

func TestGrafanaDashboardOnlyQueriesMetricsTheCollectorExposes(t *testing.T) {
	t.Parallel()
	known := collectorMetricNames(t)
	exprs := dashboardExprs(t)
	require.NotEmpty(t, exprs)
	ident := regexp.MustCompile(`\b(?:ws_|ops_|op_apply|event_publish|resync_|http_request)\w*`)

	for _, expr := range exprs {
		found := ident.FindAllString(expr, -1)
		require.NotEmpty(t, found, expr)
		for _, name := range found {
			assert.Contains(t, known, name, expr)
		}
	}
}

func TestGrafanaDashboardCoversEveryRequiredMetric(t *testing.T) {
	t.Parallel()
	joined := strings.Join(dashboardExprs(t), "\n")

	for _, name := range []string{"ws_connections", "ops_total", "op_apply_duration_seconds", "event_publish_errors_total", "ws_closed_total"} {
		assert.Contains(t, joined, name)
	}
}

func TestGrafanaProvisioningPointsAtThePrometheusService(t *testing.T) {
	t.Parallel()

	assert.Contains(t, read(t, "deploy/grafana/provisioning/datasources/prometheus.yml"), "url: http://prometheus:9090")
	assert.Contains(t, read(t, "deploy/grafana/provisioning/dashboards/cardo.yml"), "path: /var/lib/grafana/dashboards")
	assert.Contains(t, read(t, "docker-compose.yml"), "./deploy/grafana/dashboards:/var/lib/grafana/dashboards:ro")
}
