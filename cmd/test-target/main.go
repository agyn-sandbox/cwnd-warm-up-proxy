package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/metrics"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/tui"
)

type serverFlags struct {
	listen  string
	showTUI bool
}

func parseFlags() serverFlags {
	var cfg serverFlags
	flag.StringVar(&cfg.listen, "listen", "127.0.0.1:9000", "address to listen on")
	flag.BoolVar(&cfg.showTUI, "tui", true, "render live metrics dashboard")
	flag.Parse()
	return cfg
}

type targetMetrics struct {
	listener string
	window   time.Duration
	step     time.Duration

	rxReal    *metrics.SlidingCounter
	reqWindow *metrics.SlidingCounter

	totalReal    atomic.Int64
	requestTotal atomic.Int64

	mu          sync.Mutex
	lastBytes   int64
	lastAt      time.Time
	lastLatency time.Duration
	err         error
}

func newTargetMetrics(listener string) *targetMetrics {
	window := 10 * time.Second
	step := 500 * time.Millisecond
	return &targetMetrics{
		listener:  listener,
		window:    window,
		step:      step,
		rxReal:    metrics.NewSlidingCounter(window, step),
		reqWindow: metrics.NewSlidingCounter(window, step),
	}
}

func (m *targetMetrics) RecordUpload(bytes int64, latency time.Duration) {
	if m == nil {
		return
	}
	m.rxReal.Add(bytes)
	m.reqWindow.Add(1)
	m.totalReal.Add(bytes)
	m.requestTotal.Add(1)
	m.mu.Lock()
	m.lastBytes = bytes
	m.lastLatency = latency
	m.lastAt = time.Now()
	m.mu.Unlock()
}

func (m *targetMetrics) RecordError(err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.err = err
	m.mu.Unlock()
}

func (m *targetMetrics) Snapshot() tui.Snapshot {
	windowSeconds := m.window.Seconds()
	if windowSeconds <= 0 {
		windowSeconds = 1
	}
	rxRate := float64(m.rxReal.Sum()) / windowSeconds
	reqRate := float64(m.reqWindow.Sum()) / windowSeconds
	totalBytes := m.totalReal.Load()
	totalReq := m.requestTotal.Load()
	lastBytes, lastLatency, lastAt, err := m.lastState()
	lines := []string{
		fmt.Sprintf("listening=%s", m.listener),
		fmt.Sprintf("requests total=%d recent=%.2f/s", totalReq, reqRate),
		fmt.Sprintf("rx real: rate=%s total=%s", tui.FormatRate(rxRate), tui.FormatBytes(totalBytes)),
		fmt.Sprintf("rx dummy: rate=%s total=%s", tui.FormatRate(0), tui.FormatBytes(0)),
	}
	if !lastAt.IsZero() {
		lines = append(lines, fmt.Sprintf(
			"last upload: bytes=%s duration=%s ago=%s",
			tui.FormatBytes(lastBytes),
			tui.FormatDuration(lastLatency),
			tui.FormatDuration(time.Since(lastAt)),
		))
	} else {
		lines = append(lines, "last upload: none yet")
	}
	if err != nil {
		lines = append(lines, fmt.Sprintf("error: %v", err))
	}
	return tui.Snapshot{
		Timestamp: time.Now(),
		Title:     "test-target",
		Lines:     lines,
	}
}

func (m *targetMetrics) lastState() (bytes int64, latency time.Duration, at time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastBytes, m.lastLatency, m.lastAt, m.err
}

func main() {
	cfg := parseFlags()
	metrics := newTargetMetrics(cfg.listen)
	var dashboard *tui.Dashboard
	if cfg.showTUI {
		dashboard = tui.NewDashboard(metrics, os.Stdout, 500*time.Millisecond)
		dashboard.Start()
		defer dashboard.Stop()
	}
	mux := newMux(metrics)
	server := &http.Server{
		Addr:              cfg.listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		log.Printf("received signal %s, shutting down", s)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}()

	log.Printf("test-target listening on %s", cfg.listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		if metrics != nil {
			metrics.RecordError(err)
		}
		log.Fatalf("listen: %v", err)
	}
}

func newMux(metrics *targetMetrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/upload", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleUpload(w, r, metrics)
	}))
	return mux
}

type uploadResponse struct {
	BytesReceived int64 `json:"bytes_received"`
	ServerTimeMS  int64 `json:"server_time_ms"`
}

func handleUpload(w http.ResponseWriter, r *http.Request, metrics *targetMetrics) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()
	defer r.Body.Close()
	bytesRead, err := io.Copy(io.Discard, r.Body)
	if err != nil {
		log.Printf("upload read error: %v", err)
		if metrics != nil {
			metrics.RecordError(err)
		}
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	latency := time.Since(start)
	if metrics != nil {
		metrics.RecordUpload(bytesRead, latency)
	}
	resp := uploadResponse{
		BytesReceived: bytesRead,
		ServerTimeMS:  latency.Milliseconds(),
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("encode response: %v", err)
		if metrics != nil {
			metrics.RecordError(err)
		}
	}
}
