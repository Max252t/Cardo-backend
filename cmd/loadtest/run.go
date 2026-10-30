package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
)

const (
	initialColumns   = 4
	memberAddPace    = 60 * time.Millisecond
	connectParallel  = 10
	fillOpsPerSecond = 15
	fillWorkers      = 20
	drainTimeout     = 6 * time.Second
	settleTimeout    = 10 * time.Second
	catchUpTimeout   = 10 * time.Second
	defaultPassword  = "loadtest-password-1"
)

type options struct {
	URL                 string
	Clients             int
	Rate                float64
	Duration            time.Duration
	Warmup              time.Duration
	ResyncEvents        int
	ResyncTrials        int
	RegisterParallelism int
	Seed                int64
	P95Target           time.Duration
	ResyncTarget        time.Duration
	Password            string
}

func (o options) validate() error {
	switch {
	case o.Clients < 2:
		return errors.New("clients must be at least 2")
	case o.Rate <= 0:
		return errors.New("rate must be positive")
	case o.Duration <= 0:
		return errors.New("duration must be positive")
	case o.ResyncTrials < 0 || o.ResyncEvents < 0:
		return errors.New("resync settings must not be negative")
	case o.RegisterParallelism < 1:
		return errors.New("register-parallelism must be at least 1")
	}
	return nil
}

type runner struct {
	opts    options
	logf    func(format string, args ...any)
	api     *apiClient
	col     *collector
	world   *world
	gen     *workload
	boardID uuid.UUID
	clients []*wsClient
	report  Report
}

func run(ctx context.Context, opts options, progress io.Writer) (Report, error) {
	if err := opts.validate(); err != nil {
		return Report{}, err
	}
	r := &runner{
		opts:  opts,
		api:   newAPIClient(opts.URL),
		col:   newCollector(),
		world: newWorld(),
	}
	r.gen = newWorkload(r.world, opts.Seed)
	r.logf = func(format string, args ...any) {
		_, _ = fmt.Fprintf(progress, format+"\n", args...)
	}
	r.report = Report{
		StartedAt: time.Now().UTC(),
		Config: ConfigInfo{
			URL:          opts.URL,
			Clients:      opts.Clients,
			RatePerSec:   opts.Rate,
			DurationSec:  opts.Duration.Seconds(),
			WarmupSec:    opts.Warmup.Seconds(),
			ResyncEvents: opts.ResyncEvents,
			ResyncTrials: opts.ResyncTrials,
			Seed:         opts.Seed,
		},
	}
	defer func() {
		for _, c := range r.clients {
			c.stop()
		}
	}()
	if err := r.setup(ctx); err != nil {
		return r.report, err
	}
	if err := r.warmupAndMeasure(ctx); err != nil {
		return r.report, err
	}
	if err := r.resyncPhase(ctx); err != nil {
		return r.report, err
	}
	r.finish()
	return r.report, nil
}

func (r *runner) setup(ctx context.Context) error {
	accounts, err := r.registerAll(ctx)
	if err != nil {
		return err
	}
	started := time.Now()
	boardID, err := r.api.createBoard(ctx, accounts[0].Token, "Load test "+time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("create board: %w", err)
	}
	r.boardID = boardID
	for _, acc := range accounts[1:] {
		if err := r.api.addMember(ctx, accounts[0].Token, boardID, acc.Email); err != nil {
			return fmt.Errorf("add member %s: %w", acc.Email, err)
		}
		select {
		case <-time.After(memberAddPace):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.report.Setup.BoardMs = ms(time.Since(started))
	r.logf("board %s created with %d members in %s", boardID, len(accounts), time.Since(started).Round(time.Millisecond))

	url := r.api.wsURL(boardID)
	for i, acc := range accounts {
		r.clients = append(r.clients, newWSClient(i, acc, url, r.col, r.world))
	}
	return r.connectAll(ctx)
}

func (r *runner) registerAll(ctx context.Context) ([]account, error) {
	started := time.Now()
	runID := uuid.NewString()[:8]
	accounts := make([]account, r.opts.Clients)
	errs := make(chan error, r.opts.Clients)
	sem := make(chan struct{}, r.opts.RegisterParallelism)
	var wg sync.WaitGroup
	for i := range accounts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			email := fmt.Sprintf("lt-%s-%d@example.com", runID, i)
			acc, err := r.api.register(ctx, email, fmt.Sprintf("Load %d", i), r.opts.Password)
			if err != nil {
				errs <- fmt.Errorf("register %s: %w", email, err)
				return
			}
			accounts[i] = acc
		}()
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return nil, err
	}
	r.report.Setup.RegisterMs = ms(time.Since(started))
	r.logf("registered %d users in %s", len(accounts), time.Since(started).Round(time.Millisecond))
	return accounts, nil
}

