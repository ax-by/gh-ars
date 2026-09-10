package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"gh-ars/internal/domain"
	"gh-ars/internal/executor"
	"gh-ars/internal/runtime"
)

// fakeRT 는 Events 열기 실패를 흉내 내는 runtime.Runtime 대역이다.
type fakeRT struct {
	mu       sync.Mutex
	eventsN  int
	openErr  error
	infoCall int
	ord      []string // 호출 순서(events → info → 관측)
	emit     string   // 비어 있지 않으면 events 를 연 직후 그 이름의 die 를 흘린다
}

func (f *fakeRT) rec(what string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ord = append(f.ord, what)
}

func (f *fakeRT) order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ord...)
}

func (f *fakeRT) Kind() domain.RuntimeKind { return domain.RuntimeDocker }
func (f *fakeRT) Info(context.Context) (runtime.Info, error) {
	f.rec("Info")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infoCall++
	return runtime.Info{CPUs: 1, MemoryBytes: 1 << 30}, nil
}
func (f *fakeRT) Pull(context.Context, string) error { return nil }
func (f *fakeRT) List(context.Context, string) ([]runtime.Container, error) {
	f.rec("List")
	return nil, nil
}
func (f *fakeRT) Create(context.Context, runtime.CreateSpec) error        { return nil }
func (f *fakeRT) CopyIn(context.Context, string, io.Reader, string) error { return nil }
func (f *fakeRT) Start(context.Context, string) error                     { return nil }
func (f *fakeRT) Remove(context.Context, string, bool) error              { return nil }
func (f *fakeRT) VolumeCreate(context.Context, string, map[string]string) error {
	return nil
}
func (f *fakeRT) VolumeList(context.Context, string) ([]string, error) {
	f.rec("VolumeList")
	return nil, nil
}
func (f *fakeRT) VolumeRemove(context.Context, string) error { return nil }

// Events 는 openErr 가 있으면 실제 구현처럼 닫힌 채널 + errCh 로 열기 실패를 알린다.
func (f *fakeRT) Events(ctx context.Context, _ string) (<-chan runtime.Event, <-chan error) {
	f.rec("Events")
	f.mu.Lock()
	f.eventsN++
	f.mu.Unlock()
	ev := make(chan runtime.Event)
	errCh := make(chan error, 1)
	if f.openErr != nil {
		errCh <- f.openErr
		close(errCh)
		close(ev)
		return ev, errCh
	}
	emit := f.emit
	go func() {
		// 열자마자 하나 흘린다: 관측 중에 난 die 도 스트림에 남아 있다가 소비 단계에서 전달돼야 한다.
		if emit != "" {
			select {
			case ev <- runtime.Event{Name: emit, Action: "die"}:
			case <-ctx.Done():
			}
		}
		<-ctx.Done()
		close(ev)
		errCh <- ctx.Err()
		close(errCh)
	}()
	return ev, errCh
}

type recSink struct {
	mu        sync.Mutex
	resynced  int
	unhealthy int
	failed    int
	failErr   error
	events    []runtime.Event
	last      Snapshot
	ord       []string // Resynced/Event 콜백 순서
}

func (s *recSink) Event(_ string, ev runtime.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	s.ord = append(s.ord, "Event")
}
func (s *recSink) Unhealthy(string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unhealthy++
}
func (s *recSink) Failed(_ string, reason error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed++
	s.failErr = reason
}
func (s *recSink) Resynced(_ string, snap Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resynced++
	s.last = snap
	s.ord = append(s.ord, "Resynced")
}

