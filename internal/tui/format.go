package tui

import (
	"fmt"
	"time"
)

var units = []struct {
	suffix string
	value  float64
}{
	{"GiB", 1 << 30},
	{"MiB", 1 << 20},
	{"KiB", 1 << 10},
}

// FormatRate renders a bytes-per-second value using human readable units.
func FormatRate(bps float64) string {
	if bps < 0 {
		bps = 0
	}
	for _, unit := range units {
		if bps >= unit.value {
			return fmt.Sprintf("%.2f %s/s", bps/unit.value, unit.suffix)
		}
	}
	return fmt.Sprintf("%.2f B/s", bps)
}

// FormatBytes renders a byte total using human readable units.
func FormatBytes(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}
	value := float64(bytes)
	for _, unit := range units {
		if value >= unit.value {
			return fmt.Sprintf("%.2f %s", value/unit.value, unit.suffix)
		}
	}
	return fmt.Sprintf("%d B", bytes)
}

// FormatDuration renders durations with millisecond or second precision.
func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}
