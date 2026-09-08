// Package machine 은 머신 하나를 맡는 에이전트다: 접속 유지, preflight(info)·pre-pull,
// events 스트림 수신, 단절 시 백오프 재시작과 재동기화. [§7.1, §10, DESIGN §7]
//
// Phase 7 범위: local executor + docker 만. SSH(Phase 8), podman 경로 고정·sudo·R16
// (Phase 9·10)은 후속 Phase 가 채운다. 예산 판정(R21·R22)은 Controller 가 `plan` 으로
// 한다(DESIGN §3.4, §8) — 이 패키지는 `runtime.Info` 를 전달할 뿐이다.
package machine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gh-ars/internal/domain"
	"gh-ars/internal/executor"
	"gh-ars/internal/runtime"
)

// Spec 은 에이전트를 만드는 데 필요한 설정 값이다(config 타입에 의존하지 않는다). [§6.0]
type Spec struct {
	Name    string
	Local   bool
	Runtime domain.RuntimeKind
	// Images 는 pre-pull 대상이다: runner 이미지, sidecar 면 sidecar 이미지도. [§7.1-4]
	Images []string
}

// Snapshot 은 머신 한 대의 부품 관측 결과다. Controller 가 plan.Observed 로 바꾼다. [§7.1-7, §8.3]
type Snapshot struct {
	At         time.Time // 관측 시작 시각. 그 뒤에 Starting 이 된 unit 은 Creating 예외로 판정에서 뺀다 [§8.3]
	Containers []runtime.Container
	Volumes    []string
	Slices     []string
}

// Sink 는 에이전트 goroutine 이 Controller 로 보내는 통지다. 구현은 inbox 메시지로 바꾼다. [DESIGN §6, §7]
type Sink interface {
	Event(machine string, ev runtime.Event)
	Unhealthy(machine string, reason error)
	Resynced(machine string, snap Snapshot)
}

// ErrUnsupported 는 아직 구현되지 않은 executor/runtime 조합이다(Phase 8~10).
var ErrUnsupported = errors.New("machine: 아직 지원하지 않는 구성")

// Agent 는 머신 하나의 통로다. Runtime 은 Controller 가 unit 생성·정리 명령에 직접 쓴다. [DESIGN §7]
type Agent struct {
	name string
	ex   executor.Executor
	rt   runtime.Runtime
	log  *slog.Logger
	spec Spec
}

// New 는 executor 와 runtime 을 조립한다. 네트워크·프로세스 실행은 하지 않는다. [DESIGN §7]
func New(spec Spec, log *slog.Logger) (*Agent, error) {
	if !spec.Local {
		return nil, fmt.Errorf("%w: SSH 머신 %q (Phase 8)", ErrUnsupported, spec.Name)
	}
	if spec.Runtime != domain.RuntimeDocker {
		return nil, fmt.Errorf("%w: runtime %q 머신 %q (Phase 9)", ErrUnsupported, spec.Runtime, spec.Name)
	}
	if log == nil {
		log = slog.Default()
	}
	ex := executor.NewLocal()
	// docker 는 §10.2 규칙 2 에 따라 sudo 를 쓰지 않는다.
	rt := runtime.NewDocker(ex, false)
	return &Agent{name: spec.Name, ex: ex, rt: rt, log: log.With("machine", spec.Name), spec: spec}, nil
}

// NewWithRuntime 은 준비된 Runtime 으로 에이전트를 만든다. 테스트와 후속 Phase(ssh/podman)가 쓴다.
func NewWithRuntime(spec Spec, rt runtime.Runtime, log *slog.Logger) *Agent {
	if log == nil {
		log = slog.Default()
	}
	return &Agent{name: spec.Name, rt: rt, log: log.With("machine", spec.Name), spec: spec}
}

func (a *Agent) Name() string             { return a.name }
func (a *Agent) Runtime() runtime.Runtime { return a.rt }

// Preflight 는 §7.1-3 의 runtime 동작 확인(`info`)과 §7.1-4 의 pre-pull 이다.
// 오류는 "도달 불가·명령 실패" 로, 호출자가 unhealthy 로 처리한다(프로세스는 계속).
// 설정·환경 모순(R16)은 sidecar 모드에서만 나오며 Phase 10·12 에서 추가된다. [§7.1-3, §7.1-4]
func (a *Agent) Preflight(ctx context.Context) (runtime.Info, error) {
	info, err := a.rt.Info(ctx)
	if err != nil {
		return runtime.Info{}, fmt.Errorf("preflight: %w", err)
	}
	for _, img := range a.spec.Images {
		if err := a.rt.Pull(ctx, img); err != nil {
			return runtime.Info{}, fmt.Errorf("pre-pull %s: %w", img, err)
		}
		a.log.Info("image pulled", "image", img)
	}
	return info, nil
}

