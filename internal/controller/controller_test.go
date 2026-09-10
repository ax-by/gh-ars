package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gh-ars/internal/config"
	"gh-ars/internal/domain"
	"gh-ars/internal/machine"
	"gh-ars/internal/resource"
	"gh-ars/internal/runtime"
)

// TestDesired_S7_2_3_CreateToStarting: msgDesired → desired 계산 → spread → startUnit(create→cp→start) → Starting.
// SetMaxRunners 에 capacity 가 반영된다. [§7.2-1, §7.2-3, §7.2-4]
func TestDesired_S7_2_3_CreateToStarting(t *testing.T) {
	h := newHarness(t, 0)

	if got := h.desired(1); got != 1 {
		t.Fatalf("desired reply %d, want 1", got)
	}
	if got := h.max.last(); got != 4 {
		t.Fatalf("SetMaxRunners %d, want capacity 4", got) // [§7.2-1]
	}
	u := h.onlyUnit()
	if u.State != domain.StateCreating || u.Machine != testMachine {
		t.Fatalf("unit %+v, want Creating on %s", u, testMachine)
	}
	wantName := testScaleSet + "-" + testMachine + "-" + string(u.ID)
	if u.RunnerName != wantName {
		t.Fatalf("RunnerName %q, want %q", u.RunnerName, wantName) // [§4.3]
	}

	m := h.pumpUntil(isUnitStarted).(msgUnitStarted)
	if m.Err != nil {
		t.Fatalf("startUnit err: %v", m.Err)
	}
	ctr := domain.ContainerName(u.ID, domain.RoleRunner)
	wantCalls(t, h.gh.snapshot(), "GenerateJIT 7 "+wantName)
	wantCalls(t, h.rt.snapshot(),
		"Create "+ctr,
		"CopyIn "+ctr+" "+runtime.RunnerHome,
		"Start "+ctr,
	) // [§7.2-4] create → cp → start
	if u.State != domain.StateStarting {
		t.Fatalf("state %s, want Starting", u.State)
	}
	if h.c.runnerIDs[u.ID] != 101 {
		t.Fatalf("runnerID %d, want 101", h.c.runnerIDs[u.ID])
	}

	// CreateSpec: 래퍼 entrypoint, 라벨 세트, none 모드 예산, JIT 는 env 에 없음. [§4.2, §7.2-4, §9.3 표]
	spec := h.rt.lastSpec(t)
	ep, cmd, _ := runtime.Wrapper(domain.ModeNone)
	if strings.Join(spec.Entrypoint, " ") != strings.Join(ep, " ") || strings.Join(spec.Cmd, " ") != strings.Join(cmd, " ") {
		t.Fatalf("entrypoint/cmd = %q %q", spec.Entrypoint, spec.Cmd)
	}
	if spec.Image != testImage || spec.CPUs != 1 || spec.MemoryBytes != 1<<30 || spec.Privileged || spec.CgroupParent != "" {
		t.Fatalf("spec %+v", spec)
	}
	wantLabels := map[string]string{
		domain.LabelUnit: string(u.ID), domain.LabelRole: "runner", domain.LabelScaleSet: testScaleSet,
		domain.LabelMode: "none", domain.LabelMachine: testMachine,
	}
	for k, v := range wantLabels {
		if spec.Labels[k] != v {
			t.Fatalf("label %s=%q, want %q", k, spec.Labels[k], v)
		}
	}
	for k, v := range spec.Env {
		if strings.Contains(v, "JIT-") || k == "ACTIONS_RUNNER_INPUT_JITCONFIG" {
			t.Fatalf("JIT 가 env 에 들어갔다: %s", k)
		}
	}
	// tar 스트림에 .jitconfig 가 실려 갔다.
	tar := h.rt.copied[ctr]
	if !strings.Contains(tar, ".jitconfig") || !strings.Contains(tar, "JIT-"+wantName+"\n") {
		t.Fatalf("CopyIn tar 에 .jitconfig 내용이 없다 (len=%d)", len(tar))
	}

	// 같은 assigned 로 다시 와도 running 이 이미 1 이라 생성하지 않는다.
	h.rt.reset()
	h.desired(1)
	if len(h.c.units) != 1 || len(h.rt.snapshot()) != 0 {
		t.Fatalf("중복 생성: units=%d calls=%v", len(h.c.units), h.rt.snapshot())
	}
}

// TestDesired_S7_2_1_CapacityFollowsHealth: 머신 unhealthy → capacity 0 → SetMaxRunners(0), 생성 없음(pending). [§7.2-1, §8.1]
func TestDesired_S7_2_1_CapacityFollowsHealth(t *testing.T) {
	h := newHarness(t, 0)
	h.c.handle(msgHealth{Machine: testMachine, Health: domain.Unhealthy, Err: errors.New("events closed")})
	if got := h.max.last(); got != 0 {
		t.Fatalf("SetMaxRunners %d, want 0", got)
	}
	if got := h.desired(2); got != 0 || len(h.c.units) != 0 {
		t.Fatalf("desired=%d units=%d, want 0/0 (후보 머신 없음)", got, len(h.c.units))
	}
	// 재동기화로 healthy 복귀 → capacity 4.
	h.c.handle(msgResynced{Machine: testMachine, Obs: h.c.observed(testMachine, machine.Snapshot{})})
	if got := h.max.last(); got != 4 {
		t.Fatalf("SetMaxRunners %d, want 4", got)
	}
}

// TestResynced_S7_1_3_ReconnectAppliesBudget: 시작 시 미도달이라 예산이 0 이던 머신은 재접속만으로는
// 배치되지 못한다. 그 회차의 info 로 physicalMax·effectiveMax 를 다시 계산해야 capacity 에 든다.
// 재계산에서 R21 위반이 드러나면(= 재판정) 상태는 그대로 두고 physicalMax 만 0 으로 반영한다 —
// Failed 는 최초 판정 전용이다(TestStart_R21_FirstJudgmentFails). [§7.1 재접속 회차, §8.1, R21, R22]
func TestResynced_S7_1_3_ReconnectAppliesBudget(t *testing.T) {
	h := newHarness(t, 0)
	m := h.c.machineByName(testMachine)
	m.Health, m.PhysicalMax, m.EffectiveMax = domain.Unhealthy, 0, 0 // 시작 시 preflight 미도달
	delete(h.c.reached, testMachine)
	h.c.recomputeAll()
	if got := h.max.last(); got != 0 {
		t.Fatalf("SetMaxRunners %d, want 0", got)
	}

	resync := func(info runtime.Info) {
		h.c.handle(msgResynced{Machine: testMachine, Info: info, Obs: h.c.observed(testMachine, machine.Snapshot{})})
	}
	resync(runtime.Info{CPUs: 4, MemoryBytes: 4 << 30}) // unit 예산 1cpu/1Gi → physicalMax 4
	if m.Health != domain.Healthy || m.PhysicalMax != 4 || m.EffectiveMax != 4 {
		t.Fatalf("machine %+v, want Healthy 4/4", *m)
	}
	if got := h.max.last(); got != 4 {
		t.Fatalf("SetMaxRunners %d, want 4", got)
	}
	if got := h.desired(1); got != 1 || len(h.c.units) != 1 {
		t.Fatalf("desired=%d units=%d, want 1/1 (복귀한 머신에 배치돼야 한다)", got, len(h.c.units))
	}
	h.pumpUntil(isUnitStarted)

	// 한 번 통과했던 머신의 재판정에서 예산이 unit 예산 아래로 떨어지면(VM 축소 등) R21 위반이지만
	// Failed 도 Unhealthy 도 아니다: 상태를 유지한 채 physicalMax 0 으로만 반영한다. 배치는 그것만으로
	// 막히고, 상태를 내리면 그 머신의 Dying 정리와 die 수신까지 끊긴다. [§7.1 재접속 회차, R21]
	resync(runtime.Info{CPUs: 0.5, MemoryBytes: 512 << 20})
	if m.Health != domain.Healthy {
		t.Fatalf("health %s, want Healthy (재판정 위반은 상태를 바꾸지 않는다)", m.Health)
	}
	if m.PhysicalMax != 0 || m.EffectiveMax != 0 {
		t.Fatalf("physicalMax=%d effectiveMax=%d, want 0/0", m.PhysicalMax, m.EffectiveMax)
	}
	if got := h.max.last(); got != 0 {
		t.Fatalf("SetMaxRunners %d, want 0 (physicalMax 0 은 capacity 에서 빠진다)", got)
	}
	// 예산이 회복되면 다음 회차에 자동 복귀한다.
	resync(runtime.Info{CPUs: 4, MemoryBytes: 4 << 30})
	if m.Health != domain.Healthy || m.PhysicalMax != 4 {
		t.Fatalf("machine %+v, want Healthy 4 (예산 회복 시 자동 복귀)", *m)
	}
	if got := h.max.last(); got != 4 {
		t.Fatalf("SetMaxRunners %d, want 4", got)
	}
}

