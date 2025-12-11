package tui

import "testing"

func TestFormatBytesRate(t *testing.T) {
	if out := formatBytesRate(1024); out != "1.00 KB/s" {
		t.Fatalf("unexpected format: %s", out)
	}
	if out := formatBytesRate(0); out != "0.00 B/s" {
		t.Fatalf("expected zero rate formatting, got %s", out)
	}
}

func TestFormatMbps(t *testing.T) {
	if out := formatMbps(20_000_000); out != "20.00 Mbps" {
		t.Fatalf("unexpected mbps format: %s", out)
	}
}
