package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"gh-ars/internal/config"
	"gh-ars/internal/domain"
	"gh-ars/internal/github"
	"gh-ars/internal/machine"
	"gh-ars/internal/resource"
	"gh-ars/internal/runtime"
)

// calls 는 goroutine 이 남기는 호출 기록이다. 순서 검증용. [DESIGN §11]
type calls struct {
	mu     sync.Mutex
	list   []string
	shared *calls // 있으면 같은 항목을 공유 이력에도 남긴다(GitHub·runtime 교차 순서 검증용)
}

func (c *calls) add(format string, a ...any) {
	c.mu.Lock()
	c.list = append(c.list, fmt.Sprintf(format, a...))
	c.mu.Unlock()
	if c.shared != nil {
		c.shared.add(format, a...)
	}
}

func (c *calls) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.list...)
}

func (c *calls) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = nil
}

// fakeGH 는 github.Client 대역이다. [DESIGN §11]
type fakeGH struct {
	calls
	mu        sync.Mutex
	nextID    int64
	runners   map[string]int64 // 등록된 runner 이름 → id (GetRunner found)
	authErr   error
	jitErr    error
	getErr    error
	removeErr error
	session   *fakeSession
}

func newFakeGH() *fakeGH { return &fakeGH{nextID: 100, runners: map[string]int64{}} }

func (f *fakeGH) register(name string, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runners[name] = id
}

// CheckAuth 는 §7.1-2 다. authErr 로 실패를 흉내 낸다.
func (f *fakeGH) CheckAuth(_ context.Context, group string) error {
	f.add("CheckAuth %s", group)
	return f.authErr
}

func (f *fakeGH) EnsureScaleSet(_ context.Context, name, group string) (int, error) {
	f.add("EnsureScaleSet %s %s", name, group)
	return 7, nil
}

func (f *fakeGH) DeleteScaleSet(context.Context, string, string) error { return nil }

