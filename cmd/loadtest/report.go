package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

type ConfigInfo struct {
	URL          string  `json:"url"`
	Clients      int     `json:"clients"`
	RatePerSec   float64 `json:"rate_ops_per_sec"`
	DurationSec  float64 `json:"duration_sec"`
	WarmupSec    float64 `json:"warmup_sec"`
	ResyncEvents int     `json:"resync_events"`
	ResyncTrials int     `json:"resync_trials"`
	Seed         int64   `json:"seed"`
}

type SetupInfo struct {
	RegisterMs float64 `json:"register_ms"`
	BoardMs    float64 `json:"board_and_members_ms"`
	ConnectMs  float64 `json:"connect_all_ms"`
}

type OpsInfo struct {
	Sent          int64          `json:"sent"`
	Nacked        int            `json:"nacked"`
	Delivered     int            `json:"fully_delivered"`
	MissingEvents int            `json:"missing_receiver_events"`
	Nacks         map[string]int `json:"nacks_by_reason"`
	SendErrors    int            `json:"send_errors"`
}

type ResyncTrial struct {
	Victim         int             `json:"victim"`
	LastSeq        int64           `json:"last_seq"`
	TargetSeq      int64           `json:"target_seq"`
	Missed         int64           `json:"missed_events"`
	Handshake      handshakeResult `json:"handshake"`
	CaughtUpMs     float64         `json:"caught_up_ms"`
	CaughtUp       bool            `json:"caught_up"`
	FillDurationMs float64         `json:"fill_duration_ms"`
}

type Targets struct {
	P95LimitMs    float64 `json:"p95_limit_ms"`
	ResyncLimitMs float64 `json:"resync_limit_ms"`
	P95Pass       bool    `json:"p95_pass"`
	ResyncPass    bool    `json:"resync_pass"`
}

type Report struct {
	StartedAt     time.Time       `json:"started_at"`
	Config        ConfigInfo      `json:"config"`
	Setup         SetupInfo       `json:"setup"`
	Connect       Dist            `json:"connect_handshake"`
	Latency       Dist            `json:"latency_all"`
	LatencySelf   Dist            `json:"latency_sender"`
	LatencyPeer   Dist            `json:"latency_peers"`
	Ops           OpsInfo         `json:"ops"`
	Resync        []ResyncTrial   `json:"resync"`
	SnapshotSync  handshakeResult `json:"snapshot_resync"`
	ResyncMaxMs   float64         `json:"resync_max_ms"`
	Failures      []string        `json:"failures"`
	Targets       Targets         `json:"targets"`
	BoardEventSeq int64           `json:"board_final_seq"`
}

func (r Report) Passed() bool {
	return r.Targets.P95Pass && r.Targets.ResyncPass && len(r.Failures) == 0
}

func verdict(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func writeDist(w io.Writer, label string, d Dist) {
	_, _ = fmt.Fprintf(w, "  %-14s n=%-7d min=%7.1f  mean=%7.1f  p50=%7.1f  p95=%7.1f  p99=%7.1f  max=%7.1f ms\n",
		label, d.Count, d.MinMs, d.MeanMs, d.P50Ms, d.P95Ms, d.P99Ms, d.MaxMs)
}

func (r Report) WriteText(w io.Writer) {
	c := r.Config
	_, _ = fmt.Fprintf(w, "Cardo load test against %s\n", c.URL)
	_, _ = fmt.Fprintf(w, "  clients=%d rate=%.1f ops/s duration=%.0fs warmup=%.0fs seed=%d\n\n", c.Clients, c.RatePerSec, c.DurationSec, c.WarmupSec, c.Seed)
	_, _ = fmt.Fprintf(w, "Setup\n  register %d users: %.0f ms, board and members: %.0f ms, connect all clients: %.0f ms\n",
		c.Clients, r.Setup.RegisterMs, r.Setup.BoardMs, r.Setup.ConnectMs)
	writeDist(w, "handshake", r.Connect)
	_, _ = fmt.Fprintf(w, "\nSend-to-receive latency (op sent by one client, event received by a client)\n")
	writeDist(w, "all receivers", r.Latency)
	writeDist(w, "peers only", r.LatencyPeer)
	writeDist(w, "sender only", r.LatencySelf)
	_, _ = fmt.Fprintf(w, "  target p95 < %.0f ms: %s\n", r.Targets.P95LimitMs, verdict(r.Targets.P95Pass))
	_, _ = fmt.Fprintf(w, "\nOperations\n  sent=%d nacked=%d fully delivered=%d missing receiver events=%d send errors=%d\n",
		r.Ops.Sent, r.Ops.Nacked, r.Ops.Delivered, r.Ops.MissingEvents, r.Ops.SendErrors)
	if len(r.Ops.Nacks) > 0 {
		reasons := make([]string, 0, len(r.Ops.Nacks))
		for k := range r.Ops.Nacks {
			reasons = append(reasons, k)
		}
		sort.Strings(reasons)
		parts := make([]string, 0, len(reasons))
		for _, k := range reasons {
			parts = append(parts, fmt.Sprintf("%s=%d", k, r.Ops.Nacks[k]))
		}
		_, _ = fmt.Fprintf(w, "  nacks: %s\n", strings.Join(parts, " "))
	}
	_, _ = fmt.Fprintf(w, "\nResync after a disconnect (hello with last_seq)\n")
	for i, t := range r.Resync {
		_, _ = fmt.Fprintf(w, "  trial %d: client %d missed %d events -> %s with %d events, dial %.0f ms, first sync %.0f ms, ready %.0f ms, caught up %.0f ms (%v)\n",
			i+1, t.Victim, t.Missed, t.Handshake.Kind, t.Handshake.Events, t.Handshake.DialMs, t.Handshake.FirstSyncMs, t.Handshake.ReadyMs, t.CaughtUpMs, t.CaughtUp)
	}
	if r.SnapshotSync.Kind != "" {
		_, _ = fmt.Fprintf(w, "  full snapshot (hello without last_seq): ready %.0f ms\n", r.SnapshotSync.ReadyMs)
	}
	_, _ = fmt.Fprintf(w, "  slowest resync %.0f ms, target < %.0f ms: %s\n", r.ResyncMaxMs, r.Targets.ResyncLimitMs, verdict(r.Targets.ResyncPass))
	if len(r.Failures) > 0 {
		_, _ = fmt.Fprintf(w, "\nFailures (%d)\n", len(r.Failures))
		for _, f := range r.Failures {
			_, _ = fmt.Fprintf(w, "  %s\n", f)
		}
	}
	_, _ = fmt.Fprintf(w, "\nResult: %s\n", verdict(r.Passed()))
}
