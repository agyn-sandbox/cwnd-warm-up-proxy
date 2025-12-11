package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/app"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/config"
)

func TestProxyEndToEnd(t *testing.T) {
	upstream := startUpstream(t)
	defer upstream.Shutdown()

	proxyPort := allocatePort(t)

	cfgPath := writeConfig(t, upstream.host, upstream.port, proxyPort)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	application, err := app.New(cfg)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	application.SetRendererOutput(io.Discard)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- application.Run(ctx)
	}()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()

	waitForProxy(t, client, baseURL+"/status")

	// POST echo request to exercise real traffic and header forwarding.
	req, err := http.NewRequest(http.MethodPost, baseURL+"/echo?msg=hello", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Test", "tester")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Proxy-Connection", "should-strip")
	req.Header.Set("Te", "trailers")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("echo request: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read echo body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from echo, got %d", resp.StatusCode)
	}
	if string(body) != "payload" {
		t.Fatalf("unexpected echo body %q", string(body))
	}

	echoHeaders := upstream.WaitForEchoHeaders(t)
	if got := echoHeaders.Get("X-Test"); got != "tester" {
		t.Fatalf("expected X-Test header preserved, got %q", got)
	}
	if got := echoHeaders.Get("Connection"); got != "" {
		t.Fatalf("expected Connection header stripped, got %q", got)
	}
	if got := echoHeaders.Get("Proxy-Connection"); got != "" {
		t.Fatalf("expected Proxy-Connection stripped, got %q", got)
	}
	if got := echoHeaders.Values("Te"); len(got) != 1 || got[0] != "trailers" {
		t.Fatalf("expected TE header limited to trailers, got %v", got)
	}

	// GET /status via proxy for additional smoke coverage.
	statusResp, err := client.Get(baseURL + "/status")
	if err != nil {
		t.Fatalf("status request: %v", err)
	}
	statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 from status endpoint, got %d", statusResp.StatusCode)
	}

	// Wait for metrics counters to reflect dummy and real traffic.
	waitForCounters(t, application, func(snapshot counterSnapshot) bool {
		return snapshot.DummyTx > 0 && snapshot.RealTx > 0 && snapshot.RealRx > 0
	})

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("app run error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for proxy shutdown")
	}
}

type counterSnapshot struct {
	DummyTx uint64
	DummyRx uint64
	RealTx  uint64
	RealRx  uint64
}

func waitForCounters(t *testing.T, application *app.App, predicate func(counterSnapshot) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap := application.Counters().Snapshot()
		snapshot := counterSnapshot{DummyTx: snap.DummyTx, DummyRx: snap.DummyRx, RealTx: snap.RealTx, RealRx: snap.RealRx}
		if predicate(snapshot) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	snap := application.Counters().Snapshot()
	t.Fatalf("metrics did not reach expected values: %+v", snap)
}

type upstreamServer struct {
	srv         *http.Server
	listener    net.Listener
	host        string
	port        int
	echoHeaders chan http.Header
}

func startUpstream(t *testing.T) *upstreamServer {
	t.Helper()

	echoHeaders := make(chan http.Header, 4)
	mux := http.NewServeMux()
	mux.HandleFunc("/warmup-upload", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		headerCopy := cloneHeader(r.Header)
		select {
		case echoHeaders <- headerCopy:
		default:
		}
		payload, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Upstream", "echo")
		w.WriteHeader(http.StatusOK)
		if len(payload) > 0 {
			w.Write(payload)
		}
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "ok")
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Handler: h2c.NewHandler(mux, &http2.Server{})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			panic(fmt.Sprintf("upstream serve: %v", err))
		}
	}()

	tcpAddr := ln.Addr().(*net.TCPAddr)
	return &upstreamServer{
		srv:         srv,
		listener:    ln,
		host:        tcpAddr.IP.String(),
		port:        tcpAddr.Port,
		echoHeaders: echoHeaders,
	}
}

func (u *upstreamServer) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = u.srv.Shutdown(ctx)
}

func (u *upstreamServer) WaitForEchoHeaders(t *testing.T) http.Header {
	t.Helper()
	select {
	case h := <-u.echoHeaders:
		return h
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for echo headers")
	}
	return nil
}

func cloneHeader(h http.Header) http.Header {
	dup := make(http.Header, len(h))
	for k, v := range h {
		copied := make([]string, len(v))
		copy(copied, v)
		dup[k] = copied
	}
	return dup
}

func allocatePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitForProxy(t *testing.T, client *http.Client, url string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("proxy did not respond at %s", url)
}

func writeConfig(t *testing.T, host string, upstreamPort, proxyPort int) string {
	t.Helper()

	cfg := map[string]any{
		"target": map[string]any{
			"host":     host,
			"port":     upstreamPort,
			"protocol": "http2",
			"tls":      false,
		},
		"pool": map[string]any{
			"pool_size":               1,
			"bandwidth_mbps":          10,
			"warm_up_interval_ms":     50,
			"warm_up_size_bytes":      32768,
			"warmup_path":             "/warmup-upload",
			"warmup_method":           "POST",
			"per_connection_dwell_ms": 0,
			"warm_up_headers": map[string]string{
				"X-Warmup": "true",
			},
		},
		"server": map[string]any{
			"port":             proxyPort,
			"support_http1_1":  true,
			"support_h2c":      true,
			"read_timeout_ms":  0,
			"write_timeout_ms": 0,
			"idle_timeout_ms":  0,
		},
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