func (r *runner) connectAll(ctx context.Context) error {
	started := time.Now()
	results := make([]handshakeResult, len(r.clients))
	errs := make(chan error, len(r.clients))
	sem := make(chan struct{}, connectParallel)
	var wg sync.WaitGroup
	for i, c := range r.clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := c.connect(ctx, nil)
			if err != nil {
				errs <- err
				return
			}
			results[i] = res
		}()
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return err
	}
	handshakes := make([]time.Duration, len(results))
	for i, res := range results {
		handshakes[i] = res.Ready
	}
	r.report.Connect = summarize(handshakes)
	r.report.Setup.ConnectMs = ms(time.Since(started))
	r.logf("connected %d clients in %s", len(r.clients), time.Since(started).Round(time.Millisecond))
	return r.createColumns(ctx)
}

func (r *runner) createColumns(ctx context.Context) error {
	for i := range initialColumns {
		op := plannedOp{opType: boardcontent.OpColumnCreate, id: uuid.New(), payload: boardcontent.ColumnCreate{
			ColumnID: uuid.New(),
			Title:    fmt.Sprintf("Column %d", i+1),
		}}
		if err := r.clients[0].sendOp(op, false); err != nil {
			return fmt.Errorf("create column: %w", err)
		}
	}
	deadline := time.Now().Add(settleTimeout)
	for r.world.columnCount() < initialColumns {
		if time.Now().After(deadline) {
			return errors.New("columns were not created in time")
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (r *runner) generate(ctx context.Context, d time.Duration, record bool) int {
	interval := time.Duration(float64(time.Second) / r.opts.Rate)
	start := time.Now()
	deadline := start.Add(d)
	errCount := 0
	for i := 0; ; i++ {
		due := start.Add(time.Duration(i) * interval)
		if !due.Before(deadline) {
			return errCount
		}
		if wait := time.Until(due); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return errCount
			}
		}
		sender := r.clients[i%len(r.clients)]
		if err := sender.sendOp(r.gen.next(), record); err != nil {
			errCount++
			r.col.failure(fmt.Sprintf("send from client %d: %v", sender.idx, err))
		}
	}
}

func (r *runner) warmupAndMeasure(ctx context.Context) error {
	if r.opts.Warmup > 0 {
		r.logf("warming up for %s", r.opts.Warmup)
		r.generate(ctx, r.opts.Warmup, false)
	}
	r.logf("measuring for %s at %.1f ops/s", r.opts.Duration, r.opts.Rate)
	r.col.recording.Store(true)
	r.report.Ops.SendErrors = r.generate(ctx, r.opts.Duration, true)
	if err := ctx.Err(); err != nil {
		return err
	}
	r.drain(ctx)
	r.col.recording.Store(false)
	return nil
}

func (r *runner) drain(ctx context.Context) {
	deadline := time.Now().Add(drainTimeout)
	for time.Now().Before(deadline) {
		s := r.col.delivery(len(r.clients))
		if s.MissingEvents == 0 {
			return
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return
		}
	}
}

func (r *runner) resyncPhase(ctx context.Context) error {
	if r.opts.ResyncTrials == 0 || r.opts.ResyncEvents == 0 {
		return nil
	}
	for trial := range r.opts.ResyncTrials {
		victim := r.clients[1+trial%(len(r.clients)-1)]
		res, err := r.resyncTrial(ctx, victim)
		if err != nil {
			return fmt.Errorf("resync trial %d: %w", trial+1, err)
		}
		r.report.Resync = append(r.report.Resync, res)
		r.logf("resync trial %d: %s of %d events, ready in %.0f ms", trial+1, res.Handshake.Kind, res.Handshake.Events, res.Handshake.ReadyMs)
	}
	return r.snapshotResync(ctx)
}

func (r *runner) snapshotResync(ctx context.Context) error {
	victim := r.clients[1]
	victim.disconnect()
	res, err := victim.connect(ctx, nil)
	if err != nil {
		return fmt.Errorf("snapshot resync: %w", err)
	}
	r.report.SnapshotSync = res
	return nil
}

func (r *runner) resyncTrial(ctx context.Context, victim *wsClient) (ResyncTrial, error) {
	lastSeq := victim.lastSeq.Load()
	victim.disconnect()
	fillStart := time.Now()
	if err := r.fill(ctx, victim, r.opts.ResyncEvents); err != nil {
		return ResyncTrial{}, err
	}
	target := r.waitForSettle(ctx)
	trial := ResyncTrial{
		Victim:         victim.idx,
		LastSeq:        lastSeq,
		TargetSeq:      target,
		Missed:         target - lastSeq,
		FillDurationMs: ms(time.Since(fillStart)),
	}
	started := time.Now()
	res, err := victim.connect(ctx, &lastSeq)
	if err != nil {
		return trial, err
	}
	trial.Handshake = res
	deadline := started.Add(catchUpTimeout)
	for victim.lastSeq.Load() < target && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	trial.CaughtUp = victim.lastSeq.Load() >= target
	trial.CaughtUpMs = ms(time.Since(started))
	return trial, nil
}

func (r *runner) fill(ctx context.Context, victim *wsClient, total int) error {
	var senders []*wsClient
	for _, c := range r.clients {
		if c != victim {
			senders = append(senders, c)
		}
	}
	workers := min(fillWorkers, len(senders))
	var next atomic.Int64
	var failed atomic.Int64
	var wg sync.WaitGroup
	interval := time.Second / fillOpsPerSecond
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := senders[w]
			for ctx.Err() == nil {
				if next.Add(1) > int64(total) {
					return
				}
				if err := c.sendOp(r.gen.next(), false); err != nil {
					failed.Add(1)
					return
				}
				time.Sleep(interval)
			}
		}()
	}
	wg.Wait()
	if n := failed.Load(); n > 0 {
		return fmt.Errorf("%d fill workers failed to send", n)
	}
	return ctx.Err()
}

