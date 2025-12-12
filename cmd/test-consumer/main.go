package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/metrics"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/tui"
)

const (
	defaultPayloadSize = 5 * 1024 * 1024
)

type consumerMetrics struct {
	window time.Duration
	step   time.Duration

	txReal  *metrics.SlidingCounter
	txDummy *metrics.SlidingCounter

	totalReal  atomic.Int64
	totalDummy atomic.Int64

	mu           sync.Mutex
	start        time.Time
	mode         string
	target       string
	totalBytes   int64
	lastResp     uploadResponse
	lastDuration time.Duration
	err          error
	done         bool
}

func newConsumerMetrics(mode, target string) *consumerMetrics {
	window := 10 * time.Second
	step := 500 * time.Millisecond
	return &consumerMetrics{
		window:  window,
		step:    step,
		txReal:  metrics.NewSlidingCounter(window, step),
		txDummy: metrics.NewSlidingCounter(window, step),
		mode:    mode,
		target:  target,
	}
}

func (m *consumerMetrics) Start(total int64) {
	m.mu.Lock()
	if m.start.IsZero() {
		m.start = time.Now()
		m.totalBytes = total
	}
	m.mu.Unlock()
}

func (m *consumerMetrics) WrapReader(r io.Reader) io.Reader {
	return io.TeeReader(r, &metricsRecorder{metrics: m})
}

func (m *consumerMetrics) recordReal(n int) {
	if n <= 0 {
		return
	}
	m.txReal.Add(int64(n))
	m.totalReal.Add(int64(n))
}

func (m *consumerMetrics) recordDummy(n int) {
	if n <= 0 {
		return
	}
	m.txDummy.Add(int64(n))
	m.totalDummy.Add(int64(n))
}

func (m *consumerMetrics) RecordError(err error) {
	m.mu.Lock()
	m.err = err
	m.done = true
	m.mu.Unlock()
}

func (m *consumerMetrics) RecordResult(resp uploadResponse, elapsed time.Duration) {
	m.mu.Lock()
	m.lastResp = resp
	m.lastDuration = elapsed
	m.done = true
	m.mu.Unlock()
}

func (m *consumerMetrics) Snapshot() tui.Snapshot {
	windowSeconds := m.window.Seconds()
	if windowSeconds <= 0 {
		windowSeconds = 1
	}
	mode, target, start, totalBytes, lastResp, lastDuration, err, done := m.snapshotState()
	realTotal := m.totalReal.Load()
	dummyTotal := m.totalDummy.Load()
	realRate := float64(m.txReal.Sum()) / windowSeconds
	dummyRate := float64(m.txDummy.Sum()) / windowSeconds
	var elapsed time.Duration
	if !start.IsZero() {
		elapsed = time.Since(start)
	}
	avgRate := 0.0
	if elapsed > 0 {
		avgRate = float64(realTotal) / elapsed.Seconds()
	}
	progress := "-"
	if totalBytes > 0 {
		ratio := float64(realTotal) / float64(totalBytes)
		if ratio > 1 {
			ratio = 1
		}
		progress = fmt.Sprintf("%.1f%%", ratio*100)
	}
	lines := []string{
		fmt.Sprintf("mode=%s target=%s", mode, target),
		fmt.Sprintf("elapsed=%s avg_real=%s progress=%s", tui.FormatDuration(elapsed), tui.FormatRate(avgRate), progress),
		fmt.Sprintf("tx real: rate=%s total=%s", tui.FormatRate(realRate), tui.FormatBytes(realTotal)),
		fmt.Sprintf("tx dummy: rate=%s total=%s", tui.FormatRate(dummyRate), tui.FormatBytes(dummyTotal)),
	}
	if done {
		lines = append(lines, fmt.Sprintf("last upload: bytes=%d duration=%s server_time=%dms", lastResp.BytesReceived, tui.FormatDuration(lastDuration), lastResp.ServerTimeMS))
	}
	if err != nil {
		lines = append(lines, fmt.Sprintf("error: %v", err))
	}
	if !done && start.IsZero() {
		lines = append(lines, "status=waiting for upload")
	}
	return tui.Snapshot{
		Timestamp: time.Now(),
		Title:     "test-consumer",
		Lines:     lines,
	}
}