func (f *fakeGH) GenerateJIT(_ context.Context, scaleSetID int, runnerName string) (string, github.RunnerRef, error) {
	f.add("GenerateJIT %d %s", scaleSetID, runnerName)
	if f.jitErr != nil {
		return "", github.RunnerRef{}, f.jitErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.runners[runnerName] = f.nextID
	return "JIT-" + runnerName, github.RunnerRef{ID: f.nextID, Name: runnerName}, nil
}

func (f *fakeGH) GetRunner(_ context.Context, runnerName string) (github.RunnerRef, bool, error) {
	f.add("GetRunner %s", runnerName)
	if f.getErr != nil {
		return github.RunnerRef{}, false, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.runners[runnerName]
	if !ok {
		return github.RunnerRef{}, false, nil
	}
	return github.RunnerRef{ID: id, Name: runnerName}, true, nil
}

func (f *fakeGH) RemoveRunner(_ context.Context, runnerID int64) error {
	f.add("RemoveRunner %d", runnerID)
	if f.removeErr != nil {
		return f.removeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, id := range f.runners {
		if id == runnerID {
			delete(f.runners, name)
		}
	}
	return nil
}

func (f *fakeGH) NewSession(_ context.Context, scaleSetID int, owner string) (github.Session, error) {
	f.add("NewSession %d %s", scaleSetID, owner)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.session == nil {
		return nil, errors.New("fake: no session")
	}
	return f.session, nil
}

// fakeRT 는 runtime.Runtime 대역이다. 실제 docker 는 쓰지 않는다. [DESIGN §11]
type fakeRT struct {
	calls
	mu        sync.Mutex
	specs     []runtime.CreateSpec
	copied    map[string]string // container → tar 내용
	createErr error
	copyErr   error
	startErr  error
	startFn   func(ctx context.Context) error // 있으면 Start 가 이것을 부른다
	removeErr error
	volRmErr  error
	volRmOnce error // 다음 VolumeRemove 한 번만 실패
}

func newFakeRT() *fakeRT { return &fakeRT{copied: map[string]string{}} }

func (f *fakeRT) Kind() domain.RuntimeKind { return domain.RuntimeDocker }
func (f *fakeRT) Info(context.Context) (runtime.Info, error) {
	return runtime.Info{CPUs: 4, MemoryBytes: 8 << 30}, nil
}
func (f *fakeRT) Pull(_ context.Context, image string) error {
	f.add("Pull %s", image)
	return nil
}
func (f *fakeRT) ImageExists(context.Context, string) (bool, error)         { return true, nil }
func (f *fakeRT) List(context.Context, string) ([]runtime.Container, error) { return nil, nil }
func (f *fakeRT) Create(_ context.Context, spec runtime.CreateSpec) error {
	f.add("Create %s", spec.Name)
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.mu.Unlock()
	return f.createErr
}
func (f *fakeRT) CopyIn(_ context.Context, container string, tar io.Reader, destDir string) error {
	f.add("CopyIn %s %s", container, destDir)
	b, _ := io.ReadAll(tar)
	f.mu.Lock()
	f.copied[container] = string(b)
	f.mu.Unlock()
	return f.copyErr
}
func (f *fakeRT) Start(ctx context.Context, container string) error {
	f.add("Start %s", container)
	if f.startFn != nil {
		return f.startFn(ctx)
	}
	return f.startErr
}
func (f *fakeRT) Remove(_ context.Context, container string, force bool) error {
	f.add("Remove %s force=%v", container, force)
	return f.removeErr
}
func (f *fakeRT) VolumeCreate(_ context.Context, name string, _ map[string]string) error {
	f.add("VolumeCreate %s", name)
	return nil
}
func (f *fakeRT) VolumeList(context.Context, string) ([]string, error) { return nil, nil }
func (f *fakeRT) VolumeRemove(_ context.Context, name string) error {
	f.add("VolumeRemove %s", name)
	f.mu.Lock()
	once := f.volRmOnce
	f.volRmOnce = nil
	f.mu.Unlock()
	if once != nil {
		return once
	}
	return f.volRmErr
}
func (f *fakeRT) Events(ctx context.Context, _ string) (<-chan runtime.Event, <-chan error) {
	ev := make(chan runtime.Event)
	errCh := make(chan error, 1)
	go func() {
		<-ctx.Done()
		close(ev)
		errCh <- ctx.Err()
		close(errCh)
	}()
	return ev, errCh
}

func (f *fakeRT) lastSpec(t *testing.T) runtime.CreateSpec {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.specs) == 0 {
		t.Fatal("Create 가 호출되지 않았다")
	}
	return f.specs[len(f.specs)-1]
}

// fakeAgent 는 MachineAgent 대역이다.
type fakeAgent struct {
	calls
	name   string
	rt     *fakeRT
	snap   machine.Snapshot
	images []string // NewAgent 가 받은 pre-pull 대상
}

func (a *fakeAgent) Name() string             { return a.name }
func (a *fakeAgent) Runtime() runtime.Runtime { return a.rt }
func (a *fakeAgent) Close() error {
	a.add("Close %s", a.name)
	return nil
}

// Preflight 는 실제 Agent 처럼 info 뒤 pre-pull 을 기록한다. [§7.1-3, §7.1-4]
func (a *fakeAgent) Preflight(ctx context.Context) (runtime.Info, error) {
	info, err := a.rt.Info(ctx)
	if err != nil {
		return runtime.Info{}, err
	}
	for _, img := range a.images {
		if err := a.rt.Pull(ctx, img); err != nil {
			return runtime.Info{}, err
		}
	}
	return info, nil
}
func (a *fakeAgent) Observe(context.Context) (machine.Snapshot, error) {
	a.add("Observe %s", a.name)
	return a.snap, nil
}

// Run 은 실제 Agent 처럼 events 를 연 뒤 Observe → Resynced 를 보내고 ctx 취소까지 기다린다. [§7.1-7, §7.1-8]
func (a *fakeAgent) Run(ctx context.Context, sink machine.Sink) {
	a.add("Run %s", a.name)
	snap, _ := a.Observe(ctx)
	// 실제 Agent 처럼 회차의 info 를 실어 보낸다(재접속 머신의 예산 재계산 재료). [§7.1-3, R21]
	if info, err := a.rt.Info(ctx); err == nil {
		snap.Info = info
	}
	sink.Resynced(a.name, snap)
	<-ctx.Done()
}

// fakeMax 는 listener.SetMaxRunners 기록이다. [§7.2-1]
type fakeMax struct {
	mu   sync.Mutex
	vals []int
}

func (f *fakeMax) SetMaxRunners(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vals = append(f.vals, n)
}

func (f *fakeMax) last() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.vals) == 0 {
		return -1
	}
	return f.vals[len(f.vals)-1]
}

