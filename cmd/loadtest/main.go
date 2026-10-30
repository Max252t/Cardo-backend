package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	exitOK     = 0
	exitError  = 1
	exitMissed = 2
	exitUsage  = 64
	defaultURL = "http://localhost:8080"
	runSlack   = 10 * time.Minute
)

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr))
}

func execute(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts := options{}
	var asJSON, enforce bool
	fs.StringVar(&opts.URL, "url", defaultURL, "base URL of the API or the nginx entry point")
	fs.IntVar(&opts.Clients, "clients", 50, "number of WebSocket clients on one board")
	fs.Float64Var(&opts.Rate, "rate", 20, "total operations per second across all clients")
	fs.DurationVar(&opts.Duration, "duration", 60*time.Second, "length of the measured phase")
	fs.DurationVar(&opts.Warmup, "warmup", 5*time.Second, "warm-up phase before measuring")
	fs.IntVar(&opts.ResyncEvents, "resync-events", 1000, "events the board advances while a client is disconnected")
	fs.IntVar(&opts.ResyncTrials, "resync-trials", 3, "number of resync measurements (0 disables the resync phase)")
	fs.IntVar(&opts.RegisterParallelism, "register-parallelism", 4, "concurrent registrations during setup")
	fs.Int64Var(&opts.Seed, "seed", time.Now().UnixNano(), "seed for the operation mix")
	fs.DurationVar(&opts.P95Target, "p95-target", 200*time.Millisecond, "latency target for the 95th percentile")
	fs.DurationVar(&opts.ResyncTarget, "resync-target", 2*time.Second, "resync time target")
	fs.StringVar(&opts.Password, "password", defaultPassword, "password for the generated users")
	fs.BoolVar(&asJSON, "json", false, "print the report as JSON instead of text")
	fs.BoolVar(&enforce, "enforce", false, "exit with status 2 when a target is missed")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, opts.Duration+opts.Warmup+runSlack)
	defer cancel()

	report, err := run(ctx, opts, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "load test failed: %v\n", err)
		return exitError
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			_, _ = fmt.Fprintf(stderr, "encode report: %v\n", err)
			return exitError
		}
	} else {
		report.WriteText(stdout)
	}
	if enforce && !report.Passed() {
		return exitMissed
	}
	return exitOK
}
