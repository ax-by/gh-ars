package plan

import (
	"sort"
	"time"

	"gh-ars/internal/domain"
	"gh-ars/internal/runtime"
)

// Observed 는 머신 하나의 재동기화 스냅샷이다. GitHub 정보는 없다.
// 등록 대조는 Controller 의 tick 이 한다 (DESIGN §5 책임 경계). [§7.1-7, §8.3]
type Observed struct {
	Machine    string
	Containers []runtime.Container // ps -a --filter label=gh-ars.unit (종료된 컨테이너 포함)
	Volumes    []string
	Slices     []string
}

// ReconcileConfig 는 Foreign 판정에 쓰는 YAML 쪽 정보다. [§8.3]
type ReconcileConfig struct {
	KnownScaleSets map[string]domain.Mode
}

// ActionKind 는 재동기화 결과 Controller 가 할 일이다. [§8.3]
type ActionKind string

const (
	ActionAdopt        ActionKind = "Adopt"
	ActionRemoveUnit   ActionKind = "RemoveUnit"
	ActionRemoveOrphan ActionKind = "RemoveOrphan"
)

// OrphanKind 는 runner 컨테이너 없이 남은 부품의 종류다. [§8.3]
type OrphanKind string

const (
	OrphanContainer OrphanKind = "container"
	OrphanVolume    OrphanKind = "volume"
	OrphanSlice     OrphanKind = "slice"
)

// Orphan 은 unit 없이 남은 부품 하나다. 항상 rm 대상. [§8.3]
type Orphan struct {
	Kind OrphanKind
	Name string
}

// Action 은 Kind 에 따라 Unit(Adopt, RemoveUnit) 또는 Orphan(RemoveOrphan)만 채운다.
// RemoveUnit 실행의 첫 단계가 GitHub 등록 처리다(별도 Action 이 아니다). [§8.3 정리 순서]
type Action struct {
	Kind   ActionKind
	Unit   domain.Unit
	Orphan Orphan
}

// observedUnit 은 한 unit id 에 대해 이 머신에서 관측된 부품 집합이다.
type observedUnit struct {
	runner  *runtime.Container
	sidecar *runtime.Container
	parts   domain.Parts
}

// isAlive 는 runner 컨테이너가 살아 있는지다. §8.3 의 모든 판정 기준.
// paused 는 프로세스가 남아 있으므로 살아 있는 쪽이다(gh-ars 가 pause 하지는 않는다).
// created(start 전 중단)·exited·dead 는 살아 있지 않다.
func isAlive(state string) bool {
	return state == "running" || state == "restarting" || state == "paused"
}

