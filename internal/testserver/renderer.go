package testserver

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"
)

type Renderer struct {
	stats   *Stats
	sampler *BandwidthSampler
	refresh time.Duration
	out     io.Writer
}

func NewRenderer(stats *Stats, sampler *BandwidthSampler, refresh time.Duration) *Renderer {
	return &Renderer{
		stats:   stats,
		sampler: sampler,
		refresh: refresh,
		out:     io.Discard,
	}
}

func (r *Renderer) SetOutput(out io.Writer) {
	if out == nil {
		return
	}
	r.out = out
}

func (r *Renderer) Start(ctxDone <-chan struct{}) {
	ticker := time.NewTicker(r.refresh)
	defer ticker.Stop()

	r.render()
	for {
		select {
		case <-ctxDone:
			return
		case <-ticker.C:
			r.render()
		}
	}
}

func (r *Renderer) render() {
	if r.out == nil {
		return
	}

	snap := r.stats.Snapshot()
	window := r.sampler.Window()
	buf := bytes.NewBuffer(nil)

	fmt.Fprint(buf, "\033[H\033[2J")
	fmt.Fprintln(buf, "Upload Test Server")
	fmt.Fprintln(buf, strings.Repeat("=", 20))
	fmt.Fprintf(buf, "Window: %s (%d samples)\n", window.Duration.Round(time.Millisecond), window.SampleCount)
	fmt.Fprintf(buf, "Total uploads: %d\n", snap.TotalUploads)
	fmt.Fprintf(buf, "Total bytes: %d (%.2f MB)\n", snap.TotalBytes, float64(snap.TotalBytes)/1_000_000)

	rate := window.BytesPerSecond()
	fmt.Fprintf(buf, "Inbound rate: %.2f bytes/s (%.2f Mbps)\n", rate, window.Mbps())

	if snap.LastTime.IsZero() {
		fmt.Fprintln(buf, "Last upload: n/a")
	} else {
		fmt.Fprintf(buf, "Last upload: %d bytes in %s @ %s\n",
			snap.LastSize, snap.LastDuration.Round(time.Millisecond), snap.LastTime.Format(time.RFC3339))
	}

	buf.WriteString("\nPress Ctrl+C to stop.\n")

	_, _ = r.out.Write(buf.Bytes())
}
