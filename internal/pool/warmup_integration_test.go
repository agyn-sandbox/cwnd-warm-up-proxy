package pool

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/config"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/metrics"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/testserver"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/upstream"
)

func TestWarmupDummyTrafficAgainstWarmupUpload(t *testing.T) {
	cfg := testserver.Config{Port: 0, SupportHTTP11: true, SupportH2C: true}
	srv, cancelServer, errCh := startWarmupServer(t, cfg)
	defer stopWarmupServer(t, cancelServer, errCh)

	configPath := writeWarmupConfig(t, srv.Port())
	proxyCfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	counters := &metrics.Counters{}
	dialer := upstream.NewDialer(proxyCfg)
	session := NewSession(0, proxyCfg, dialer, counters)
	defer session.Close()

	sessCtx, cancelSession := context.WithCancel(context.Background())
	defer cancelSession()

	if err := session.Start(sessCtx); err != nil {
		t.Fatalf("start session: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var dummyBytes uint64
	for time.Now().Before(deadline) {
		snapshot := counters.Snapshot()
		dummyBytes = snapshot.DummyTx
		if dummyBytes > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if dummyBytes == 0 {
		t.Fatalf("expected warm-up dummy traffic to be recorded")
	}
}

func startWarmupServer(t *testing.T, cfg testserver.Config) (*testserver.Server, context.CancelFunc, <-chan error) {
	t.Helper()
	srv, err := testserver.New(cfg)
	if err != nil {
		t.Fatalf("new test server: %v", err)
	}
	srv.Renderer().SetOutput(io.Discard)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Run(ctx)
	}()

	select {
	case <-srv.Ready():
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatalf("test server did not become ready in time")
	}

	return srv, cancel, errCh
}

func stopWarmupServer(t *testing.T, cancel context.CancelFunc, errCh <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("test server run error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for test server shutdown")
	}
}

func writeWarmupConfig(t *testing.T, port int) string {
	t.Helper()
	content := fmt.Sprintf(`{
  "target": {
    "host": "127.0.0.1",
    "port": %d,
    "protocol": "http2",
    "tls": false
  },
  "pool": {
    "pool_size": 1,
    "bandwidth_mbps": 10,
    "warm_up_interval_ms": 200,
    "warm_up_size_bytes": 65536,
    "warmup_path": "/warmup-upload",
    "warmup_method": "POST",
    "warm_up_headers": {},
    "per_connection_dwell_ms": 0
  },
  "server": {
    "port": 8080,
    "support_http1_1": true,
    "support_h2c": true
  }
}`, port)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
