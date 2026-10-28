package pubsub_test

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/pubsub"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

const waitLimit = 5 * time.Second

type pausableProxy struct {
	addr     string
	upstream string
	listener net.Listener

	mu     sync.Mutex
	cond   *sync.Cond
	paused bool
	closed bool
	conns  []net.Conn
}

func newPausableProxy(t *testing.T, upstream string) *pausableProxy {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &pausableProxy{addr: ln.Addr().String(), upstream: upstream, listener: ln}
	p.cond = sync.NewCond(&p.mu)
	go p.acceptLoop()
	t.Cleanup(p.shutdown)
	return p
}

func (p *pausableProxy) acceptLoop() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		up, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", p.upstream)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, client, up)
		p.mu.Unlock()
		go func() {
			_, _ = io.Copy(up, client)
			_ = up.Close()
		}()
		go p.pipeDown(up, client)
	}
}

func (p *pausableProxy) pipeDown(up, client net.Conn) {
	buf := make([]byte, 4096)
	for {
		n, err := up.Read(buf)
		if n > 0 {
			p.mu.Lock()
			for p.paused && !p.closed {
				p.cond.Wait()
			}
			p.mu.Unlock()
			if _, werr := client.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			_ = client.Close()
			return
		}
	}
}

func (p *pausableProxy) pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = true
}

func (p *pausableProxy) resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = false
	p.cond.Broadcast()
}

func (p *pausableProxy) shutdown() {
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	conns := append([]net.Conn(nil), p.conns...)
	p.mu.Unlock()
	_ = p.listener.Close()
	for _, c := range conns {
		_ = c.Close()
	}
}

func proxiedClient(t *testing.T, direct *redis.Client) (*redis.Client, *pausableProxy) {
	t.Helper()
	proxy := newPausableProxy(t, direct.Options().Addr)
	opts := *direct.Options()
	opts.Addr = proxy.addr
	client := redis.NewClient(&opts)
	t.Cleanup(func() { _ = client.Close() })
	return client, proxy
}

func background(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newChannels(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = protocol.BoardChannel(uuid.New())
	}
	return out
}

func drainBuffered(b *pubsub.RedisBroker) {
	for {
		select {
		case <-b.Messages():
		default:
			return
		}
	}
}

func expectMarkerWithoutChannels(t *testing.T, sub *pubsub.RedisBroker, marker string, forbidden ...string) {
	t.Helper()
	deadline := time.After(waitLimit)
	for {
		select {
		case m := <-sub.Messages():
			require.NotContains(t, forbidden, m.Channel, "message delivered after unsubscribe returned")
			if m.Channel == marker {
				return
			}
		case <-deadline:
			t.Fatal("marker message was not received")
		}
	}
}

func completes(t *testing.T, fn func() error) error {
	t.Helper()
	return async(fn).result(t)
}

type asyncCall struct {
	done chan struct{}
	err  error
}

func async(fn func() error) *asyncCall {
	c := &asyncCall{done: make(chan struct{})}
	go func() {
		c.err = fn()
		close(c.done)
	}()
	return c
}

func (c *asyncCall) finished() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *asyncCall) result(t *testing.T) error {
	t.Helper()
	select {
	case <-c.done:
		return c.err
	case <-time.After(waitLimit):
		t.Fatal("call did not return in time")
		return nil
	}
}

func TestRedisBrokerUnsubscribeStopsDeliveryOnceItReturns(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx := background(t)
	pub := pubsub.NewRedisBroker(ctx, client)
	defer func() { _ = pub.Close() }()

	for i := range 25 {
		sub := pubsub.NewRedisBroker(ctx, client)
		chans := newChannels(1)
		marker := protocol.PresenceChannel(uuid.New())
		require.NoError(t, sub.Subscribe(ctx, chans[0], marker), i)

		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = pub.Publish(ctx, chans[0], []byte("a"))
				}
			}
		}()
		receive(t, sub)
		require.NoError(t, sub.Unsubscribe(ctx, chans[0]), i)
		close(stop)
		wg.Wait()

		drainBuffered(sub)
		require.NoError(t, pub.Publish(ctx, marker, []byte("marker")))
		expectMarkerWithoutChannels(t, sub, marker, chans[0])
		require.NoError(t, sub.Close())
	}
}

func TestRedisBrokerUnsubscribeManyChannelsAtOnce(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx := background(t)
	pub := pubsub.NewRedisBroker(ctx, client)
	sub := pubsub.NewRedisBroker(ctx, client)
	defer func() { _ = pub.Close() }()
	defer func() { _ = sub.Close() }()

	for i := range 10 {
		chans := newChannels(6)
		keep := protocol.PresenceChannel(uuid.New())
		require.NoError(t, sub.Subscribe(ctx, append(chans, keep)...), i)
		require.NoError(t, sub.Unsubscribe(ctx, chans...), i)

		for _, ch := range chans {
			require.NoError(t, pub.Publish(ctx, ch, []byte("late")))
		}
		require.NoError(t, pub.Publish(ctx, keep, []byte("marker")))
		expectMarkerWithoutChannels(t, sub, keep, chans...)
		require.NoError(t, sub.Unsubscribe(ctx, keep))
	}
}