func (m *consumerMetrics) snapshotState() (mode, target string, start time.Time, total int64, last uploadResponse, lastDur time.Duration, err error, done bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode, m.target, m.start, m.totalBytes, m.lastResp, m.lastDuration, m.err, m.done
}

type metricsRecorder struct {
	metrics *consumerMetrics
}

func (r *metricsRecorder) Write(p []byte) (int, error) {
	if len(p) > 0 {
		r.metrics.recordReal(len(p))
	}
	return len(p), nil
}

type consumerFlags struct {
	target    string
	filePath  string
	useSocks  bool
	socksAddr string
	timeout   time.Duration
	showTUI   bool
}

func parseFlags() consumerFlags {
	var cfg consumerFlags
	flag.StringVar(&cfg.target, "target", "http://127.0.0.1:9000/upload", "target upload URL")
	flag.StringVar(&cfg.filePath, "file", "", "path to payload file (optional)")
	flag.BoolVar(&cfg.useSocks, "use-socks", false, "route traffic through SOCKS5 proxy")
	flag.StringVar(&cfg.socksAddr, "socks", "127.0.0.1:1080", "SOCKS5 proxy address")
	flag.DurationVar(&cfg.timeout, "timeout", 60*time.Second, "request timeout")
	flag.BoolVar(&cfg.showTUI, "tui", true, "render live metrics dashboard")
	flag.Parse()
	return cfg
}

type uploadResponse struct {
	BytesReceived int64 `json:"bytes_received"`
	ServerTimeMS  int64 `json:"server_time_ms"`
}

func main() {
	cfg := parseFlags()
	payloadFile, err := ensurePayload(cfg.filePath)
	if err != nil {
		log.Fatalf("prepare payload: %v", err)
	}
	if cfg.filePath == "" {
		log.Printf("generated payload file: %s", payloadFile)
	}
	client, err := newHTTPClient(cfg)
	if err != nil {
		log.Fatalf("http client: %v", err)
	}
	mode := "direct"
	if cfg.useSocks {
		mode = fmt.Sprintf("SOCKS %s", cfg.socksAddr)
	}

	var dashboard *tui.Dashboard
	var meter *consumerMetrics
	if cfg.showTUI {
		meter = newConsumerMetrics(mode, cfg.target)
		dashboard = tui.NewDashboard(meter, os.Stdout, 500*time.Millisecond)
		dashboard.Start()
		defer dashboard.Stop()
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	resp, elapsed, err := performUpload(ctx, client, cfg.target, payloadFile, meter)
	if err != nil {
		if meter != nil {
			meter.RecordError(err)
		}
		log.Fatalf("upload failed: %v", err)
	}
	if meter != nil {
		meter.RecordResult(resp, elapsed)
	}
	log.Printf("mode: %s", mode)
	log.Printf("client_time_ms=%d", elapsed.Milliseconds())
	if elapsed > 0 {
		throughput := float64(resp.BytesReceived) / (1024 * 1024) / elapsed.Seconds()
		log.Printf("throughput_mibps=%.2f", throughput)
	}
	log.Printf("server_bytes=%d server_time_ms=%d", resp.BytesReceived, resp.ServerTimeMS)
}

func ensurePayload(path string) (string, error) {
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if errors.Is(err, os.ErrNotExist) {
			if err := writePayloadFile(path); err != nil {
				return "", err
			}
			return path, nil
		} else {
			return "", err
		}
	}
	name := fmt.Sprintf("test-consumer-%d.txt", time.Now().UnixNano())
	tmpPath := filepath.Join(os.TempDir(), name)
	if err := writePayloadFile(tmpPath); err != nil {
		return "", err
	}
	return tmpPath, nil
}

