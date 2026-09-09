package controller

import (
	"fmt"
	"time"

	"gh-ars/internal/domain"
	"gh-ars/internal/plan"
	"gh-ars/internal/runtime"
)

// inbox 메시지. 루프(handle)만 상태를 만진다. [DESIGN §6]
type (
	msgDesired struct { // listener HandleDesiredRunnerCount. Assigned = TotalAssignedJobs [§7.2-3]
		ScaleSet string
		Assigned int
		Reply    chan int
	}
	msgEvent struct { // 머신 events 스트림 한 건 [§7.2-5]
		Machine string
		Ev      runtime.Event
	}
	msgHealth struct { // 머신 health 변화. Healthy 복귀는 msgResynced 가 맡는다 [§7.1-8]
		Machine string
		// Health 는 Unhealthy(재접속 대상) 또는 Failed(설정·환경 모순, 재접속 없음)다. [DESIGN §3.3]
		Health domain.Health
		Err    error
	}
	msgResynced struct { // 전체 동기화 스냅샷 [§7.1-7, §8.3]
		Machine string
		At      time.Time // 관측 시각(0 이면 판정 시각으로 본다)
		// Info 는 그 회차의 runtime info 다. 재접속으로 healthy 가 되는 머신의 예산·physicalMax·
		// effectiveMax 를 여기서 다시 계산한다(시작 시 미도달이었으면 그때까지 0 이다). [§7.1-3, R21, R22]
		Info runtime.Info
		Obs  plan.Observed
	}
	msgTick        struct{} // 30s [§8.3 상수 표]
	msgUnitStarted struct { // 비동기 create→cp→start 완료 [§7.2-4]
		Unit       domain.UnitID
		ScaleSet   string
		RunnerName string
		RunnerID   int64 // GenerateJIT 가 준 id. unit 이 이미 죽었어도 pendingCompletion 대조에 필요하다
		Err        error
	}
	msgJobStarted struct { // listener HandleJobStarted → Busy [§7.2-3]
		ScaleSet, RunnerName string
	}
	msgJobCompleted struct { // listener HandleJobCompleted → Completed / pendingCompletion [§7.2-3]
		ScaleSet, RunnerName string
		RunnerID             int64
	}
	msgCleanupDone struct { // 비동기 cleanupUnit 결과 [§8.3]
		Unit domain.UnitID
		Err  error
	}
	// msgRegistration 은 tick 이 goroutine 으로 뺀 GetRunner 대조 결과다. 느린 GitHub 호출을
	// 루프 안에서 하지 않기 위한 것으로, 판정 자체는 msgTick 의 규칙(DESIGN §6)을 그대로 적용한다. [§8.3]
	msgRegistration struct {
		Unit     domain.UnitID
		Found    bool
		RunnerID int64 // Found 일 때. 입양 unit 의 id 를 여기서 처음 안다
		Err      error
	}
	msgSessionStarted struct{ ScaleSet string } // 메시지 세션 (재)시작 → pendingCompletion 비움 [§7.2-3 안전장치 1]
)

// handle 은 루프 본체다. 메시지 하나를 처리한다. 테스트는 이것을 직접 부른다. [DESIGN §6]
func (c *Controller) handle(m any) {
	switch m := m.(type) {
	case msgDesired:
		n := c.handleDesired(m.ScaleSet, m.Assigned)
		if m.Reply != nil {
			m.Reply <- n
		}
	case msgEvent:
		c.handleEvent(m)
	case msgHealth:
		c.handleHealth(m)
	case msgResynced:
		c.handleResynced(m)
	case msgTick:
		c.handleTick()
	case msgUnitStarted:
		c.handleUnitStarted(m)
	case msgJobStarted:
		c.handleJobStarted(m)
	case msgJobCompleted:
		c.handleJobCompleted(m)
	case msgCleanupDone:
		c.handleCleanupDone(m)
	case msgRegistration:
		c.handleRegistration(m)
	case msgSessionStarted:
		if ss := c.scaleSets[m.ScaleSet]; ss != nil {
			clear(ss.pending)
		}
	default:
		c.log.Error("unknown message", "type", fmt.Sprintf("%T", m))
	}
}