// Observe 는 전체 동기화 재료를 모은다: `ps -a --filter label=gh-ars.unit`, 같은 필터의 볼륨.
// slice 목록은 systemd 패키지(Phase 12)가 붙을 때 채운다. [§7.1-7, §4.2]
func (a *Agent) Observe(ctx context.Context) (Snapshot, error) {
	at := time.Now()
	ctrs, err := a.rt.List(ctx, domain.UnitLabelFilter)
	if err != nil {
		return Snapshot{}, fmt.Errorf("observe: %w", err)
	}
	vols, err := a.rt.VolumeList(ctx, domain.UnitLabelFilter)
	if err != nil {
		return Snapshot{}, fmt.Errorf("observe: %w", err)
	}
	return Snapshot{At: at, Containers: ctrs, Volumes: vols}, nil
}

// Run 은 events 스트림을 유지한다. 회차마다 events 를 연 뒤 전체 동기화 스냅샷(Resynced)을 보내고
// 스트림을 소비한다. 스트림이 끝나면 unhealthy 를 알리고 백오프로 재시작한다. ctx 취소까지 돌아간다. [§7.1-7, §7.1-8, §10.1]
//
// 순서: events 를 먼저 열고 그 다음 Observe 한다. 반대로 하면 그 사이의 die 를 놓친다. 시작 시(§7.1-7)에도
// 같은 회차를 타므로 Controller 는 첫 Resynced 를 "events 준비 완료 + 동기화 완료" 신호로 쓴다.
// 첫 회차는 Resynced 또는 Unhealthy 중 하나를 반드시 보낸다(Controller 가 그것을 기다린다).
func (a *Agent) Run(ctx context.Context, sink Sink) {
	var bo Backoff
	first := true
	for {
		served, err := a.serve(ctx, sink)
		if ctx.Err() != nil {
			return
		}
		if served || first {
			// healthy 였던 스트림이 끝났다(또는 첫 회차가 실패했다): 재시작이 성공할 때까지 unhealthy.
			// 다음 성공에서 Resynced 가 healthy 로 되돌린다.
			sink.Unhealthy(a.name, err)
		}
		if served {
			bo.Reset()
		}
		first = false
		a.log.Warn("machine unhealthy, will retry", "err", err)
		if bo.Wait(ctx) != nil {
			return
		}
	}
}

// serve 는 events 스트림 한 회차다: events 열기 → info 로 데몬 생존 확인 → Observe → Resynced → 소비.
// 스트림이 끝난 이유를 돌려준다(정상 종료는 없다). served 는 Resynced 까지 갔는지(= healthy)다.
func (a *Agent) serve(ctx context.Context, sink Sink) (served bool, err error) {
	ectx, cancel := context.WithCancel(ctx)
	defer cancel()
	evCh, errCh := a.rt.Events(ectx, domain.UnitLabelFilter)
	if _, err := a.rt.Info(ectx); err != nil {
		cancel()
		drain(evCh, errCh)
		return false, fmt.Errorf("info: %w", err)
	}
	snap, err := a.Observe(ectx)
	if err != nil {
		cancel()
		drain(evCh, errCh)
		return false, err
	}
	// 스트림이 열리자마자 끝났으면(열기 실패) healthy 로 보고하지 않는다. Runtime.Events 는 열기 실패를
	// 이미 닫힌 채널 + errCh 로 알린다. [§7.1-8]
	select {
	case err := <-errCh:
		drain(evCh, nil)
		return false, fmt.Errorf("events: %w", err)
	default:
	}
	sink.Resynced(a.name, snap)
	for ev := range evCh {
		sink.Event(a.name, ev)
	}
	return true, <-errCh
}

// drain 은 취소한 events 스트림을 끝까지 비운다(goroutine 누수 방지).
func drain(evCh <-chan runtime.Event, errCh <-chan error) {
	for range evCh {
	}
	if errCh != nil {
		<-errCh
	}
}
