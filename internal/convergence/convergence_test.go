package convergence_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/simclient"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

const (
	seedEnv        = "CONVERGENCE_SEED"
	opsEnv         = "CONVERGENCE_OPS"
	quiescenceWait = 90 * time.Second
	runDeadline    = 4 * time.Minute

	losslessGapWait = 3 * time.Second
	shortDivisor    = 4
	shortMinimumOps = 20
)

func TestMain(m *testing.M) {
	testutil.RunMain(m)
}

type scenario struct {
	transport    transport
	clients      int
	ops          int
	dropChance   float64
	eventLoss    float64
	blackoutOps  int
	hotSpot      float64
	pruneEvents  bool
	maxOpGap     time.Duration
	idlePoll     time.Duration
	gapWait      time.Duration
	allowedNacks map[string]bool
}

func seedFor(t *testing.T) int64 {
	t.Helper()
	seed := time.Now().UnixNano()
	if raw := os.Getenv(seedEnv); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		require.NoError(t, err, seedEnv)
		seed = parsed
	}
	t.Logf("%s=%d", seedEnv, seed)
	return seed
}

func opsFor(t *testing.T, def int) int {
	t.Helper()
	raw := os.Getenv(opsEnv)
	if raw == "" {
		if testing.Short() {
			return max(def/shortDivisor, shortMinimumOps)
		}
		return def
	}
	n, err := strconv.Atoi(raw)
	require.NoError(t, err, opsEnv)
	return n
}

func baseNacks() map[string]bool {
	return map[string]bool{
		boardcontent.ReasonVersionConflict: true,
		boardcontent.ReasonNotFound:        true,
	}
}

func drive(ctx context.Context, c *simclient.Client, rng *rand.Rand, sc scenario) {
	created := 0
	for created < sc.ops && ctx.Err() == nil {
		time.Sleep(time.Duration(rng.Int63n(int64(sc.maxOpGap) + 1)))
		if c.RandomOp() {
			created++
		} else {
			time.Sleep(5 * time.Millisecond)
		}
		if rng.Float64() < sc.dropChance {
			offline := time.Duration(rng.Intn(150)) * time.Millisecond
			if rng.Intn(10) == 0 {
				offline = time.Duration(500+rng.Intn(600)) * time.Millisecond
			}
			c.Drop(offline)
		}
	}
}

func startClients(t *testing.T, s *stack, sc scenario, seed int64, profile simclient.Profile) []*simclient.Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	clients := make([]*simclient.Client, 0, sc.clients)
	for i := range sc.clients {
		user := s.owner
		if i > 0 {
			user = s.members[(i-1)%len(s.members)]
		}
		c := simclient.New(simclient.Config{
			Name:      fmt.Sprintf("c%d", i),
			BoardID:   s.boardID,
			Endpoints: s.endpoints(),
			Token:     s.token(t, user.ID),
			Seed:      seed + int64(i) + 1,
			Profile:   profile,
			IdlePoll:  sc.idlePoll,
			GapWait:   sc.gapWait,
			Logf:      t.Logf,
		})
		c.Start(ctx)
		clients = append(clients, c)
	}
	t.Cleanup(func() {
		cancel()
		for _, c := range clients {
			c.Stop()
		}
	})
	return clients
}

func waitConnected(t *testing.T, clients []*simclient.Client) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, c := range clients {
			st := c.Status()
			if !st.Live || !st.Initialized {
				return false
			}
		}
		return true
	}, 30*time.Second, 50*time.Millisecond, "clients must complete the first handshake")
}

func waitQuiescent(t *testing.T, s *stack, clients []*simclient.Client) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(quiescenceWait)
	stable := 0
	var lastReason string
	for time.Now().Before(deadline) {
		snap, err := s.snapshot(ctx)
		require.NoError(t, err)
		lastReason = ""
		for i, c := range clients {
			st := c.Status()
			switch {
			case !st.Live:
				lastReason = fmt.Sprintf("client %d is not live", i)
			case st.Pending != 0:
				lastReason = fmt.Sprintf("client %d has %d pending ops", i, st.Pending)
			case st.Buffered != 0:
				lastReason = fmt.Sprintf("client %d has %d buffered events", i, st.Buffered)
			case st.State.Seq != snap.Seq:
				lastReason = fmt.Sprintf("client %d is at seq %d, database at %d", i, st.State.Seq, snap.Seq)
			}
			if lastReason != "" {
				break
			}
		}
		if lastReason == "" {
			stable++
			if stable >= 4 {
				return
			}
		} else {
			stable = 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	for i, c := range clients {
		st := c.Status()
		t.Logf("client %d: live=%v pending=%d buffered=%d seq=%d stats=%+v", i, st.Live, st.Pending, st.Buffered, st.State.Seq, st.Stats)
	}
	t.Fatalf("clients did not reach quiescence in %s: %s", quiescenceWait, lastReason)
}

func runPruner(ctx context.Context, t *testing.T, s *stack, wg *sync.WaitGroup) {
	defer wg.Done()
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.store.PruneEvents(ctx, time.Now().Add(time.Hour), 15, 100); err != nil && ctx.Err() == nil {
				t.Errorf("prune events: %v", err)
				return
			}
		}
	}
}

