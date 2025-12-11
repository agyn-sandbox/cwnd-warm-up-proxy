package metrics

import (
	"sync"
	"time"
)

type sample struct {
	ts       time.Time
	snapshot Snapshot
}

type Sampler struct {
	counters *Counters
	interval time.Duration
	window   time.Duration

	mu      sync.Mutex
	samples []sample
}

func NewSampler(counters *Counters, interval, window time.Duration) *Sampler {
	return &Sampler{
		counters: counters,
		interval: interval,
		window:   window,
		samples:  make([]sample, 0, int(window/interval)+2),
	}
}

func (s *Sampler) Start(stop <-chan struct{}) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	s.record(time.Now())

	for {
		select {
		case <-stop:
			return
		case t := <-ticker.C:
			s.record(t)
		}
	}
}

func (s *Sampler) record(now time.Time) {
	snap := s.counters.Snapshot()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.samples = append(s.samples, sample{ts: now, snapshot: snap})

	cutoff := now.Add(-s.window)
	idx := 0
	for idx < len(s.samples)-1 && s.samples[idx].ts.Before(cutoff) {
		idx++
	}
	if idx > 0 {
		s.samples = s.samples[idx:]
	}
}

type WindowStats struct {
	Duration     time.Duration
	SampleCount  int
	DummyTxBytes uint64
	DummyRxBytes uint64
	RealTxBytes  uint64
	RealRxBytes  uint64
}

func (s *Sampler) Window() WindowStats {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.samples) < 2 {
		return WindowStats{}
	}

	first := s.samples[0]
	last := s.samples[len(s.samples)-1]

	duration := last.ts.Sub(first.ts)
	if duration <= 0 {
		duration = s.interval
	}

	return WindowStats{
		Duration:     duration,
		SampleCount:  len(s.samples),
		DummyTxBytes: last.snapshot.DummyTx - first.snapshot.DummyTx,
		DummyRxBytes: last.snapshot.DummyRx - first.snapshot.DummyRx,
		RealTxBytes:  last.snapshot.RealTx - first.snapshot.RealTx,
		RealRxBytes:  last.snapshot.RealRx - first.snapshot.RealRx,
	}
}

func (w WindowStats) DummyTxRate() float64 {
	if w.Duration <= 0 {
		return 0
	}
	return float64(w.DummyTxBytes) / w.Duration.Seconds()
}

func (w WindowStats) RealTxRate() float64 {
	if w.Duration <= 0 {
		return 0
	}
	return float64(w.RealTxBytes) / w.Duration.Seconds()
}

func (w WindowStats) RealRxRate() float64 {
	if w.Duration <= 0 {
		return 0
	}
	return float64(w.RealRxBytes) / w.Duration.Seconds()
}

func (w WindowStats) EstimatedBandwidthBps() float64 {
	if w.Duration <= 0 {
		return 0
	}
	total := float64(w.DummyTxBytes+w.RealTxBytes) * 8
	return total / w.Duration.Seconds()
}