func (r *runner) waitForSettle(ctx context.Context) int64 {
	last := r.col.maxSeq.Load()
	stableSince := time.Now()
	deadline := time.Now().Add(settleTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return last
		}
		cur := r.col.maxSeq.Load()
		if cur != last {
			last = cur
			stableSince = time.Now()
			continue
		}
		if time.Since(stableSince) > 700*time.Millisecond {
			return last
		}
	}
	return last
}

func (r *runner) finish() {
	all, self, peer := r.col.distributions()
	r.report.Latency, r.report.LatencySelf, r.report.LatencyPeer = all, self, peer
	d := r.col.delivery(len(r.clients))
	r.report.Ops.Sent = d.OpsSent
	r.report.Ops.Nacked = d.OpsNacked
	r.report.Ops.Delivered = d.OpsComplete
	r.report.Ops.MissingEvents = d.MissingEvents
	r.report.Ops.Nacks = r.col.nackCounts()
	r.report.Failures = r.col.failureList()
	r.report.BoardEventSeq = r.col.maxSeq.Load()
	for _, t := range r.report.Resync {
		r.report.ResyncMaxMs = max(r.report.ResyncMaxMs, t.CaughtUpMs, t.Handshake.ReadyMs)
		if !t.CaughtUp {
			r.report.Failures = append(r.report.Failures, fmt.Sprintf("client %d did not catch up to seq %d", t.Victim, t.TargetSeq))
		}
	}
	if d.MissingEvents > 0 {
		r.report.Failures = append(r.report.Failures, fmt.Sprintf("%d receiver events never arrived", d.MissingEvents))
	}
	r.report.Targets = Targets{
		P95LimitMs:    ms(r.opts.P95Target),
		ResyncLimitMs: ms(r.opts.ResyncTarget),
		P95Pass:       all.Count > 0 && all.P95Ms < ms(r.opts.P95Target),
		ResyncPass:    r.report.ResyncMaxMs < ms(r.opts.ResyncTarget),
	}
}
