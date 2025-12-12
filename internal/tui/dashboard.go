package tui

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Snapshot holds aggregated runtime metrics exposed by the overlay.
type Snapshot struct {
	Timestamp time.Time
	Subflows  []SubflowStat
	Streams   []StreamStat
}

type SubflowStat struct {
	ID         int
	RTT        time.Duration
	Throughput float64 // bytes/sec
}

type StreamStat struct {
	ID          uint32
	Outstanding uint32
	Throughput  float64
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
			fmt.Fprintf(d.Writer, "\n[%s] Subflows:%d Streams:%d\n", snap.Timestamp.Format(time.RFC3339), len(snap.Subflows), len(snap.Streams))
			for _, sf := range snap.Subflows {
				fmt.Fprintf(d.Writer, "  sf=%d rtt=%s throughput=%.2fBps\n", sf.ID, sf.RTT, sf.Throughput)
			}
			for _, st := range snap.Streams {
				fmt.Fprintf(d.Writer, "  stream=%d outstanding=%d throughput=%.2fBps\n", st.ID, st.Outstanding, st.Throughput)
			}
		case <-d.stop:
			return
		}
	}
}

func (d *Dashboard) Stop() {
	d.once.Do(func() { close(d.stop) })
}
