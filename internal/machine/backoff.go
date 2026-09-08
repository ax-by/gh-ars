package machine

import (
	"context"
	"math/rand/v2"
	"time"
)

// 재접속·재시작 백오프 상수. 1s 시작, ×2, 최대 30s, ±20% jitter, 성공 시 리셋. [§7.1-8, §10.1]
const (
	backoffInitial = time.Second
	BackoffMax     = 30 * time.Second
	backoffJitter  = 0.2
)

// Backoff 는 §7.1-8 의 지수 백오프 수열이다. SSH 재접속, local events 재시작,
// listener 재시작(DESIGN §4.5)이 같은 수열을 쓴다.
type Backoff struct {
	next time.Duration
	rand func() float64 // [0,1). nil 이면 math/rand
}

// Next 는 이번 대기 시간을 돌려주고 다음 값을 2배(최대 30s)로 올린다. jitter ±20%.
func (b *Backoff) Next() time.Duration {
	if b.next <= 0 {
		b.next = backoffInitial
	}
	base := b.next
	b.next = min(b.next*2, BackoffMax)
	r := rand.Float64
	if b.rand != nil {
		r = b.rand
	}
	// (1-jitter) .. (1+jitter)
	f := 1 + backoffJitter*(2*r()-1)
	return time.Duration(float64(base) * f)
}

// Reset 은 성공 시 수열을 처음으로 되돌린다.
func (b *Backoff) Reset() { b.next = 0 }

// Wait 는 Next 만큼 기다린다. ctx 취소면 그 오류를 돌려준다.
func (b *Backoff) Wait(ctx context.Context) error {
	t := time.NewTimer(b.Next())
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
