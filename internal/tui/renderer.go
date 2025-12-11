package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/metrics"
	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/pool"
)

type Renderer struct {
	sampler *metrics.Sampler
	pool    *pool.Pool
	refresh time.Duration
	out     io.Writer
}

func NewRenderer(sampler *metrics.Sampler, p *pool.Pool, refresh time.Duration) *Renderer {
	return &Renderer{
		sampler: sampler,
		pool:    p,
		refresh: refresh,
		out:     os.Stdout,
	}
}

func (r *Renderer) SetOutput(out io.Writer) {
	if out == nil {
		return
	}
	r.out = out
}

func (r *Renderer) Start(ctx context.Context) {
	ticker := time.NewTicker(r.refresh)
	defer ticker.Stop()

	r.render()

	for {
		select {
		case <-ctx.Done():
			r.clear()
			return
		case <-ticker.C:
			r.render()
		}
	}
}

func (r *Renderer) render() {
	stats := r.sampler.Window()
	sessions := r.pool.States()

	fmt.Fprintf(r.out, "\033[H\033[2J")
	fmt.Fprintf(r.out, "cwnd-warm-up-proxy v0.1\n")
	fmt.Fprintf(r.out, "Window: %0.1fs (%d samples)\n", stats.Duration.Seconds(), stats.SampleCount)
	fmt.Fprintf(r.out, "Estimated bandwidth: %s\n", formatMbps(stats.EstimatedBandwidthBps()))
	fmt.Fprintf(r.out, "Dummy Tx: %s  Real Tx: %s  Real Rx: %s\n",
		formatBytesRate(stats.DummyTxRate()),
		formatBytesRate(stats.RealTxRate()),
		formatBytesRate(stats.RealRxRate()),
	)
	fmt.Fprintf(r.out, "Sessions:\n")
	for _, sess := range sessions {
		status := "active"
		if sess.Paused {
			status = "paused"
		} else if sess.Phase == pool.WarmupPhaseError {
			status = "error"
		} else if sess.Phase == pool.WarmupPhaseReconnecting {
			status = "reconnecting"
		}
		fmt.Fprintf(r.out, "  [%d] phase=%s status=%s connected=%t target=%d bps",
			sess.ID, sess.Phase, status, sess.Connected, sess.TargetBPS)
		if sess.LastError != "" {
			fmt.Fprintf(r.out, " error=%s", sess.LastError)
		}
		fmt.Fprintln(r.out)
	}
}

func (r *Renderer) clear() {
	fmt.Fprintf(r.out, "\033[H\033[2J")
}

func formatBytesRate(rate float64) string {
	units := []string{"B/s", "KB/s", "MB/s", "GB/s", "TB/s"}
	value := rate
	idx := 0
	for value >= 1024 && idx < len(units)-1 {
		value /= 1024
		idx++
	}
	return fmt.Sprintf("%0.2f %s", value, units[idx])
}

func formatMbps(bps float64) string {
	if bps <= 0 {
		return "0.00 Mbps"
	}
	return fmt.Sprintf("%0.2f Mbps", bps/1_000_000)
}