// clock 은 테스트가 움직이는 시계다.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// harness 는 Run 을 부르지 않고 handle 로 메시지를 직접 넣는다. goroutine 결과는 inbox 에서 꺼낸다. [DESIGN §11]
type harness struct {
	t     *testing.T
	c     *Controller
	gh    *fakeGH
	rt    *fakeRT
	agent *fakeAgent
	max   *fakeMax
	clock *clock
	all   *calls // GitHub·runtime·agent 호출을 한 이력에 순서대로
	ids   int
}

const (
	testScaleSet = "ss"
	testMachine  = "m1"
	testImage    = "ghcr.io/actions/actions-runner:2.337.0"
)

func newHarness(t *testing.T, minRunners int) *harness {
	t.Helper()
	h := &harness{t: t, gh: newFakeGH(), rt: newFakeRT(), max: &fakeMax{}, all: &calls{},
		clock: &clock{t: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}}
	h.agent = &fakeAgent{name: testMachine, rt: h.rt}
	h.gh.shared, h.rt.shared, h.agent.shared = h.all, h.all, h.all
	cfg := &config.Config{
		ScaleSets: []config.ScaleSet{{
			Name: testScaleSet, RunnerGroup: "Default", MinRunners: minRunners, MaxRunners: 10,
			Unit: resource.Budget{CPU: 1, MemoryBytes: 1 << 30}, Mode: domain.ModeNone,
			RunnerImage: testImage, Machines: []string{testMachine},
		}},
		Machines: []config.Machine{{Name: testMachine, ScaleSet: testScaleSet, Local: true, Runtime: domain.RuntimeDocker}},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	h.c = New(cfg, h.gh, log, Options{
		NewAgent: func(spec machine.Spec, _ *slog.Logger) (MachineAgent, error) {
			h.agent.images = spec.Images
			return h.agent, nil
		},
		NewUnitID: func() domain.UnitID {
			h.ids++
			return domain.UnitID(fmt.Sprintf("01K4Z9V6H8QW3T5R7Y9B2C4%03d", h.ids)) // 26자 Crockford
		},
		Now: h.clock.now,
	})
	h.c.agents[testMachine] = h.agent
	m := h.c.machineByName(testMachine)
	m.Health = domain.Healthy
	m.PhysicalMax, m.EffectiveMax = 4, 4
	h.c.reached[testMachine] = true
	ss := h.c.scaleSets[testScaleSet]
	ss.GitHubID = 7
	var ms maxSetter = h.max
	ss.lst.Store(&ms)
	return h
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// recv 는 goroutine 이 보낸 다음 메시지를 꺼낸다.
func (h *harness) recv() any {
	h.t.Helper()
	select {
	case m := <-h.c.inbox:
		return m
	case <-time.After(5 * time.Second):
		h.t.Fatal("inbox 에서 메시지를 기다리다 타임아웃")
		return nil
	}
}

// pump 는 다음 메시지를 꺼내 handle 에 넣고 돌려준다.
func (h *harness) pump() any {
	h.t.Helper()
	m := h.recv()
	h.c.handle(m)
	return m
}

// pumpUntil 은 want 타입의 메시지가 처리될 때까지 pump 한다.
func (h *harness) pumpUntil(match func(any) bool) any {
	h.t.Helper()
	for i := 0; i < 20; i++ {
		m := h.pump()
		if match(m) {
			return m
		}
	}
	h.t.Fatal("기대한 메시지가 오지 않았다")
	return nil
}

// pumpEach 는 goroutine 순서와 무관하게 matchers 각각이 한 번씩 처리될 때까지 pump 한다.
func (h *harness) pumpEach(matchers ...func(any) bool) {
	h.t.Helper()
	left := append([]func(any) bool(nil), matchers...)
	for i := 0; i < 20 && len(left) > 0; i++ {
		m := h.pump()
		for j, match := range left {
			if match(m) {
				left = append(left[:j], left[j+1:]...)
				break
			}
		}
	}
	if len(left) > 0 {
		h.t.Fatalf("기대한 메시지 %d개가 오지 않았다", len(left))
	}
}

func (h *harness) desired(assigned int) int {
	h.t.Helper()
	reply := make(chan int, 1)
	h.c.handle(msgDesired{ScaleSet: testScaleSet, Assigned: assigned, Reply: reply})
	return <-reply
}

func (h *harness) die(id domain.UnitID, role domain.Role) {
	h.c.handle(msgEvent{Machine: testMachine, Ev: runtime.Event{Name: domain.ContainerName(id, role), Action: "die", ExitCode: 0, At: h.clock.now()}})
}

func (h *harness) onlyUnit() *domain.Unit {
	h.t.Helper()
	if len(h.c.units) != 1 {
		h.t.Fatalf("unit 수 %d, want 1", len(h.c.units))
	}
	for _, u := range h.c.units {
		return u
	}
	return nil
}

// addUnit 은 상태에 unit 을 직접 넣는다(startUnit 을 거치지 않음).
func (h *harness) addUnit(state domain.UnitState, opts ...func(*domain.Unit)) *domain.Unit {
	id := h.c.opts.NewUnitID()
	u := &domain.Unit{
		ID: id, ScaleSet: testScaleSet, Machine: testMachine, Mode: domain.ModeNone, State: state,
		CreatedAt: h.clock.now(), RunnerName: domain.RunnerName(testScaleSet, testMachine, id),
		Parts: domain.Parts{Runner: true},
	}
	for _, o := range opts {
		o(u)
	}
	h.c.units[id] = u
	return u
}

func isUnitStarted(m any) bool { _, ok := m.(msgUnitStarted); return ok }
func isCleanupDone(m any) bool { _, ok := m.(msgCleanupDone); return ok }
func isCleanupOf(id domain.UnitID) func(any) bool {
	return func(m any) bool { d, ok := m.(msgCleanupDone); return ok && d.Unit == id }
}
func isRegistration(m any) bool { _, ok := m.(msgRegistration); return ok }

func wantCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("호출 순서\n got: %q\nwant: %q", got, want)
	}
}

func snapshotOf(ctrs []runtime.Container, vols []string) machine.Snapshot {
	return machine.Snapshot{Containers: ctrs, Volumes: vols}
}

func hasCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

// fakeSession 은 github.Session 대역이다. 초기 통계 TotalAssignedJobs 를 주고 GetMessage 는 ctx 취소까지 막힌다.
type fakeSession struct {
	calls
	assigned int
	mu       sync.Mutex
	maxCaps  []int
	closed   bool
}

func (s *fakeSession) Session() scaleset.RunnerScaleSetSession {
	var id [16]byte
	id[0] = 1
	return scaleset.RunnerScaleSetSession{SessionID: id, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: s.assigned}}
}

func (s *fakeSession) GetMessage(ctx context.Context, _ int, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	s.mu.Lock()
	s.maxCaps = append(s.maxCaps, maxCapacity)
	s.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *fakeSession) DeleteMessage(context.Context, int) error { return nil }
func (s *fakeSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	return ids, nil
}
func (s *fakeSession) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *fakeSession) firstMaxCap() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.maxCaps) == 0 {
		return -1
	}
	return s.maxCaps[0]
}

func (s *fakeSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("타임아웃: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