func runScenario(t *testing.T, sc scenario) {
	t.Helper()
	if sc.maxOpGap <= 0 {
		sc.maxOpGap = 20 * time.Millisecond
	}
	lossless := sc.eventLoss == 0 && sc.blackoutOps == 0
	if lossless && sc.idlePoll <= 0 {
		sc.idlePoll = time.Hour
	}
	if lossless && !sc.pruneEvents && sc.gapWait <= 0 {
		sc.gapWait = losslessGapWait
	}
	if sc.allowedNacks == nil {
		sc.allowedNacks = baseNacks()
	}
	seed := seedFor(t)
	s := newStack(t, sc.transport, sc.clients, 2, seed)
	s.filter.setProbability(sc.eventLoss)
	clients := startClients(t, s, sc, seed, simclient.Profile{HotSpot: sc.hotSpot})
	waitConnected(t, clients)
	t.Cleanup(func() {
		if t.Failed() {
			dumpDiagnostics(t, s, clients)
		}
	})

	runCtx, cancel := context.WithTimeout(context.Background(), runDeadline)
	defer cancel()
	var pruners sync.WaitGroup
	if sc.pruneEvents {
		pruners.Add(1)
		go runPruner(runCtx, t, s, &pruners)
	}
	started := time.Now()
	var drivers sync.WaitGroup
	for i, c := range clients {
		drivers.Add(1)
		go func() {
			defer drivers.Done()
			drive(runCtx, c, rand.New(rand.NewSource(seed+1000+int64(i))), sc)
		}()
	}
	drivers.Wait()
	require.NoError(t, runCtx.Err(), "drivers must finish before the run deadline")
	t.Logf("drivers finished %d ops per client in %s", sc.ops, time.Since(started).Round(time.Millisecond))

	waitQuiescent(t, s, clients)
	if sc.blackoutOps > 0 {
		s.filter.setBlackout(true)
		for range sc.blackoutOps {
			clients[0].RandomOp()
			time.Sleep(30 * time.Millisecond)
		}
		waitQuiescent(t, s, clients)
		s.filter.setBlackout(false)
	}
	cancel()
	pruners.Wait()
	t.Logf("converged in %s", time.Since(started).Round(time.Millisecond))
	assertConverged(t, s, clients, sc)
}

