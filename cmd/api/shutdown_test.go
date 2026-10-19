package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func startTestServer(t *testing.T, handler http.Handler, lc *lifecycle) (*http.Server, string) {
	t.Helper()
	var lcfg net.ListenConfig
	ln, err := lcfg.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: time.Second,
		BaseContext:       func(net.Listener) context.Context { return lc.baseCtx },
	}
	go func() { _ = srv.Serve(ln) }()
	return srv, "http://" + ln.Addr().String()
}

func TestShutdownCancelsInFlightRequestsAfterTimeout(t *testing.T) {
	lc := newLifecycle()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	srv, url := startTestServer(t, handler, lc)

	go func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
		if err != nil {
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	<-started

	start := time.Now()
	err := shutdown(srv, lc, 0, 100*time.Millisecond, slog.New(slog.DiscardHandler))
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "unexpected error %v", err)
	assert.True(t, lc.draining.Load())
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request context was not cancelled")
	}
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestShutdownWaitsForInFlightRequests(t *testing.T) {
	lc := newLifecycle()
	started := make(chan struct{})
	finished := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(50 * time.Millisecond)
		assert.NoError(t, r.Context().Err(), "graceful shutdown must not cancel requests that finish in time")
		w.WriteHeader(http.StatusNoContent)
		close(finished)
	})
	srv, url := startTestServer(t, handler, lc)

	done := make(chan int, 1)
	go func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
		if err != nil {
			done <- 0
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		_ = resp.Body.Close()
		done <- resp.StatusCode
	}()
	<-started

	require.NoError(t, shutdown(srv, lc, 10*time.Millisecond, 5*time.Second, slog.New(slog.DiscardHandler)))
	<-finished
	assert.Equal(t, http.StatusNoContent, <-done)
	assert.Error(t, lc.baseCtx.Err(), "base context is cancelled after shutdown")
}