func writePayloadFile(path string) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	pattern := []byte("The quick brown fox jumps over the lazy dog. ")
	written := 0
	for written < defaultPayloadSize {
		remain := defaultPayloadSize - written
		chunk := pattern
		if remain < len(pattern) {
			chunk = pattern[:remain]
		}
		if _, err := file.Write(chunk); err != nil {
			return err
		}
		written += len(chunk)
	}
	return file.Sync()
}

func newHTTPClient(cfg consumerFlags) (*http.Client, error) {
	transport := &http.Transport{
		DisableKeepAlives: true,
	}
	if cfg.useSocks {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialViaSocks(ctx, network, cfg.socksAddr, addr)
		}
	}
	return &http.Client{Transport: transport}, nil
}

func performUpload(ctx context.Context, client *http.Client, target, filePath string, meter *consumerMetrics) (uploadResponse, time.Duration, error) {
	var out uploadResponse
	file, err := os.Open(filePath)
	if err != nil {
		return out, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return out, 0, err
	}
	body := io.Reader(file)
	if meter != nil {
		meter.Start(info.Size())
		body = meter.WrapReader(file)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err != nil {
		return out, 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = info.Size()
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return out, 0, err
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return out, elapsed, fmt.Errorf("unexpected status %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, elapsed, err
	}
	return out, elapsed, nil
}

func dialViaSocks(ctx context.Context, network, socksAddr, destAddr string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, socksAddr)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()
	if err := sendSocksGreeting(conn); err != nil {
		return nil, err
	}
	if err := requestSocksConnect(conn, destAddr); err != nil {
		return nil, err
	}
	success = true
	return conn, nil
}

func sendSocksGreeting(rw io.ReadWriter) error {
	if _, err := rw.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return err
	}
	var resp [2]byte
	if _, err := io.ReadFull(rw, resp[:]); err != nil {
		return err
	}
	if resp[0] != 0x05 {
		return errors.New("socks: unexpected version")
	}
	if resp[1] != 0x00 {
		return errors.New("socks: authentication required")
	}
	return nil
}

func requestSocksConnect(conn net.Conn, dest string) error {
	host, portStr, err := net.SplitHostPort(dest)
	if err != nil {
		return err
	}
	port, err := parsePort(portStr)
	if err != nil {
		return err
	}
	buf := []byte{0x05, 0x01, 0x00}
	ip := net.ParseIP(host)
	switch {
	case ip == nil:
		buf = append(buf, 0x03)
		buf = append(buf, byte(len(host)))
		buf = append(buf, host...)
	case ip.To4() != nil:
		buf = append(buf, 0x01)
		buf = append(buf, ip.To4()...)
	default:
		buf = append(buf, 0x04)
		buf = append(buf, ip.To16()...)
	}
	buf = append(buf, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(buf); err != nil {
		return err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return err
	}
	if head[0] != 0x05 {
		return errors.New("socks: unexpected version in reply")
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks: connect failed with code %d", head[1])
	}
	addrType := head[3]
	var discard int
	switch addrType {
	case 0x01:
		discard = 4
	case 0x03:
		var ln [1]byte
		if _, err := io.ReadFull(conn, ln[:]); err != nil {
			return err
		}
		discard = int(ln[0])
	case 0x04:
		discard = 16
	default:
		return errors.New("socks: unknown address type")
	}
	if discard > 0 {
		trash := make([]byte, discard)
		if _, err := io.ReadFull(conn, trash); err != nil {
			return err
		}
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return err
	}
	return nil
}

func parsePort(port string) (uint16, error) {
	if port == "" {
		return 0, errors.New("empty port")
	}
	value, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0, err
	}
	if value == 0 {
		return 0, errors.New("port must be greater than zero")
	}
	return uint16(value), nil
}
