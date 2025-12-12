package tui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Snapshot represents a rendered view of runtime metrics.
type Snapshot struct {
	Timestamp time.Time
	Title     string
	Lines     []string
}

// Provider yields new snapshots when invoked.
type Provider interface {
	Snapshot() Snapshot
}

// Dashboard periodically renders the metrics snapshot to the configured writer.
type Dashboard struct {
	Provider Provider
	Writer   io.Writer
	Interval time.Duration
	stop     chan struct{}
	once     sync.Once
}

func NewDashboard(p Provider, w io.Writer, interval time.Duration) *Dashboard {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	return &Dashboard{
		Provider: p,
		Writer:   w,
		Interval: interval,
		stop:     make(chan struct{}),
	}
}

func (d *Dashboard) Start() {
	go d.loop()
}

func (d *Dashboard) loop() {
	ticker := time.NewTicker(d.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if d.Provider == nil || d.Writer == nil {
				continue
			}
			snap := d.Provider.Snapshot()
			title := snap.Title
			if title == "" {
				title = "metrics"
			}
			fmt.Fprintf(d.Writer, "\n[%s] %s\n", snap.Timestamp.Format(time.RFC3339), title)
			for _, line := range snap.Lines {
				line = strings.TrimRight(line, "\n")
				if line == "" {
					continue
				}
				fmt.Fprintf(d.Writer, "  %s\n", line)
			}
		case <-d.stop:
			return
		}
	}
}

func (d *Dashboard) Stop() {
	d.once.Do(func() { close(d.stop) })
}
