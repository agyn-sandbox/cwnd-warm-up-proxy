package metrics

import (
	"testing"
	"time"
)

func TestSlidingCounter(t *testing.T) {
	counter := NewSlidingCounter(2*time.Second, 100*time.Millisecond)
	counter.Add(10)
	counter.Add(5)
	if got := counter.Sum(); got != 15 {
		t.Fatalf("expected sum 15 got %d", got)
	}
	time.Sleep(3 * time.Second)
	if got := counter.Sum(); got != 0 {
		t.Fatalf("expected sum to roll off, got %d", got)
	}
}
