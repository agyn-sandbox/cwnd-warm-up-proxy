package overlay

import (
	"sync"
	"time"
)

// Scheduler chooses the next subflow to transmit on. We keep a simple weighted
// round-robin that can adapt weights based on recent throughput.

type Scheduler struct {
	mu       sync.Mutex
	entries  []*scheduleEntry
	position int
}

type scheduleEntry struct {
	ref    *Subflow
	weight int
	quota  int
}

func NewScheduler() *Scheduler {
	return &Scheduler{}
}

func (s *Scheduler) Attach(subflow *Subflow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := &scheduleEntry{ref: subflow, weight: 1, quota: 1}
	s.entries = append(s.entries, entry)
}

func (s *Scheduler) Detach(subflowID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := s.entries[:0]
	for _, e := range s.entries {
		if e.ref.ID != subflowID {
			filtered = append(filtered, e)
		}
	}
	s.entries = filtered
	if s.position >= len(s.entries) {
		s.position = 0
	}
}

const (
	minWeight     = 1
	maxWeight     = 8
	weightQuantum = 64 << 10 // bytes per second step
	goodputWindow = 10 * time.Second
)

func (s *Scheduler) refreshWeightsLocked() {
	for _, entry := range s.entries {
		throughput := entry.ref.tx.Sum()
		weight := weightFromThroughput(throughput)
		if weight != entry.weight {
			entry.weight = weight
			entry.quota = weight
		} else if entry.quota <= 0 {
			entry.quota = entry.weight
		}
	}
}

func (s *Scheduler) Next() *Subflow {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) == 0 {
		return nil
	}
	s.refreshWeightsLocked()
	for range s.entries {
		entry := s.entries[s.position]
		if entry.quota == 0 {
			entry.quota = entry.weight
			s.position = (s.position + 1) % len(s.entries)
			continue
		}
		entry.quota--
		if entry.quota == 0 {
			s.position = (s.position + 1) % len(s.entries)
		}
		return entry.ref
	}
	return s.entries[0].ref
}

func weightFromThroughput(bytes int64) int {
	if bytes <= 0 {
		return minWeight
	}
	rate := float64(bytes) / goodputWindow.Seconds()
	weight := int(rate/float64(weightQuantum)) + 1
	if weight < minWeight {
		return minWeight
	}
	if weight > maxWeight {
		return maxWeight
	}
	return weight
}