// handleDesired 는 §7.2-3 이다. 신규 unit 생성은 여기서만 일어난다. 반환값은 running 목표(desired).
//
//	assigned = max(0, TotalAssignedJobs − |pendingCompletion|)
//	desired  = min(capacity, max(minRunners, assigned))
//	create   = desired − running
func (c *Controller) handleDesired(name string, totalAssigned int) int {
	ss := c.scaleSets[name]
	if ss == nil {
		c.log.Error("desired for unknown scale set", "scaleSet", name)
		return 0
	}
	capacity := c.recomputeCapacity(ss)
	assigned := max(0, totalAssigned-len(ss.pending))
	desired := plan.Desired(capacity, ss.MinRunners, assigned)
	running := plan.Running(c.unitList(), ss.Name)
	c.log.Debug("desired", "scaleSet", ss.Name, "totalAssigned", totalAssigned, "pending", len(ss.pending),
		"capacity", capacity, "desired", desired, "running", running)
	if create := desired - running; create > 0 {
		c.createUnits(ss, create)
	} else if remove := running - desired; remove > 0 {
		// 유휴 runner 축소(Draining)는 Phase 11. [§7.2-3]
		c.log.Debug("scale-down candidates present (not implemented yet)", "scaleSet", ss.Name, "remove", remove)
	}
	return desired
}

// createUnits 는 spread 로 머신을 고르고 Creating unit 을 등록한 뒤 startUnit goroutine 을 띄운다.
// 후보 머신이 없으면 pending 으로 남긴다(job 은 GitHub 큐에서 대기). [§7.2-3, §8.2]
func (c *Controller) createUnits(ss *scaleSetState, n int) {
	if c.syncing {
		// 전체 동기화가 끝나기 전에는 배치하지 않는다: 아직 입양하지 못한 머신의 slot 점유를 모른 채
		// spread 를 돌리면 §8.2 의 여유 슬롯 판단이 틀린다(이 구간에 오는 것은 다른 머신의 die 가
		// 부르는 minRunners 보충뿐이며, 세션이 열린 뒤 첫 desired 계산이 그대로 메운다). [§7.1-7 → §7.1-9]
		c.log.Debug("initial sync in progress, deferring unit creation", "scaleSet", ss.Name, "n", n)
		return
	}
	for i := 0; i < n; i++ {
		now := c.opts.Now()
		mname, ok := plan.Spread(c.machinesOf(ss.Name), plan.Occupied(c.unitList()), now)
		if !ok {
			c.log.Warn("no machine has a free slot, job stays pending", "scaleSet", ss.Name, "pending", n-i)
			return
		}
		id := c.opts.NewUnitID()
		u := &domain.Unit{
			ID: id, ScaleSet: ss.Name, Machine: mname, Mode: ss.Mode, State: domain.StateCreating,
			CreatedAt: now, RunnerName: domain.RunnerName(ss.Name, mname, id),
		}
		c.units[id] = u
		c.machineByName(mname).LastPlacedAt = now
		c.log.Info("unit creating", "unit", id, "scaleSet", ss.Name, "machine", mname, "runner", u.RunnerName)
		// goroutine 은 상태를 만지지 않는다: 루프에서 복사한 값만 넘긴다. [DESIGN §1-2, §6]
		snapshot, ssCopy, rt := *u, ss.ScaleSet, c.agents[mname].Runtime()
		c.spawn(func() { c.startUnit(snapshot, ssCopy, rt) })
	}
}