// TestStart_R21_FirstJudgmentFails: 그 머신의 **최초** 판정에서 R21 위반이면 시작 실패다.
// 재판정의 완화(physicalMax 0 + 경고)가 최초 판정까지 무르게 만들지 않는다. [§7.1-3, §7.1 재접속 회차, R21]
func TestStart_R21_FirstJudgmentFails(t *testing.T) {
	h := newHarness(t, 0)
	m := h.c.machineByName(testMachine)
	if err := h.c.applyInfoAtStart(m, runtime.Info{CPUs: 0.5, MemoryBytes: 512 << 20}); err == nil {
		t.Fatal("최초 판정의 R21 위반이 오류가 아니다")
	}
}

// TestSync_S7_1_7_NoPlacementBeforeInitialSync: 전체 동기화가 끝나기 전에는 unit 을 만들지 않는다.
// 아직 입양하지 못한 머신의 slot 점유를 모르는 상태에서 배치하면 §8.2 의 여유 슬롯 판단이 틀린다.
// 보충은 세션이 열린 뒤 첫 desired 계산이 한다. [§7.1-7 → §7.1-9, §8.2]
func TestSync_S7_1_7_NoPlacementBeforeInitialSync(t *testing.T) {
	h := newHarness(t, 1) // minRunners=1
	u := h.addUnit(domain.StateRunning)
	h.c.syncing = true

	h.c.handle(msgEvent{Machine: testMachine, Ev: runtime.Event{Name: domain.ContainerName(u.ID, domain.RoleRunner), Action: "die"}})
	if n := len(h.c.units); n != 1 || h.c.units[u.ID].State != domain.StateDying {
		t.Fatalf("units=%d state=%s, want 죽은 unit 1개만(보충 없음)", n, h.c.units[u.ID].State)
	}

	// 동기화가 끝나면 평소대로 보충한다.
	h.pumpUntil(isCleanupDone)
	h.c.syncing = false
	if got := h.desired(0); got != 1 || len(h.c.units) != 1 {
		t.Fatalf("desired=%d units=%d, want 1/1 (minRunners 보충)", got, len(h.c.units))
	}
}

// TestTick_S8_3_StartingPromoted: Starting + 등록 있음 → Running. [§8.3]
func TestTick_S8_3_StartingPromoted(t *testing.T) {
	h := newHarness(t, 0)
	u := h.addUnit(domain.StateStarting)
	h.gh.register(u.RunnerName, 55)
	h.c.handle(msgTick{})
	h.pumpUntil(isCheckPassDone)
	if u.State != domain.StateRunning {
		t.Fatalf("state %s, want Running", u.State)
	}
	wantCalls(t, h.gh.snapshot(), "GetRunner "+u.RunnerName)
}

// TestTick_S8_3_StartingGrace: Starting + 미등록: grace 이내 대기, 초과 시 Dying → 정리. [§8.3]
func TestTick_S8_3_StartingGrace(t *testing.T) {
	h := newHarness(t, 0)
	u := h.addUnit(domain.StateStarting)

	h.clock.advance(grace - time.Second)
	h.c.handle(msgTick{})
	h.pumpUntil(isCheckPassDone)
	if u.State != domain.StateStarting {
		t.Fatalf("grace 이내인데 %s", u.State)
	}

	h.clock.advance(2 * time.Second)
	h.c.handle(msgTick{})
	h.pumpUntil(isCheckPassDone)
	if u.State != domain.StateDying {
		t.Fatalf("grace 초과인데 %s", u.State)
	}
	h.pumpUntil(isCleanupDone)
	if _, ok := h.c.units[u.ID]; ok {
		t.Fatal("정리 후에도 unit 이 남아 있다")
	}
}

// TestTick_S8_3_RunningUnregistered: Running + 미등록 → grace 없이 즉시 Dying. Draining 은 대조 제외. [§8.3]
func TestTick_S8_3_RunningUnregistered(t *testing.T) {
	h := newHarness(t, 0)
	u := h.addUnit(domain.StateRunning)
	d := h.addUnit(domain.StateDraining)
	h.c.handle(msgTick{})
	h.pumpUntil(isCheckPassDone)
	if u.State != domain.StateDying {
		t.Fatalf("state %s, want Dying", u.State)
	}
	if d.State != domain.StateDraining {
		t.Fatalf("Draining 이 바뀌었다: %s", d.State)
	}
	for _, call := range h.gh.snapshot() {
		if strings.Contains(call, d.RunnerName) && strings.HasPrefix(call, "GetRunner") {
			t.Fatalf("Draining unit 을 대조했다: %v", h.gh.snapshot())
		}
	}
	h.pumpUntil(isCleanupDone)
	if _, ok := h.c.units[u.ID]; ok {
		t.Fatal("정리 후에도 unit 이 남아 있다")
	}
	// tick 은 대조 결과가 오기 전엔 같은 unit 을 다시 조회하지 않는다.
	s := h.addUnit(domain.StateStarting)
	h.gh.reset()
	h.c.handle(msgTick{})
	h.c.handle(msgTick{})
	h.pumpUntil(isCheckPassDone)
	wantCalls(t, h.gh.snapshot(), "GetRunner "+s.RunnerName)
}

// TestCleanup_S8_3_Order: GetRunner → RemoveRunner → 컨테이너 → sidecar → 볼륨 순서. 각 단계 멱등. [§8.3]
func TestCleanup_S8_3_Order(t *testing.T) {
	h := newHarness(t, 0)
	u := h.addUnit(domain.StateDying, func(u *domain.Unit) {
		u.Parts = domain.Parts{Runner: true, Sidecar: true, Volumes: [3]bool{true, true, true}}
	})
	h.gh.register(u.RunnerName, 55)
	h.c.startCleanup(u)
	m := h.pumpUntil(isCleanupDone).(msgCleanupDone)
	if m.Err != nil {
		t.Fatalf("cleanup err: %v", m.Err)
	}
	wantCalls(t, h.all.snapshot(),
		"GetRunner "+u.RunnerName,
		"RemoveRunner 55",
		"Remove "+domain.ContainerName(u.ID, domain.RoleRunner)+" force=true",
		"Remove "+domain.ContainerName(u.ID, domain.RoleSidecar)+" force=true",
		"VolumeRemove "+domain.VolumeName(u.ID, domain.VolumeWork),
		"VolumeRemove "+domain.VolumeName(u.ID, domain.VolumeSock),
		"VolumeRemove "+domain.VolumeName(u.ID, domain.VolumeExternals),
	)
	if _, ok := h.c.units[u.ID]; ok {
		t.Fatal("Removed 인데 상태에 남아 있다")
	}

	// 등록 없음(GetRunner nil,nil) → RemoveRunner 생략. none 모드는 runner 컨테이너만.
	h.all.reset()
	u2 := h.addUnit(domain.StateDying)
	h.c.startCleanup(u2)
	h.pumpUntil(isCleanupDone)
	wantCalls(t, h.all.snapshot(), "GetRunner "+u2.RunnerName, "Remove "+domain.ContainerName(u2.ID, domain.RoleRunner)+" force=true")

	// 정리 실패 → Dying 유지, slot 점유 유지, tick 에서 재시도. [§8.3]
	h.rt.removeErr = errors.New("rm failed")
	u3 := h.addUnit(domain.StateDying)
	h.c.startCleanup(u3)
	if m := h.pumpUntil(isCleanupDone).(msgCleanupDone); m.Err == nil {
		t.Fatal("정리 실패가 성공으로 보고됐다")
	}
	if u3.State != domain.StateDying {
		t.Fatalf("state %s, want Dying 유지", u3.State)
	}
	if got := h.desired(4); len(h.c.units) != 4 {
		// Dying(부품 잔존)이 slot 1개를 점유해 effectiveMax 4 중 3개만 생성된다.
		t.Fatalf("desired=%d units=%d, want 4 (Dying 1 + 신규 3)", got, len(h.c.units))
	}
	h.rt.removeErr = nil
	h.rt.reset()
	h.c.handle(msgTick{})
	h.pumpUntil(isCleanupDone)
	if _, ok := h.c.units[u3.ID]; ok {
		t.Fatal("재시도 후에도 unit 이 남아 있다")
	}
}

