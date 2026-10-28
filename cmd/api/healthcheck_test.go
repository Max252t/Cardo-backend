package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func listenerPort(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	return port
}

func TestRunHealthcheckReflectsReadiness(t *testing.T) {
	t.Parallel()
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/readyz", r.URL.Path)
	}))
	defer ready.Close()
	draining := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer draining.Close()

	env := func(port string) func(string) string {
		return func(string) string { return "127.0.0.1:" + port }
	}
	require.NoError(t, runHealthcheck(env(listenerPort(t, ready))))
	assert.Error(t, runHealthcheck(env(listenerPort(t, draining))))
	assert.Error(t, runHealthcheck(func(string) string { return "bogus" }))
}

func TestHealthcheckRequested(t *testing.T) {
	t.Parallel()
	assert.True(t, healthcheckRequested([]string{"api", "healthcheck"}))
	assert.False(t, healthcheckRequested([]string{"api"}))
	assert.False(t, healthcheckRequested([]string{"api", "serve"}))
}
