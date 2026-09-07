package plan

import (
	"strings"
	"testing"
	"time"

	"gh-ars/internal/domain"
	"gh-ars/internal/runtime"
)

// uid 는 tag 로 끝나는 유효한 ULID(26자)를 만든다. [§4.1]
func uid(tag string) domain.UnitID {
	return domain.UnitID("01" + strings.Repeat("A", domain.UnitIDLen-2-len(tag)) + tag)
}

func labels(id domain.UnitID, role domain.Role, scaleSet, mode, machine string) map[string]string {
	l := map[string]string{
		domain.LabelUnit:     string(id),
		domain.LabelScaleSet: scaleSet,
		domain.LabelMode:     mode,
		domain.LabelMachine:  machine,
	}
	if role != "" {
		l[domain.LabelRole] = string(role)
	}
	return l
}

func ctr(id domain.UnitID, role domain.Role, state string, created time.Time, scaleSet, mode, machine string) runtime.Container {
	return runtime.Container{
		Name:    domain.ContainerName(id, role),
		Labels:  labels(id, role, scaleSet, mode, machine),
		State:   state,
		Created: created,
	}
}

func known() map[string]domain.Mode {
	return map[string]domain.Mode{"build": domain.ModeSidecar, "test": domain.ModeNone}
}

func only(t *testing.T, actions []Action, kind ActionKind) Action {
	t.Helper()
	var found []Action
	for _, a := range actions {
		if a.Kind == kind {
			found = append(found, a)
		}
	}
	if len(found) != 1 {
		t.Fatalf("actions = %+v, want %s 1개", actions, kind)
	}
	return found[0]
}

// [§8.3] runner 컨테이너가 살아 있고 라벨의 scale set 이 YAML 에 없음 → 입양(Foreign).
func TestReconcile_S8_3_AdoptForeignScaleSet(t *testing.T) {
	id := uid("F1")
	created := t0.Add(-3 * time.Minute)
	obs := Observed{
		Machine:    "m1",
		Containers: []runtime.Container{ctr(id, domain.RoleRunner, "running", created, "gone", "none", "m1")},
	}
	actions := Reconcile(obs, nil, t0, ReconcileConfig{KnownScaleSets: known()})
	a := only(t, actions, ActionAdopt)
	if !a.Unit.Foreign {
		t.Fatalf("Foreign = false, want true")
	}
	if a.Unit.State != domain.StateStarting {
		t.Fatalf("State = %s, want Starting", a.Unit.State)
	}
	if !a.Unit.Adopted {
		t.Fatalf("Adopted = false, want true")
	}
	if a.Unit.Machine != "m1" {
		t.Fatalf("Machine = %q, want m1", a.Unit.Machine)
	}
	if !a.Unit.CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt = %v, want %v (컨테이너 생성 시각)", a.Unit.CreatedAt, created)
	}
	if a.Unit.ScaleSet != "gone" || a.Unit.Mode != domain.ModeNone {
		t.Fatalf("Unit = %+v, want scaleSet/mode 를 라벨에서 복원", a.Unit)
	}
	if !a.Unit.Parts.Runner || a.Unit.Parts.Sidecar {
		t.Fatalf("Parts = %+v, want runner 만", a.Unit.Parts)
	}
}

// [§8.3] runner 컨테이너가 살아 있고 라벨의 machine 이름이 YAML 과 다름
// → 입양. 소속은 도달한 현재 머신. GitHub 등록 이름은 등록 당시 값(라벨)을 쓴다. [§4.3]
func TestReconcile_S8_3_AdoptMachineMismatch(t *testing.T) {
	id := uid("M1")
	obs := Observed{
		Machine:    "m1",
		Containers: []runtime.Container{ctr(id, domain.RoleRunner, "running", t0, "build", "sidecar", "old-name")},
	}
	a := only(t, Reconcile(obs, nil, t0, ReconcileConfig{KnownScaleSets: known()}), ActionAdopt)
	if a.Unit.Machine != "m1" {
		t.Fatalf("Machine = %q, want m1 (도달한 머신)", a.Unit.Machine)
	}
	if a.Unit.Foreign {
		t.Fatalf("Foreign = true, want false (scale set 은 YAML 에 있음)")
	}
	if want := domain.RunnerName("build", "old-name", id); a.Unit.RunnerName != want {
		t.Fatalf("RunnerName = %q, want %q", a.Unit.RunnerName, want)
	}
}

