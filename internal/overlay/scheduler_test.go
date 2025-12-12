package overlay

import (
	"testing"
	"time"

	"github.com/agyn-sandbox/cwnd-warm-up-proxy/internal/metrics"
)

func TestSchedulerWeightsFollowGoodput(t *testing.T) {
	sched := NewScheduler()
	fast := &Subflow{
		ID: 0,
		tx: metrics.NewSlidingCounter(10*time.Second, 500*time.Millisecond),
	}
	slow := &Subflow{
		ID: 1,
		tx: metrics.NewSlidingCounter(10*time.Second, 500*time.Millisecond),
	}
	fast.tx.Add(int64(weightQuantum) * 10)
	slow.tx.Add(int64(weightQuantum) / 10)

	sched.Attach(fast)
	sched.Attach(slow)

	fastCount := 0
	slowCount := 0
	for i := 0; i < 6; i++ {
		sf := sched.Next()
		if sf == nil {
			t.Fatalf("scheduler returned nil subflow")
		}
		if sf.ID == fast.ID {
			fastCount++
		} else if sf.ID == slow.ID {
			slowCount++
		}
	}
	if fastCount <= slowCount {
		t.Fatalf("expected fast subflow to dominate: fast=%d slow=%d", fastCount, slowCount)
	}
}