// handleEvent 는 runner 컨테이너의 die 를 Dying 으로 보내고 정리를 띄운다. [§7.2-5, §8.3]
func (c *Controller) handleEvent(m msgEvent) {
	if m.Ev.Action != "die" {
		return
	}
	id, role, ok := domain.ParseContainerName(m.Ev.Name)
	if !ok {
		return
	}
	u := c.units[id]
	if role == domain.RoleSidecar {
		// runner 가 살아 있으면 로그만. runner die 때 함께 정리된다. [DESIGN §6]
		c.log.Warn("sidecar died", "unit", id, "machine", m.Machine, "exitCode", m.Ev.ExitCode)
		return
	}
	if u == nil {
		c.log.Warn("die for unknown unit", "unit", id, "machine", m.Machine)
		return
	}
	if u.State == domain.StateDying || u.State == domain.StateRemoved {
		return
	}
	c.log.Info("runner died", "unit", id, "state", u.State, "exitCode", m.Ev.ExitCode)
	ss := c.scaleSets[u.ScaleSet]
	u.Parts.Runner = true // exited 컨테이너가 남아 있다
	c.markDying(u)
	// minRunners 미달분만 보충한다. assigned 기반 생성은 다음 msgDesired 에서. [§7.2-5]
	// 보충 목표도 capacity 로 자른다: R24 가 경고만 하고 지나가는 경우(미도달 머신이 있는 시작)
	// minRunners > capacity 로 돌 수 있는데, 그때 미달분을 그대로 만들면 maxRunners 를 넘는다. [§7.2-3]
	if ss != nil {
		warm := plan.Desired(plan.Capacity(ss.ScaleSet, c.machines), ss.MinRunners, 0)
		if short := warm - plan.Running(c.unitList(), ss.Name); short > 0 {
			c.createUnits(ss, short)
		}
	}
}

// markDying 은 살아 있던 unit 을 Dying 으로 보내는 유일한 전이다(die, tick 미등록, 기동 타임아웃, 시작 실패, 재동기화).
// busy 였고 완료 표시가 없으면 어느 경로로 죽었든 통계가 아직 반영하지 않은 완료분으로 세어
// pendingCompletion 에 넣는다 — die 가 아니라 tick 의 미등록 판정으로 먼저 Dying 이 되는 경우
// (job 종료 직후 등록이 먼저 사라진다, §8.3)에도 빈 폴링의 캐시 통계로 유휴 runner 를 만들지 않기 위함이다. [§7.2-3, §7.2-5]
func (c *Controller) markDying(u *domain.Unit) {
	if u.State != domain.StateDying {
		if ss := c.scaleSets[u.ScaleSet]; ss != nil && u.Busy && !u.Completed {
			ss.pending[u.RunnerName] = pendingEntry{At: c.opts.Now(), Machine: u.Machine, RunnerID: c.runnerIDs[u.ID]}
		}
	}
	u.State = domain.StateDying
	c.startCleanup(u)
}

func (c *Controller) startCleanup(u *domain.Unit) {
	if c.cleaning[u.ID] {
		return
	}
	m := c.machineByName(u.Machine)
	if m == nil || m.Health != domain.Healthy {
		return // unhealthy 머신의 Dying 은 복귀 후 전체 동기화에서 정리 [§8.3]
	}
	agent := c.agents[u.Machine]
	if agent == nil {
		return
	}
	c.cleaning[u.ID] = true
	snapshot := *u
	c.spawn(func() { c.cleanupUnit(snapshot, agent.Runtime()) })
}

// handleHealth 는 머신 health 를 갱신하고 capacity 를 다시 계산한다.
// Failed 는 되돌리지 않는다: 그 머신은 배치·capacity 와 재접속 대상에서 영구 제외다. [§7.1-8, §10.1, DESIGN §3.3, R21]
func (c *Controller) handleHealth(m msgHealth) {
	mc := c.machineByName(m.Machine)
	if mc == nil || mc.Health == domain.Failed {
		return
	}
	switch m.Health {
	case domain.Healthy:
		mc.Health = domain.Healthy
	case domain.Failed:
		c.log.Error("machine failed, excluded permanently", "machine", m.Machine, "err", m.Err)
		mc.Health = domain.Failed
	default:
		if mc.Health == domain.Healthy {
			c.log.Warn("machine unhealthy", "machine", m.Machine, "err", m.Err)
		}
		mc.Health = domain.Unhealthy
	}
	c.recomputeAll()
}

