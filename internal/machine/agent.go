// Package machine 은 머신 하나를 맡는 에이전트다: 접속 유지, preflight(id -u·info·R16·R21)·
// pre-pull, events 스트림 수신, 단절 시 백오프 재접속과 재동기화. [§7.1, §10, DESIGN §7]
//
// 설정·환경 모순(R16, R21)은 ErrFatal 로 구분한다: 시작 시에는 프로세스 시작 실패,
// 재접속 후에는 그 머신을 Failed 로 두고 재접속을 중단한다(Sink.Failed). 예산 값 자체의
// 판정(R21)은 설정을 아는 Controller 가 Spec.Verify 로 넘긴다(DESIGN §3.4, §8).
package machine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"gh-ars/internal/domain"
	"gh-ars/internal/executor"
	"gh-ars/internal/runtime"
)

// SSH 는 머신 하나의 SSH 접속 재료다. config 타입에 의존하지 않기 위해 여기서 다시 정의한다
// (의존 방향: controller → machine → executor. DESIGN §2). [§10.1]
type SSH struct {
	Host                      string
	Port                      int
	User                      string
	KeyFile                   string
	KeyPassphrase             string
	KnownHostsFile            string
	Fingerprint               string
	InsecureSkipHostKeyVerify bool
}

func (s *SSH) toConfig(log *slog.Logger) executor.SSHConfig {
	return executor.SSHConfig{
		Host: s.Host, Port: s.Port, User: s.User,
		KeyFile: s.KeyFile, KeyPassphrase: s.KeyPassphrase,
		KnownHostsFile: s.KnownHostsFile, Fingerprint: s.Fingerprint,
		InsecureSkipHostKeyVerify: s.InsecureSkipHostKeyVerify, Log: log,
	}
}

// SliceChecker 는 sidecar 모드 preflight 의 systemctl 확인이다(§10.2 규칙 4). 구현은
// systemd 패키지(Phase 12)가 채운다. [DESIGN §7 4단계]
type SliceChecker interface {
	Check(ctx context.Context) error
}

// Spec 은 에이전트를 만드는 데 필요한 설정 값이다(config 타입에 의존하지 않는다). [§6.0]
type Spec struct {
	Name    string
	Local   bool
	Runtime domain.RuntimeKind
	Mode    domain.Mode // R16 분기(§10.2 규칙 2·3, §9.2)
	// Images 는 pre-pull 대상이다: runner 이미지, sidecar 면 sidecar 이미지도. [§7.1-4]
	Images []string
	// SSH 는 Local 이 아닐 때 필수다. [§10.1]
	SSH *SSH

	// --- 조립 훅(값이 아니라 의존성이다) ---

	// Verify 는 preflight 의 info 로 머신 예산(R21)을 재판정한다. 비-nil 오류는 ErrFatal 로
	// 감싸져 Failed 가 된다. 설정 값을 아는 Controller 가 넣는다(순수 판정만 한다). [R21]
	Verify func(info runtime.Info) error
	// NewSlices 는 preflight 가 고정한 경로(executor, sudo=!root)로 SliceChecker 를 만든다.
	// nil 이고 Mode 가 sidecar 면 아직 지원하지 않는 구성이다(Phase 12). [§10.2 규칙 4]
	NewSlices func(ex executor.Executor, sudo bool) SliceChecker
	// NewExecutor 는 접속을 만든다. nil 이면 Local/SSH 기본 경로. 테스트가 대역을 넣는다.
	NewExecutor func() (executor.Executor, error)
}

// Snapshot 은 머신 한 대의 부품 관측 결과다. Controller 가 plan.Observed 로 바꾼다. [§7.1-7, §8.3]
type Snapshot struct {
	At         time.Time // 관측 시작 시각. 그 뒤에 Starting 이 된 unit 은 Creating 예외로 판정에서 뺀다 [§8.3]
	Containers []runtime.Container
	Volumes    []string
	Slices     []string
	// Info 는 이 회차의 `info` 결과다. Run 의 회차가 채운다(Observe 단독 호출은 비운다).
	// 재접속으로 healthy 가 된 머신의 예산·physicalMax·effectiveMax 를 Controller 가 다시
	// 계산해야 하기 때문이다 — 시작 시 미도달이었던 머신은 그때까지 예산이 0 이다. [§7.1-3, §8.1, R21, R22]
	Info runtime.Info
}