// TestDie_S7_2_5_NoNewUnit: busy unit 의 die 뒤 캐시된 assigned 로 생성하지 않는다(pendingCompletion). minRunners 만 보충. [§7.2-3, §7.2-5]
func TestDie_S7_2_5_NoNewUnit(t *testing.T) {
	h := newHarness(t, 0)
	u := h.addUnit(domain.StateRunning)
	h.c.handle(msgJobStarted{ScaleSet: testScaleSet, RunnerName: u.RunnerName})
	if !u.Busy {
		t.Fatal("JobStarted 뒤 Busy 가 아니다")
	}

	h.die(u.ID, domain.RoleRunner)
	if u.State != domain.StateDying {
		t.Fatalf("state %s, want Dying", u.State)
	}
	if _, ok := h.c.scaleSets[testScaleSet].pending[u.RunnerName]; !ok {
		t.Fatal("busy unit 의 die 가 pendingCompletion 에 없다")
	}
	h.pumpUntil(isCleanupDone)

	// 빈 폴링: 직전 통계(1)가 그대로 온다 → 보정으로 0 → 생성 없음.
	if got := h.desired(1); got != 0 || len(h.c.units) != 0 {
		t.Fatalf("die 직후 캐시 값으로 생성했다: desired=%d units=%d", got, len(h.c.units))
	}
	// JobCompleted 가 뒤늦게 오면 집합에서 빠지고, 새 통계(0)에서도 생성 없음.
	h.c.handle(msgJobCompleted{ScaleSet: testScaleSet, RunnerName: u.RunnerName})
	if len(h.c.scaleSets[testScaleSet].pending) != 0 {
		t.Fatal("JobCompleted 뒤에도 pendingCompletion 에 남아 있다")
	}
	if got := h.desired(0); got != 0 || len(h.c.units) != 0 {
		t.Fatalf("desired=%d units=%d", got, len(h.c.units))
	}
	// 새 job 이 오면(assigned 1, pending 0) 생성된다.
	if got := h.desired(1); got != 1 || len(h.c.units) != 1 {
		t.Fatalf("새 job 에서 생성되지 않음: desired=%d units=%d", got, len(h.c.units))
	}
	h.pumpUntil(isUnitStarted)

	// JobCompleted 가 die 보다 먼저 오면 Completed 표시만 남고 die 시 집합에 넣지 않는다.
	u2 := h.onlyUnit()
	h.c.handle(msgJobStarted{ScaleSet: testScaleSet, RunnerName: u2.RunnerName})
	h.c.handle(msgJobCompleted{ScaleSet: testScaleSet, RunnerName: u2.RunnerName})
	if !u2.Completed {
		t.Fatal("살아 있는 unit 의 JobCompleted 가 Completed 로 남지 않았다")
	}
	h.die(u2.ID, domain.RoleRunner)
	if len(h.c.scaleSets[testScaleSet].pending) != 0 {
		t.Fatal("Completed unit 의 die 가 pendingCompletion 에 들어갔다")
	}
	h.pumpUntil(isCleanupDone)

	// pendingCompletion 5분 만료 (안전장치 2).
	u3 := h.addUnit(domain.StateRunning, func(u *domain.Unit) { u.Busy = true })
	h.die(u3.ID, domain.RoleRunner)
	h.pumpUntil(isCleanupDone)
	h.clock.advance(pendingExpiry + time.Second)
	h.c.handle(msgTick{})
	if len(h.c.scaleSets[testScaleSet].pending) != 0 {
		t.Fatal("5분 지난 pendingCompletion 항목이 남아 있다")
	}
}