func TestRedisBrokerUnsubscribeOfUnknownChannelReturns(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx := background(t)
	b := pubsub.NewRedisBroker(ctx, client)
	defer func() { _ = b.Close() }()
	ch := newChannels(1)[0]

	require.NoError(t, completes(t, func() error { return b.Unsubscribe(ctx, ch) }))
	require.NoError(t, b.Subscribe(ctx, ch))
	require.NoError(t, completes(t, func() error { return b.Unsubscribe(ctx, ch) }))
	require.NoError(t, completes(t, func() error { return b.Unsubscribe(ctx, ch) }))
}

func TestRedisBrokerUnsubscribeWithoutChannelsDoesNotBlock(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx := background(t)
	b := pubsub.NewRedisBroker(ctx, client)
	defer func() { _ = b.Close() }()

	require.NoError(t, b.Subscribe(ctx, newChannels(2)...))
	assert.NoError(t, completes(t, func() error { return b.Unsubscribe(ctx) }))
}

func TestRedisBrokerSubscribeUnsubscribeFlapping(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx := background(t)
	pub := pubsub.NewRedisBroker(ctx, client)
	sub := pubsub.NewRedisBroker(ctx, client)
	defer func() { _ = pub.Close() }()
	defer func() { _ = sub.Close() }()
	ch := newChannels(1)[0]
	marker := protocol.PresenceChannel(uuid.New())
	require.NoError(t, sub.Subscribe(ctx, marker))

	for i := range 40 {
		require.NoError(t, sub.Subscribe(ctx, ch), i)
		require.NoError(t, sub.Unsubscribe(ctx, ch), i)
	}
	drainBuffered(sub)
	require.NoError(t, pub.Publish(ctx, ch, []byte("ignored")))
	require.NoError(t, pub.Publish(ctx, marker, []byte("marker")))
	expectMarkerWithoutChannels(t, sub, marker, ch)

	require.NoError(t, sub.Subscribe(ctx, ch))
	require.NoError(t, pub.Publish(ctx, ch, []byte("live")))
	m := receive(t, sub)
	assert.Equal(t, ch, m.Channel)
	assert.Equal(t, "live", string(m.Payload))
}

func TestRedisBrokerConcurrentSubscribeAndUnsubscribeOnTheSameChannel(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx := background(t)
	b := pubsub.NewRedisBroker(ctx, client)
	defer func() { _ = b.Close() }()

	for i := range 20 {
		ch := newChannels(1)[0]
		require.NoError(t, b.Subscribe(ctx, ch))
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for range 4 {
			wg.Add(2)
			go func() {
				defer wg.Done()
				errs <- b.Unsubscribe(ctx, ch)
			}()
			go func() {
				defer wg.Done()
				errs <- b.Subscribe(ctx, ch)
			}()
		}
		finished := make(chan struct{})
		go func() { wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(waitLimit):
			t.Fatalf("iteration %d: concurrent calls did not return", i)
		}
		close(errs)
		for err := range errs {
			assert.NoError(t, err)
		}
		require.NoError(t, b.Unsubscribe(ctx, ch))
	}
}

func TestRedisBrokerConcurrentUnsubscribesOnDistinctChannels(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx := background(t)
	b := pubsub.NewRedisBroker(ctx, client)
	defer func() { _ = b.Close() }()
	chans := newChannels(30)
	require.NoError(t, b.Subscribe(ctx, chans...))

	var wg sync.WaitGroup
	errs := make(chan error, len(chans))
	for _, ch := range chans {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- b.Unsubscribe(ctx, ch)
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(waitLimit):
		t.Fatal("concurrent unsubscribes did not return")
	}
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}
}

func TestRedisBrokerSubscribeAndUnsubscribeFailAfterClose(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx := background(t)
	b := pubsub.NewRedisBroker(ctx, client)
	ch := newChannels(1)[0]
	require.NoError(t, b.Subscribe(ctx, ch))
	require.NoError(t, b.Close())

	assert.Error(t, completes(t, func() error { return b.Unsubscribe(ctx, ch) }))
	assert.Error(t, completes(t, func() error { return b.Subscribe(ctx, ch) }))
}

func TestRedisBrokerUnsubscribeWaitsForServerConfirmation(t *testing.T) {
	direct := testutil.NewRedis(t)
	client, proxy := proxiedClient(t, direct)
	ctx := background(t)
	b := pubsub.NewRedisBroker(ctx, client)
	defer func() { _ = b.Close() }()
	ch := newChannels(1)[0]
	require.NoError(t, b.Subscribe(ctx, ch))

	proxy.pause()
	call := async(func() error { return b.Unsubscribe(ctx, ch) })

	assert.Never(t, call.finished, 250*time.Millisecond, 10*time.Millisecond, "Unsubscribe returned before the server confirmed it")

	proxy.resume()
	require.NoError(t, call.result(t))
}

