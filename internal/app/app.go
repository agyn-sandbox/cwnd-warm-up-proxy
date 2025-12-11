package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/config"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/metrics"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/pool"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/proxy"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/tui"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/upstream"
)

type App struct {
	cfg      *config.Config
	counters *metrics.Counters
	pool     *pool.Pool
	sampler  *metrics.Sampler
	renderer *tui.Renderer
	server   *http.Server
}

func New(cfg *config.Config) (*App, error) {
	counters := &metrics.Counters{}
	dialer := upstream.NewDialer(cfg)
	connPool, err := pool.New(cfg, dialer, counters)
	if err != nil {
		return nil, err
	}

	sampler := metrics.NewSampler(counters, 100*time.Millisecond, 10*time.Second)
	renderer := tui.NewRenderer(sampler, connPool, 500*time.Millisecond)

	proxyHandler := proxy.NewHandler(cfg, connPool, counters)
	handler := buildInboundHandler(cfg, proxyHandler)

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      handler,
		ReadTimeout:  cfg.ReadTimeout(),
		WriteTimeout: cfg.WriteTimeout(),
		IdleTimeout:  cfg.IdleTimeout(),
	}

	return &App{
		cfg:      cfg,
		counters: counters,
		pool:     connPool,
		sampler:  sampler,
		renderer: renderer,
		server:   server,
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := a.pool.Start(ctx); err != nil {
		return fmt.Errorf("start pool: %w", err)
	}

	errCh := make(chan error, 1)

	go a.sampler.Start(ctx.Done())
	go a.renderer.Start(ctx)

	go func() {
		err := a.server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		a.shutdown()
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
		return ctx.Err()
	case err := <-errCh:
		cancel()
		a.shutdown()
		return err
	}
}

func (a *App) shutdown() {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = a.server.Shutdown(shutdownCtx)
	a.pool.Close()
}

func (a *App) Counters() *metrics.Counters {
	return a.counters
}

func (a *App) SetRendererOutput(w io.Writer) {
	if w == nil {
		return
	}
	if a.renderer != nil {
		a.renderer.SetOutput(w)
	}
}

func buildInboundHandler(cfg *config.Config, base http.Handler) http.Handler {
	handler := base
	if !cfg.SupportHTTP11() {
		handler = requireHTTP2(handler)
	}
	if cfg.SupportH2C() {
		handler = h2c.NewHandler(handler, &http2.Server{})
	}
	return handler
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