// handleResynced 는 §7.1-7 전체 동기화다. 부품은 스냅샷이 권위: known unit 의 Parts 를 덮어쓰고
// State·Busy 는 유지한 뒤 plan.Reconcile 결과(Adopt/RemoveUnit/RemoveOrphan)를 적용한다. [§8.3, DESIGN §6]
func (c *Controller) handleResynced(m msgResynced) {
	seen := partsOf(m.Obs)
	// 관측 뒤에 Starting 이 된 unit 은 스냅샷이 찍힐 때 Creating 이었다: §8.3 Creating 예외를 그대로 적용해
	// 판정에서 뺀다(그 부품은 다음 동기화가 본다). 관측 시각이 없으면 처리 시각 기준이라 제외되지 않는다.
	inFlight := func(u *domain.Unit) bool {
		if u.State == domain.StateCreating {
			return true
		}
		at, ok := c.startedAt[u.ID]
		return ok && !m.At.IsZero() && at.After(m.At)
	}
	for _, u := range c.units {
		if u.Machine == m.Machine && !inFlight(u) {
			u.Parts = seen[u.ID]
		}
	}
	known := map[domain.UnitID]domain.Unit{}
	for id, u := range c.units {
		k := *u
		if inFlight(u) {
			k.State = domain.StateCreating
		}
		known[id] = k
	}
	cfg := plan.ReconcileConfig{KnownScaleSets: map[string]domain.Mode{}}
	for name, ss := range c.scaleSets {
		cfg.KnownScaleSets[name] = ss.Mode
	}
	mc := c.machineByName(m.Machine)
	if mc != nil && mc.Health != domain.Failed { // Failed 머신은 복귀하지 않는다 [DESIGN §3.3]
		// 예산 재계산이 먼저다: 시작 시 미도달이었던 머신은 physicalMax·effectiveMax 가 0 이라
		// healthy 로만 돌려놓으면 capacity 에 한 칸도 보태지 못하고 배치에서도 계속 빠진다.
		// 여기서 드러난 R21 위반은 §7.1-3 대로 그 머신만 Failed 로 만든다. [§7.1-3, §8.1, R21, R22]
		if m.Info.CPUs > 0 && m.Info.MemoryBytes > 0 { // info 를 담은 회차의 스냅샷만
			if err := c.applyInfo(mc, m.Info); err != nil {
				c.log.Error("machine failed, excluded permanently", "machine", m.Machine, "err", err)
				mc.Health = domain.Failed
				c.recomputeAll()
				return
			}
		}
		if mc.Health != domain.Healthy {
			c.log.Info("machine healthy", "machine", m.Machine)
		}
		mc.Health = domain.Healthy // 정리·입양 명령을 보낼 수 있어야 하므로 먼저 복귀시킨다
	}
	orphans := map[domain.UnitID]domain.Parts{}
	for _, a := range plan.Reconcile(m.Obs, known, c.opts.Now(), cfg) {
		switch a.Kind {
		case plan.ActionAdopt:
			u := a.Unit
			c.units[u.ID] = &u
			c.log.Info("unit adopted", "unit", u.ID, "scaleSet", u.ScaleSet, "machine", u.Machine, "foreign", u.Foreign, "runner", u.RunnerName)
		case plan.ActionRemoveUnit:
			u := c.units[a.Unit.ID]
			if u == nil {
				nu := a.Unit
				u = &nu
				c.units[u.ID] = u
			} else {
				u.Parts = a.Unit.Parts
			}
			c.log.Info("unit removal from resync", "unit", u.ID, "state", u.State)
			c.markDying(u)
		case plan.ActionRemoveOrphan:
			id, p := orphanPart(a.Orphan)
			cur := orphans[id]
			cur.Sidecar, cur.Slice = cur.Sidecar || p.Sidecar, cur.Slice || p.Slice
			for i := range cur.Volumes {
				cur.Volumes[i] = cur.Volumes[i] || p.Volumes[i]
			}
			orphans[id] = cur
		}
	}
	// 머신 내부 고아(runner 없는 sidecar/볼륨/slice)는 항상 rm 한다. 부품 집합을 runner 이름 없는
	// Dying unit 으로 등록해 §8.3 정리 순서(sidecar → 볼륨 → slice)와 tick 재시도를 그대로 탄다.
	// GitHub 등록 단계는 이름을 모르므로 건너뛴다(고아 등록은 GitHub 이 자동 제거, §3.2). [§8.3]
	for id, p := range orphans {
		if _, known := c.units[id]; known {
			continue
		}
		u := &domain.Unit{ID: id, Machine: m.Machine, Mode: domain.ModeSidecar, State: domain.StateDying, CreatedAt: c.opts.Now(), Parts: p, Foreign: true}
		c.units[id] = u
		c.log.Info("orphan parts found, removing", "unit", id, "machine", m.Machine, "parts", fmt.Sprintf("%+v", p))
		c.startCleanup(u)
	}
	// 이 머신 소속 unit 의 pendingCompletion 항목을 비운다. [§7.2-3 안전장치 1]
	for _, ss := range c.scaleSets {
		for name, e := range ss.pending {
			if e.Machine == m.Machine {
				delete(ss.pending, name)
			}
		}
	}
	c.recomputeAll()
}

