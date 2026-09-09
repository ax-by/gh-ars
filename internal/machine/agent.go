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

// probeTimeout 은 즉시 끝나야 하는 확인 명령(`id -u`, `info`, `ps`, `volume ls`) 한 묶음의 상한이다.
// SPEC 은 이 값을 정하지 않지만 상한이 없으면 매달린 명령 하나가 그 머신의 첫 동기화를 막고,
// Controller 는 머신마다 첫 통지를 기다리므로(§7.1-7 → §7.1-9) 모든 scale set 의 세션 시작이 영영
// 오지 않는다. pre-pull(이미지 크기에 비례)과 events(장기 스트림)에는 걸지 않는다. tick 간격과 같은 값. [§8.3 상수 표]
const probeTimeout = 30 * time.Second

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
	for _, img := range a.spec.Images { // 6. pre-pull. 실패 → unhealthy [§7.1-4]
		if err := c.rt.Pull(ctx, img); err != nil {
			_ = c.ex.Close()
			return runtime.Info{}, fmt.Errorf("pre-pull %s: %w", img, err)
		}
		a.log.Info("image pulled", "image", img)
	}
	a.replaceConn(c)
	a.log.Info("machine connected", "runtime", a.spec.Runtime, "sudo", c.sudo, "root", c.root)
	return info, nil
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
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	at := time.Now()
	ctrs, err := rt.List(ctx, domain.UnitLabelFilter)
	if err != nil {
		return Snapshot{}, fmt.Errorf("observe: %w", err)
	}
	vols, err := rt.VolumeList(ctx, domain.UnitLabelFilter)
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
	if !a.live {
		if _, err := a.Preflight(ctx); err != nil {
			return round{err: err}
		}
	}
	rt := a.Runtime()
	if rt == nil { // Close 이후. 다음 회차가 다시 접속한다
		a.live = false
		return round{err: errors.New("접속 없음")}
	}
	ectx, cancel := context.WithCancel(ctx)
	defer cancel()
	// events 스트림 자체에는 상한이 없어야 하지만 **여는 동안**은 원격 응답을 기다린다(SSH 는 채널
	// 개설·exec ack). 여기서 매달리면 이 머신의 첫 통지가 오지 않아 모든 scale set 의 세션 시작이
	// 막히므로(§7.1-7 → §7.1-9), 열기 구간만 타이머로 ectx 를 취소해 상한을 건다. [§7.1-8, §10.1]
	openTimer := time.AfterFunc(probeTimeout, cancel)
	evCh, errCh := rt.Events(ectx, domain.UnitLabelFilter)
	openTimer.Stop()
	openedAt := time.Now()
	life := func() time.Duration { return time.Since(openedAt) }
	// 즉시 끝나야 하는 확인 명령에만 상한을 건다.
	ictx, icancel := context.WithTimeout(ectx, probeTimeout)
	info, err := rt.Info(ictx)
	icancel()
	if err != nil {
		cancel()
		drain(evCh, errCh)
		return round{life: life(), err: fmt.Errorf("info: %w", err)}
	}
	// 회차마다 그 회차의 info 로 예산을 다시 판정한다(R21). preflight 를 건너뛰는 회차(local 의
	// events 재시작)에도 위반이 드러나면 Failed 가 되어 goroutine 이 끝나야 하기 때문이다. [§7.1-3, R21]
	if a.spec.Verify != nil {
		if err := a.spec.Verify(info); err != nil {
			cancel()
			drain(evCh, errCh)
			return round{life: life(), err: fatalf("머신 %q: %s", a.name, err)}
		}
	}
	snap, err := a.Observe(ectx)
	if err != nil {
		cancel()
		drain(evCh, errCh)
		return round{life: life(), err: err}
	}
	snap.Info = info // 재접속으로 healthy 가 된 머신의 예산 재계산 재료 [§7.1-3, R21, R22]
	// 스트림이 열리자마자 끝났으면(열기 실패) healthy 로 보고하지 않는다. Runtime.Events 는 열기 실패를
	// 이미 닫힌 채널 + errCh 로 알린다. [§7.1-8]
	select {
	case err := <-errCh:
		drain(evCh, nil)
		return round{life: life(), err: fmt.Errorf("events: %w", err)}
	default:
	}
	sink.Resynced(a.name, snap)
	for ev := range evCh {
		sink.Event(a.name, ev)
	}
	return round{served: true, life: life(), err: <-errCh}
}

// drain 은 취소한 events 스트림을 끝까지 비운다(goroutine 누수 방지).
func drain(evCh <-chan runtime.Event, errCh <-chan error) {
	for range evCh {
	}
	if errCh != nil {
		<-errCh
	}
}
