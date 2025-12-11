package pool

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/config"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/metrics"
)

func TestStreamWarmupPauseResume(t *testing.T) {
	cfg := loadTestConfig(t)

	session := &Session{
		id:        0,
		cfg:       cfg,
		counters:  &metrics.Counters{},
		controlCh: make(chan controlMessage, 16),
	}
	session.state.Store(SessionState{ID: 0})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw := io.Pipe()
	var written atomic.Int64

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				written.Add(int64(n))
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		if err := session.streamWarmup(ctx, pw); err != nil {
			t.Logf("streamWarmup exited with %v", err)
		}
		pw.Close()
	}()

	time.Sleep(150 * time.Millisecond)
	first := written.Load()
	if first == 0 {
		t.Fatal("expected warm-up to produce bytes before pause")
	}

	session.controlCh <- controlMessage{typ: controlPause}
	time.Sleep(150 * time.Millisecond)
	pausedTotal := written.Load()
	if pausedTotal-first > warmupChunkSize {
		t.Fatalf("expected warm-up pause to stop writes, delta=%d", pausedTotal-first)
	}

	session.controlCh <- controlMessage{typ: controlResume}
	time.Sleep(200 * time.Millisecond)
	resumed := written.Load()
	if resumed <= pausedTotal {
		t.Fatal("expected warm-up to resume after resume signal")
	}

	session.controlCh <- controlMessage{typ: controlStop}
	cancel()
	<-done
}

func TestBeginEndRealTraffic(t *testing.T) {
	session := &Session{controlCh: make(chan controlMessage, 4)}

	session.beginReal()
	msg := <-session.controlCh
	if msg.typ != controlPause {
		t.Fatalf("expected pause message, got %v", msg.typ)
	}

	session.beginReal()
	select {
	case msg = <-session.controlCh:
		t.Fatalf("unexpected message on second begin: %v", msg.typ)
	default:
	}

	session.endReal()
	select {
	case msg = <-session.controlCh:
		t.Fatalf("unexpected resume while requests still active: %v", msg.typ)
	default:
	}

	session.endReal()
	msg = <-session.controlCh
	if msg.typ != controlResume {
		t.Fatalf("expected resume message, got %v", msg.typ)
	}
}