// Sink 는 에이전트 goroutine 이 Controller 로 보내는 통지다. 구현은 inbox 메시지로 바꾼다. [DESIGN §6, §7]
type Sink interface {
	Event(machine string, ev runtime.Event)
	Unhealthy(machine string, reason error)
	// Failed 는 재접속 후 preflight 에서 드러난 설정·환경 모순(R16, R21)이다. 이 통지 뒤
	// 에이전트 goroutine 은 종료하며 다시 접속하지 않는다. [§7.1-3, DESIGN §3.3, R21]
	Failed(machine string, reason error)
	Resynced(machine string, snap Snapshot)
}

// ErrUnsupported 는 아직 구현되지 않은 조합이다(sidecar 는 Phase 12).
var ErrUnsupported = errors.New("machine: 아직 지원하지 않는 구성")

// 코드 상수. SPEC §8.3 상수 표를 따른다.
const (
	// probeTimeout 은 즉시 끝나야 하는 확인 명령(`id -u`, `info`, `ps`, `volume ls`)과 events
	// 스트림 **열기** 의 상한이다. 스트림 유지에는 걸지 않는다. [§8.3 "preflight·관측 probe 상한"]
	probeTimeout = 30 * time.Second
	// pullTimeout 은 **머신 하나의 pre-pull 단계 전체**의 상한이다(이미지 여러 개를 받아도 합쳐서
	// 이 값이다 — 같은 머신의 이미지들은 어차피 같은 대역폭을 나눠 쓴다). 이미지 크기에 비례하므로
	// probe 보다 길지만, preflight 는 첫 통지보다 앞이고 머신별로 병렬이라(§7.1-3) 이 값이 곧 응답
	// 없는 pull 이 세션 시작을 막는 최악의 시간이다. [§7.1-4, §8.3 "pre-pull 상한"]
	pullTimeout = 5 * time.Minute
)

// Agent 는 머신 하나의 통로다. Runtime 은 Controller 가 unit 생성·정리 명령에 직접 쓴다. [DESIGN §7]
type Agent struct {
	name string
	log  *slog.Logger
	spec Spec

	// cur 는 마지막으로 성립한 접속이다. Controller goroutine(Runtime())과 Run goroutine 이
	// 공유하므로 원자적으로 교체한다. 한 번 성립하면 재접속 실패 중에도 비우지 않는다
	// (명령이 실패할 뿐이고, 정리 경로가 nil 을 만나지 않는다).
	cur atomic.Pointer[conn]
	// live 는 cur 가 살아 있는지다. Run goroutine 과 (Run 전의) Preflight 만 만진다.
	live bool
	// fixed 는 NewWithRuntime 으로 만든 에이전트다: 접속·preflight 없이 주어진 Runtime 만 쓴다.
	fixed bool
	// verified 는 Spec.Verify(R21)가 이 머신에서 한 번이라도 통과했는지다. 위반의 등급이 여기서
	// 갈린다: 최초 판정이면 ErrFatal, 이미 통과했던 머신의 재판정이면 경고다. [§7.1 재접속 회차, R21]
	verified bool
	// budgetViolated 는 마지막 판정이 위반이었는지다(전이 시점 1회만 Error 로 남기기 위해).
	budgetViolated bool
	// prePulled 는 pre-pull 이 한 번이라도 성공했는지다. 최초 접속 회차는 동기화 앞에서, 재접속
	// 회차는 동기화 뒤에서 pull 한다. [§7.1-4, §7.1 재접속 회차]
	prePulled bool
	// podmanSudo 는 첫 preflight 가 고정한 podman 경로다(§10.2 규칙 3). 재접속에서는 다시 협상하지
	// 않는다: 경로가 바뀌면 컨테이너 저장소가 통째로 달라져(rootless 저장소는 root 의 podman 에게
	// 보이지 않는다) 이미 도는 unit 이 사라진 것처럼 보이고 재동기화가 그 부품을 지운다. [§10.2 규칙 3, §8.3]
	podmanSudo *bool
}