// TestDie_S7_2_5_ReplenishCappedByCapacity: die 뒤 minRunners 보충도 capacity 를 넘지 않는다.
// R24 는 미도달 머신이 있으면 minRunners > capacity 여도 경고만 하고 시작하므로, 미달분을 그대로
// 만들면 maxRunners 를 넘게 된다. [§7.2-3, §7.2-5, R24]
func TestDie_S7_2_5_ReplenishCappedByCapacity(t *testing.T) {
	h := newHarness(t, 3)                      // minRunners=3
	h.c.scaleSets[testScaleSet].MaxRunners = 1 // capacity = min(effectiveMax 4, 1) = 1
	u := h.addUnit(domain.StateRunning)
	h.die(u.ID, domain.RoleRunner)
	created := 0
	for _, x := range h.c.units {
		if x.State == domain.StateCreating {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("보충 생성 %d개, want 1 (capacity 1)", created)
	}
	h.pumpEach(isCleanupDone, isUnitStarted)
}

// TestDie_S7_2_5_MinRunnersReplenish: minRunners=1 인 scale set 의 unit 이 die 하면 미달분만 즉시 보충한다. [§7.2-5]
func TestDie_S7_2_5_MinRunnersReplenish(t *testing.T) {
	h := newHarness(t, 1)
	u := h.addUnit(domain.StateRunning)
	h.die(u.ID, domain.RoleRunner)
	var created []*domain.Unit
	for _, x := range h.c.units {
		if x.State == domain.StateCreating {
			created = append(created, x)
		}
	}
	if len(created) != 1 {
		t.Fatalf("보충 생성 %d개, want 1", len(created))
	}
	h.pumpEach(isCleanupDone, isUnitStarted)
	if created[0].State != domain.StateStarting {
		t.Fatalf("보충 unit state %s", created[0].State)
	}
	// 그 뒤 die 가 또 와도 assigned 기반 생성은 없다(min 1 은 이미 충족).
	if got := h.desired(0); got != 1 || len(h.c.units) != 1 {
		t.Fatalf("desired=%d units=%d, want 1/1", got, len(h.c.units))
	}
}

// TestDie_S7_2_5_CreatingDie: Creating 중 die → 즉시 Dying, 뒤늦은 msgUnitStarted 는 무시. [DESIGN §3.2]
func TestDie_S7_2_5_CreatingDie(t *testing.T) {
	h := newHarness(t, 0)
	h.desired(1)
	u := h.onlyUnit()
	ctr := domain.ContainerName(u.ID, domain.RoleRunner)
	started := h.recv().(msgUnitStarted) // startUnit 은 끝났지만 아직 루프가 처리하지 않았다

	h.die(u.ID, domain.RoleRunner)
	if u.State != domain.StateDying {
		t.Fatalf("state %s, want Dying", u.State)
	}
	h.c.handle(started)
	if u.State != domain.StateDying {
		t.Fatalf("늦게 온 msgUnitStarted 가 상태를 바꿨다: %s", u.State)
	}
	h.pumpUntil(isCleanupDone)
	if _, ok := h.c.units[u.ID]; ok {
		t.Fatal("정리 후에도 unit 이 남아 있다")
	}
	// GitHub 등록은 정리 순서대로 지워졌다(GenerateJIT 가 등록했으므로 found). 등록 제거가 rm 앞.
	wantCalls(t, h.all.snapshot(),
		"GenerateJIT 7 "+u.RunnerName,
		"Create "+ctr, "CopyIn "+ctr+" "+runtime.RunnerHome, "Start "+ctr,
		"GetRunner "+u.RunnerName, "RemoveRunner 101",
		"Remove "+ctr+" force=true")
}

// TestStartUnit_S7_2_4_FailureRollback: start 실패 → 역순 정리(GetRunner → RemoveRunner → rm) → Dying → Removed. [§7.2-4, §8.3]
func TestStartUnit_S7_2_4_FailureRollback(t *testing.T) {
	h := newHarness(t, 0)
	h.rt.startErr = errors.New("start failed")
	h.desired(1)
	u := h.onlyUnit()
	ctr := domain.ContainerName(u.ID, domain.RoleRunner)

	m := h.recv().(msgUnitStarted) // 아직 처리하지 않는다: 처리하면 cleanupUnit 이 떠서 이력이 섞인다
	if m.Err == nil {
		t.Fatal("start 실패가 성공으로 보고됐다")
	}
	// startUnit 의 되돌리기: 등록 제거가 컨테이너 rm 앞에 온다.
	wantCalls(t, h.all.snapshot(),
		"GenerateJIT 7 "+u.RunnerName,
		"Create "+ctr, "CopyIn "+ctr+" "+runtime.RunnerHome, "Start "+ctr,
		"GetRunner "+u.RunnerName, "RemoveRunner 101",
		"Remove "+ctr+" force=true")
	h.c.handle(m)
	if u.State != domain.StateDying {
		t.Fatalf("state %s, want Dying", u.State)
	}
	h.pumpUntil(isCleanupDone)
	if _, ok := h.c.units[u.ID]; ok {
		t.Fatal("정리 후에도 unit 이 남아 있다")
	}
	// 다음 msgDesired 에서 재배치된다.
	h.rt.startErr = nil
	h.rt.reset()
	h.gh.reset()
	h.desired(1)
	if len(h.c.units) != 1 {
		t.Fatalf("재배치되지 않음: units=%d", len(h.c.units))
	}
	h.pumpUntil(isUnitStarted)
	if h.onlyUnit().State != domain.StateStarting {
		t.Fatalf("재배치 unit state %s", h.onlyUnit().State)
	}

	// JIT 생성 실패: 컨테이너 명령 없이 실패 보고, 되돌릴 부품 없음.
	h.rt.reset()
	h.gh.reset()
	h.gh.jitErr = errors.New("jit failed")
	h.desired(2)
	m = h.pumpUntil(isUnitStarted).(msgUnitStarted)
	if m.Err == nil {
		t.Fatal("JIT 실패가 성공으로 보고됐다")
	}
	for _, call := range h.rt.snapshot() {
		if strings.HasPrefix(call, "Create") || strings.HasPrefix(call, "Start") {
			t.Fatalf("JIT 실패 후 컨테이너를 만들었다: %v", h.rt.snapshot())
		}
	}
}

// TestResync_S8_3_AdoptAndOrphan: 전체 동기화에서 살아 있는 runner 입양(Starting), exited 정리, 고아 볼륨 rm. [§7.1-7, §8.3]
func TestResync_S8_3_AdoptAndOrphan(t *testing.T) {
	h := newHarness(t, 0)
	alive := domain.UnitID("01K4Z9V6H8QW3T5R7Y9B2C4AAA")
	dead := domain.UnitID("01K4Z9V6H8QW3T5R7Y9B2C4BBB")
	orphan := domain.UnitID("01K4Z9V6H8QW3T5R7Y9B2C4CCC")
	created := h.clock.now().Add(-time.Minute)
	labels := func(id domain.UnitID) map[string]string {
		return map[string]string{domain.LabelUnit: string(id), domain.LabelRole: "runner", domain.LabelScaleSet: testScaleSet, domain.LabelMode: "none", domain.LabelMachine: testMachine}
	}
	h.c.handle(msgResynced{Machine: testMachine, Obs: h.c.observed(testMachine, snapshotOf(
		[]runtime.Container{
			{Name: domain.ContainerName(alive, domain.RoleRunner), State: "running", Created: created, Labels: labels(alive)},
			{Name: domain.ContainerName(dead, domain.RoleRunner), State: "exited", Created: created, Labels: labels(dead)},
		},
		[]string{domain.VolumeName(orphan, domain.VolumeWork)},
	))})
	a := h.c.units[alive]
	if a == nil || a.State != domain.StateStarting || !a.Adopted || a.Foreign || !a.CreatedAt.Equal(created) {
		t.Fatalf("입양 unit %+v", a)
	}
	if a.RunnerName != domain.RunnerName(testScaleSet, testMachine, alive) {
		t.Fatalf("입양 RunnerName %q", a.RunnerName)
	}
	d := h.c.units[dead]
	if d == nil || d.State != domain.StateDying {
		t.Fatalf("exited unit %+v, want Dying", d)
	}
	// 고아 볼륨은 runner 이름 없는 Dying unit 으로 §8.3 순서(sidecar → 볼륨)를 타고 GitHub 단계는 건너뛴다.
	if o := h.c.units[orphan]; o == nil || o.State != domain.StateDying || o.RunnerName != "" {
		t.Fatalf("고아 unit %+v", o)
	}
	h.pumpEach(isCleanupOf(dead), isCleanupOf(orphan))
	if _, ok := h.c.units[dead]; ok {
		t.Fatal("exited unit 이 정리되지 않았다")
	}
	if _, ok := h.c.units[orphan]; ok {
		t.Fatal("고아 부품이 정리되지 않았다")
	}
	if !hasCall(h.rt.snapshot(), "VolumeRemove "+domain.VolumeName(orphan, domain.VolumeWork)) {
		t.Fatalf("고아 볼륨이 rm 되지 않았다: %v", h.rt.snapshot())
	}
	for _, call := range h.gh.snapshot() {
		if strings.Contains(call, string(orphan)) {
			t.Fatalf("고아 부품 정리가 GitHub 을 호출했다: %v", h.gh.snapshot())
		}
	}
	// 입양된 unit 은 running 집계에 들어가 중복 생성이 없다.
	if got := h.desired(1); got != 1 || len(h.c.units) != 1 {
		t.Fatalf("입양 뒤 중복 생성: desired=%d units=%d", got, len(h.c.units))
	}
}

// TestRun_S7_1_WalkingSkeleton: 실제 listener 를 거쳐 §7.1 시작 순서(preflight·pre-pull → scale set 확보 →
// 동기화 → events → 세션)를 밟고, 초기 통계 TotalAssignedJobs=1 로 unit 이 생성되며, GetMessage 의
// maxCapacity 에 capacity 가 실리고, ctx 취소 시 세션을 닫는다. [§7.1, §7.2-1, §7.3]
func TestRun_S7_1_WalkingSkeleton(t *testing.T) {
	h := newHarness(t, 0)
	h.gh.session = &fakeSession{assigned: 1}
	h.c.units = map[domain.UnitID]*domain.Unit{} // Run 이 상태를 처음부터 조립한다

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.c.Run(ctx) }()

	waitFor(t, "runner 컨테이너 start", func() bool {
		for _, c := range h.rt.snapshot() {
			if strings.HasPrefix(c, "Start ") {
				return true
			}
		}
		return false
	})
	waitFor(t, "GetMessage 호출", func() bool { return h.gh.session.firstMaxCap() >= 0 })
	if got := h.gh.session.firstMaxCap(); got != 4 {
		t.Fatalf("GetMessage maxCapacity %d, want 4", got) // [§7.2-1]
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run 이 ctx 취소 뒤 끝나지 않았다")
	}
	if !h.gh.session.isClosed() {
		t.Fatal("세션이 닫히지 않았다") // [§7.3]
	}
	// 시작 순서: 인증 확인(§7.1-2) → pre-pull(§7.1-4) → scale set 확보(§7.1-5) → events 열기(§7.1-8) →
	// 동기화(§7.1-7) → 세션(§7.1-9) → JIT·create→cp→start(루프). 인증 확인이 preflight 앞인 것은
	// 잘못된 토큰이 pre-pull 을 다 기다린 뒤가 아니라 즉시 드러나게 하기 위함이다.
	// 여기서 보는 것은 Controller 쪽 순서(에이전트 진입 → 동기화 → 세션)까지다. 에이전트 안의
	// "events 를 먼저 열고 Observe" 순서는 실물 Agent 로 machine 패키지가 검증한다
	// (TestRun_S7_1_7_ResyncAfterEventsOpen). fakeAgent 는 스트림을 열지 않는다.
	all := h.all.snapshot()
	wantPrefix := []string{"CheckAuth Default", "Pull " + testImage, "EnsureScaleSet ss Default", "Run m1", "Observe m1", "NewSession 7 gh-ars", "GenerateJIT 7 ss-m1-", "Create ", "CopyIn ", "Start "}
	if len(all) < len(wantPrefix) {
		t.Fatalf("호출 이력 %q", all)
	}
	for i, w := range wantPrefix {
		if !strings.HasPrefix(all[i], w) {
			t.Fatalf("호출 순서 [%d] %q, want prefix %q (전체 %q)", i, all[i], w, all)
		}
	}
	// §7.3: 실행 중 컨테이너를 kill 하지 않는다.
	for _, c := range all {
		if strings.HasPrefix(c, "Remove ") {
			t.Fatalf("종료 시 컨테이너를 지웠다: %q", all)
		}
	}
}

