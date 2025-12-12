package testserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type Config struct {
	Port          int
	SupportHTTP11 bool
	SupportH2C    bool
}

func (c Config) validate() error {
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("port must be between 0-65535, got %d", c.Port)
	}
	return nil
}

type Server struct {
	cfg      Config
	stats    *Stats
	sampler  *BandwidthSampler
	renderer *Renderer

	mu       sync.RWMutex
	server   *http.Server
	listener net.Listener
	port     int

	ready chan struct{}
}

func New(cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	stats := NewStats()
	sampler := NewBandwidthSampler(stats, 500*time.Millisecond, 10*time.Second)
	renderer := NewRenderer(stats, sampler, 500*time.Millisecond)

	srv := &Server{
		cfg:      cfg,
		stats:    stats,
		sampler:  sampler,
		renderer: renderer,
		ready:    make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/upload", srv.handleUpload)

	handler := http.Handler(mux)
	if !cfg.SupportHTTP11 {
		handler = requireHTTP2(handler)
	}
	if cfg.SupportH2C {
		handler = h2c.NewHandler(handler, &http2.Server{})
	}

	srv.server = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}

	return srv, nil
}

func (s *Server) Stats() *Stats {
	return s.stats
}

func (s *Server) Sampler() *BandwidthSampler {
	return s.sampler
}

func (s *Server) Renderer() *Renderer {
	return s.renderer
}

func (s *Server) Ready() <-chan struct{} {
	return s.ready
}

func (s *Server) Port() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.port
}

func (s *Server) Address() string {
	port := s.Port()
	if port == 0 {
		return ""
	}
	return fmt.Sprintf("127.0.0.1:%d", port)
}

func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", s.cfg.Port))
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.listener = ln
	s.port = ln.Addr().(*net.TCPAddr).Port
	s.mu.Unlock()

	select {
	case <-s.ready:
	default:
		close(s.ready)
	}

	samplerCtx, samplerCancel := context.WithCancel(ctx)
	go s.sampler.Start(samplerCtx.Done())

	rendererCtx, rendererCancel := context.WithCancel(ctx)
	go s.renderer.Start(rendererCtx.Done())

	errCh := make(chan error, 1)
	go func() {
		if err := s.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		samplerCancel()
		rendererCancel()
		s.shutdown()
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
		return ctx.Err()
	case err := <-errCh:
		samplerCancel()
		rendererCancel()
		return err
	}
}

func (s *Server) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.server.Shutdown(ctx)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, indexHTML)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	reader, err := r.MultipartReader()
	if err != nil {
		http.Error(w, fmt.Sprintf("create multipart reader: %v", err), http.StatusBadRequest)
		return
	}

	start := time.Now()
	var total uint64
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("read multipart: %v", err), http.StatusBadRequest)
			return
		}
		n, err := io.Copy(io.Discard, part)
		part.Close()
		if err != nil {
			http.Error(w, fmt.Sprintf("read part: %v", err), http.StatusBadRequest)
			return
		}
		total += uint64(n)
	}

	duration := time.Since(start)
	completedAt := time.Now()
	s.stats.RecordUpload(total, duration, completedAt)
	s.sampler.Record(completedAt)

	w.Header().Set("Content-Type", "application/json")
	response := map[string]any{
		"bytes":       total,
		"duration_ms": duration.Seconds() * 1000,
	}
	_ = json.NewEncoder(w).Encode(response)
}

func requireHTTP2(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor < 2 {
			http.Error(w, "HTTP/2 required", http.StatusHTTPVersionNotSupported)
			return
		}
		next.ServeHTTP(w, r)
	})
}

const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Upload Test Server</title>
</head>
<body>
  <h1>Upload Test Server</h1>
  <form method="post" action="/upload" enctype="multipart/form-data">
    <label for="file">Choose file:</label>
    <input type="file" id="file" name="file" required>
    <button type="submit">Upload</button>
  </form>
</body>
</html>`
