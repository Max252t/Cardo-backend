package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

const (
	healthcheckCommand = "healthcheck"
	healthcheckTimeout = 3 * time.Second
	defaultHTTPAddr    = ":8080"
)

func runHealthcheck(getenv func(string) string) error {
	addr := getenv("HTTP_ADDR")
	if addr == "" {
		addr = defaultHTTPAddr
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse HTTP_ADDR: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", port)+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz returned %d", resp.StatusCode)
	}
	return nil
}

func healthcheckRequested(args []string) bool {
	return len(args) > 1 && args[1] == healthcheckCommand
}

func exitOnHealthcheck() {
	if !healthcheckRequested(os.Args) {
		return
	}
	if err := runHealthcheck(os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