// TestRun_S7_1_8_EventsOpenFailure: events 열기가 실패하면 info/Observe 가 성공해도 Resynced(healthy)를 보내지
// 않고 Unhealthy 만 보낸 뒤 백오프로 재시도한다. [§7.1-8]
func TestRun_S7_1_8_EventsOpenFailure(t *testing.T) {
	rt := &fakeRT{openErr: errors.New("events: docker daemon down")}
	a := NewWithRuntime(Spec{Name: "m1"}, rt, nil)
	sink := &recSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	a.Run(ctx, sink) // 첫 시도 즉시 실패 → 1s(±20%) 백오프 → 두 번째 시도 → 취소

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.resynced != 0 {
		t.Fatalf("열기 실패인데 Resynced %d회", sink.resynced)
	}
	if sink.unhealthy != 1 {
		t.Fatalf("Unhealthy %d회, want 1 (첫 회차만 통지, 이후는 이미 unhealthy)", sink.unhealthy)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.eventsN < 2 {
		t.Fatalf("재시도되지 않음: Events %d회", rt.eventsN)
	}
}

// TestRun_S7_1_7_ResyncAfterEventsOpen: 회차 순서는 events 열기 → info → 관측 → Resynced 다.
// 관측 중에 난 die 는 스트림에 남아 있다가 Resynced 뒤에 전달된다(반대 순서면 그 die 를 놓친다).
// 스냅샷에는 그 회차의 info 가 실려 Controller 가 예산을 다시 계산할 수 있다. [§7.1-7, §7.1-8, DESIGN §7]
func TestRun_S7_1_7_ResyncAfterEventsOpen(t *testing.T) {
	rt := &fakeRT{emit: "gh-ars-01K4Z9V6H8QW3T5R7Y9B2C4001-runner"}
	a := NewWithRuntime(Spec{Name: "m1"}, rt, nil)
	sink := &recSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	a.Run(ctx, sink)

	if got := strings.Join(rt.order(), ","); got != "Events,Info,List,VolumeList" {
		t.Fatalf("회차 순서 = %s, want Events,Info,List,VolumeList", got)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.resynced != 1 || sink.unhealthy != 0 {
		t.Fatalf("resynced=%d unhealthy=%d, want 1/0 (취소는 unhealthy 가 아니다)", sink.resynced, sink.unhealthy)
	}
	if len(sink.events) != 1 || sink.events[0].Name != rt.emit {
		t.Fatalf("관측 중에 난 die 를 놓쳤다: %+v", sink.events)
	}
	if got := strings.Join(sink.ord, ","); got != "Resynced,Event" {
		t.Fatalf("통지 순서 = %s, want Resynced,Event (동기화 뒤에 스트림을 소비한다)", got)
	}
	if sink.last.Info.CPUs != 1 || sink.last.Info.MemoryBytes != 1<<30 {
		t.Fatalf("스냅샷에 회차의 info 가 없다: %+v", sink.last.Info)
	}
}

// TestRun_R21_VerifyOnPreflightSkippedRound: preflight 를 건너뛰는 회차 — local 머신의 events 재시작이
// 이 모양이다 — 에도 그 회차의 info 로 예산을 다시 판정한다. NewWithRuntime 에이전트는 접속·preflight
// 단계가 없어 그 회차를 그대로 재현한다. 위반이면 Resynced 없이 Failed 로 끝난다. [§7.1-3, R21]
func TestRun_R21_VerifyOnPreflightSkippedRound(t *testing.T) {
	rt := &fakeRT{}
	a := NewWithRuntime(Spec{Name: "m1", Local: true, Verify: func(info runtime.Info) error {
		if info.CPUs < 2 {
			return errors.New("R21 resources (cpu=1) < unit (cpu=2)")
		}
		return nil
	}}, rt, nil)
	sink := &recSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.Run(ctx, sink)

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.failed != 1 || sink.resynced != 0 {
		t.Fatalf("failed=%d resynced=%d, want 1/0", sink.failed, sink.resynced)
	}
	if !errors.Is(sink.failErr, ErrFatal) {
		t.Fatalf("Failed 사유가 ErrFatal 이 아니다: %v", sink.failErr)
	}
}

// TestClose_S7_3_ClosesConnection: 종료 시 접속을 닫는다. 여러 번 불러도 안전하다. [§7.3]
func TestClose_S7_3_ClosesConnection(t *testing.T) {
	ex := &fakeExec{h: uid("0", func(_ bool, argv []string) (executor.Result, error) {
		if isCmd(argv, "docker", "info") {
			return ok(dockerInfoJSON("systemd", "2"))
		}
		return executor.Result{}, nil
	})}
	a, err := New(Spec{Name: "m1", Local: true, Runtime: domain.RuntimeDocker, Mode: domain.ModeNone,
		NewExecutor: func() (executor.Executor, error) { return ex, nil }}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close 재호출: %v", err)
	}
	if ex.closed != 1 {
		t.Fatalf("executor Close %d회, want 1", ex.closed)
	}
	if a.Runtime() != nil {
		t.Fatal("닫힌 뒤에도 Runtime 이 남아 있다")
	}
}

// TestRun_R21_FailedAfterReconnect: 재접속 preflight 에서 설정·환경 모순(R21)이 드러나면 Unhealthy 가
// 아니라 Failed 를 보내고 goroutine 을 끝낸다 — 재접속 대상에서도 빠진다. [§7.1-3, DESIGN §3.3, R21]
func TestRun_R21_FailedAfterReconnect(t *testing.T) {
	execN := 0
	spec := Spec{Name: "m1", Local: true, Runtime: domain.RuntimeDocker, Mode: domain.ModeNone}
	// 1회차: 도달 불가(unhealthy → 백오프 재시도). 2회차: info 는 오지만 R21 위반.
	spec.NewExecutor = func() (executor.Executor, error) {
		execN++
		if execN == 1 {
			return &fakeExec{h: func(bool, []string) (executor.Result, error) {
				return executor.Result{}, errors.New("dial tcp: connection refused")
			}}, nil
		}
		return &fakeExec{h: uid("0", func(_ bool, argv []string) (executor.Result, error) {
			if isCmd(argv, "docker", "info") {
				return ok(dockerInfoJSON("systemd", "2"))
			}
			return executor.Result{}, nil
		})}, nil
	}
	spec.Verify = func(runtime.Info) error {
		if execN == 1 {
			return nil
		}
		return errors.New("R21 resources (cpu=1) < unit (cpu=2)")
	}
	a, err := New(spec, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sink := &recSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { a.Run(ctx, sink); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("Failed 통지 후에도 Run 이 끝나지 않았다(재접속을 계속했다)")
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.failed != 1 || sink.unhealthy != 1 || sink.resynced != 0 {
		t.Fatalf("failed=%d unhealthy=%d resynced=%d, want 1/1/0", sink.failed, sink.unhealthy, sink.resynced)
	}
	if !errors.Is(sink.failErr, ErrFatal) {
		t.Fatalf("Failed 사유가 ErrFatal 이 아니다: %v", sink.failErr)
	}
	if execN != 2 {
		t.Fatalf("접속 시도 %d회, want 2 (Failed 뒤 재접속 없음)", execN)
	}
}

// TestRun_R16_FailedOnFirstRound: 첫 회차의 모순도 Unhealthy 가 아니라 Failed 다(Controller 는
// 시작 시 Preflight 결과로 이미 시작 실패하지만, 그때 미도달이었던 머신이 여기로 온다). [§7.1-3, R16]
func TestRun_R16_FailedOnFirstRound(t *testing.T) {
	spec := Spec{Name: "m1", Local: true, Runtime: domain.RuntimeDocker, Mode: domain.ModeSidecar, NewSlices: noSlices}
	spec.NewExecutor = func() (executor.Executor, error) {
		return &fakeExec{h: uid("0", func(_ bool, argv []string) (executor.Result, error) {
			if isCmd(argv, "docker", "info") {
				return ok(dockerInfoJSON("cgroupfs", "2"))
			}
			return executor.Result{}, nil
		})}, nil
	}
	a, err := New(spec, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sink := &recSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.Run(ctx, sink)

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.failed != 1 || sink.unhealthy != 0 {
		t.Fatalf("failed=%d unhealthy=%d, want 1/0", sink.failed, sink.unhealthy)
	}
}

// TestServe_S7_1_8_OpenTimeoutReportsCause: 취소 사유 규약(DESIGN §7). 여러 단계를 덮는 회차 ctx 를
// 마감 타이머가 취소하면, 뒤따르는 단계의 "context canceled" 가 아니라 취소 사유를 보고한다. [§7.1-8]
func TestServe_S7_1_8_OpenTimeoutReportsCause(t *testing.T) {
	reason := errors.New("events 열기 상한 초과: 30s")

	// 1) 타이머가 사유를 남기고 취소한 경우: 사유로 치환된다.
	ctx, cancel := context.WithCancelCause(context.Background())
	stop := watchdog(cancel, time.Millisecond, reason)
	<-ctx.Done()
	stop()
	cancel(nil) // 회차 실패 경로가 다시 취소해도 사유는 첫 취소가 남긴 것이다
	if got := reported(ctx, fmt.Errorf("info: %w", ctx.Err())); !errors.Is(got, reason) {
		t.Fatalf("사유로 치환되지 않았다: %v", got)
	}

	// 2) stop 이 먼저 불린 경우: 사유가 없으므로 관측한 오류를 그대로 보고한다.
	ctx2, cancel2 := context.WithCancelCause(context.Background())
	stop2 := watchdog(cancel2, time.Hour, reason)
	stop2()
	real := errors.New("docker daemon down")
	if got := reported(ctx2, fmt.Errorf("info: %w", real)); !errors.Is(got, real) {
		t.Fatalf("관측한 오류가 바뀌었다: %v", got)
	}
	// 3) 부모 ctx 종료(사유 없는 취소)도 치환하지 않는다.
	cancel2(nil)
	if got := reported(ctx2, fmt.Errorf("info: %w", real)); !errors.Is(got, real) {
		t.Fatalf("사유 없는 취소가 치환됐다: %v", got)
	}
}
