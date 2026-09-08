// Package controller 는 상태를 소유하는 단일 goroutine 이다. listener 콜백·머신 이벤트·타이머를
// inbox 메시지로 직렬화하고, 느린 작업(unit 생성·정리, GitHub 조회)은 goroutine 으로 빼서
// 결과를 다시 메시지로 받는다. [§7, §8.3, DESIGN §6]
//
// Phase 7 범위(walking skeleton): none 모드, local docker. 축소(Draining)·Dying 재시도 완성·
// R24 세부·sidecar 는 후속 Phase(11, 12)가 채운다.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"

	"gh-ars/internal/config"
	"gh-ars/internal/domain"
	"gh-ars/internal/github"
	"gh-ars/internal/machine"
	"gh-ars/internal/plan"
	"gh-ars/internal/resource"
	"gh-ars/internal/runtime"
)

// 코드 상수. 설정으로 노출하지 않는다. [§8.3 상수 표, §3.2]
const (
	grace         = 5 * time.Minute  // 등록 대기 grace [§8.3]
	tickInterval  = 30 * time.Second // 상태 대조 tick [§8.3]
	pendingExpiry = 5 * time.Minute  // pendingCompletion 항목 유지 상한 [§7.2-3]
	startTimeout  = 2 * time.Minute  // create→cp→start 완료까지 [§7.2-4]
	// cleanupTimeout 은 정리 한 회차의 상한이다. SPEC 은 정하지 않지만 상한이 없으면 명령이 매달릴 때
	// 그 unit 의 정리가 영영 "진행 중" 으로 남아 tick 재시도(§8.3)가 일어나지 않는다. 기동 타임아웃과 같은 값.
	cleanupTimeout = startTimeout
)

// MachineAgent 는 Controller 가 머신 에이전트에 요구하는 것이다. 구현은 *machine.Agent, 테스트는 fake. [DESIGN §7, §11]
type MachineAgent interface {
	Name() string
	Runtime() runtime.Runtime
	Preflight(ctx context.Context) (runtime.Info, error)
	// Run 은 events 를 열고 Resynced(전체 동기화 스냅샷)를 보낸 뒤 스트림을 소비한다. 첫 회차는
	// Resynced 또는 Unhealthy 를 반드시 보낸다. [§7.1-7, §7.1-8]
	Run(ctx context.Context, sink machine.Sink)
}

// Options 는 테스트가 바꿔 끼우는 의존성이다. nil 필드는 기본값을 쓴다.
type Options struct {
	NewAgent  func(spec machine.Spec, log *slog.Logger) (MachineAgent, error)
	NewUnitID func() domain.UnitID
	Now       func() time.Time
}

