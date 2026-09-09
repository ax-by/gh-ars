package machine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gh-ars/internal/domain"
	"gh-ars/internal/executor"
	"gh-ars/internal/runtime"
)

// ErrFatal 은 preflight 가 찾아낸 설정·환경 모순(R16, R21)이다. 시작 시에는 프로세스 시작
// 실패, 재접속 후에는 그 머신을 Failed(영구 제외, 재접속 안 함)로 만든다. 도달 불가·명령
// 실패(unhealthy)와 구분하기 위한 표식이다. [§7.1-3, DESIGN §3.3, R16, R21]
var ErrFatal = errors.New("machine: 설정·환경 모순")

func fatalf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrFatal, fmt.Sprintf(format, a...))
}

// 기대값. sidecar 모드 전제(§9.2). podman 의 "v2" 는 runtime 이 "2" 로 정규화한다.
const (
	cgroupDriverSystemd = "systemd"
	cgroupVersion2      = "2"
)

// conn 은 한 번의 접속으로 고정된 실행 경로다: executor 와 그 위의 runtime(§10.2 규칙 3의
// podman sudo 경로 포함). 재접속마다 통째로 교체한다. [§10.1, §10.2]
type conn struct {
	ex   executor.Executor
	rt   runtime.Runtime
	sudo bool // podman 경로 고정 결과. docker 는 항상 false [§10.2 규칙 2·3]
	root bool // id -u == 0. systemctl 의 sudo 여부를 정한다 [§10.2 규칙 1·4]
}

// connect 는 §7.1-3 의 preflight 순서다(DESIGN §7): 접속 → `id -u` → runtime `info`(경로 고정)
// → sidecar R16 → R21 재판정. pre-pull(§7.1-4)은 호출자 Preflight 가 이어서 한다.
func (a *Agent) connect(ctx context.Context) (*conn, runtime.Info, error) {
	ex, err := a.newExecutor() // 1. SSH 머신은 host key 검증 후 접속(타임아웃 10s), local 은 생략 [§10.1]
	if err != nil {
		return nil, runtime.Info{}, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = ex.Close()
		}
	}()
	// 2~5단계는 전부 즉시 끝나야 하는 확인 명령이다. 하나가 매달리면 이 머신의 첫 동기화가
	// 막히고 세션 시작까지 밀리므로 상한을 건다(pre-pull 은 호출자가 상한 없이 한다).
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	root, err := isRoot(ctx, ex) // 2. [§10.2 규칙 1]
	if err != nil {
		return nil, runtime.Info{}, err
	}
	rt, sudo, info, err := a.fixRuntimePath(ctx, ex) // 3. [§10.2 규칙 2·3]
	if err != nil {
		return nil, runtime.Info{}, err
	}
	if a.spec.Mode == domain.ModeSidecar { // 4. [R16, §9.2, §10.2 규칙 4]
		if err := a.checkSidecar(ctx, ex, info, root); err != nil {
			return nil, runtime.Info{}, err
		}
	}
	if a.spec.Verify != nil { // 5. 머신 예산 판정(R21)은 설정을 아는 Controller 가 넘긴다 [§7.1-3, R21]
		if err := a.spec.Verify(info); err != nil {
			return nil, runtime.Info{}, fatalf("머신 %q: %s", a.name, err)
		}
	}
	ok = true
	return &conn{ex: ex, rt: rt, sudo: sudo, root: root}, info, nil
}

// newExecutor 는 이 머신의 실행 통로를 만든다. local 은 상태가 없고, SSH 는 접속을 맺는다.
// 접속 실패는 도달 불가이므로 unhealthy(재접속 대상)다. [§10.1]
func (a *Agent) newExecutor() (executor.Executor, error) {
	if a.spec.NewExecutor != nil {
		return a.spec.NewExecutor()
	}
	if a.spec.Local {
		return executor.NewLocal(), nil
	}
	return executor.NewSSH(a.spec.SSH.toConfig(a.log))
}

