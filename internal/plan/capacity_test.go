package plan

import (
	"testing"
	"time"

	"gh-ars/internal/domain"
	"gh-ars/internal/resource"
)

func gi(n int64) int64 { return n << 30 }

// [§8.1] physicalMax = floor(min(machine.cpu/unit.cpu, machine.mem/unit.mem)).
func TestPhysicalMax_S8_1(t *testing.T) {
	tests := []struct {
		name    string
		machine resource.Budget
		unit    resource.Budget
		want    int
	}{
		{"cpu 제약", resource.Budget{CPU: 8, MemoryBytes: gi(64)}, resource.Budget{CPU: 2, MemoryBytes: gi(4)}, 4},
		{"memory 제약", resource.Budget{CPU: 8, MemoryBytes: gi(6)}, resource.Budget{CPU: 1, MemoryBytes: gi(2)}, 3},
		{"소수 cpu", resource.Budget{CPU: 2, MemoryBytes: gi(8)}, resource.Budget{CPU: 0.5, MemoryBytes: gi(1)}, 4},
		{"나누어떨어지지 않음", resource.Budget{CPU: 5, MemoryBytes: gi(9)}, resource.Budget{CPU: 2, MemoryBytes: gi(2)}, 2},
		{"unit 이 머신보다 큼", resource.Budget{CPU: 1, MemoryBytes: gi(1)}, resource.Budget{CPU: 2, MemoryBytes: gi(1)}, 0},
		{"이진 오차 (0.3/0.1 = 2.9999999999999996)", resource.Budget{CPU: 0.3, MemoryBytes: gi(64)}, resource.Budget{CPU: 0.1, MemoryBytes: gi(1)}, 3},
		{"진짜 정수 미만은 내림", resource.Budget{CPU: 1.9999999995, MemoryBytes: gi(64)}, resource.Budget{CPU: 1, MemoryBytes: gi(1)}, 1},
		{"unit cpu 0 은 방어적으로 0", resource.Budget{CPU: 8, MemoryBytes: gi(8)}, resource.Budget{CPU: 0, MemoryBytes: gi(1)}, 0},
		{"unit memory 0 은 방어적으로 0", resource.Budget{CPU: 8, MemoryBytes: gi(8)}, resource.Budget{CPU: 1, MemoryBytes: 0}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PhysicalMax(tc.machine, tc.unit); got != tc.want {
				t.Fatalf("PhysicalMax(%+v, %+v) = %d, want %d", tc.machine, tc.unit, got, tc.want)
			}
		})
	}
}

// [R22, §8.1] machines[].maxRunners 는 하향(cap)만 가능하다. 생략(nil)이면 physicalMax 그대로.
func TestEffectiveMax_R22(t *testing.T) {
	n := func(v int) *int { return &v }
	tests := []struct {
		name       string
		physical   int
		maxRunners *int
		want       int
	}{
		{"미지정", 4, nil, 4},
		{"하향", 4, n(2), 2},
		{"상향 시도는 cap (경고 대상)", 4, n(8), 4},
		{"같음", 4, n(4), 4},
		{"0", 4, n(0), 0},
		{"음수는 0", 4, n(-1), 0},
		{"physicalMax 0", 0, n(3), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveMax(tc.physical, tc.maxRunners); got != tc.want {
				t.Fatalf("EffectiveMax(%d, %v) = %d, want %d", tc.physical, tc.maxRunners, got, tc.want)
			}
		})
	}
}

// [R23, §8.1] capacity = min(ss.maxRunners, Σ effectiveMax(healthy m)). Unhealthy/Failed 는 제외.
func TestCapacity_R23(t *testing.T) {
	ss := domain.ScaleSet{Name: "build", MaxRunners: 10}
	machines := []domain.Machine{
		{Name: "m1", ScaleSet: "build", Health: domain.Healthy, EffectiveMax: 3},
		{Name: "m2", ScaleSet: "build", Health: domain.Healthy, EffectiveMax: 2},
		{Name: "m3", ScaleSet: "build", Health: domain.Unhealthy, EffectiveMax: 5},
		{Name: "m4", ScaleSet: "build", Health: domain.Failed, EffectiveMax: 5},
		{Name: "m5", ScaleSet: "other", Health: domain.Healthy, EffectiveMax: 7},
	}
	if got := Capacity(ss, machines); got != 5 {
		t.Fatalf("Capacity = %d, want 5 (healthy 합산)", got)
	}

	// ss.maxRunners 가 합보다 작으면 cap (R23 경고 후 cap).
	capped := domain.ScaleSet{Name: "build", MaxRunners: 4}
	if got := Capacity(capped, machines); got != 4 {
		t.Fatalf("Capacity = %d, want 4 (maxRunners cap)", got)
	}

	// healthy 머신이 없으면 0.
	none := []domain.Machine{{Name: "m3", ScaleSet: "build", Health: domain.Unhealthy, EffectiveMax: 5}}
	if got := Capacity(ss, none); got != 0 {
		t.Fatalf("Capacity = %d, want 0", got)
	}
}

