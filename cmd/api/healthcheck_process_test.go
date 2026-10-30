package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const helperEnv = "CARDO_HEALTHCHECK_HELPER"

const continuedExitCode = 3

func TestHealthcheckHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		t.Skip("subprocess helper")
	}
	os.Args = []string{"api"}
	if mode == "healthcheck" {
		os.Args = []string{"api", "healthcheck"}
	}
	exitOnHealthcheck()
	os.Exit(continuedExitCode)
}

type helperResult struct {
	code   int
	stderr string
}

func runHelper(t *testing.T, mode, httpAddr string) helperResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHealthcheckHelperProcess$")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, "HTTP_ADDR="+httpAddr)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return helperResult{code: 0, stderr: stderr.String()}
	}
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	return helperResult{code: exitErr.ExitCode(), stderr: stderr.String()}
}

func serverWithStatus(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	return port
}

func refusedPort(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	require.NoError(t, ln.Close())
	return port
}

func envWith(addr string) func(string) string {
	return func(key string) string {
		if key == "HTTP_ADDR" {
			return addr
		}
		return ""
	}
}

func TestHealthcheckProcessExitsZeroWhenReady(t *testing.T) {
	t.Parallel()
	res := runHelper(t, "healthcheck", "127.0.0.1:"+serverWithStatus(t, http.StatusOK))

	assert.Equal(t, 0, res.code, res.stderr)
}

func TestHealthcheckProcessExitsOneWhenNotReady(t *testing.T) {
	t.Parallel()
	res := runHelper(t, "healthcheck", ":"+serverWithStatus(t, http.StatusServiceUnavailable))

	assert.Equal(t, 1, res.code)
	assert.Contains(t, res.stderr, "readyz returned 503")
}

func TestHealthcheckProcessExitsOneWhenNothingListens(t *testing.T) {
	t.Parallel()
	res := runHelper(t, "healthcheck", ":"+refusedPort(t))

	assert.Equal(t, 1, res.code)
	assert.NotEmpty(t, res.stderr)
}

func TestHealthcheckProcessExitsOneForMalformedAddress(t *testing.T) {
	t.Parallel()
	res := runHelper(t, "healthcheck", "not-an-address")

	assert.Equal(t, 1, res.code)
	assert.Contains(t, res.stderr, "parse HTTP_ADDR")
}

func TestHealthcheckProcessDoesNotRequireOtherConfiguration(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHealthcheckHelperProcess$")
	cmd.Env = []string{helperEnv + "=healthcheck", "HTTP_ADDR=:" + serverWithStatus(t, http.StatusOK)}
	if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
		cmd.Env = append(cmd.Env, "SystemRoot="+systemRoot)
	}

	require.NoError(t, cmd.Run())
}

func TestStartupContinuesWithoutHealthcheckArgument(t *testing.T) {
	t.Parallel()
	res := runHelper(t, "serve", ":"+serverWithStatus(t, http.StatusOK))

	assert.Equal(t, continuedExitCode, res.code)
}

func TestRunHealthcheckAcceptsEveryListenAddressShape(t *testing.T) {
	t.Parallel()
	port := serverWithStatus(t, http.StatusOK)
	for _, addr := range []string{":" + port, "0.0.0.0:" + port, "127.0.0.1:" + port, "[::]:" + port, "unresolvable.invalid:" + port} {
		assert.NoError(t, runHealthcheck(envWith(addr)), addr)
	}
}

func TestRunHealthcheckUsesOnlyThePortOfHTTPAddr(t *testing.T) {
	t.Parallel()
	hit := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hit <- r.Method + " " + r.URL.Path
	}))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)

	require.NoError(t, runHealthcheck(envWith("10.255.255.1:"+port)))

	assert.Equal(t, "GET /readyz", <-hit)
}

func TestRunHealthcheckFailsOnEveryNon200Status(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			err := runHealthcheck(envWith(":" + serverWithStatus(t, status)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), fmt.Sprint(status))
		})
	}
}

func TestRunHealthcheckFailsWhenNothingListens(t *testing.T) {
	t.Parallel()
	err := runHealthcheck(envWith(":" + refusedPort(t)))

	require.Error(t, err)
	var opErr *net.OpError
	assert.True(t, errors.As(err, &opErr), "expected a network error, got %v", err)
}

func TestRunHealthcheckRejectsMalformedAddresses(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"bogus", "8080", "localhost", "::1", "[::1", "a:b:c"} {
		t.Run(addr, func(t *testing.T) {
			t.Parallel()
			err := runHealthcheck(envWith(addr))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "HTTP_ADDR")
		})
	}
}

func TestRunHealthcheckFallsBackToTheDefaultAddress(t *testing.T) {
	t.Parallel()
	assert.Equal(t, ":8080", defaultHTTPAddr)
	var asked []string
	_ = runHealthcheck(func(key string) string {
		asked = append(asked, key)
		return ""
	})
	assert.Equal(t, []string{"HTTP_ADDR"}, asked)
}

func TestRunHealthcheckGivesUpOnAHangingServer(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)

	started := time.Now()
	err = runHealthcheck(envWith(":" + port))

	require.Error(t, err)
	assert.Less(t, time.Since(started), healthcheckTimeout+2*time.Second)
	assert.GreaterOrEqual(t, time.Since(started), healthcheckTimeout-100*time.Millisecond)
}

func TestHealthcheckRequestedEdgeCases(t *testing.T) {
	t.Parallel()
	assert.False(t, healthcheckRequested(nil))
	assert.False(t, healthcheckRequested([]string{}))
	assert.False(t, healthcheckRequested([]string{"healthcheck"}))
	assert.False(t, healthcheckRequested([]string{"api", "Healthcheck"}))
	assert.False(t, healthcheckRequested([]string{"api", "--healthcheck"}))
	assert.False(t, healthcheckRequested([]string{"api", ""}))
	assert.True(t, healthcheckRequested([]string{"api", "healthcheck", "extra"}))
	assert.True(t, healthcheckRequested([]string{"/usr/local/bin/api", "healthcheck"}))
}
