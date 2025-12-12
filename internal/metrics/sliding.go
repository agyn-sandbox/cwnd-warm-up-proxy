package metrics

import (
	"sync"
	"time"
)

// SlidingCounter tracks values over a fixed time window using a ring buffer of
// per-bucket totals.
type SlidingCounter struct {
	window time.Duration
	step   time.Duration

	mu      sync.Mutex
	buckets []bucket
	index   int
	last    time.Time
}

type bucket struct {
	stamp time.Time
	value int64
}

func NewSlidingCounter(window, step time.Duration) *SlidingCounter {
	if step <= 0 {
		step = 500 * time.Millisecond
	}
	bucketCount := int(window / step)
	if bucketCount <= 0 {
		bucketCount = 1
	}
	return &SlidingCounter{
		window:  window,
		step:    step,
		buckets: make([]bucket, bucketCount),
	}
}

func (s *SlidingCounter) Add(value int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roll(time.Now())
	s.buckets[s.index].value += value
	s.buckets[s.index].stamp = time.Now()
}

func (s *SlidingCounter) Sum() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roll(time.Now())
	var total int64
	cutoff := time.Now().Add(-s.window)
	for _, b := range s.buckets {
		if b.stamp.After(cutoff) {
			total += b.value
		}
	}
	return total
}

func (s *SlidingCounter) roll(now time.Time) {
	if s.last.IsZero() {
		s.last = now
		return
	}
	elapsed := now.Sub(s.last)
	if elapsed < s.step {
		return
	}
	steps := int(elapsed / s.step)
	for i := 0; i < steps; i++ {
		s.index = (s.index + 1) % len(s.buckets)
		s.buckets[s.index] = bucket{}
	}
	s.last = now
}
