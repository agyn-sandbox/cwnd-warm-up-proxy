package pool

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

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
