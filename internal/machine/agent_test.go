package machine

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"gh-ars/internal/domain"
	"gh-ars/internal/runtime"
)

// fakeRT 는 Events 열기 실패를 흉내 내는 runtime.Runtime 대역이다.
type fakeRT struct {
	mu       sync.Mutex
	eventsN  int
	openErr  error
	infoCall int
}

func (f *fakeRT) Kind() domain.RuntimeKind { return domain.RuntimeDocker }
func (f *fakeRT) Info(context.Context) (runtime.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infoCall++
	return runtime.Info{CPUs: 1, MemoryBytes: 1 << 30}, nil
}
func (f *fakeRT) Pull(context.Context, string) error                        { return nil }
func (f *fakeRT) List(context.Context, string) ([]runtime.Container, error) { return nil, nil }
func (f *fakeRT) Create(context.Context, runtime.CreateSpec) error          { return nil }
func (f *fakeRT) CopyIn(context.Context, string, io.Reader, string) error   { return nil }
func (f *fakeRT) Start(context.Context, string) error                       { return nil }
func (f *fakeRT) Remove(context.Context, string, bool) error                { return nil }
func (f *fakeRT) VolumeCreate(context.Context, string, map[string]string) error {
	return nil
}
func (f *fakeRT) VolumeList(context.Context, string) ([]string, error) { return nil, nil }
func (f *fakeRT) VolumeRemove(context.Context, string) error           { return nil }

// Events 는 openErr 가 있으면 실제 구현처럼 닫힌 채널 + errCh 로 열기 실패를 알린다.
func (f *fakeRT) Events(ctx context.Context, _ string) (<-chan runtime.Event, <-chan error) {
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
	go func() {
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
}

func (s *recSink) Event(string, runtime.Event) {}
func (s *recSink) Unhealthy(string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unhealthy++
}
func (s *recSink) Resynced(string, Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resynced++
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

// TestRun_S7_1_7_ResyncAfterEventsOpen: 정상 열기면 Resynced 를 보내고 스트림을 유지한다. [§7.1-7, §7.1-8]
func TestRun_S7_1_7_ResyncAfterEventsOpen(t *testing.T) {
	rt := &fakeRT{}
	a := NewWithRuntime(Spec{Name: "m1"}, rt, nil)
	sink := &recSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	a.Run(ctx, sink)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.resynced != 1 || sink.unhealthy != 0 {
		t.Fatalf("resynced=%d unhealthy=%d, want 1/0 (취소는 unhealthy 가 아니다)", sink.resynced, sink.unhealthy)
	}
}