// orphanPart 는 RemoveOrphan 하나를 unit id 와 부품 하나로 바꾼다. [§8.3]
func orphanPart(o plan.Orphan) (domain.UnitID, domain.Parts) {
	var p domain.Parts
	switch o.Kind {
	case plan.OrphanContainer:
		id, role, _ := domain.ParseContainerName(o.Name)
		p.Sidecar = role == domain.RoleSidecar
		p.Runner = role == domain.RoleRunner
		return id, p
	case plan.OrphanVolume:
		id, kind, _ := domain.ParseVolumeName(o.Name)
		for i, k := range domain.VolumeKinds {
			if k == kind {
				p.Volumes[i] = true
			}
		}
		return id, p
	case plan.OrphanSlice:
		id, _ := domain.ParseSliceName(o.Name)
		p.Slice = true
		return id, p
	}
	return "", p
}

// partsOf 는 스냅샷을 unit 별 부품 집합으로 묶는다. plan.Reconcile 의 collect 와 같은 규칙. [§4.2, §4.3]
func partsOf(obs plan.Observed) map[domain.UnitID]domain.Parts {
	out := map[domain.UnitID]domain.Parts{}
	for _, ctr := range obs.Containers {
		id, role, ok := domain.ParseContainerName(ctr.Name)
		if !ok {
			continue
		}
		p := out[id]
		switch role {
		case domain.RoleRunner:
			p.Runner = true
		case domain.RoleSidecar:
			p.Sidecar = true
		}
		out[id] = p
	}
	for _, v := range obs.Volumes {
		id, kind, ok := domain.ParseVolumeName(v)
		if !ok {
			continue
		}
		p := out[id]
		for i, k := range domain.VolumeKinds {
			if k == kind {
				p.Volumes[i] = true
			}
		}
		out[id] = p
	}
	for _, s := range obs.Slices {
		id, ok := domain.ParseSliceName(s)
		if !ok {
			continue
		}
		p := out[id]
		p.Slice = true
		out[id] = p
	}
	return out
}

// handleTick 은 GitHub 등록 대조의 유일한 주체다. [§8.3, DESIGN §6]
//   - Starting/Running: GetRunner 대조를 goroutine 으로 띄운다(Draining 제외). 결과는 msgRegistration.
//   - Creating: 기동 타임아웃 2분 초과 → Dying.
//   - Dying: healthy 머신이고 정리가 진행 중이 아니면 재시도.
//   - pendingCompletion: 5분 지난 항목 제거.
func (c *Controller) handleTick() {
	now := c.opts.Now()
	var toCheck []domain.Unit
	for _, u := range c.units {
		switch u.State {
		case domain.StateStarting, domain.StateRunning:
			if !c.checking[u.ID] {
				c.checking[u.ID] = true
				toCheck = append(toCheck, *u)
			}
		case domain.StateCreating:
			if now.Sub(u.CreatedAt) > startTimeout {
				c.log.Warn("unit start timeout", "unit", u.ID, "since", now.Sub(u.CreatedAt))
				u.Parts.Runner = true
				c.markDying(u)
			}
		case domain.StateDying:
			c.startCleanup(u)
		}
	}
	if len(toCheck) > 0 {
		c.spawn(func() { c.checkRegistrations(toCheck) })
	}
	for _, ss := range c.scaleSets {
		for name, e := range ss.pending {
			if now.Sub(e.At) > pendingExpiry {
				c.log.Warn("pendingCompletion expired", "scaleSet", ss.Name, "runner", name)
				delete(ss.pending, name)
			}
		}
		for id, at := range ss.completedIDs {
			if now.Sub(at) > pendingExpiry {
				delete(ss.completedIDs, id)
			}
		}
	}
}