// New 는 조립만 한다. 접속·명령 실행은 Preflight/Run 에서 한다. [DESIGN §7]
func New(spec Spec, log *slog.Logger) (*Agent, error) {
	if log == nil {
		log = slog.Default()
	}
	switch spec.Runtime {
	case domain.RuntimeDocker, domain.RuntimePodman:
	default:
		return nil, fmt.Errorf("%w: runtime %q 머신 %q", ErrUnsupported, spec.Runtime, spec.Name)
	}
	if !spec.Local && spec.SSH == nil && spec.NewExecutor == nil {
		return nil, fmt.Errorf("machine %q: SSH 설정이 없다", spec.Name)
	}
	// sidecar 는 slice·볼륨·sidecar 컨테이너를 다루는 Phase 12 가 채운다. 그때까지는 시작 시 거부한다
	// (unhealthy 로 두면 원인이 preflight 실패처럼 보인다).
	if spec.Mode == domain.ModeSidecar && spec.NewSlices == nil {
		return nil, fmt.Errorf("%w: sidecar 모드 머신 %q (Phase 12)", ErrUnsupported, spec.Name)
	}
	return &Agent{name: spec.Name, log: log.With("machine", spec.Name), spec: spec}, nil
}

// NewWithRuntime 은 준비된 Runtime 으로 에이전트를 만든다. 접속·preflight 단계가 없으므로
// events 회차만 검증하는 테스트가 쓴다.
func NewWithRuntime(spec Spec, rt runtime.Runtime, log *slog.Logger) *Agent {
	if log == nil {
		log = slog.Default()
	}
	a := &Agent{name: spec.Name, log: log.With("machine", spec.Name), spec: spec, live: true, fixed: true}
	a.cur.Store(&conn{rt: rt})
	return a
}

func (a *Agent) Name() string { return a.name }

// Runtime 은 현재 접속의 Runtime 이다. 첫 접속 전에는 nil 이다(그 머신은 아직 unhealthy 라
// 배치·정리 대상이 아니다). [§8.2, §8.3]
func (a *Agent) Runtime() runtime.Runtime {
	c := a.cur.Load()
	if c == nil {
		return nil
	}
	return c.rt
}

// Preflight 는 §7.1-3 의 접속·확인과 §7.1-4 의 pre-pull 이다. 도달 불가·명령 실패는 그대로
// 돌려주고(호출자가 unhealthy 로 처리, 프로세스는 계속), 설정·환경 모순은 ErrFatal 로 감싼다.
// 성공하면 이 접속이 이후 명령의 경로가 된다. [§7.1-3, §7.1-4, §10.2]
func (a *Agent) Preflight(ctx context.Context) (runtime.Info, error) {
	c, info, err := a.connect(ctx)
	if err != nil {
		return runtime.Info{}, err
	}
	if err := a.prePull(ctx, c); err != nil { // 6. 최초 접속 회차의 pre-pull 자리 [§7.1-4]
		_ = c.ex.Close()
		return runtime.Info{}, err
	}
	a.prePulled = true
	a.replaceConn(c)
	a.log.Info("machine connected", "runtime", a.spec.Runtime, "sudo", c.sudo, "root", c.root)
	return info, nil
}

// prePull 은 §7.1-4 다. 실패(상한 초과 포함)는 unhealthy 지만, **그 이미지가 이미 머신에 있으면
// 경고로 낮추고 성공으로 본다**: R13 이 `:latest`·태그 없음을 막고 기본 태그도 릴리스마다 고정하므로
// 캐시된 이미지가 곧 그 이미지다. 이 완화가 없으면 레지스트리 장애 하나가 멀쩡한 머신을 unhealthy 에
// 가둔다(이미지 부재와 같은 결과가 된다). [§7.1-4, R13]
func (a *Agent) prePull(ctx context.Context, c *conn) error {
	// 상한은 단계 전체에 한 번 건다. [§8.3 "pre-pull 상한"]
	pctx, pcancel := context.WithTimeout(ctx, pullTimeout)
	defer pcancel()
	for _, img := range a.spec.Images {
		err := c.rt.Pull(pctx, img)
		if err == nil {
			a.log.Info("image pulled", "image", img)
			continue
		}
		// 로컬 이미지 확인은 상한 ctx 가 아니라 **부모 ctx** 로 돈다: 상한 초과로 pctx 가 만료된
		// 뒤에 그것으로 확인하면 위 완화가 정확히 필요한 순간(레지스트리가 응답하지 않아 상한에
		// 걸린 순간)에 무력해진다. [§7.1-4]
		ictx, icancel := context.WithTimeout(ctx, probeTimeout) // 확인 명령 1회당 상한 [§8.3]
		ok, exErr := c.rt.ImageExists(ictx, img)
		icancel()
		if ok {
			a.log.Warn("image pull failed, using the image already on the machine", "image", img, "err", err)
			continue
		}
		if exErr != nil {
			err = errors.Join(err, exErr)
		}
		return fmt.Errorf("pre-pull %s: %w", img, err)
	}
	return nil
}