// TestRun_S7_1_2_AuthCheckedBeforePreflight: 인증이 실패하면 preflight·pre-pull 을 시작하지도 않고
// 즉시 시작 실패한다. 이 확인이 없으면 잘못된 토큰이 §7.1-5 에서야 드러나 그때까지 머신 수 ×
// 이미지당 최대 5분을 버린다. [§7.1-2, §7.1-3, §7.1-4]
func TestRun_S7_1_2_AuthCheckedBeforePreflight(t *testing.T) {
	h := newHarness(t, 0)
	h.gh.authErr = errors.New("401 Unauthorized")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := h.c.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("Run err = %v, want 인증 오류로 시작 실패", err)
	}
	for _, c := range h.all.snapshot() {
		if strings.HasPrefix(c, "Pull ") || strings.HasPrefix(c, "EnsureScaleSet ") {
			t.Fatalf("인증 실패인데 이후 단계를 진행했다: %q", h.all.snapshot())
		}
	}
}

// TestTick_S8_3_BusyUnregisteredCompensates: busy unit 이 die 보다 먼저 tick 의 미등록 판정으로 Dying 이 돼도
// pendingCompletion 에 들어가고, 정리 뒤 이름이 빈 JobCompleted 가 RunnerID 로 항목을 지운다. [§7.2-3, §8.3, DESIGN §4.5]
func TestTick_S8_3_BusyUnregisteredCompensates(t *testing.T) {
	h := newHarness(t, 0)
	u := h.addUnit(domain.StateStarting)
	h.gh.register(u.RunnerName, 55)
	h.c.handle(msgTick{})
	h.pumpUntil(isCheckPassDone)
	if u.State != domain.StateRunning || h.c.runnerIDs[u.ID] != 55 {
		t.Fatalf("state=%s runnerID=%d", u.State, h.c.runnerIDs[u.ID])
	}
	h.c.handle(msgJobStarted{ScaleSet: testScaleSet, RunnerName: u.RunnerName})

	// job 종료: 등록이 먼저 사라지고 die 는 아직.
	_ = h.gh.RemoveRunner(context.Background(), 55)
	h.c.handle(msgTick{})
	h.pumpUntil(isCheckPassDone)
	if u.State != domain.StateDying {
		t.Fatalf("state %s, want Dying", u.State)
	}
	e, ok := h.c.scaleSets[testScaleSet].pending[u.RunnerName]
	if !ok || e.RunnerID != 55 {
		t.Fatalf("pendingCompletion 항목 %+v ok=%v", e, ok)
	}
	h.pumpUntil(isCleanupDone)
	h.die(u.ID, domain.RoleRunner) // 뒤늦은 die 는 무시
	if got := h.desired(1); got != 0 || len(h.c.units) != 0 {
		t.Fatalf("캐시 통계로 생성했다: desired=%d units=%d", got, len(h.c.units))
	}
	h.c.handle(msgJobCompleted{ScaleSet: testScaleSet, RunnerName: "", RunnerID: 55})
	if len(h.c.scaleSets[testScaleSet].pending) != 0 {
		t.Fatal("RunnerID 만 있는 JobCompleted 가 항목을 지우지 못했다")
	}
	if got := h.desired(1); got != 1 || len(h.c.units) != 1 {
		t.Fatalf("보정 해제 뒤 생성되지 않음: desired=%d units=%d", got, len(h.c.units))
	}
	h.pumpUntil(isUnitStarted)
}

