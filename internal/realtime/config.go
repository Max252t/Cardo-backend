package realtime

import "time"

const (
	DefaultSendBuffer            = 256
	DefaultMaxMessageBytes       = 64 << 10
	DefaultPingInterval          = 30 * time.Second
	DefaultPongWait              = 60 * time.Second
	DefaultWriteWait             = 10 * time.Second
	DefaultHelloTimeout          = 15 * time.Second
	DefaultPresenceInterval      = 15 * time.Second
	DefaultOpTimeout             = 15 * time.Second
	DefaultSyncTimeout           = 30 * time.Second
	DefaultBusTimeout            = 5 * time.Second
	DefaultCloseGrace            = 2 * time.Second
	DefaultOpsPerSecond          = 20
	DefaultOpsBurst              = 20
	DefaultRateAbuseLimit        = 100
	DefaultRateAbuseWindow       = 10 * time.Second
	DefaultMaxProtocolViolations = 2
	DefaultMaxDeltaEvents        = 5000
	DefaultDeltaChunk            = 500
	DefaultPendingLimit          = 4096
)

type Config struct {
	AllowedOrigins        []string
	SendBuffer            int
	MaxMessageBytes       int64
	PingInterval          time.Duration
	PongWait              time.Duration
	WriteWait             time.Duration
	HelloTimeout          time.Duration
	PresenceInterval      time.Duration
	OpTimeout             time.Duration
	SyncTimeout           time.Duration
	BusTimeout            time.Duration
	CloseGrace            time.Duration
	OpsPerSecond          float64
	OpsBurst              int
	RateAbuseLimit        int
	RateAbuseWindow       time.Duration
	MaxProtocolViolations int
	MaxDeltaEvents        int
	DeltaChunk            int
	PendingLimit          int
}

func (c Config) withDefaults() Config {
	c.SendBuffer = positive(c.SendBuffer, DefaultSendBuffer)
	if c.MaxMessageBytes <= 0 {
		c.MaxMessageBytes = DefaultMaxMessageBytes
	}
	c.PingInterval = positiveDuration(c.PingInterval, DefaultPingInterval)
	c.PongWait = positiveDuration(c.PongWait, DefaultPongWait)
	c.WriteWait = positiveDuration(c.WriteWait, DefaultWriteWait)
	c.HelloTimeout = positiveDuration(c.HelloTimeout, DefaultHelloTimeout)
	c.PresenceInterval = positiveDuration(c.PresenceInterval, DefaultPresenceInterval)
	c.OpTimeout = positiveDuration(c.OpTimeout, DefaultOpTimeout)
	c.SyncTimeout = positiveDuration(c.SyncTimeout, DefaultSyncTimeout)
	c.BusTimeout = positiveDuration(c.BusTimeout, DefaultBusTimeout)
	c.CloseGrace = positiveDuration(c.CloseGrace, DefaultCloseGrace)
	if c.OpsPerSecond <= 0 {
		c.OpsPerSecond = DefaultOpsPerSecond
	}
	c.OpsBurst = positive(c.OpsBurst, DefaultOpsBurst)
	c.RateAbuseLimit = positive(c.RateAbuseLimit, DefaultRateAbuseLimit)
	c.RateAbuseWindow = positiveDuration(c.RateAbuseWindow, DefaultRateAbuseWindow)
	c.MaxProtocolViolations = positive(c.MaxProtocolViolations, DefaultMaxProtocolViolations)
	c.MaxDeltaEvents = positive(c.MaxDeltaEvents, DefaultMaxDeltaEvents)
	c.DeltaChunk = positive(c.DeltaChunk, DefaultDeltaChunk)
	c.PendingLimit = positive(c.PendingLimit, DefaultPendingLimit)
	return c
}

func positive(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func positiveDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}
