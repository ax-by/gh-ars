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

// TestRound_S7_1_8_ResetsBackoff: 백오프 리셋 자격은 "Resynced 를 보냈는가"가 아니라 "스트림이
// 30s 이상 유지됐는가"다. 데몬이 뜨자마자 이벤트 한 건 내고 죽는 플래핑에서 매 회차 1s 로 리셋되면
// 지수 백오프가 무력화된다(§6 listener 세션과 동일 기준). [§7.1-8, DESIGN §7]
func TestRound_S7_1_8_ResetsBackoff(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    round
		want bool
	}{
		{"오래 유지된 healthy 스트림", round{served: true, life: BackoffMax}, true},
		{"이벤트 한 건 내고 즉사(플래핑)", round{served: true, life: 50 * time.Millisecond}, false},
		{"열기 실패는 오래 걸려도 리셋 없음", round{served: false, life: 2 * BackoffMax}, false},
	} {
		if got := tc.r.resetsBackoff(); got != tc.want {
			t.Fatalf("%s: resetsBackoff=%v, want %v", tc.name, got, tc.want)
		}
	}
}
