package machine

import (
	"testing"
	"time"
)

// TestBackoff_S7_1_8_Sequence: 1s→2s→4s→…→30s cap, jitter ±20%, Reset 후 1s. [§7.1-8]
func TestBackoff_S7_1_8_Sequence(t *testing.T) {
	mid := &Backoff{rand: func() float64 { return 0.5 }} // jitter 0
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i, w := range want {
		if got := mid.Next(); got != w*time.Second {
			t.Fatalf("[%d] got %v want %v", i, got, w*time.Second)
		}
	}
	mid.Reset()
	if got := mid.Next(); got != time.Second {
		t.Fatalf("Reset 후 %v, want 1s", got)
	}

	lo := &Backoff{rand: func() float64 { return 0 }}
	if got := lo.Next(); got != 800*time.Millisecond {
		t.Fatalf("jitter 하한 %v, want 800ms", got)
	}
	hi := &Backoff{rand: func() float64 { return 0.999999 }}
	if got := hi.Next(); got < 1199*time.Millisecond || got > 1200*time.Millisecond {
		t.Fatalf("jitter 상한 %v, want ≈1200ms", got)
	}
}
