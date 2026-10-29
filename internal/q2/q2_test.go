package q2

import (
	"testing"
	"time"
)

// TestClocks: CLOCK_MONOTONIC_RAW runs, and with CLOCK_MONOTONIC, which NTP may only slew.
func TestClocks(t *testing.T) {
	r0, m0 := RawNS(), MonoNS()
	time.Sleep(200 * time.Millisecond)
	r1, m1 := RawNS(), MonoNS()
	if d := r1 - r0; d < 150e6 || d > 2e9 {
		t.Fatalf("CLOCK_MONOTONIC_RAW moved %d ns in 200 ms", d)
	}
	if d := (r1 - r0) - (m1 - m0); d < -1e6 || d > 1e6 {
		t.Fatalf("CLOCK_MONOTONIC_RAW and CLOCK_MONOTONIC parted by %d ns in 200 ms", d)
	}
}