// TestStartUnit_S7_3_ShutdownNoRollback: 종료(ctx 취소)로 start 가 중단되면 되돌리지 않는다 — 컨테이너가 이미
// 실행 중일 수 있고 §7.3 은 실행 중 컨테이너를 kill 하지 않는다. 재시작 시 입양·정리한다. [§7.3, §8.3]
func TestStartUnit_S7_3_ShutdownNoRollback(t *testing.T) {
	h := newHarness(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.c.ctx = ctx
	h.rt.startFn = func(ctx context.Context) error {
		cancel() // start 응답을 기다리는 동안 SIGTERM
		<-ctx.Done()
		return ctx.Err()
	}
	h.desired(1)
	u := h.onlyUnit()
	ctr := domain.ContainerName(u.ID, domain.RoleRunner)
	// handle 하지 않고 받기만 한다: handleUnitStarted 는 비동기 정리를 띄우고 그 goroutine 의 호출이
	// 아래 이력에 섞여 들어와 판정이 흔들린다(fake 는 취소된 ctx 에서도 실행된다).
	m := h.recv().(msgUnitStarted)
	if m.Err == nil {
		t.Fatal("중단이 성공으로 보고됐다")
	}
	wantCalls(t, h.all.snapshot(), "GenerateJIT 7 "+u.RunnerName, "Create "+ctr, "CopyIn "+ctr+" "+runtime.RunnerHome, "Start "+ctr)
}

// TestCleanup_S8_3_PartialRetry: 정리가 중간(볼륨)에서 실패하면 Dying 유지, tick 재시도는 처음부터 다시 돌며
// 이미 없는 등록·컨테이너는 "없으면 성공" 으로 지나간다. [§8.3, DESIGN §6 cleanupUnit]
func TestCleanup_S8_3_PartialRetry(t *testing.T) {
	h := newHarness(t, 0)
	u := h.addUnit(domain.StateDying, func(u *domain.Unit) {
		u.Parts = domain.Parts{Runner: true, Sidecar: true, Volumes: [3]bool{true, true, true}}
	})
	h.gh.register(u.RunnerName, 55)
	h.rt.volRmOnce = errors.New("volume busy")
	runner := domain.ContainerName(u.ID, domain.RoleRunner)
	sidecar := domain.ContainerName(u.ID, domain.RoleSidecar)

	h.c.startCleanup(u)
	if m := h.pumpUntil(isCleanupDone).(msgCleanupDone); m.Err == nil {
		t.Fatal("부분 실패가 성공으로 보고됐다")
	}
	wantCalls(t, h.all.snapshot(),
		"GetRunner "+u.RunnerName, "RemoveRunner 55",
		"Remove "+runner+" force=true", "Remove "+sidecar+" force=true",
		"VolumeRemove "+domain.VolumeName(u.ID, domain.VolumeWork))
	if u.State != domain.StateDying || h.c.cleaning[u.ID] {
		t.Fatalf("state=%s cleaning=%v", u.State, h.c.cleaning[u.ID])
	}

	// 재시도: 등록은 이미 지워져 GetRunner 가 (nil,nil) → RemoveRunner 없음. 컨테이너 rm 은 없어도 성공.
	h.all.reset()
	h.c.handle(msgTick{})
	if m := h.pumpUntil(isCleanupDone).(msgCleanupDone); m.Err != nil {
		t.Fatalf("재시도 실패: %v", m.Err)
	}
	wantCalls(t, h.all.snapshot(),
		"GetRunner "+u.RunnerName,
		"Remove "+runner+" force=true", "Remove "+sidecar+" force=true",
		"VolumeRemove "+domain.VolumeName(u.ID, domain.VolumeWork),
		"VolumeRemove "+domain.VolumeName(u.ID, domain.VolumeSock),
		"VolumeRemove "+domain.VolumeName(u.ID, domain.VolumeExternals))
	if _, ok := h.c.units[u.ID]; ok {
		t.Fatal("재시도 후에도 unit 이 남아 있다")
	}
}

// TestDie_S7_2_3_LateStartResultKeepsRunnerID: Creating 중 JobStarted → die 가 msgUnitStarted 보다 먼저 처리돼도
// 늦게 온 결과의 runnerID 가 pendingCompletion 항목에 남아 이름 없는 JobCompleted 로 지워진다. [§7.2-3, DESIGN §4.5]
func TestDie_S7_2_3_LateStartResultKeepsRunnerID(t *testing.T) {
	h := newHarness(t, 0)
	h.desired(1)
	u := h.onlyUnit()
	started := h.recv().(msgUnitStarted)
	h.c.handle(msgJobStarted{ScaleSet: testScaleSet, RunnerName: u.RunnerName})
	h.die(u.ID, domain.RoleRunner)
	e, ok := h.c.scaleSets[testScaleSet].pending[u.RunnerName]
	if !ok || e.RunnerID != 0 {
		t.Fatalf("pending %+v ok=%v (아직 id 를 모른다)", e, ok)
	}
	h.c.handle(started)
	if e := h.c.scaleSets[testScaleSet].pending[u.RunnerName]; e.RunnerID != 101 {
		t.Fatalf("늦은 start 결과의 runnerID 가 반영되지 않음: %+v", e)
	}
	h.pumpUntil(isCleanupDone)
	h.c.handle(msgJobCompleted{ScaleSet: testScaleSet, RunnerName: "", RunnerID: 101})
	if len(h.c.scaleSets[testScaleSet].pending) != 0 {
		t.Fatal("id 만 있는 JobCompleted 가 항목을 지우지 못했다")
	}
}

// TestResync_S8_3_CreatingExceptionAfterSnapshot: 관측 뒤에 Starting 이 된 unit 은 스냅샷에 컨테이너가 없어도
// Creating 예외로 판정에서 빠진다(자기가 만들던 unit 을 지우지 않는다). [§8.3 Creating 예외]
func TestResync_S8_3_CreatingExceptionAfterSnapshot(t *testing.T) {
	h := newHarness(t, 0)
	snapAt := h.clock.now()
	h.desired(1)
	u := h.onlyUnit()
	h.clock.advance(time.Second)
	h.pumpUntil(isUnitStarted) // Starting, startedAt = snapAt+1s
	if u.State != domain.StateStarting {
		t.Fatalf("state %s", u.State)
	}
	// 관측은 start 전에 찍혔고 컨테이너가 없다.
	h.c.handle(msgResynced{Machine: testMachine, At: snapAt, Obs: h.c.observed(testMachine, machine.Snapshot{})})
	if u.State != domain.StateStarting || !u.Parts.Runner {
		t.Fatalf("스냅샷 이전에 만들던 unit 이 판정됐다: state=%s parts=%+v", u.State, u.Parts)
	}
	// 관측이 start 뒤라면 컨테이너 부재는 정리 대상이다.
	h.clock.advance(time.Second)
	h.c.handle(msgResynced{Machine: testMachine, At: h.clock.now(), Obs: h.c.observed(testMachine, machine.Snapshot{})})
	if u.State != domain.StateDying {
		t.Fatalf("start 뒤 관측에서 컨테이너가 없는데 %s", u.State)
	}
	h.pumpUntil(isCleanupDone)
}

// TestDie_S7_2_3_IDOnlyCompletionBeforeStartResult: JobStarted → die → id 만 있는 JobCompleted → msgUnitStarted
// 순서에서도 pendingCompletion 항목이 남지 않는다. [§7.2-3, DESIGN §4.5]
func TestDie_S7_2_3_IDOnlyCompletionBeforeStartResult(t *testing.T) {
	h := newHarness(t, 0)
	h.desired(1)
	u := h.onlyUnit()
	started := h.recv().(msgUnitStarted)
	h.c.handle(msgJobStarted{ScaleSet: testScaleSet, RunnerName: u.RunnerName})
	h.die(u.ID, domain.RoleRunner)
	h.c.handle(msgJobCompleted{ScaleSet: testScaleSet, RunnerName: "", RunnerID: 101})
	ss := h.c.scaleSets[testScaleSet]
	if len(ss.pending) != 1 || len(ss.completedIDs) != 1 {
		t.Fatalf("pending=%d completedIDs=%d, want 1/1 (아직 id 를 모른다)", len(ss.pending), len(ss.completedIDs))
	}
	h.c.handle(started)
	if len(ss.pending) != 0 || len(ss.completedIDs) != 0 {
		t.Fatalf("start 결과 뒤에도 pending=%d completedIDs=%d", len(ss.pending), len(ss.completedIDs))
	}
	h.pumpUntil(isCleanupDone)
	if got := h.desired(1); got != 1 || len(h.c.units) != 1 {
		t.Fatalf("보정이 남아 생성이 막혔다: desired=%d units=%d", got, len(h.c.units))
	}
	h.pumpUntil(isUnitStarted)
}

// TestTick_S8_3_AdoptedLearnsRunnerID: 입양 unit(JIT 없음)은 tick 등록 확인으로 id 를 알게 되고, 그 전에 온
// 이름 없는 JobCompleted 가 그때 Completed 로 적용돼 die 시 pendingCompletion 에 들어가지 않는다. [§7.2-3, DESIGN §4.5]
func TestTick_S8_3_AdoptedLearnsRunnerID(t *testing.T) {
	h := newHarness(t, 0)
	u := h.addUnit(domain.StateStarting, func(u *domain.Unit) { u.Adopted = true })
	h.gh.register(u.RunnerName, 77)
	h.c.handle(msgJobStarted{ScaleSet: testScaleSet, RunnerName: u.RunnerName})
	h.c.handle(msgJobCompleted{ScaleSet: testScaleSet, RunnerName: "", RunnerID: 77})
	ss := h.c.scaleSets[testScaleSet]
	if len(ss.completedIDs) != 1 || u.Completed {
		t.Fatalf("보관 전: completedIDs=%d completed=%v", len(ss.completedIDs), u.Completed)
	}
	h.c.handle(msgTick{})
	h.pumpUntil(isCheckPassDone)
	if u.State != domain.StateRunning || !u.Completed || len(ss.completedIDs) != 0 || h.c.runnerIDs[u.ID] != 77 {
		t.Fatalf("등록 확인 뒤: state=%s completed=%v completedIDs=%d id=%d", u.State, u.Completed, len(ss.completedIDs), h.c.runnerIDs[u.ID])
	}
	h.die(u.ID, domain.RoleRunner)
	if len(ss.pending) != 0 {
		t.Fatal("Completed unit 의 die 가 pendingCompletion 에 들어갔다")
	}
	h.pumpUntil(isCleanupDone)
}

// TestResync_S7_2_3_PendingKeptForUnitDyingInSameResync: 단절 구간에 busy unit 이 죽었고 JobCompleted 는
// 아직 오지 않은 상태에서 재동기화가 그 unit 을 Dying 으로 보내면, markDying 이 넣은 pendingCompletion
// 항목이 같은 재동기화 처리가 끝난 뒤에도 남아 있어야 한다. 비우기가 Reconcile 적용보다 뒤에 오면
// 방금 넣은 항목이 곧바로 지워져 DESIGN §4.5 의 "재동기화" 갱신 지점이 죽은 조항이 되고, 빈 폴링의
// 캐시 통계에서 완료분이 빠지지 않아 유휴 runner 가 생긴다. [§7.2-3, DESIGN §4.5, §6]
func TestResync_S7_2_3_PendingKeptForUnitDyingInSameResync(t *testing.T) {
	h := newHarness(t, 0)
	u := h.addUnit(domain.StateRunning, func(u *domain.Unit) { u.Busy = true }) // JobCompleted 미도착
	ss := h.c.scaleSets[testScaleSet]
	// 단절 중 죽어 exited 로 남은 컨테이너를 재동기화가 발견한다 → RemoveUnit → markDying.
	h.c.handle(msgResynced{Machine: testMachine, Obs: h.c.observed(testMachine, snapshotOf(
		[]runtime.Container{{
			Name: domain.ContainerName(u.ID, domain.RoleRunner), State: "exited", Created: u.CreatedAt,
			Labels: map[string]string{
				domain.LabelUnit: string(u.ID), domain.LabelRole: "runner",
				domain.LabelScaleSet: testScaleSet, domain.LabelMode: "none", domain.LabelMachine: testMachine},
		}}, nil,
	))})
	if u.State != domain.StateDying {
		t.Fatalf("state %s, want Dying (재동기화가 exited 를 정리 대상으로 봐야 한다)", u.State)
	}
	if _, ok := ss.pending[u.RunnerName]; !ok {
		t.Fatalf("pendingCompletion 이 비었다: 같은 재동기화가 넣은 항목을 스스로 지웠다 (%v)", ss.pending)
	}
	// 반대로 낡은 항목(다른 unit, 단절 전에 쌓인 것)은 이 재동기화가 털어낸다.
	ss.pending["gh-ars-stale"] = pendingEntry{At: h.clock.now(), Machine: testMachine}
	h.c.handle(msgResynced{Machine: testMachine, Obs: h.c.observed(testMachine, machine.Snapshot{})})
	if _, ok := ss.pending["gh-ars-stale"]; ok {
		t.Fatal("낡은 항목이 남았다: 비우기가 동작하지 않았다")
	}
}

// TestTick_S8_3_OneCheckPassAtATime: 등록 대조 회차는 한 번에 하나다. 앞 회차가 도는 중에 tick 이
// 또 오면 새 회차를 띄우지 않는다 — 겹쳐 띄우면 그때 비어 있는 unit 은 정확히 방금 대조를 마친
// 앞부분이라 앞부분만 반복해 돌고 꼬리가 계속 밀린다. 호출을 흩어도 라이브러리가 뮤텍스로
// 직렬화하므로 벽시계 시간은 줄지 않고 주 경로만 밀린다. [§8.3 "상태 대조 tick", DESIGN §4.4]
func TestTick_S8_3_OneCheckPassAtATime(t *testing.T) {
	h := newHarness(t, 0)
	u1 := h.addUnit(domain.StateRunning)
	u2 := h.addUnit(domain.StateRunning)
	h.gh.mu.Lock()
	h.gh.runners[u1.RunnerName], h.gh.runners[u2.RunnerName] = 1, 2 // 등록돼 있다 → 상태 유지
	h.gh.mu.Unlock()

	h.c.handle(msgTick{}) // 회차 1 시작
	h.c.handle(msgTick{}) // 도는 중 → 새 회차 없음
	h.pumpUntil(isCheckPassDone)
	if got := len(h.gh.snapshot()); got != 2 {
		t.Fatalf("GetRunner %d회, want 2 (unit 하나당 한 번): %v", got, h.gh.snapshot())
	}
	if h.c.checkPass {
		t.Fatal("회차가 끝났는데 checkPass 가 남아 있다(다음 회차가 영영 안 뜬다)")
	}
	// 회차가 끝난 뒤의 tick 은 정상적으로 새 회차를 띄운다.
	h.c.handle(msgTick{})
	h.pumpUntil(isCheckPassDone)
	if got := len(h.gh.snapshot()); got != 4 {
		t.Fatalf("GetRunner %d회, want 4", got)
	}
	if u1.State != domain.StateRunning || u2.State != domain.StateRunning {
		t.Fatalf("등록된 unit 의 상태가 바뀌었다: %s %s", u1.State, u2.State)
	}
}

// preflightProbe 는 preflight 가 머신별로 **동시에** 도는지를 재는 MachineAgent 대역이다.
// 모든 머신이 preflight 에 들어올 때까지 서로를 기다린다: 순차로 돌면 첫 머신이 영영 짝을
// 만나지 못해 timedOut 이 선다. [§7.1-3]
type preflightProbe struct {
	fakeAgent
	arrived  chan<- struct{}
	release  <-chan struct{}
	timedOut *atomic.Bool
	err      error
	// resync 가 참이면 Run 이 실제 Agent 처럼 Resynced 를 보낸다. 거짓이면 아무 통지도 보내지
	// 않는다 — 미도달 머신의 에이전트가 재접속 pre-pull 에 매달려 있는 상황을 재현한다.
	resync bool
}

func (p *preflightProbe) Preflight(context.Context) (runtime.Info, error) {
	p.arrived <- struct{}{}
	select {
	case <-p.release:
	case <-time.After(2 * time.Second):
		p.timedOut.Store(true) // 순차 실행이었다
	}
	if p.err != nil {
		return runtime.Info{}, p.err
	}
	return runtime.Info{CPUs: 4, MemoryBytes: 8 << 30}, nil
}

func (p *preflightProbe) Run(ctx context.Context, sink machine.Sink) {
	if p.resync {
		sink.Resynced(p.name, machine.Snapshot{Info: runtime.Info{CPUs: 4, MemoryBytes: 8 << 30}})
	}
	<-ctx.Done()
}

// TestRun_S7_1_3_PreflightParallelAndOrdered: 시작 preflight 는 머신별 병렬이고, 설정·환경 모순이
// 여러 머신에서 나오면 첫 건에서 멈추지 않고 전부 보고한 뒤 시작 실패한다. 보고 순서는 어느
// goroutine 이 먼저 끝났는지가 아니라 **설정에 적힌 머신 순서**다(로그가 실행마다 같아야 한다).
// 순차로 돌면 pre-pull 상한이 "세션 시작을 막는 시간"을 묶는다는 목적을 달성하지 못한다. [§7.1-3, §8.3]
func TestRun_S7_1_3_PreflightParallelAndOrdered(t *testing.T) {
	names := []string{"m1", "m2", "m3"}
	arrived := make(chan struct{}, len(names))
	release := make(chan struct{})
	var timedOut atomic.Bool
	// m1 과 m3 이 모순, m2 는 정상. m1 을 늦게 끝내 "먼저 끝난 순서"와 설정 순서를 어긋나게 한다.
	fatalOf := map[string]error{
		"m1": fmt.Errorf("%w: R21 머신 %q", machine.ErrFatal, "m1"),
		"m3": fmt.Errorf("%w: R16 머신 %q", machine.ErrFatal, "m3"),
	}
	cfg := &config.Config{
		ScaleSets: []config.ScaleSet{{
			Name: testScaleSet, RunnerGroup: "Default", MaxRunners: 10,
			Unit: resource.Budget{CPU: 1, MemoryBytes: 1 << 30}, Mode: domain.ModeNone,
			RunnerImage: testImage, Machines: names,
		}},
	}
	for _, n := range names {
		cfg.Machines = append(cfg.Machines, config.Machine{Name: n, ScaleSet: testScaleSet, Local: true, Runtime: domain.RuntimeDocker})
	}
	gh := newFakeGH()
	c := New(cfg, gh, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		NewAgent: func(spec machine.Spec, _ *slog.Logger) (MachineAgent, error) {
			p := &preflightProbe{arrived: arrived, release: release, timedOut: &timedOut, err: fatalOf[spec.Name]}
			p.name, p.rt = spec.Name, newFakeRT()
			return p, nil
		},
	})
	go func() { // 전원이 preflight 에 들어오면 풀어준다
		for range names {
			<-arrived
		}
		close(release)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := c.Run(ctx)
	if timedOut.Load() {
		t.Fatal("preflight 가 순차로 돌았다(머신끼리 만나지 못했다)")
	}
	if err == nil {
		t.Fatal("설정·환경 모순이 있는데 시작에 성공했다")
	}
	msg := err.Error()
	i1, i3 := strings.Index(msg, `"m1"`), strings.Index(msg, `"m3"`)
	if i1 < 0 || i3 < 0 {
		t.Fatalf("모순 머신을 전부 보고하지 않았다: %v", err)
	}
	if i1 > i3 {
		t.Fatalf("보고 순서가 설정 머신 순서가 아니다: %v", err)
	}
	if strings.Contains(msg, `"m2"`) {
		t.Fatalf("정상 머신이 실패로 보고됐다: %v", err)
	}
}

// TestRun_S7_1_4_NoWaitForUnreachedMachine: 시작 preflight 에 실패한 머신은 세션 시작을 기다리게
// 하지 않는다. 기다리면 그 머신의 에이전트 첫 회차가 Preflight(접속 + pre-pull)를 다시 돌아
// §8.3 이 "세션 시작을 막을 수 있는 최악의 시간"이라고 규정한 pre-pull 상한이 두 번 얹힌다.
// 그 머신은 배치·capacity 대상이 아니므로 기다릴 이유도 없다. [§7.1-4, §7.1-7 → §7.1-9, §8.3]
func TestRun_S7_1_4_NoWaitForUnreachedMachine(t *testing.T) {
	names := []string{"m1", "m2"}
	cfg := &config.Config{
		ScaleSets: []config.ScaleSet{{
			Name: testScaleSet, RunnerGroup: "Default", MaxRunners: 10,
			Unit: resource.Budget{CPU: 1, MemoryBytes: 1 << 30}, Mode: domain.ModeNone,
			RunnerImage: testImage, Machines: names,
		}},
	}
	for _, n := range names {
		cfg.Machines = append(cfg.Machines, config.Machine{Name: n, ScaleSet: testScaleSet, Local: true, Runtime: domain.RuntimeDocker})
	}
	gh := newFakeGH()
	arrived := make(chan struct{}, len(names))
	release := make(chan struct{})
	close(release)
	var timedOut atomic.Bool
	c := New(cfg, gh, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		NewAgent: func(spec machine.Spec, _ *slog.Logger) (MachineAgent, error) {
			p := &preflightProbe{arrived: arrived, release: release, timedOut: &timedOut}
			p.name, p.rt = spec.Name, newFakeRT()
			if spec.Name == "m1" { // 미도달: pre-pull 실패. 에이전트도 아무 통지를 보내지 않는다
				p.err = errors.New("pre-pull img: 상한 초과")
			} else {
				p.resync = true
			}
			return p, nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// m1 이 아무 통지도 보내지 않아도 세션이 열려야 한다.
	sessionTried := func() bool {
		for _, call := range gh.snapshot() {
			if strings.HasPrefix(call, "NewSession") {
				return true
			}
		}
		return false
	}
	deadline := time.After(2 * time.Second)
	for !sessionTried() {
		select {
		case err := <-done:
			t.Fatalf("세션을 열기 전에 Run 이 끝났다: %v", err)
		case <-deadline:
			t.Fatal("미도달 머신(m1)의 첫 통지를 기다리느라 세션이 열리지 않았다")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// TestRegistration_S7_2_3_LateIDBackfillsPending: 입양 busy unit 이 죽고 정리까지 끝난 뒤 도착한
// 등록 대조 결과라도 그 runner id 는 배운다. 입양 unit 은 GenerateJIT 결과가 없어 이 응답이 유일한
// id 출처이고, 버리면 이름 없는 JobCompleted 와 대조할 수단이 사라져 pendingCompletion 항목이
// 만료(5분)까지 남아 1개 과소 배치가 된다. 두 도착 순서 모두 만료를 기다리지 않아야 한다.
// [§7.2-3, DESIGN §4.5]
func TestRegistration_S7_2_3_LateIDBackfillsPending(t *testing.T) {
	const runnerID = int64(4242)

	// (가) 대조 결과가 먼저, 이름 없는 JobCompleted 가 나중.
	h := newHarness(t, 0)
	u := h.addUnit(domain.StateRunning, func(u *domain.Unit) { u.Busy = true; u.Adopted = true })
	ss := h.c.scaleSets[testScaleSet]
	name := u.RunnerName
	h.c.handle(msgEvent{Machine: testMachine, Ev: runtime.Event{Name: domain.ContainerName(u.ID, domain.RoleRunner), Action: "die"}})
	h.pumpUntil(isCleanupOf(u.ID))
	if _, ok := h.c.units[u.ID]; ok {
		t.Fatal("정리가 끝나지 않았다")
	}
	if e, ok := ss.pending[name]; !ok || e.RunnerID != 0 {
		t.Fatalf("pending %+v, want 항목 있음 + RunnerID 0 (아직 모른다)", ss.pending)
	}
	h.c.handle(msgRegistration{Unit: u.ID, Found: true, RunnerID: runnerID, ScaleSet: testScaleSet, RunnerName: name})
	if e := ss.pending[name]; e.RunnerID != runnerID {
		t.Fatalf("늦은 대조 결과의 runner id 를 버렸다: %+v", ss.pending)
	}
	h.c.handle(msgJobCompleted{ScaleSet: testScaleSet, RunnerID: runnerID}) // 이름 없는 콜백
	if _, ok := ss.pending[name]; ok {
		t.Fatalf("JobCompleted 로 항목이 빠지지 않았다(만료를 기다리게 된다): %+v", ss.pending)
	}

	// (나) 반대 순서: 이름 없는 JobCompleted 가 먼저 도착해 completedIDs 에 보관되고, 늦은 대조
	// 결과가 id 를 배우는 순간 즉시 대조된다.
	h2 := newHarness(t, 0)
	u2 := h2.addUnit(domain.StateRunning, func(u *domain.Unit) { u.Busy = true; u.Adopted = true })
	ss2 := h2.c.scaleSets[testScaleSet]
	name2 := u2.RunnerName
	h2.c.handle(msgEvent{Machine: testMachine, Ev: runtime.Event{Name: domain.ContainerName(u2.ID, domain.RoleRunner), Action: "die"}})
	h2.pumpUntil(isCleanupOf(u2.ID))
	h2.c.handle(msgJobCompleted{ScaleSet: testScaleSet, RunnerID: runnerID})
	if _, ok := ss2.completedIDs[runnerID]; !ok {
		t.Fatalf("이름 없는 JobCompleted 가 보관되지 않았다: %+v", ss2.completedIDs)
	}
	h2.c.handle(msgRegistration{Unit: u2.ID, Found: true, RunnerID: runnerID, ScaleSet: testScaleSet, RunnerName: name2})
	if _, ok := ss2.pending[name2]; ok {
		t.Fatalf("id 를 배운 뒤에도 항목이 남았다: %+v", ss2.pending)
	}
	if _, ok := ss2.completedIDs[runnerID]; ok {
		t.Fatalf("대조된 completedIDs 항목이 남았다: %+v", ss2.completedIDs)
	}
}