func (o *Options) defaults() {
	if o.NewAgent == nil {
		o.NewAgent = func(spec machine.Spec, log *slog.Logger) (MachineAgent, error) {
			return machine.New(spec, log)
		}
	}
	if o.NewUnitID == nil {
		o.NewUnitID = func() domain.UnitID { return domain.UnitID(ulid.Make().String()) } // [§4.1]
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// maxSetter 는 listener.SetMaxRunners 다. 테스트는 기록용 대역을 넣는다. [§7.2-1]
type maxSetter interface{ SetMaxRunners(int) }

// scaleSetState 는 scale set 하나의 Controller 측 상태다. [DESIGN §4.5, §6]
type scaleSetState struct {
	domain.ScaleSet
	// pending 은 pendingCompletion: busy unit 이 die 했지만 JobCompleted 가 아직 안 온 runner 이름 → 항목. [§7.2-3]
	pending map[string]pendingEntry
	// completedIDs 는 아직 대조하지 못한 이름 없는 JobCompleted 의 RunnerID → 수신 시각이다. msgUnitStarted 가
	// id 를 알려주면 적용한다(JobStarted → die → JobCompleted → msgUnitStarted 순서). 만료는 pendingExpiry. [§7.2-3]
	completedIDs map[int64]time.Time
	// capacity 는 마지막으로 계산한 capacity 다. listener goroutine 이 생성 시 읽는다. [§7.2-1]
	capacity atomic.Int32
	// lst 는 실행 중인 listener 다. listener goroutine 이 넣고 루프가 SetMaxRunners 에 쓴다.
	lst atomic.Pointer[maxSetter]
}

// setCapacity 는 capacity 를 저장하고 listener 가 있으면 SetMaxRunners 로 반영한다. [§7.2-1]
func (s *scaleSetState) setCapacity(n int) {
	s.capacity.Store(int32(n))
	if l := s.lst.Load(); l != nil {
		(*l).SetMaxRunners(n)
	}
}

// pendingEntry 는 pendingCompletion 항목이다. Machine 은 재동기화 시 그 머신 소속 항목만 비우기 위해 둔다. [§7.2-3 안전장치]
type pendingEntry struct {
	At       time.Time
	Machine  string
	RunnerID int64 // 이름이 빈 JobCompleted 를 RunnerID 로 대조하기 위해 보관 [DESIGN §4.5]
}

// Controller 는 상태 소유 goroutine 이다. 상태 필드는 루프(handle)만 만진다. [DESIGN §1-2, §6]
type Controller struct {
	cfg  *config.Config
	gh   github.Client
	log  *slog.Logger
	opts Options

	ctx   context.Context // Run 의 ctx. goroutine 이 파생 ctx 를 만든다
	inbox chan any
	done  chan struct{} // 루프 종료. goroutine 의 send 가 막히지 않게 한다
	wg    sync.WaitGroup

	scaleSets map[string]*scaleSetState
	ssOrder   []string
	machines  []domain.Machine // YAML 순서. plan.Spread 의 tie-break ③ [§8.2]
	budgets   map[string]resource.Budget
	agents    map[string]MachineAgent
	units     map[domain.UnitID]*domain.Unit
	runnerIDs map[domain.UnitID]int64     // GenerateJIT 가 준 runner id. JobCompleted 의 RunnerID 대조용 [DESIGN §4.5]
	cleaning  map[domain.UnitID]bool      // cleanupUnit goroutine 진행 중
	checking  map[domain.UnitID]bool      // tick 의 GetRunner 대조 진행 중
	startedAt map[domain.UnitID]time.Time // Creating → Starting 전이 시각. 그 전에 찍힌 스냅샷의 판정에서 제외 [§8.3 Creating 예외]
	reached   map[string]bool             // 시작 시 preflight 도달 여부. R24 판정 [§6.2]
}

// New 는 설정으로 상태를 조립한다. 네트워크·프로세스 실행은 Run 에서 한다.
func New(cfg *config.Config, gh github.Client, log *slog.Logger, opts Options) *Controller {
	if log == nil {
		log = slog.Default()
	}
	opts.defaults()
	c := &Controller{
		cfg:       cfg,
		gh:        gh,
		log:       log,
		opts:      opts,
		ctx:       context.Background(),
		inbox:     make(chan any, 64),
		done:      make(chan struct{}),
		scaleSets: map[string]*scaleSetState{},
		budgets:   map[string]resource.Budget{},
		agents:    map[string]MachineAgent{},
		units:     map[domain.UnitID]*domain.Unit{},
		runnerIDs: map[domain.UnitID]int64{},
		cleaning:  map[domain.UnitID]bool{},
		checking:  map[domain.UnitID]bool{},
		startedAt: map[domain.UnitID]time.Time{},
		reached:   map[string]bool{},
	}
	for _, s := range cfg.ScaleSets {
		c.scaleSets[s.Name] = &scaleSetState{
			ScaleSet: domain.ScaleSet{
				Name: s.Name, RunnerGroup: s.RunnerGroup, MinRunners: s.MinRunners, MaxRunners: s.MaxRunners,
				Unit: s.Unit, Mode: s.Mode, RunnerImage: s.RunnerImage, SidecarImage: s.SidecarImage, Machines: s.Machines,
			},
			pending:      map[string]pendingEntry{},
			completedIDs: map[int64]time.Time{},
		}
		c.ssOrder = append(c.ssOrder, s.Name)
	}
	for _, m := range cfg.Machines {
		c.machines = append(c.machines, domain.Machine{
			Name: m.Name, ScaleSet: m.ScaleSet, Runtime: m.Runtime, Local: m.Local, Health: domain.Unhealthy,
		})
	}
	return c
}

// Run 은 §7.1 시작 순서를 밟은 뒤 §7.2 루프에 들어간다. ctx 취소로 끝난다(§7.3). [§7.1, §7.2, §7.3]
func (c *Controller) Run(ctx context.Context) error {
	c.ctx = ctx
	for _, w := range c.cfg.Warnings {
		c.log.Warn(w)
	}

	// §7.1-3·4 preflight + pre-pull. 도달 불가·명령 실패는 unhealthy, 설정·환경 모순(R21)은 시작 실패.
	for i := range c.machines {
		m := &c.machines[i]
		ss := c.scaleSets[m.ScaleSet]
		agent, err := c.opts.NewAgent(c.agentSpec(*m, ss), c.log)
		if err != nil {
			return err
		}
		c.agents[m.Name] = agent
		info, err := agent.Preflight(ctx)
		if err != nil {
			c.log.Warn("machine unhealthy at start", "machine", m.Name, "err", err)
			continue
		}
		if err := c.applyInfo(m, info); err != nil {
			return err
		}
		c.reached[m.Name] = true
		m.Health = domain.Healthy
	}

	// §7.1-5 scale set 확보. 종료 시 삭제하지 않는다.
	for _, name := range c.ssOrder {
		ss := c.scaleSets[name]
		id, err := c.gh.EnsureScaleSet(ctx, ss.Name, ss.RunnerGroup)
		if err != nil {
			return err
		}
		ss.GitHubID = id
	}

	// §7.1-6 capacity. R23 cap 경고, R24 는 도달한 머신 기준.
	if err := c.initialCapacity(); err != nil {
		return err
	}

	// §7.1-7·8 전체 동기화 + events 스트림. 에이전트가 events 를 연 뒤 스냅샷을 보낸다(그 사이의 die 를
	// 놓치지 않는 순서). unhealthy 머신도 에이전트가 백오프로 재접속을 시도한다.
	for _, m := range c.machines {
		agent := c.agents[m.Name]
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			agent.Run(ctx, sink{c})
		}()
	}
	// 세션을 열기 전에 머신마다 첫 동기화(Resynced) 또는 실패(Unhealthy)를 기다린다. 그래야 listener 의
	// 첫 desired 로 만든 unit 의 die 를 events 가 받는다. [§7.1-7 → §7.1-9 순서]
	if err := c.awaitInitialSync(ctx); err != nil {
		return err
	}

	// §7.1-9 메시지 세션 + 루프.
	for _, name := range c.ssOrder {
		ss := c.scaleSets[name]
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.runListener(ctx, ss)
		}()
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	c.log.Info("controller running", "scaleSets", len(c.scaleSets), "machines", len(c.machines))
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case m := <-c.inbox:
			c.handle(m)
		case <-ticker.C:
			c.handle(msgTick{})
		}
	}
	// §7.3: 세션·스트림만 정리한다. 실행 중 컨테이너는 kill 하지 않는다.
	close(c.done)
	c.wg.Wait()
	c.log.Info("controller stopped")
	return nil
}