// [§7.2-3] desired = min(capacity, max(minRunners, TotalAssignedJobs)).
func TestDesired_S7_2_3(t *testing.T) {
	tests := []struct {
		name                           string
		capacity, minRunners, assigned int
		want                           int
	}{
		{"job 없음 → warm runner", 10, 2, 0, 2},
		{"job 이 min 초과", 10, 2, 5, 5},
		{"capacity 제한", 3, 2, 5, 3},
		{"minRunners 가 capacity 초과 (R24 경고 상황)", 2, 5, 0, 2},
		{"전부 0", 0, 0, 0, 0},
		{"capacity 0 (전 머신 unhealthy)", 0, 2, 4, 0},
		{"음수 방어", -1, -1, -1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Desired(tc.capacity, tc.minRunners, tc.assigned); got != tc.want {
				t.Fatalf("Desired(%d, %d, %d) = %d, want %d", tc.capacity, tc.minRunners, tc.assigned, got, tc.want)
			}
		})
	}
}

// [§7.2-3] 축소 후보: Running 이고 busy 표시 없는 unit 을 오래된 순으로 remove 개.
// Creating/Starting/Draining/Dying 은 건드리지 않는다.
func TestScaleDown_S7_2_3(t *testing.T) {
	base := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	units := []domain.Unit{
		{ID: "u-running-new", State: domain.StateRunning, CreatedAt: at(30)},
		{ID: "u-busy", State: domain.StateRunning, CreatedAt: at(0), Busy: true},
		{ID: "u-running-old", State: domain.StateRunning, CreatedAt: at(10)},
		{ID: "u-starting", State: domain.StateStarting, CreatedAt: at(1)},
		{ID: "u-creating", State: domain.StateCreating, CreatedAt: at(2)},
		{ID: "u-draining", State: domain.StateDraining, CreatedAt: at(3)},
		{ID: "u-dying", State: domain.StateDying, CreatedAt: at(4)},
		{ID: "u-running-mid", State: domain.StateRunning, CreatedAt: at(20)},
	}

	got := ScaleDown(units, 2)
	want := []domain.UnitID{"u-running-old", "u-running-mid"}
	if len(got) != len(want) {
		t.Fatalf("ScaleDown(_, 2) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ScaleDown(_, 2) = %v, want %v", got, want)
		}
	}

	// 후보보다 많이 요청해도 후보 수를 넘지 않는다.
	if got := ScaleDown(units, 10); len(got) != 3 {
		t.Fatalf("ScaleDown(_, 10) = %v, want 후보 3개", got)
	}
	if got := ScaleDown(units, 0); len(got) != 0 {
		t.Fatalf("ScaleDown(_, 0) = %v, want 없음", got)
	}
	if got := ScaleDown(units, -1); len(got) != 0 {
		t.Fatalf("ScaleDown(_, -1) = %v, want 없음", got)
	}
}

// [§7.2-3] CreatedAt 이 같으면 unit id 순으로 결정적이어야 한다.
func TestScaleDown_S7_2_3_DeterministicTie(t *testing.T) {
	same := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	units := []domain.Unit{
		{ID: "01B", State: domain.StateRunning, CreatedAt: same},
		{ID: "01A", State: domain.StateRunning, CreatedAt: same},
		{ID: "01C", State: domain.StateRunning, CreatedAt: same},
	}
	for i := 0; i < 5; i++ {
		got := ScaleDown(units, 2)
		if len(got) != 2 || got[0] != "01A" || got[1] != "01B" {
			t.Fatalf("ScaleDown = %v, want [01A 01B]", got)
		}
	}
}
