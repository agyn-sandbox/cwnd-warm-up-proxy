package testserver

import (
	"sync"
	"time"
)

type byteSample struct {
	ts    time.Time
	total uint64
}

type BandwidthSampler struct {
	stats    *Stats
	interval time.Duration
	window   time.Duration

	mu      sync.Mutex
	samples []byteSample
}

func NewBandwidthSampler(stats *Stats, interval, window time.Duration) *BandwidthSampler {
	return &BandwidthSampler{
		stats:    stats,
		interval: interval,
		window:   window,
		samples:  make([]byteSample, 0, int(window/interval)+2),
	}
}

func (s *BandwidthSampler) Start(stop <-chan struct{}) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	s.Record(time.Now())

	for {
		select {
		case <-stop:
			return
		case t := <-ticker.C:
			s.Record(t)
		}
	}
}

func (s *BandwidthSampler) Record(now time.Time) {
	total := s.stats.TotalBytes()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.samples = append(s.samples, byteSample{ts: now, total: total})

	cutoff := now.Add(-s.window)
	idx := 0
	for idx < len(s.samples)-1 && s.samples[idx].ts.Before(cutoff) {
		idx++
	}
	if idx > 0 {
		s.samples = s.samples[idx:]
	}
}

type BandwidthWindow struct {
	Duration    time.Duration
	SampleCount int
	Bytes       uint64
}

func (s *BandwidthSampler) Window() BandwidthWindow {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.samples) < 2 {
		return BandwidthWindow{}
	}

	first := s.samples[0]
	last := s.samples[len(s.samples)-1]
	duration := last.ts.Sub(first.ts)
	if duration <= 0 {
		duration = s.interval
	}
	bytes := last.total - first.total
	return BandwidthWindow{
		Duration:    duration,
		SampleCount: len(s.samples),
		Bytes:       bytes,
	}
}

func (w BandwidthWindow) BytesPerSecond() float64 {
	if w.Duration <= 0 {
		return 0
	}
	return float64(w.Bytes) / w.Duration.Seconds()
}

func (w BandwidthWindow) Mbps() float64 {
	return w.BytesPerSecond() * 8 / 1_000_000
}
