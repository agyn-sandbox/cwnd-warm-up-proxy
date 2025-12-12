package overlay

import "sync"

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

func (s *Scheduler) Record(subflowID int, bytes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.entries {
		if entry.ref.ID == subflowID {
			entry.weight = clamp(entry.weight+1, 1, 8)
			entry.quota = entry.weight
			break
		}
	}
}

func (s *Scheduler) Next() *Subflow {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) == 0 {
		return nil
	}
	for i := 0; i < len(s.entries); i++ {
		s.position = (s.position + 1) % len(s.entries)
		entry := s.entries[s.position]
		if entry.quota > 0 {
			entry.quota--
			return entry.ref
		}
		entry.quota = entry.weight
	}
	return s.entries[s.position].ref
}

func clamp(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