// handleRegistration 은 tick 대조 결과를 적용한다. [§8.3 "runner 살아 있는데 등록 없음" 행]
//   - Starting: 등록되면 Running. 미등록이면 grace(5분) 이내 대기, 초과면 Dying.
//   - Running: 미등록이면 grace 없이 Dying.
func (c *Controller) handleRegistration(m msgRegistration) {
	delete(c.checking, m.Unit)
	u := c.units[m.Unit]
	if u == nil {
		return
	}
	if m.Err != nil {
		c.log.Warn("registration check failed", "unit", u.ID, "runner", u.RunnerName, "err", m.Err)
		return
	}
	if m.Found {
		c.learnRunnerID(u.ScaleSet, u.RunnerName, u, m.RunnerID)
	}
	switch u.State {
	case domain.StateStarting:
		if m.Found {
			u.State = domain.StateRunning
			c.log.Info("runner registered", "unit", u.ID, "runner", u.RunnerName)
			return
		}
		if age := c.opts.Now().Sub(u.CreatedAt); age > grace {
			c.log.Warn("runner not registered within grace", "unit", u.ID, "runner", u.RunnerName, "age", age)
			c.markDying(u)
		}
	case domain.StateRunning:
		if !m.Found {
			c.log.Info("runner registration gone, removing unit", "unit", u.ID, "runner", u.RunnerName)
			c.markDying(u)
		}
	}
}

// handleUnitStarted 는 create→cp→start 결과다. 이미 Dying(Creating 중 die)이면 무시한다. [§7.2-4, DESIGN §3.2]
func (c *Controller) handleUnitStarted(m msgUnitStarted) {
	u := c.units[m.Unit]
	if m.RunnerID != 0 {
		c.learnRunnerID(m.ScaleSet, m.RunnerName, u, m.RunnerID)
	}
	if u == nil || u.State != domain.StateCreating {
		return
	}
	c.startedAt[u.ID] = c.opts.Now()
	if m.Err != nil {
		c.log.Error("unit start failed", "unit", u.ID, "machine", u.Machine, "err", m.Err)
		u.Parts.Runner = true // startUnit 이 역순 정리했더라도 남은 것이 있을 수 있어 정리 경로를 한 번 더 탄다
		c.markDying(u)
		return
	}
	u.State = domain.StateStarting
	u.Parts.Runner = true
	if u.Mode == domain.ModeSidecar {
		u.Parts = domain.Parts{Runner: true, Sidecar: true, Volumes: [3]bool{true, true, true}, Slice: true}
	}
	c.log.Info("unit started", "unit", u.ID, "runner", u.RunnerName, "runnerID", m.RunnerID)
}

// learnRunnerID 는 runner id 를 처음 알게 된 시점(msgUnitStarted, 입양 unit 의 tick 등록 확인)에 부른다.
// unit 이 그 사이 죽었거나(Creating 중 die) 이미 정리됐어도 남긴다: busy 로 죽은 unit 의 pendingCompletion
// 항목은 이름이 빈 JobCompleted 를 이 id 로 대조하고, id 를 몰라 보관해 둔 완료가 있으면 지금 적용한다. [§7.2-3, DESIGN §4.5]
func (c *Controller) learnRunnerID(scaleSet, runnerName string, u *domain.Unit, id int64) {
	if u != nil {
		c.runnerIDs[u.ID] = id
	}
	ss := c.scaleSets[scaleSet]
	if ss == nil {
		return
	}
	if e, ok := ss.pending[runnerName]; ok && e.RunnerID == 0 {
		e.RunnerID = id
		ss.pending[runnerName] = e
	}
	if _, done := ss.completedIDs[id]; done {
		delete(ss.completedIDs, id)
		delete(ss.pending, runnerName)
		if u != nil && u.State != domain.StateDying && u.State != domain.StateRemoved {
			u.Completed = true
		}
	}
}