// [§8.3] runner 살아 있고 sidecar/볼륨/slice 일부 없음 → kill 하지 않는다. 고아 정리도 하지 않는다.
func TestReconcile_S8_3_AlivePartialParts(t *testing.T) {
	id := uid("P1")
	obs := Observed{
		Machine:    "m1",
		Containers: []runtime.Container{ctr(id, domain.RoleRunner, "running", t0, "build", "sidecar", "m1")},
		Volumes:    []string{domain.VolumeName(id, domain.VolumeWork)},
		Slices:     []string{domain.SliceName(id)},
	}
	actions := Reconcile(obs, nil, t0, ReconcileConfig{KnownScaleSets: known()})
	if len(actions) != 1 {
		t.Fatalf("actions = %+v, want Adopt 1개만", actions)
	}
	a := only(t, actions, ActionAdopt)
	want := domain.Parts{Runner: true, Sidecar: false, Volumes: [3]bool{true, false, false}, Slice: true}
	if a.Unit.Parts != want {
		t.Fatalf("Parts = %+v, want %+v", a.Unit.Parts, want)
	}
}

// [§8.3] runner 컨테이너가 exited 로 남아 있음 → unit 정리. 남은 부품은 Parts 로 넘긴다.
func TestReconcile_S8_3_ExitedRunner(t *testing.T) {
	id := uid("E1")
	obs := Observed{
		Machine: "m1",
		Containers: []runtime.Container{
			ctr(id, domain.RoleRunner, "exited", t0.Add(-time.Hour), "build", "sidecar", "m1"),
			ctr(id, domain.RoleSidecar, "running", t0.Add(-time.Hour), "build", "sidecar", "m1"),
		},
		Volumes: domain.VolumeNames(id),
		Slices:  []string{domain.SliceName(id)},
	}
	actions := Reconcile(obs, nil, t0, ReconcileConfig{KnownScaleSets: known()})
	if len(actions) != 1 {
		t.Fatalf("actions = %+v, want RemoveUnit 1개만 (부품은 unit 정리로 처리)", actions)
	}
	a := only(t, actions, ActionRemoveUnit)
	if a.Unit.State != domain.StateDying {
		t.Fatalf("State = %s, want Dying", a.Unit.State)
	}
	want := domain.Parts{Runner: true, Sidecar: true, Volumes: [3]bool{true, true, true}, Slice: true}
	if a.Unit.Parts != want {
		t.Fatalf("Parts = %+v, want %+v", a.Unit.Parts, want)
	}
	if a.Unit.ID != id || a.Unit.RunnerName != domain.RunnerName("build", "m1", id) {
		t.Fatalf("Unit = %+v, want id/RunnerName 복원", a.Unit)
	}
}

// [§8.3] paused runner 는 프로세스가 남아 있으므로 살아 있는 것으로 보고 입양한다.
func TestReconcile_S8_3_PausedRunnerAlive(t *testing.T) {
	id := uid("PA")
	obs := Observed{
		Machine:    "m1",
		Containers: []runtime.Container{ctr(id, domain.RoleRunner, "paused", t0, "build", "sidecar", "m1")},
	}
	a := only(t, Reconcile(obs, nil, t0, ReconcileConfig{KnownScaleSets: known()}), ActionAdopt)
	if a.Unit.State != domain.StateStarting {
		t.Fatalf("State = %s, want Starting", a.Unit.State)
	}
}

// [§8.3] start 전에 죽어 created 로 남은 runner 도 살아 있지 않으므로 정리 대상이다.
func TestReconcile_S8_3_CreatedRunner(t *testing.T) {
	id := uid("C1")
	obs := Observed{
		Machine:    "m1",
		Containers: []runtime.Container{ctr(id, domain.RoleRunner, "created", t0, "build", "none", "m1")},
	}
	a := only(t, Reconcile(obs, nil, t0, ReconcileConfig{KnownScaleSets: known()}), ActionRemoveUnit)
	if a.Unit.ID != id {
		t.Fatalf("Unit = %+v, want %s 정리", a.Unit, id)
	}
}

