package metrics

import (
	"testing"
	"time"
)

func TestWindowStatsRates(t *testing.T) {
	counters := &Counters{}
	sampler := NewSampler(counters, 50*time.Millisecond, 200*time.Millisecond)

	now := time.Now()
	sampler.record(now)

	counters.AddDummyTx(500)
	counters.AddRealTx(1500)
	counters.AddRealRx(1000)

	sampler.record(now.Add(200 * time.Millisecond))

	window := sampler.Window()
	if window.SampleCount < 2 {
		t.Fatalf("expected at least two samples, got %d", window.SampleCount)
	}

	if got, want := int(window.DummyTxBytes), 500; got != want {
		t.Fatalf("dummy bytes mismatch: got %d, want %d", got, want)
	}
	if got, want := int(window.RealTxBytes), 1500; got != want {
		t.Fatalf("real tx bytes mismatch: got %d, want %d", got, want)
	}

	rate := window.RealTxRate()
	if rate <= 0 {
		t.Fatalf("expected positive real tx rate, got %f", rate)
	}

	mbps := window.EstimatedBandwidthBps()
	if mbps <= 0 {
		t.Fatalf("expected positive bandwidth, got %f", mbps)
	}
}