func (c *Controller) handleJobStarted(m msgJobStarted) {
	u := c.unitByRunnerName(m.ScaleSet, m.RunnerName)
	if u == nil {
		c.log.Warn("JobStarted for unknown runner", "scaleSet", m.ScaleSet, "runner", m.RunnerName)
		return
	}
	u.Busy = true
}

// handleJobCompleted: unit 이 살아 있으면 Completed, 이미 die 했거나 없으면 pendingCompletion 에서 뺀다. [§7.2-3]
func (c *Controller) handleJobCompleted(m msgJobCompleted) {
	ss := c.scaleSets[m.ScaleSet]
	if ss == nil {
		return
	}
	name := m.RunnerName
	u := c.unitByRunnerName(m.ScaleSet, name)
	if u == nil && name == "" {
		for id, rid := range c.runnerIDs {
			if rid == m.RunnerID && c.units[id] != nil && c.units[id].ScaleSet == m.ScaleSet {
				u = c.units[id]
			}
		}
	}
	if u != nil && u.State != domain.StateDying && u.State != domain.StateRemoved {
		u.Completed = true
		return
	}
	if u != nil {
		name = u.RunnerName
	}
	if name == "" {
		// 이미 정리된 unit: pendingCompletion 항목이 RunnerID 를 보관한다.
		for n, e := range ss.pending {
			if e.RunnerID == m.RunnerID {
				name = n
			}
		}
	}
	if name != "" {
		delete(ss.pending, name)
		return
	}
	if m.RunnerID != 0 {
		// 아직 id 를 모르는 unit(msgUnitStarted 전)일 수 있다. 보관했다가 id 를 알게 되면 적용한다. [§7.2-3]
		ss.completedIDs[m.RunnerID] = c.opts.Now()
	}
}

// handleCleanupDone: 성공이면 Removed(상태에서 삭제, slot 해제). 실패면 Dying 유지, 다음 tick 재시도. [§8.3]
func (c *Controller) handleCleanupDone(m msgCleanupDone) {
	delete(c.cleaning, m.Unit)
	u := c.units[m.Unit]
	if u == nil {
		return
	}
	if m.Err != nil {
		c.log.Warn("unit cleanup failed, will retry", "unit", u.ID, "err", m.Err)
		return
	}
	delete(c.units, u.ID)
	delete(c.runnerIDs, u.ID)
	delete(c.startedAt, u.ID)
	c.log.Info("unit removed", "unit", u.ID, "scaleSet", u.ScaleSet, "machine", u.Machine)
}

// --- helpers ---

func (c *Controller) unitList() []domain.Unit {
	out := make([]domain.Unit, 0, len(c.units))
	for _, u := range c.units {
		out = append(out, *u)
	}
	return out
}

func (c *Controller) machinesOf(scaleSet string) []domain.Machine {
	var out []domain.Machine
	for _, m := range c.machines {
		if m.ScaleSet == scaleSet {
			out = append(out, m)
		}
	}
	return out
}

func (c *Controller) machineByName(name string) *domain.Machine {
	for i := range c.machines {
		if c.machines[i].Name == name {
			return &c.machines[i]
		}
	}
	return nil
}

func (c *Controller) unitByRunnerName(scaleSet, runnerName string) *domain.Unit {
	if runnerName == "" {
		return nil
	}
	for _, u := range c.units {
		if u.ScaleSet == scaleSet && u.RunnerName == runnerName {
			return u
		}
	}
	return nil
}

func (c *Controller) recomputeAll() {
	for _, ss := range c.scaleSets {
		c.recomputeCapacity(ss)
	}
}

// spawn 은 goroutine 을 wg 에 등록해 Run 종료 시 기다린다.
func (c *Controller) spawn(f func()) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		f()
	}()
}
