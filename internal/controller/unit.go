package controller

import (
	"context"
	"errors"
	"fmt"

	"gh-ars/internal/domain"
	"gh-ars/internal/runtime"
)

// errSidecarUnsupported: sidecar 모드(볼륨·slice·sidecar 컨테이너)는 Phase 12. [§9]
var errSidecarUnsupported = errors.New("controller: sidecar 모드는 아직 지원하지 않는다 (Phase 12)")

// startUnit 은 goroutine 이다. 상태를 직접 만지지 않고 결과를 msgUnitStarted 로 보낸다. [§7.2-4, DESIGN §6]
//
//	JIT 생성(GitHub) → runner 컨테이너 Create(entrypoint 래퍼, 라벨, 리소스) → CopyIn(jittar) → Start
//
// 전체에 기동 타임아웃 2분. 실패하면 만든 부품을 §8.3 정리 순서(GetRunner → RemoveRunner 포함)로 되돌린다.
// JIT 값은 tar 스트림(stdin)으로만 전달하고 로그·argv·env 어디에도 남기지 않는다.
func (c *Controller) startUnit(u domain.Unit, ss domain.ScaleSet, rt runtime.Runtime) {
	ctx, cancel := context.WithTimeout(c.ctx, startTimeout)
	defer cancel()
	log := c.log.With("unit", u.ID, "machine", u.Machine, "runner", u.RunnerName)

	result := msgUnitStarted{Unit: u.ID, ScaleSet: u.ScaleSet, RunnerName: u.RunnerName}
	encoded, ref, err := c.gh.GenerateJIT(ctx, ss.GitHubID, u.RunnerName)
	if err != nil {
		result.Err = err
		c.send(result)
		return
	}
	result.RunnerID = ref.ID
	log.Debug("jit generated", "runnerID", ref.ID, "jitLen", len(encoded))

	err = c.launchRunner(ctx, u, ss, rt, encoded)
	if err != nil {
		if c.ctx.Err() != nil {
			// 프로세스 종료로 중단됐다. 컨테이너가 이미 start 됐을 수 있으므로 되돌리지 않는다
			// (§7.3: 실행 중 컨테이너는 kill 하지 않는다). 재시작 시 입양 또는 exited 정리(§8.3).
			log.Warn("unit start interrupted by shutdown, leaving parts for adoption", "err", err)
			result.Err = err
			c.send(result)
			return
		}
		log.Warn("unit start failed, rolling back", "err", err)
		// 되돌리기: 취소된 ctx 로는 명령이 안 나가므로 분리한 ctx 를 쓴다. [DESIGN §6 startUnit]
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(c.ctx), cleanupTimeout)
		defer rcancel()
		if rerr := c.cleanupParts(rctx, u, rt); rerr != nil {
			log.Warn("rollback incomplete, cleanup will retry", "err", rerr)
		}
		result.Err = err
		c.send(result)
		return
	}
	c.send(result)
}

// launchRunner 는 create → cp → start 3단계다. attach 의미론에 의존하지 않는다. [§7.2-4]
func (c *Controller) launchRunner(ctx context.Context, u domain.Unit, ss domain.ScaleSet, rt runtime.Runtime, encoded string) error {
	if u.Mode != domain.ModeNone {
		return errSidecarUnsupported
	}
	entrypoint, cmd, err := runtime.Wrapper(u.Mode)
	if err != nil {
		return err
	}
	name := domain.ContainerName(u.ID, domain.RoleRunner)
	spec := runtime.CreateSpec{
		Name:        name,
		Image:       ss.RunnerImage,
		Labels:      domain.Labels(u, domain.RoleRunner),
		Entrypoint:  entrypoint,
		Cmd:         cmd,
		CPUs:        ss.Unit.CPU,         // none 모드: 컨테이너 1개에 예산 [§8.1, §9.3 표]
		MemoryBytes: ss.Unit.MemoryBytes, // 〃
	}
	if err := rt.Create(ctx, spec); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	tar, err := runtime.JITTar(encoded)
	if err != nil {
		return err
	}
	if err := rt.CopyIn(ctx, name, tar, runtime.RunnerHome); err != nil {
		return fmt.Errorf("cp: %w", err)
	}
	if err := rt.Start(ctx, name); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	return nil
}