func assertConverged(t *testing.T, s *stack, clients []*simclient.Client, sc scenario) {
	t.Helper()
	snap, err := s.snapshot(context.Background())
	require.NoError(t, err)
	want := protocol.FromSnapshot(snap)
	var created, nacked, gapEvents, pollEvents, reloads, selfDrops, resyncs int
	for i, c := range clients {
		st := c.Status()
		stats := st.Stats
		t.Logf("client %d: seq=%d ops=%d sent=%d localDrops=%d reconnects=%d selfDrops=%d dialFailures=%d gapFetches=%d gapEvents=%d pollEvents=%d snapshots=%d gone=%d resync=%d nacks=%v",
			i, st.State.Seq, stats.OpsCreated, stats.OpsSent, stats.LocalDrops, stats.Reconnects, stats.SelfDrops, stats.DialFailures,
			stats.GapFetches, stats.GapFetchEvents, stats.PollEvents, stats.SnapshotReloads, stats.Gone, stats.ResyncRequired, stats.Nacks)
		assert.Empty(t, st.Stats.Failures, "client %d failures", i)
		assert.Zero(t, st.Pending, "client %d pending ops", i)
		assert.Empty(t, st.State.Diff(want), "client %d view differs from the database", i)
		for reason, n := range stats.Nacks {
			assert.True(t, sc.allowedNacks[reason], "client %d received %d unexpected %q nacks", i, n, reason)
			nacked += n
		}
		created += stats.OpsCreated
		gapEvents += stats.GapFetchEvents
		pollEvents += stats.PollEvents
		reloads += stats.SnapshotReloads
		selfDrops += stats.SelfDrops
		resyncs += stats.ResyncRequired
	}
	t.Logf("totals: ops=%d nacks=%d db_seq=%d gap_fetch_events=%d poll_events=%d snapshot_reloads=%d self_drops=%d resync_nacks=%d dropped_messages=%d",
		created, nacked, snap.Seq, gapEvents, pollEvents, reloads, selfDrops, resyncs, s.filter.dropped.Load())
	assert.GreaterOrEqual(t, created, sc.ops*sc.clients, "every client must create its ops")
	assert.Greater(t, int(snap.Seq), created/4, "most operations must have produced events")
	if sc.dropChance > 0 {
		assert.Positive(t, selfDrops, "the run must include connection drops")
	}
	if sc.eventLoss > 0 || sc.blackoutOps > 0 {
		assert.Positive(t, s.filter.dropped.Load(), "the run must lose pub/sub messages")
		assert.Positive(t, gapEvents+pollEvents, "lost events must be recovered over REST")
	}
	if sc.eventLoss == 0 && sc.blackoutOps == 0 && !sc.pruneEvents {
		assert.Zero(t, gapEvents+pollEvents, "without message loss no event may need a REST fetch")
	}
	if sc.hotSpot > 0 {
		var rebalances int
		require.NoError(t, s.store.Pool().QueryRow(context.Background(),
			`SELECT count(*) FROM board_events WHERE board_id = $1 AND type = 'column.rebalanced'`, s.boardID).Scan(&rebalances))
		t.Logf("column.rebalanced events: %d", rebalances)
		assert.Positive(t, rebalances, "the hot spot must force at least one rebalance")
	}
}

func TestConvergenceRedis(t *testing.T) {
	runScenario(t, scenario{
		transport:  transportRedis,
		clients:    5,
		ops:        opsFor(t, 200),
		dropChance: 0.05,
	})
}

func TestConvergenceMemory(t *testing.T) {
	runScenario(t, scenario{
		transport:  transportMemory,
		clients:    5,
		ops:        opsFor(t, 200),
		dropChance: 0.05,
	})
}

func TestConvergenceUnderEventLossRedis(t *testing.T) {
	runScenario(t, scenario{
		transport:   transportRedis,
		clients:     5,
		ops:         opsFor(t, 60),
		dropChance:  0.03,
		eventLoss:   0.04,
		blackoutOps: 4,
		maxOpGap:    40 * time.Millisecond,
	})
}

func TestConvergenceUnderEventLossMemory(t *testing.T) {
	runScenario(t, scenario{
		transport:   transportMemory,
		clients:     5,
		ops:         opsFor(t, 60),
		dropChance:  0.03,
		eventLoss:   0.04,
		blackoutOps: 4,
		maxOpGap:    40 * time.Millisecond,
	})
}

func TestConvergenceWithPrunedEvents(t *testing.T) {
	nacks := baseNacks()
	nacks[boardcontent.ReasonResyncRequired] = true
	runScenario(t, scenario{
		transport:    transportMemory,
		clients:      5,
		ops:          opsFor(t, 150),
		dropChance:   0.1,
		pruneEvents:  true,
		allowedNacks: nacks,
	})
}

func TestConvergenceRebalanceHotSpot(t *testing.T) {
	runScenario(t, scenario{
		transport:  transportMemory,
		clients:    3,
		ops:        opsFor(t, 250),
		dropChance: 0.02,
		hotSpot:    0.97,
		maxOpGap:   10 * time.Millisecond,
	})
}

func dumpDiagnostics(t *testing.T, s *stack, clients []*simclient.Client) {
	t.Helper()
	snap, err := s.snapshot(context.Background())
	if err == nil {
		for i := range s.hubLogs {
			t.Logf("instance %d: board events never seen by its hub: %v (of %d)", i, s.hubLogs[i].missing(snap.Seq), snap.Seq)
		}
	}
	for i, c := range clients {
		st := c.Status()
		if st.Stats.GapFetches == 0 && len(st.Stats.Failures) == 0 {
			continue
		}
		t.Logf("client %d trace:\n%s", i, strings.Join(st.Stats.Trace, "\n"))
	}
}