// judgeBudget 은 그 회차의 info 로 R21 을 재판정한다(§7.1 "재접속 회차가 다시 도는 범위").
// 위반의 등급은 재접속 여부가 아니라 **그 머신을 처음 판정하는가**로 갈린다:
//   - 최초 판정에서 위반 → ErrFatal(시작 시 시작 실패, 시작 시 미도달이었으면 Failed).
//   - 이미 통과했던 머신의 재판정에서 위반 → 오류가 아니다. 회차는 그대로 진행하고 Resynced 에
//     그 info 를 실어 보낸다. Controller 의 applyInfo 가 physicalMax 0 을 계산해 반영하므로 배치는
//     그것만으로 막히고, Unhealthy 로 내릴 때 따라오는 해악(Dying 정리 보류, die 수신 단절)이 없다.
//
// 로그는 전이 시점 1회만 Error, 이어지는 회차는 Debug 다(30s 마다 같은 오류를 찍지 않는다). [§7.1, R21]
func (a *Agent) judgeBudget(info runtime.Info) error {
	if a.spec.Verify == nil {
		return nil
	}
	err := a.spec.Verify(info)
	switch {
	case err == nil:
		if a.budgetViolated {
			a.log.Info("machine budget recovered", "cpus", info.CPUs, "memoryBytes", info.MemoryBytes)
		}
		a.verified, a.budgetViolated = true, false
	case !a.verified:
		return fatalf("머신 %q: %s", a.name, err)
	case !a.budgetViolated:
		a.budgetViolated = true
		a.log.Error("machine budget shrank below unit budget, placement blocked", "err", err)
	default:
		a.log.Debug("machine budget still below unit budget", "err", err)
	}
	return nil
}

// replaceConn 은 새 접속으로 교체하고 이전 접속을 닫는다.
func (a *Agent) replaceConn(c *conn) {
	old := a.cur.Swap(c)
	if old != nil && old.ex != nil && old != c {
		_ = old.ex.Close()
	}
	a.live = true
}

// dropConn 은 회차 실패 후 접속을 닫는다. cur 는 비우지 않는다(다음 접속이 교체한다).
func (a *Agent) dropConn() {
	if a.fixed {
		return
	}
	a.live = false
	if c := a.cur.Load(); c != nil && c.ex != nil {
		_ = c.ex.Close()
	}
}

// Close 는 접속을 닫는다(§7.3 "SSH 연결 정리"). 여러 번 불러도 안전하다. Controller 는 Run 이
// 어떤 경로로 끝나든(시작 실패 포함) 만들어 둔 에이전트마다 이것을 부른다.
func (a *Agent) Close() error {
	if c := a.cur.Swap(nil); c != nil && c.ex != nil {
		return c.ex.Close()
	}
	return nil
}

// Observe 는 전체 동기화 재료를 모은다: `ps -a --filter label=gh-ars.unit`, 같은 필터의 볼륨.
// slice 목록은 systemd 패키지(Phase 12)가 붙을 때 채운다. [§7.1-7, §4.2]
func (a *Agent) Observe(ctx context.Context) (Snapshot, error) {
	rt := a.Runtime()
	if rt == nil {
		return Snapshot{}, errors.New("observe: 접속 없음")
	}
	// 상한은 관측 명령 1회당 건다. [§8.3 "preflight·관측 probe 상한"]
	at := time.Now()
	lctx, lcancel := context.WithTimeout(ctx, probeTimeout)
	ctrs, err := rt.List(lctx, domain.UnitLabelFilter)
	lcancel()
	if err != nil {
		return Snapshot{}, fmt.Errorf("observe: %w", err)
	}
	vctx, vcancel := context.WithTimeout(ctx, probeTimeout)
	vols, err := rt.VolumeList(vctx, domain.UnitLabelFilter)
	vcancel()
	if err != nil {
		return Snapshot{}, fmt.Errorf("observe: %w", err)
	}
	return Snapshot{At: at, Containers: ctrs, Volumes: vols}, nil
}

