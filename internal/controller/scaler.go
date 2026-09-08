package controller

import (
	"context"
	"os"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"gh-ars/internal/machine"
)

// sessionOwner 는 메시지 세션 owner 이름이다. 식별용이라 호스트명을 쓰고 없으면 "gh-ars".
func sessionOwner() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return "gh-ars@" + h
	}
	return "gh-ars"
}

// sessionCloseTimeout 은 ctx 가 이미 취소된 종료 경로에서 세션 삭제에 주는 시간이다. [§7.3]
const sessionCloseTimeout = 10 * time.Second

// scaleSetScaler 는 scale set 하나의 listener.Scaler 다. 콜백을 inbox 메시지로 바꾼다. [DESIGN §4.5]
type scaleSetScaler struct {
	c  *Controller
	ss *scaleSetState
}

// HandleDesiredRunnerCount: count = TotalAssignedJobs. 루프에 msgDesired 를 보내고 결과를 기다린다. [§7.2-3]
func (s *scaleSetScaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	reply := make(chan int, 1)
	select {
	case s.c.inbox <- msgDesired{ScaleSet: s.ss.Name, Assigned: count, Reply: reply}:
	case <-s.c.done:
		return 0, context.Canceled
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	select {
	case n := <-reply:
		return n, nil
	case <-s.c.done:
		return 0, context.Canceled
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// HandleJobStarted: RunnerName 으로 unit 을 찾아 Busy=true. [§7.2-3]
func (s *scaleSetScaler) HandleJobStarted(_ context.Context, j *scaleset.JobStarted) error {
	s.c.send(msgJobStarted{ScaleSet: s.ss.Name, RunnerName: j.RunnerName})
	return nil
}

// HandleJobCompleted: Completed 표시 또는 pendingCompletion 에서 제거. [§7.2-3]
func (s *scaleSetScaler) HandleJobCompleted(_ context.Context, j *scaleset.JobCompleted) error {
	s.c.send(msgJobCompleted{ScaleSet: s.ss.Name, RunnerName: j.RunnerName, RunnerID: int64(j.RunnerID)})
	return nil
}

// runListener 는 scale set 하나의 세션·listener 를 유지한다. 오류면 세션을 닫고 백오프로 재시작한다.
// ctx 취소 시 세션을 닫는다(§7.3). listener.Client 에는 Close 가 없으므로 여기서 세션을 보유한다. [§7.1-9, DESIGN §4.4, §4.5]
func (c *Controller) runListener(ctx context.Context, ss *scaleSetState) {
	var bo machine.Backoff
	log := c.log.With("scaleSet", ss.Name)
	for ctx.Err() == nil {
		started := time.Now()
		err := c.serveListener(ctx, ss)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= machine.BackoffMax {
			bo.Reset() // 한동안 정상 동작했으면 성공으로 보고 수열을 되돌린다 [§7.1-8 "성공 시 리셋"]
		}
		log.Warn("listener stopped, restarting", "err", err)
		if bo.Wait(ctx) != nil {
			return
		}
	}
}

func (c *Controller) serveListener(ctx context.Context, ss *scaleSetState) error {
	session, err := c.gh.NewSession(ctx, ss.GitHubID, sessionOwner())
	if err != nil {
		return err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCloseTimeout)
		defer cancel()
		if err := session.Close(cctx); err != nil {
			c.log.Warn("session close failed", "scaleSet", ss.Name, "err", err)
		}
	}()
	// 세션 (재)시작: pendingCompletion 을 비운다(초기 세션 통계가 새 기준선). [§7.2-3 안전장치 1]
	c.send(msgSessionStarted{ScaleSet: ss.Name})

	l, err := listener.New(session, listener.Config{
		ScaleSetID: ss.GitHubID,
		MaxRunners: int(ss.capacity.Load()),
		Logger:     c.log.With("scaleSet", ss.Name, "component", "listener"),
	})
	if err != nil {
		return err
	}
	var ms maxSetter = l
	ss.lst.Store(&ms)
	defer ss.lst.Store(nil)
	l.SetMaxRunners(int(ss.capacity.Load())) // 생성과 Store 사이에 바뀐 capacity 를 반영 [§7.2-1]
	return l.Run(ctx, &scaleSetScaler{c: c, ss: ss})
}