func TestWarmupStreamErrorRetriesWithoutReconnect(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Pool.PerConnectionDwellMS = 0
	cfg.Pool.WarmUpIntervalMS = 10
	cfg.Pool.WarmUpSizeBytes = warmupChunkSize
	counters := &metrics.Counters{}

	streamErrSeen := make(chan struct{}, 1)
	secondCall := make(chan struct{}, 1)

	client := &scriptedClient{
		handler: func(call int, req *http.Request) (*http.Response, error) {
			if call == 1 {
				_, _ = io.CopyN(io.Discard, req.Body, int64(cfg.Pool.WarmUpSizeBytes))
				req.Body.Close()
				select {
				case streamErrSeen <- struct{}{}:
				default:
				}
				return nil, &http2.StreamError{Code: http2.ErrCodeCancel}
			}
			go io.Copy(io.Discard, req.Body)
			select {
			case secondCall <- struct{}{}:
			default:
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
			}, nil
		},
	}

	var dialCalls atomic.Int32
	session := &Session{
		id:        1,
		cfg:       cfg,
		counters:  counters,
		controlCh: make(chan controlMessage, 16),
		dial: func(context.Context) (*dialResult, error) {
			dialCalls.Add(1)
			return &dialResult{rawConn: &fakeNetConn{}, client: client}, nil
		},
	}
	session.state.Store(SessionState{ID: 1, TargetBPS: cfg.PerConnectionTargetBPS()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := session.Start(ctx); err != nil {
		t.Fatalf("start session: %v", err)
	}

	select {
	case <-streamErrSeen:
	case <-time.After(1 * time.Second):
		t.Fatal("expected stream error on first warm-up attempt")
	}

	select {
	case <-secondCall:
	case <-time.After(1 * time.Second):
		t.Fatal("expected warm-up retry without reconnect")
	}

	if got := dialCalls.Load(); got != 1 {
		t.Fatalf("expected single dial, got %d", got)
	}

	session.controlCh <- controlMessage{typ: controlStop}
	cancel()
	session.Close()
}

func TestWarmupConnectionErrorTriggersReconnect(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Pool.PerConnectionDwellMS = 0
	cfg.Pool.WarmUpIntervalMS = 10
	cfg.Pool.WarmUpSizeBytes = warmupChunkSize
	counters := &metrics.Counters{}

	firstCall := make(chan struct{}, 1)
	secondClientCall := make(chan struct{}, 1)

	client1 := &scriptedClient{
		handler: func(call int, req *http.Request) (*http.Response, error) {
			_, _ = io.CopyN(io.Discard, req.Body, int64(cfg.Pool.WarmUpSizeBytes))
			req.Body.Close()
			select {
			case firstCall <- struct{}{}:
			default:
			}
			return nil, io.EOF
		},
	}

	client2 := &scriptedClient{
		handler: func(call int, req *http.Request) (*http.Response, error) {
			if call == 1 {
				select {
				case secondClientCall <- struct{}{}:
				default:
				}
			}
			go io.Copy(io.Discard, req.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
			}, nil
		},
	}

	var dialCalls atomic.Int32
	clients := []*scriptedClient{client1, client2}
	var mu sync.Mutex
	session := &Session{
		id:        2,
		cfg:       cfg,
		counters:  counters,
		controlCh: make(chan controlMessage, 16),
		dial: func(context.Context) (*dialResult, error) {
			call := int(dialCalls.Load())
			if call >= len(clients) {
				call = len(clients) - 1
			}
			dialCalls.Add(1)
			mu.Lock()
			client := clients[call]
			mu.Unlock()
			return &dialResult{rawConn: &fakeNetConn{}, client: client}, nil
		},
	}
	session.state.Store(SessionState{ID: 2, TargetBPS: cfg.PerConnectionTargetBPS()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := session.Start(ctx); err != nil {
		t.Fatalf("start session: %v", err)
	}

	select {
	case <-firstCall:
	case <-time.After(1 * time.Second):
		t.Fatal("expected initial warm-up attempt")
	}

	select {
	case <-secondClientCall:
	case <-time.After(2 * time.Second):
		t.Fatal("expected reconnect to second client")
	}

	if got := dialCalls.Load(); got < 2 {
		t.Fatalf("expected at least two dials, got %d", got)
	}

	if client1.closeCount.Load() == 0 {
		t.Fatal("expected first client to be closed on reconnect")
	}

	session.controlCh <- controlMessage{typ: controlStop}
	cancel()
	session.Close()
}

func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfgContent := `{
        "target": {
            "host": "example.com",
            "port": 443,
            "protocol": "http2",
            "tls": true
        },
        "pool": {
            "pool_size": 1,
            "bandwidth_mbps": 10,
            "warm_up_interval_ms": 200,
            "warm_up_size_bytes": 1024,
            "warmup_path": "/warm",
            "warmup_method": "POST",
            "per_connection_dwell_ms": 100
        },
        "server": {
            "port": 8080
        }
    }`

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(cfgContent), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

type scriptedClient struct {
	handler    func(int, *http.Request) (*http.Response, error)
	calls      atomic.Int32
	closeCount atomic.Int32
}

func (c *scriptedClient) RoundTrip(req *http.Request) (*http.Response, error) {
	call := int(c.calls.Add(1))
	if c.handler == nil {
		return nil, io.EOF
	}
	return c.handler(call, req)
}

func (c *scriptedClient) Close() error {
	c.closeCount.Add(1)
	return nil
}

type fakeNetConn struct {
	closed atomic.Bool
}

func (c *fakeNetConn) Read(b []byte) (int, error)  { return 0, io.EOF }
func (c *fakeNetConn) Write(b []byte) (int, error) { return len(b), nil }
func (c *fakeNetConn) Close() error {
	c.closed.Store(true)
	return nil
}
func (c *fakeNetConn) LocalAddr() net.Addr              { return fakeAddr("local") }
func (c *fakeNetConn) RemoteAddr() net.Addr             { return fakeAddr("remote") }
func (c *fakeNetConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeNetConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeNetConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr string

func (a fakeAddr) Network() string { return string(a) }
func (a fakeAddr) String() string  { return string(a) }