func TestRedisBrokerSubscribeWaitsForServerConfirmation(t *testing.T) {
	direct := testutil.NewRedis(t)
	client, proxy := proxiedClient(t, direct)
	ctx := background(t)
	b := pubsub.NewRedisBroker(ctx, client)
	defer func() { _ = b.Close() }()
	warm := newChannels(1)[0]
	require.NoError(t, b.Subscribe(ctx, warm))

	proxy.pause()
	call := async(func() error { return b.Subscribe(ctx, newChannels(1)[0]) })

	assert.Never(t, call.finished, 250*time.Millisecond, 10*time.Millisecond)

	proxy.resume()
	require.NoError(t, call.result(t))
}

func TestRedisBrokerUnsubscribeHonoursContextCancellationWhileWaiting(t *testing.T) {
	direct := testutil.NewRedis(t)
	client, proxy := proxiedClient(t, direct)
	base := background(t)
	b := pubsub.NewRedisBroker(base, client)
	defer func() { _ = b.Close() }()
	ch := newChannels(1)[0]
	require.NoError(t, b.Subscribe(base, ch))

	proxy.pause()
	ctx, cancel := context.WithCancel(base)
	call := async(func() error { return b.Unsubscribe(ctx, ch) })
	time.Sleep(50 * time.Millisecond)
	cancel()

	require.ErrorIs(t, call.result(t), context.Canceled)

	proxy.resume()
	require.NoError(t, completes(t, func() error { return b.Subscribe(base, ch) }))
	require.NoError(t, completes(t, func() error { return b.Unsubscribe(base, ch) }))
}

func TestRedisBrokerUnsubscribeHonoursContextDeadlineWhileWaiting(t *testing.T) {
	direct := testutil.NewRedis(t)
	client, proxy := proxiedClient(t, direct)
	base := background(t)
	b := pubsub.NewRedisBroker(base, client)
	defer func() { _ = b.Close() }()
	ch := newChannels(1)[0]
	require.NoError(t, b.Subscribe(base, ch))

	proxy.pause()
	ctx, cancel := context.WithTimeout(base, 150*time.Millisecond)
	defer cancel()
	err := completes(t, func() error { return b.Unsubscribe(ctx, ch) })

	require.ErrorIs(t, err, context.DeadlineExceeded)
	proxy.resume()
	require.NoError(t, completes(t, func() error { return b.Unsubscribe(base, ch) }))
}

func TestRedisBrokerUnsubscribeWithCanceledContextFailsFast(t *testing.T) {
	client := testutil.NewRedis(t)
	base := background(t)
	b := pubsub.NewRedisBroker(base, client)
	defer func() { _ = b.Close() }()
	ch := newChannels(1)[0]
	require.NoError(t, b.Subscribe(base, ch))
	ctx, cancel := context.WithCancel(base)
	cancel()

	err := completes(t, func() error { return b.Unsubscribe(ctx, ch) })

	require.Error(t, err)
	require.NoError(t, completes(t, func() error { return b.Unsubscribe(base, ch) }))
}

func TestRedisBrokerCloseReleasesPendingUnsubscribe(t *testing.T) {
	direct := testutil.NewRedis(t)
	client, proxy := proxiedClient(t, direct)
	ctx := background(t)
	b := pubsub.NewRedisBroker(ctx, client)
	ch := newChannels(1)[0]
	require.NoError(t, b.Subscribe(ctx, ch))

	proxy.pause()
	call := async(func() error { return b.Unsubscribe(ctx, ch) })
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, b.Close())

	require.ErrorIs(t, call.result(t), pubsub.ErrClosed)
	proxy.resume()
}

func TestRedisBrokerCloseReleasesPendingSubscribe(t *testing.T) {
	direct := testutil.NewRedis(t)
	client, proxy := proxiedClient(t, direct)
	ctx := background(t)
	b := pubsub.NewRedisBroker(ctx, client)
	require.NoError(t, b.Subscribe(ctx, newChannels(1)[0]))

	proxy.pause()
	call := async(func() error { return b.Subscribe(ctx, newChannels(1)[0]) })
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, b.Close())

	require.ErrorIs(t, call.result(t), pubsub.ErrClosed)
	proxy.resume()
}

func TestRedisBrokerCloseRacingWithUnsubscribeNeverHangs(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx := background(t)

	for i := range 30 {
		b := pubsub.NewRedisBroker(ctx, client)
		ch := newChannels(1)[0]
		require.NoError(t, b.Subscribe(ctx, ch), i)

		call := async(func() error { return b.Unsubscribe(ctx, ch) })
		go func() { _ = b.Close() }()

		_ = call.result(t)
		_ = b.Close()
	}
}