// Run 은 접속과 events 스트림을 유지한다. 회차마다 (필요하면) 재접속·preflight 를 하고 events 를
// 연 뒤 전체 동기화 스냅샷(Resynced)을 보내고 스트림을 소비한다. 스트림이 끝나면 unhealthy 를
// 알리고 백오프로 재시작한다. ctx 취소까지 돌아간다. [§7.1-7, §7.1-8, §10.1]
//
// 순서: events 를 먼저 열고 그 다음 Observe 한다. 반대로 하면 그 사이의 die 를 놓친다. 시작 시(§7.1-7)에도
// 같은 회차를 타므로 Controller 는 첫 Resynced 를 "events 준비 완료 + 동기화 완료" 신호로 쓴다.
// 첫 회차는 Resynced 또는 Unhealthy/Failed 중 하나를 반드시 보낸다(Controller 가 그것을 기다린다).
//
// 재접속 preflight 에서 R16/R21 위반이 드러나면 Failed 를 보내고 goroutine 을 끝낸다(재접속 없음). [§7.1-3, R21]
func (a *Agent) Run(ctx context.Context, sink Sink) {
	var bo Backoff
	first := true
	for {
		r := a.serve(ctx, sink)
		if ctx.Err() != nil {
			return
		}
		// 회차가 실패했으면 접속을 버리고 다음 회차에서 preflight 부터 다시 한다(재접속). SSH 는
		// 스트림이 정상적으로 끝난 경우에도 버린다: events 를 싣고 있던 연결이므로 §10.1 의 재접속
		// 경로(host key 검증 → preflight → 전체 동기화)를 그대로 탄다. local 은 연결이 없으므로
		// healthy 였던 회차 뒤에는 events 재시작만 한다. [§7.1-8, §10.1]
		if !r.served || !a.spec.Local {
			a.dropConn()
		}
		if errors.Is(r.err, ErrFatal) {
			a.log.Error("machine failed permanently, no reconnect", "err", r.err)
			sink.Failed(a.name, r.err)
			return
		}
		if r.served || first {
			// healthy 였던 스트림이 끝났다(또는 첫 회차가 실패했다): 재시작이 성공할 때까지 unhealthy.
			// 다음 성공에서 Resynced 가 healthy 로 되돌린다.
			sink.Unhealthy(a.name, r.err)
		}
		if r.resetsBackoff() {
			bo.Reset()
		}
		first = false
		a.log.Warn("machine unhealthy, will retry", "err", r.err, "streamLife", r.life)
		if bo.Wait(ctx) != nil {
			return
		}
	}
}

// round 는 회차 하나의 결과다.
type round struct {
	served bool          // Resynced 까지 갔는가(= 그 회차가 healthy 였는가)
	life   time.Duration // events 스트림이 열려 있던 시간
	err    error
}

// resetsBackoff 는 이 회차가 백오프 수열을 되돌릴 자격이 있는지다. Resynced 를 보낸 것만으로는
// 부족하다: 데몬이 뜨자마자 이벤트 한 건 내고 죽는 플래핑에서 매 회차 1s 로 리셋되면 지수 백오프가
// 무력화된다. listener 세션과 같은 기준으로 스트림이 백오프 최대값 이상 유지된 회차만 성공으로 본다.
// [§7.1-8 "성공 시 리셋", DESIGN §7, DESIGN §6 listener 세션]
func (r round) resetsBackoff() bool { return r.served && r.life >= BackoffMax }