// Reconcile 은 재동기화 스냅샷만으로 §8.3 판정표 중 부품 집합으로 판정하는 행을 처리한다.
// GitHub 등록은 조회하지 않는다. 결과는 unit id 순으로 결정적이다. [§8.3]
func Reconcile(obs Observed, known map[domain.UnitID]domain.Unit, now time.Time, cfg ReconcileConfig) []Action {
	seen := collect(obs)

	ids := make([]domain.UnitID, 0, len(seen)+len(known))
	for id := range seen {
		ids = append(ids, id)
	}
	for id, u := range known {
		if _, ok := seen[id]; !ok && u.Machine == obs.Machine {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	actions := make([]Action, 0, len(ids))
	for _, id := range ids {
		ou := seen[id]
		ku, isKnown := known[id]

		// 아는 unit 이 Creating 이면 create→cp→start 가 진행 중이라 부품 집합이 원래 불완전하다
		// (slice·볼륨·sidecar 만 있거나 runner 가 아직 created). 판정하지 않고 넘긴다.
		// 실패·기동 타임아웃 2분·die 는 Controller 가 본다. [DESIGN §3.2, §7.2-4]
		if isKnown && ku.State == domain.StateCreating {
			continue
		}

		// 이 머신에 부품이 하나도 없는데 알고 있는 unit: 컨테이너가 사라졌으므로 정리한다.
		if ou == nil {
			actions = append(actions, Action{Kind: ActionRemoveUnit, Unit: dyingUnit(ku, obs, domain.Parts{})})
			continue
		}

		switch {
		// Dying 은 부품이 남아 있는 한 매 동기화마다 정리를 재시도한다. [§8.3]
		case isKnown && ku.State == domain.StateDying:
			actions = append(actions, Action{Kind: ActionRemoveUnit, Unit: dyingUnit(ku, obs, ou.parts)})

		// runner 컨테이너가 아예 없는 부품: 모르는 unit 이면 머신 내부 고아라 항상 rm,
		// 아는 unit 이면 남은 부품까지 unit 정리로 처리한다. [§8.3]
		case ou.runner == nil:
			if isKnown {
				actions = append(actions, Action{Kind: ActionRemoveUnit, Unit: dyingUnit(ku, obs, ou.parts)})
				continue
			}
			actions = append(actions, orphanActions(id, ou)...)

		// runner 가 살아 있음: 모르는 unit 이면 입양한다. 부품이 일부 없어도 kill 하지 않는다. [§8.3]
		case isAlive(ou.runner.State):
			if !isKnown {
				actions = append(actions, Action{Kind: ActionAdopt, Unit: adopt(id, ou, obs, cfg)})
			}

		// runner 가 exited/created 로 남음 → unit 정리. [§8.3]
		default:
			u := adopt(id, ou, obs, cfg)
			if isKnown {
				u = ku
			}
			actions = append(actions, Action{Kind: ActionRemoveUnit, Unit: dyingUnit(u, obs, ou.parts)})
		}
	}
	return actions
}

// collect 는 스냅샷을 unit id 별 부품 집합으로 묶는다.
// gh-ars 이름 규칙에 맞지 않는 항목은 우리 것이 아니므로 무시한다. [§4.2, §4.3]
func collect(obs Observed) map[domain.UnitID]*observedUnit {
	seen := make(map[domain.UnitID]*observedUnit)
	at := func(id domain.UnitID) *observedUnit {
		ou, ok := seen[id]
		if !ok {
			ou = &observedUnit{}
			seen[id] = ou
		}
		return ou
	}

	for i, c := range obs.Containers {
		id, role, ok := domain.ParseContainerName(c.Name)
		if !ok {
			continue
		}
		ou := at(id)
		switch role {
		case domain.RoleRunner:
			ou.runner = &obs.Containers[i]
			ou.parts.Runner = true
		case domain.RoleSidecar:
			ou.sidecar = &obs.Containers[i]
			ou.parts.Sidecar = true
		}
	}
	for _, name := range obs.Volumes {
		id, kind, ok := domain.ParseVolumeName(name)
		if !ok {
			continue
		}
		ou := at(id)
		for i, k := range domain.VolumeKinds {
			if k == kind {
				ou.parts.Volumes[i] = true
			}
		}
	}
	for _, name := range obs.Slices {
		id, ok := domain.ParseSliceName(name)
		if !ok {
			continue
		}
		at(id).parts.Slice = true
	}
	return seen
}

// orphanActions 는 부품별 rm Action 을 만든다. 순서는 컨테이너 → 볼륨 → slice 로 고정. [§8.3]
func orphanActions(id domain.UnitID, ou *observedUnit) []Action {
	var actions []Action
	if ou.sidecar != nil {
		actions = append(actions, Action{Kind: ActionRemoveOrphan, Orphan: Orphan{Kind: OrphanContainer, Name: domain.ContainerName(id, domain.RoleSidecar)}})
	}
	for i, k := range domain.VolumeKinds {
		if ou.parts.Volumes[i] {
			actions = append(actions, Action{Kind: ActionRemoveOrphan, Orphan: Orphan{Kind: OrphanVolume, Name: domain.VolumeName(id, k)}})
		}
	}
	if ou.parts.Slice {
		actions = append(actions, Action{Kind: ActionRemoveOrphan, Orphan: Orphan{Kind: OrphanSlice, Name: domain.SliceName(id)}})
	}
	return actions
}

// adopt 는 라벨과 컨테이너 생성 시각만으로 unit 을 복원한다.
// 소속 머신은 도달한 현재 머신이고(라벨의 machine 과 달라도 됨), 등록 이름은
// 등록 당시 값이어야 대조가 되므로 라벨의 scale set/machine 으로 만든다. [§4.3, §8.3]
func adopt(id domain.UnitID, ou *observedUnit, obs Observed, cfg ReconcileConfig) domain.Unit {
	l := ou.runner.Labels
	scaleSet := l[domain.LabelScaleSet]
	mode, isKnownSet := cfg.KnownScaleSets[scaleSet]
	if m := domain.Mode(l[domain.LabelMode]); m == domain.ModeNone || m == domain.ModeSidecar {
		mode = m // 라벨이 우선이다. 예산·mode 가 바뀌었어도 부품은 만들 때 값대로 정리해야 한다
	}
	machineLabel := l[domain.LabelMachine]
	if machineLabel == "" {
		machineLabel = obs.Machine
	}
	return domain.Unit{
		ID:         id,
		ScaleSet:   scaleSet,
		Machine:    obs.Machine,
		Mode:       mode,
		State:      domain.StateStarting,
		CreatedAt:  ou.runner.Created,
		Adopted:    true,
		Foreign:    !isKnownSet,
		RunnerName: domain.RunnerName(scaleSet, machineLabel, id),
		Parts:      ou.parts,
	}
}

// dyingUnit 은 정리 대상 unit 이다. 소속은 도달한 머신, 부품은 관측된 것만. [§8.3]
func dyingUnit(u domain.Unit, obs Observed, parts domain.Parts) domain.Unit {
	u.Machine = obs.Machine
	u.State = domain.StateDying
	u.Parts = parts
	return u
}