// cleanupUnit 은 goroutine 이다. §8.3 정리 순서를 실행하고 msgCleanupDone 을 보낸다. [§8.3, DESIGN §6]
func (c *Controller) cleanupUnit(u domain.Unit, rt runtime.Runtime) {
	ctx, cancel := context.WithTimeout(c.ctx, cleanupTimeout)
	defer cancel()
	err := c.cleanupParts(ctx, u, rt)
	c.send(msgCleanupDone{Unit: u.ID, Err: err})
}

// cleanupParts 는 unit 정리 순서다. 각 단계는 "이미 없음" 을 성공으로 보는 멱등 단계라 재시도가 안전하다. [§8.3]
//
//	GetRunner(RunnerName) → (있으면) RemoveRunner → runner 컨테이너 rm → sidecar rm → 볼륨 3개 rm → slice stop + revert
//
// 등록 제거를 컨테이너 rm 앞에 두는 이유: runner 목록 API 가 없어 컨테이너가 사라지면 등록을 찾을 방법이 없다(DECISIONS).
func (c *Controller) cleanupParts(ctx context.Context, u domain.Unit, rt runtime.Runtime) error {
	log := c.log.With("unit", u.ID, "machine", u.Machine)
	if u.RunnerName != "" { // 고아 부품 집합(runner 이름 없음)은 GitHub 단계가 없다 [§8.3]
		ref, found, err := c.gh.GetRunner(ctx, u.RunnerName)
		if err != nil {
			return fmt.Errorf("cleanup: %w", err)
		}
		if found {
			if err := c.gh.RemoveRunner(ctx, ref.ID); err != nil {
				return fmt.Errorf("cleanup: %w", err)
			}
			log.Info("runner registration removed", "runner", u.RunnerName, "runnerID", ref.ID)
		} else {
			log.Debug("runner registration absent", "runner", u.RunnerName)
		}
	}
	if err := rt.Remove(ctx, domain.ContainerName(u.ID, domain.RoleRunner), true); err != nil {
		return fmt.Errorf("cleanup runner container: %w", err)
	}
	if u.Mode != domain.ModeSidecar && !u.Parts.Sidecar && !u.Parts.Slice && u.Parts.Volumes == [3]bool{} {
		return nil
	}
	if err := rt.Remove(ctx, domain.ContainerName(u.ID, domain.RoleSidecar), true); err != nil {
		return fmt.Errorf("cleanup sidecar container: %w", err)
	}
	for _, v := range domain.VolumeNames(u.ID) {
		if err := rt.VolumeRemove(ctx, v); err != nil {
			return fmt.Errorf("cleanup volume %s: %w", v, err)
		}
	}
	if u.Parts.Slice {
		// slice stop + revert 는 systemd 패키지(Phase 12). 그때까지 Dying 으로 남아 tick 마다 재시도된다.
		return fmt.Errorf("cleanup slice %s: %w", domain.SliceName(u.ID), errSidecarUnsupported)
	}
	return nil
}

// checkRegistrations 는 goroutine 이다. tick 이 고른 unit 마다 GetRunner 로 등록을 대조한다.
// 호출은 순차다: scaleset.Client 가 Actions 서비스 호출을 인스턴스 하나짜리 뮤텍스로 직렬화하므로
// 흩어도 벽시계 시간이 줄지 않고, 뮤텍스를 더 오래 물어 주 경로(JIT 생성, RemoveRunner)만 민다.
// 끝나면 회차 종료를 알린다 — 다음 tick 은 그때까지 새 회차를 띄우지 않는다. [§8.3, DESIGN §4.4]
func (c *Controller) checkRegistrations(units []domain.Unit) {
	defer c.send(msgCheckPassDone{})
	for _, u := range units {
		ctx, cancel := context.WithTimeout(c.ctx, tickInterval)
		ref, found, err := c.gh.GetRunner(ctx, u.RunnerName)
		cancel()
		c.send(msgRegistration{Unit: u.ID, Found: found, RunnerID: ref.ID, Err: err,
			ScaleSet: u.ScaleSet, RunnerName: u.RunnerName})
	}
}