// serve 는 회차 하나다: (필요 시) 접속·preflight → events 열기 → info 로 데몬 생존 확인 →
// Observe → Resynced → 소비. 스트림이 끝난 이유를 돌려준다(정상 종료는 없다).
//
// life 는 events 스트림이 열려 있던 시간이다(회차 전체가 아니다: pre-pull 이 오래 걸린 회차가
// 스트림 즉사에도 리셋 자격을 얻으면 안 된다). 백오프 리셋 판정은 round.resetsBackoff.
func (a *Agent) serve(ctx context.Context, sink Sink) round {
	// pre-pull 의 자리는 최초 접속인지로 갈린다: 최초 접속 회차는 동기화 앞(콜드 머신은 이미지가
	// 실제로 없다), 이미 한 번 pull 에 성공했던 머신의 재접속 회차는 Resynced 뒤로 미룬다 —
	// 재동기화와 die 수신이 레지스트리 가용성에 인질로 잡히면 안 된다. [§7.1 재접속 회차, §7.1-4]
	var deferredPull *conn
	if !a.live {
		if a.prePulled {
			c, _, err := a.connect(ctx)
			if err != nil {
				return round{err: err}
			}
			a.replaceConn(c)
			a.log.Info("machine reconnected", "runtime", a.spec.Runtime, "sudo", c.sudo, "root", c.root)
			deferredPull = c
		} else if _, err := a.Preflight(ctx); err != nil {
			return round{err: err}
		}
	}
	rt := a.Runtime()
	if rt == nil { // Close 이후. 다음 회차가 다시 접속한다
		a.live = false
		return round{err: errors.New("접속 없음")}
	}
	ectx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	// events 스트림 자체에는 상한이 없어야 하지만 **여는 동안**은 원격 응답을 기다린다(SSH 는 채널
	// 개설·exec ack). 여기서 매달리면 이 머신의 첫 통지가 오지 않아 모든 scale set 의 세션 시작이
	// 막히므로(§7.1-7 → §7.1-9), 열기 구간만 타이머로 ectx 를 취소해 상한을 건다. ectx 는 이 회차의
	// 뒤따르는 단계까지 덮으므로 취소 사유 규약을 쓴다(아래 watchdog/reported). [§7.1-8, §10.1]
	stopOpenTimer := watchdog(cancel, probeTimeout, fmt.Errorf("%w: %v", errEventsOpenTimeout, probeTimeout))
	evCh, errCh := rt.Events(ectx, domain.UnitLabelFilter)
	stopOpenTimer()
	openedAt := time.Now()
	life := func() time.Duration { return time.Since(openedAt) }
	// 즉시 끝나야 하는 확인 명령에만 상한을 건다.
	ictx, icancel := context.WithTimeout(ectx, probeTimeout)
	info, err := rt.Info(ictx)
	icancel()
	if err != nil {
		cancel(nil)
		// 열기 상한이 ectx 를 취소했으면 Info 도 "context canceled" 로 끝난다. 그것을 그대로
		// 보고하면 데몬이 죽은 것처럼 보이므로 reported 가 실제 사유로 되돌린다.
		e := reported(ectx, fmt.Errorf("info: %w", err))
		// events 열기도 함께 실패했으면 그 사유를 버리지 않는다(같은 회차의 원인이 둘이다).
		// 우리가 방금 건 취소는 사유가 아니므로 뺀다.
		if evErr := drain(evCh, errCh); evErr != nil && !errors.Is(evErr, context.Canceled) {
			e = errors.Join(e, fmt.Errorf("events: %w", evErr))
		}
		return round{life: life(), err: e}
	}
	// 회차마다 그 회차의 info 로 예산을 다시 판정한다(R21): preflight 를 건너뛰는 회차(local 의
	// events 재시작)에도 예산은 바뀔 수 있다. 위반의 등급은 judgeBudget 이 가른다. [§7.1, R21]
	if err := a.judgeBudget(info); err != nil {
		cancel(nil)
		drain(evCh, errCh)
		return round{life: life(), err: err}
	}
	snap, err := a.Observe(ectx)
	if err != nil {
		cancel(nil)
		drain(evCh, errCh)
		return round{life: life(), err: reported(ectx, err)}
	}
	snap.Info = info // 재접속으로 healthy 가 된 머신의 예산 재계산 재료 [§7.1-3, R21, R22]
	// 스트림이 열리자마자 끝났으면(열기 실패) healthy 로 보고하지 않는다. Runtime.Events 는 열기 실패를
	// 이미 닫힌 채널 + errCh 로 알린다. [§7.1-8]
	select {
	case err := <-errCh:
		// 취소부터 하고 비운다. Runtime.Events 계약은 "errCh 통지 후 두 채널을 닫는다" 지만,
		// 그 계약을 어기는(또는 아직 닫는 중인) 구현을 만나면 여기서 영원히 멈춰 이 머신은
		// 재접속도 Unhealthy 통지도 하지 못한다. 취소하면 어느 구현이든 스트림이 풀린다. [§7.1-8]
		cancel(nil)
		drain(evCh, nil)
		return round{life: life(), err: fmt.Errorf("events: %w", err)}
	default:
	}
	sink.Resynced(a.name, snap)
	// 미뤄 둔 pre-pull 은 이벤트 소비와 **나란히** 돈다. 순서만 미루고 동기로 돌리면 die 수신이
	// 여전히 레지스트리에 묶인다(evCh 는 무버퍼라 Events goroutine 이 첫 이벤트에서 막힌다).
	// 실패하면 스트림은 이미 열려 있고 동기화도 끝난 뒤이므로 "Resynced 를 보낸 회차의 실패"로
	// 보고한다(Run 이 Unhealthy 를 보낸다). [§7.1 재접속 회차, §7.1-4]
	var pullCh <-chan error
	if deferredPull != nil {
		ch := make(chan error, 1)
		go func() { ch <- a.prePull(ectx, deferredPull) }()
		pullCh = ch
	}
	for {
		select {
		case ev, ok := <-evCh:
			if !ok { // 스트림 종료 = 회차 종료. 남아 있는 pull 은 ectx 취소로 접는다
				cancel(nil)
				if pullCh != nil {
					<-pullCh
				}
				return round{served: true, life: life(), err: <-errCh}
			}
			sink.Event(a.name, ev)
		case err := <-pullCh:
			pullCh = nil // nil 채널은 select 에서 영원히 막힌다 = 이 case 가 빠진다
			if err != nil {
				cancel(nil)
				drain(evCh, errCh)
				return round{served: true, life: life(), err: reported(ectx, err)}
			}
			a.prePulled = true
		}
	}
}