// isRoot 는 `id -u` 로 root 여부를 본다. 명령 자체가 실패하면 도달 불가로 본다. [§10.2 규칙 1]
func isRoot(ctx context.Context, ex executor.Executor) (bool, error) {
	res, err := ex.Run(ctx, executor.Cmd{Argv: []string{"id", "-u"}})
	if err != nil {
		return false, fmt.Errorf("preflight: id -u: %w", err)
	}
	if res.ExitCode != 0 {
		return false, fmt.Errorf("preflight: id -u: 종료 코드 %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return strings.TrimSpace(string(res.Stdout)) == "0", nil
}

// fixRuntimePath 는 §10.2 규칙 2·3 이다. podman 은 성공한 경로(sudo 없음 / `sudo -n`)를 고정해
// 이후 **모든** 명령에 같은 경로를 쓴다. 반환한 Runtime 이 그 고정 결과다.
func (a *Agent) fixRuntimePath(ctx context.Context, ex executor.Executor) (runtime.Runtime, bool, runtime.Info, error) {
	switch a.spec.Runtime {
	case domain.RuntimeDocker:
		// 규칙 2: docker 는 sudo 를 쓰지 않는다(docker 그룹 멤버 또는 root).
		rt := runtime.NewDocker(ex, false)
		info, err := rt.Info(ctx)
		if err != nil {
			return nil, false, runtime.Info{}, a.infoFailure("docker info", err)
		}
		return rt, false, info, nil

	case domain.RuntimePodman:
		// 이미 고정된 경로가 있으면(재접속) 그 경로만 쓴다. 실패는 도달 불가일 뿐이므로 다른 저장소로
		// 갈아타지 않고 unhealthy 로 둔다(다음 재접속에서 같은 경로를 다시 시도한다). [§10.2 규칙 3]
		if a.podmanSudo != nil {
			sudo := *a.podmanSudo
			rt := runtime.NewPodman(ex, sudo)
			info, err := rt.Info(ctx)
			if err != nil {
				return nil, false, runtime.Info{}, a.infoFailure("podman info", err)
			}
			if a.spec.Mode == domain.ModeSidecar && info.Rootless {
				return nil, false, runtime.Info{}, fatalf("R16 머신 %q: sidecar 모드는 rootful podman 이 필요하다(고정된 경로가 rootless)", a.name)
			}
			return rt, sudo, info, nil
		}
		// 규칙 3: `podman info` → 실패하면 `sudo -n podman info`. 성공한 쪽을 고정한다.
		sudo := false
		rt := runtime.NewPodman(ex, false)
		info, err := rt.Info(ctx)
		if err != nil {
			sudo = true
			rt = runtime.NewPodman(ex, true)
			info2, err2 := rt.Info(ctx)
			if err2 != nil {
				return nil, false, runtime.Info{}, a.infoFailure("podman info", errors.Join(err, err2))
			}
			info = info2
		}
		// sidecar 추가 규칙: rootless podman 은 sidecar 에 못 쓴다. 아직 시도하지 않은
		// `sudo -n podman info` 로 rootful 을 얻으면 그 경로로 바꾸고, 못 얻으면 R16 오류.
		if a.spec.Mode == domain.ModeSidecar && info.Rootless {
			if !sudo {
				sudoRT := runtime.NewPodman(ex, true)
				if sudoInfo, err := sudoRT.Info(ctx); err == nil && !sudoInfo.Rootless {
					a.log.Info("podman path fixed", "sudo", true, "rootless", false)
					a.podmanSudo = ptr(true)
					return sudoRT, true, sudoInfo, nil
				}
			}
			return nil, false, runtime.Info{}, fatalf("R16 머신 %q: sidecar 모드는 rootful podman 이 필요하다(고정된 경로가 rootless)", a.name)
		}
		a.log.Info("podman path fixed", "sudo", sudo, "rootless", info.Rootless)
		a.podmanSudo = ptr(sudo)
		return rt, sudo, info, nil
	}
	return nil, false, runtime.Info{}, fmt.Errorf("%w: runtime %q 머신 %q", ErrUnsupported, a.spec.Runtime, a.name)
}

// infoFailure 는 `info` 실패의 분류다: none 은 unhealthy(재시도), sidecar 는 R16 오류. [§10.2 규칙 2·3]
func (a *Agent) infoFailure(what string, err error) error {
	if a.spec.Mode == domain.ModeSidecar {
		return fatalf("R16 머신 %q: %s 실패: %s", a.name, what, err)
	}
	return fmt.Errorf("preflight: %s: %w", what, err)
}

// checkSidecar 는 sidecar scale set 머신의 R16 전제다: cgroup v2 + systemd 드라이버, systemctl 사용 가능.
// 미충족은 전부 R16 오류다. [R16, §9.2, §10.2 규칙 4]
func (a *Agent) checkSidecar(ctx context.Context, ex executor.Executor, info runtime.Info, root bool) error {
	if info.CgroupDriver != cgroupDriverSystemd || info.CgroupVersion != cgroupVersion2 {
		return fatalf("R16 머신 %q: cgroup %s/%s, sidecar 모드는 %s/%s 가 필요하다",
			a.name, info.CgroupDriver, info.CgroupVersion, cgroupDriverSystemd, cgroupVersion2)
	}
	if a.spec.NewSlices == nil {
		return fmt.Errorf("%w: sidecar 모드 머신 %q (Phase 12)", ErrUnsupported, a.name)
	}
	// 규칙 4: systemctl 은 root 가 아니면 항상 `sudo -n`.
	if err := a.spec.NewSlices(ex, !root).Check(ctx); err != nil {
		return fatalf("R16 머신 %q: systemctl 확인 실패: %s", a.name, err)
	}
	return nil
}

func ptr[T any](v T) *T { return &v }