// [§8.3] runner 컨테이너 없는 sidecar/볼륨/slice (머신 내부 고아) → 항상 rm.
func TestReconcile_S8_3_MachineOrphans(t *testing.T) {
	id := uid("V1")
	obs := Observed{
		Machine:    "m1",
		Containers: []runtime.Container{ctr(id, domain.RoleSidecar, "running", t0, "build", "sidecar", "m1")},
		Volumes:    []string{domain.VolumeName(id, domain.VolumeSock), domain.VolumeName(id, domain.VolumeWork)},
		Slices:     []string{domain.SliceName(id)},
	}
	actions := Reconcile(obs, nil, t0, ReconcileConfig{KnownScaleSets: known()})
	if len(actions) != 4 {
		t.Fatalf("actions = %+v, want 고아 4개", actions)
	}
	wantKinds := map[OrphanKind][]string{
		OrphanContainer: {domain.ContainerName(id, domain.RoleSidecar)},
		OrphanVolume:    {domain.VolumeName(id, domain.VolumeWork), domain.VolumeName(id, domain.VolumeSock)},
		OrphanSlice:     {domain.SliceName(id)},
	}
	got := map[OrphanKind][]string{}
	for _, a := range actions {
		if a.Kind != ActionRemoveOrphan {
			t.Fatalf("actions = %+v, want RemoveOrphan 만", actions)
		}
		got[a.Orphan.Kind] = append(got[a.Orphan.Kind], a.Orphan.Name)
	}
	for kind, want := range wantKinds {
		if len(got[kind]) != len(want) {
			t.Fatalf("%s 고아 = %v, want %v", kind, got[kind], want)
		}
		for _, name := range want {
			found := false
			for _, g := range got[kind] {
				if g == name {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s 고아 = %v, want %q 포함", kind, got[kind], name)
			}
		}
	}
}

// [§8.3] 이미 아는 unit 은 다시 입양하지 않는다.
func TestReconcile_S8_3_KnownAliveNoAction(t *testing.T) {
	id := uid("K1")
	obs := Observed{
		Machine:    "m1",
		Containers: []runtime.Container{ctr(id, domain.RoleRunner, "running", t0, "build", "sidecar", "m1")},
		Volumes:    domain.VolumeNames(id),
		Slices:     []string{domain.SliceName(id)},
	}
	knownUnits := map[domain.UnitID]domain.Unit{
		id: {ID: id, ScaleSet: "build", Machine: "m1", State: domain.StateRunning, Busy: true},
	}
	if actions := Reconcile(obs, knownUnits, t0, ReconcileConfig{KnownScaleSets: known()}); len(actions) != 0 {
		t.Fatalf("actions = %+v, want 없음", actions)
	}
}

// [§8.3] Dying unit 은 부품이 남아 있는 한 전체 동기화에서 정리를 재시도한다.
func TestReconcile_S8_3_KnownDyingRetry(t *testing.T) {
	id := uid("D1")
	obs := Observed{
		Machine:    "m1",
		Containers: []runtime.Container{ctr(id, domain.RoleRunner, "running", t0, "build", "sidecar", "m1")},
		Volumes:    []string{domain.VolumeName(id, domain.VolumeWork)},
	}
	knownUnits := map[domain.UnitID]domain.Unit{
		id: {ID: id, ScaleSet: "build", Machine: "m1", State: domain.StateDying, RunnerName: "build-m1-x"},
	}
	a := only(t, Reconcile(obs, knownUnits, t0, ReconcileConfig{KnownScaleSets: known()}), ActionRemoveUnit)
	if a.Unit.RunnerName != "build-m1-x" {
		t.Fatalf("RunnerName = %q, want 알고 있던 값 유지", a.Unit.RunnerName)
	}
	want := domain.Parts{Runner: true, Volumes: [3]bool{true, false, false}}
	if a.Unit.Parts != want {
		t.Fatalf("Parts = %+v, want %+v", a.Unit.Parts, want)
	}
}

// [§8.3] 이 머신 소속으로 알고 있는데 부품이 하나도 없는 unit 은 정리 대상이다.
// 생성 중(Creating)은 명령이 진행 중일 수 있으므로 건드리지 않는다. [DESIGN §3.2]
func TestReconcile_S8_3_KnownWithoutParts(t *testing.T) {
	gone, creating, elsewhere := uid("G1"), uid("N1"), uid("X1")
	knownUnits := map[domain.UnitID]domain.Unit{
		gone:      {ID: gone, ScaleSet: "build", Machine: "m1", State: domain.StateRunning},
		creating:  {ID: creating, ScaleSet: "build", Machine: "m1", State: domain.StateCreating},
		elsewhere: {ID: elsewhere, ScaleSet: "build", Machine: "m2", State: domain.StateRunning},
	}
	actions := Reconcile(Observed{Machine: "m1"}, knownUnits, t0, ReconcileConfig{KnownScaleSets: known()})
	a := only(t, actions, ActionRemoveUnit)
	if a.Unit.ID != gone {
		t.Fatalf("actions = %+v, want %s 만 정리", actions, gone)
	}
	if a.Unit.Parts != (domain.Parts{}) {
		t.Fatalf("Parts = %+v, want 빈 값", a.Unit.Parts)
	}
	if len(actions) != 1 {
		t.Fatalf("actions = %+v, want 1개", actions)
	}
}

// [DESIGN §3.2, §7.2-4] 아는 unit 이 Creating 이면 create→cp→start 진행 중이라
// 부품 집합이 불완전한 게 정상이다. 판정하지 않는다(기동 타임아웃은 Controller 담당).
func TestReconcile_S8_3_KnownCreatingUntouched(t *testing.T) {
	id := uid("CR")
	knownUnits := map[domain.UnitID]domain.Unit{
		id: {ID: id, ScaleSet: "build", Machine: "m1", State: domain.StateCreating},
	}
	cfg := ReconcileConfig{KnownScaleSets: known()}

	// runner 생성 전: slice·볼륨·sidecar 만 있다.
	obs := Observed{
		Machine:    "m1",
		Containers: []runtime.Container{ctr(id, domain.RoleSidecar, "running", t0, "build", "sidecar", "m1")},
		Volumes:    domain.VolumeNames(id),
		Slices:     []string{domain.SliceName(id)},
	}
	if actions := Reconcile(obs, knownUnits, t0, cfg); len(actions) != 0 {
		t.Fatalf("actions = %+v, want 없음 (sidecar 만 있는 기동 중간)", actions)
	}

	// create 는 끝났고 start 전: runner 가 created 상태다.
	obs.Containers = append(obs.Containers, ctr(id, domain.RoleRunner, "created", t0, "build", "sidecar", "m1"))
	if actions := Reconcile(obs, knownUnits, t0, cfg); len(actions) != 0 {
		t.Fatalf("actions = %+v, want 없음 (created runner)", actions)
	}
}

// [§4.1, §8.3] gh-ars 이름 규칙에 맞지 않는 부품은 건드리지 않는다.
// systemd 가 만드는 상위 slice(gh-ars.slice)도 여기에 포함된다.
func TestReconcile_S8_3_IgnoresUnrelated(t *testing.T) {
	obs := Observed{
		Machine: "m1",
		Containers: []runtime.Container{
			{Name: "my-app", State: "running"},
			{Name: "gh-ars-notaulid-runner", State: "exited"},
		},
		Volumes: []string{"cache", "gh-ars-notaulid-work"},
		Slices:  []string{"gh-ars.slice", "user.slice"},
	}
	if actions := Reconcile(obs, nil, t0, ReconcileConfig{KnownScaleSets: known()}); len(actions) != 0 {
		t.Fatalf("actions = %+v, want 없음", actions)
	}
}

// 같은 입력이면 같은 순서로 나와야 한다(로그·테스트 결정성).
func TestReconcile_DeterministicOrder(t *testing.T) {
	a1, a2 := uid("A1"), uid("A2")
	orphan := uid("A3")
	obs := Observed{
		Machine: "m1",
		Containers: []runtime.Container{
			ctr(a2, domain.RoleRunner, "exited", t0, "build", "sidecar", "m1"),
			ctr(a1, domain.RoleRunner, "running", t0, "build", "sidecar", "m1"),
		},
		Volumes: []string{domain.VolumeName(orphan, domain.VolumeExternals), domain.VolumeName(orphan, domain.VolumeWork)},
	}
	var first []Action
	for i := 0; i < 5; i++ {
		actions := Reconcile(obs, nil, t0, ReconcileConfig{KnownScaleSets: known()})
		if i == 0 {
			first = actions
			continue
		}
		if len(actions) != len(first) {
			t.Fatalf("actions = %+v, want %+v", actions, first)
		}
		for j := range actions {
			if actions[j].Kind != first[j].Kind || actions[j].Unit.ID != first[j].Unit.ID || actions[j].Orphan != first[j].Orphan {
				t.Fatalf("actions[%d] = %+v, want %+v", j, actions[j], first[j])
			}
		}
	}
	if len(first) != 4 {
		t.Fatalf("actions = %+v, want 4개 (Adopt, RemoveUnit, 고아 2)", first)
	}
	if first[0].Kind != ActionAdopt || first[0].Unit.ID != a1 {
		t.Fatalf("first[0] = %+v, want Adopt %s", first[0], a1)
	}
	if first[1].Kind != ActionRemoveUnit || first[1].Unit.ID != a2 {
		t.Fatalf("first[1] = %+v, want RemoveUnit %s", first[1], a2)
	}
}