// errEventsOpenTimeout 은 events 열기 상한이 회차 ctx 를 취소한 사유다. [§8.3 "preflight·관측 probe 상한"]
var errEventsOpenTimeout = errors.New("events 열기 상한 초과")

// 취소 사유 규약 [DESIGN §7]. 마감 ctx 는 그것이 감싸는 작업 **하나**에만 준다 — 그러면 사유가 곧
// context.DeadlineExceeded 다. 여러 단계를 덮는 ctx 를 타이머로 취소해야 하는 자리(여기서는 events
// 열기 상한이 회차 ctx 를 취소한다)에서만 아래 두 함수를 쓴다: 취소한 주체가 사유를 남기고,
// 그 ctx 하위에서 나온 "context canceled" 는 원인으로 보고하지 않고 사유로 치환한다.
// 그러지 않으면 뒤따르는 단계가 전부 취소로 끝나 진짜 원인을 가린다.

// watchdog 은 d 안에 stop 이 불리지 않으면 reason 을 사유로 ctx 를 취소한다.
func watchdog(cancel context.CancelCauseFunc, d time.Duration, reason error) (stop func()) {
	t := time.AfterFunc(d, func() { cancel(reason) })
	return func() { t.Stop() }
}

// reported 는 규약의 보고 쪽이다: ctx 가 우리가 남긴 사유로 취소됐으면 그 사유를, 아니면 관측한
// 오류를 그대로 돌려준다. 사유 없는 취소(부모 ctx 종료, cancel(nil))는 치환하지 않는다.
func reported(ctx context.Context, err error) error {
	cause := context.Cause(ctx)
	if cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	return err
}

// drain 은 취소한 events 스트림을 끝까지 비운다(goroutine 누수 방지). 스트림 종료 사유를
// 돌려주므로 회차 실패 원인이 둘인 경우에 그것을 잃지 않는다(errCh 가 nil 이면 nil).
func drain(evCh <-chan runtime.Event, errCh <-chan error) error {
	for range evCh {
	}
	if errCh == nil {
		return nil
	}
	return <-errCh
}