// awaitInitialSync 는 각 머신의 첫 Resynced/Unhealthy 가 올 때까지 inbox 를 처리한다. [§7.1-7, §7.1-8]
func (c *Controller) awaitInitialSync(ctx context.Context) error {
	waiting := map[string]bool{}
	for _, m := range c.machines {
		waiting[m.Name] = true
	}
	for len(waiting) > 0 {
		select {
		case <-ctx.Done():
			close(c.done)
			c.wg.Wait()
			return ctx.Err()
		case m := <-c.inbox:
			c.handle(m)
			switch m := m.(type) {
			case msgResynced:
				delete(waiting, m.Machine)
			case msgHealth:
				if !m.Healthy {
					delete(waiting, m.Machine)
				}
			}
		}
	}
	return nil
}

// agentSpec 은 config 머신 항목을 machine.Spec 으로 바꾼다. pre-pull 대상은 runner 이미지(+sidecar). [§7.1-4]
func (c *Controller) agentSpec(m domain.Machine, ss *scaleSetState) machine.Spec {
	images := []string{ss.RunnerImage}
	if ss.Mode == domain.ModeSidecar && ss.SidecarImage != "" {
		images = append(images, ss.SidecarImage)
	}
	return machine.Spec{Name: m.Name, Local: m.Local, Runtime: m.Runtime, Images: images}
}

// applyInfo 는 `info` 결과로 머신 예산과 physicalMax·effectiveMax 를 정한다. [§7.1-3, §8.1, R21, R22]
//
// R21: resources 명시 → 탐지값을 상한으로 cap(경고). physicalMax 0 → 오류(시작 실패).
// R22: maxRunners > physicalMax → 경고 후 cap.
func (c *Controller) applyInfo(m *domain.Machine, info runtime.Info) error {
	cm := c.cfgMachine(m.Name)
	ss := c.scaleSets[m.ScaleSet]
	detected := resource.Budget{CPU: info.CPUs, MemoryBytes: info.MemoryBytes}
	budget := detected
	if cm.Resources != nil {
		budget = *cm.Resources
		if budget.CPU > detected.CPU {
			c.log.Warn("R21 machine cpu exceeds detected, capped", "machine", m.Name, "configured", budget.CPU, "detected", detected.CPU)
			budget.CPU = detected.CPU
		}
		if budget.MemoryBytes > detected.MemoryBytes {
			c.log.Warn("R21 machine memory exceeds detected, capped", "machine", m.Name, "configured", budget.MemoryBytes, "detected", detected.MemoryBytes)
			budget.MemoryBytes = detected.MemoryBytes
		}
	}
	physical := plan.PhysicalMax(budget, ss.Unit)
	if physical == 0 {
		return fmt.Errorf("R21 machine %q: resources (cpu=%g, memory=%d) < scale set %q unit (cpu=%g, memory=%d)",
			m.Name, budget.CPU, budget.MemoryBytes, ss.Name, ss.Unit.CPU, ss.Unit.MemoryBytes)
	}
	if cm.MaxRunners != nil && *cm.MaxRunners > physical {
		c.log.Warn("R22 machine maxRunners exceeds physicalMax, capped", "machine", m.Name, "maxRunners", *cm.MaxRunners, "physicalMax", physical)
	}
	c.budgets[m.Name] = budget
	m.PhysicalMax = physical
	m.EffectiveMax = plan.EffectiveMax(physical, cm.MaxRunners)
	c.log.Info("machine ready", "machine", m.Name, "cpu", budget.CPU, "memoryBytes", budget.MemoryBytes,
		"physicalMax", physical, "effectiveMax", m.EffectiveMax)
	return nil
}

func (c *Controller) cfgMachine(name string) config.Machine {
	for _, m := range c.cfg.Machines {
		if m.Name == name {
			return m
		}
	}
	return config.Machine{}
}

// initialCapacity 는 시작 시 capacity 를 계산하고 R23 cap·R24 를 판정한다. [§7.1-6, R23, R24]
func (c *Controller) initialCapacity() error {
	var errs []error
	for _, name := range c.ssOrder {
		ss := c.scaleSets[name]
		sum := 0
		allReached := true
		for _, m := range c.machines {
			if m.ScaleSet != ss.Name {
				continue
			}
			if !c.reached[m.Name] {
				allReached = false
			}
			if m.Health == domain.Healthy {
				sum += m.EffectiveMax
			}
		}
		if ss.MaxRunners > sum && allReached {
			c.log.Warn("R23 scale set maxRunners exceeds sum of effectiveMax, capped", "scaleSet", ss.Name, "maxRunners", ss.MaxRunners, "sum", sum)
		}
		cap := c.recomputeCapacity(ss)
		if ss.MinRunners > cap {
			if allReached {
				errs = append(errs, fmt.Errorf("R24 scale set %q: minRunners %d > capacity %d", ss.Name, ss.MinRunners, cap))
			} else {
				c.log.Warn("R24 minRunners exceeds capacity of reached machines (some unreachable), continuing",
					"scaleSet", ss.Name, "minRunners", ss.MinRunners, "capacity", cap)
			}
		}
	}
	return errors.Join(errs...)
}

// recomputeCapacity 는 healthy 머신만으로 capacity 를 다시 계산해 listener 에 반영한다. [§7.2-1, §8.1]
func (c *Controller) recomputeCapacity(ss *scaleSetState) int {
	n := plan.Capacity(ss.ScaleSet, c.machines)
	if int(ss.capacity.Load()) != n {
		c.log.Info("capacity changed", "scaleSet", ss.Name, "capacity", n)
	}
	ss.setCapacity(n)
	return n
}

// observed 는 머신 스냅샷을 plan.Observed 로 바꾼다. [DESIGN §5]
func (c *Controller) observed(machineName string, snap machine.Snapshot) plan.Observed {
	return plan.Observed{Machine: machineName, Containers: snap.Containers, Volumes: snap.Volumes, Slices: snap.Slices}
}

func (c *Controller) resyncMsg(machineName string, snap machine.Snapshot) msgResynced {
	return msgResynced{Machine: machineName, At: snap.At, Obs: c.observed(machineName, snap)}
}

// send 는 goroutine 이 루프로 메시지를 보낸다. 루프가 끝났으면 버린다.
func (c *Controller) send(m any) {
	select {
	case c.inbox <- m:
	case <-c.done:
	}
}

// sink 는 machine.Sink 를 inbox 메시지로 바꾼다. [DESIGN §7]
type sink struct{ c *Controller }

func (s sink) Event(m string, ev runtime.Event) { s.c.send(msgEvent{Machine: m, Ev: ev}) }
func (s sink) Unhealthy(m string, reason error) {
	s.c.send(msgHealth{Machine: m, Healthy: false, Err: reason})
}
func (s sink) Resynced(m string, snap machine.Snapshot) { s.c.send(s.c.resyncMsg(m, snap)) }
