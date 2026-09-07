package plan

import (
	"testing"
	"time"

	"gh-ars/internal/domain"
)

var t0 = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

// [§8.2] 후보는 healthy 이고 여유 슬롯 > 0. 기준은 running/effectiveMax 사용률 최저.
func TestSpread_S8_2_LowestUtilization(t *testing.T) {
	machines := []domain.Machine{
		{Name: "small", Health: domain.Healthy, EffectiveMax: 4},
		{Name: "big", Health: domain.Healthy, EffectiveMax: 8},
	}
	// small 2/4 = 0.5, big 2/8 = 0.25 → big.
	got, ok := Spread(machines, map[string]int{"small": 2, "big": 2}, t0)
	if !ok || got != "big" {
		t.Fatalf("Spread = %q, %v, want \"big\", true", got, ok)
	}
	// 여유 슬롯 절대값이 큰 big 이 아니라 사용률이 낮은 small 을 골라야 한다.
	got, ok = Spread(machines, map[string]int{"small": 0, "big": 4}, t0)
	if !ok || got != "small" {
		t.Fatalf("Spread = %q, %v, want \"small\", true", got, ok)
	}
}

// [§8.2] tie-break ① 여유 슬롯 절대값 큰 쪽.
func TestSpread_S8_2_TieBreakFreeSlots(t *testing.T) {
	machines := []domain.Machine{
		{Name: "small", Health: domain.Healthy, EffectiveMax: 2},
		{Name: "big", Health: domain.Healthy, EffectiveMax: 8},
	}
	// 둘 다 사용률 0.5. 여유 슬롯 small 1, big 4 → big.
	got, ok := Spread(machines, map[string]int{"small": 1, "big": 4}, t0)
	if !ok || got != "big" {
		t.Fatalf("Spread = %q, %v, want \"big\", true", got, ok)
	}
}

// [§8.2] tie-break ② 마지막 배치 시각이 오래된 쪽(round-robin).
func TestSpread_S8_2_TieBreakLastPlacedAt(t *testing.T) {
	machines := []domain.Machine{
		{Name: "m1", Health: domain.Healthy, EffectiveMax: 4, LastPlacedAt: t0.Add(-1 * time.Minute)},
		{Name: "m2", Health: domain.Healthy, EffectiveMax: 4, LastPlacedAt: t0.Add(-10 * time.Minute)},
	}
	got, ok := Spread(machines, map[string]int{"m1": 1, "m2": 1}, t0)
	if !ok || got != "m2" {
		t.Fatalf("Spread = %q, %v, want \"m2\", true", got, ok)
	}
	// 한 번도 배치되지 않은 머신(zero time)이 가장 오래된 쪽이다.
	machines[1].LastPlacedAt = time.Time{}
	if got, ok := Spread(machines, map[string]int{"m1": 1, "m2": 1}, t0); !ok || got != "m2" {
		t.Fatalf("Spread = %q, %v, want \"m2\", true", got, ok)
	}
}

// [§8.2] tie-break ③ 전부 동률이면 YAML(슬라이스) 순서. 반복 호출에도 같은 답.
func TestSpread_S8_2_TieBreakYAMLOrder(t *testing.T) {
	machines := []domain.Machine{
		{Name: "a", Health: domain.Healthy, EffectiveMax: 4, LastPlacedAt: t0},
		{Name: "b", Health: domain.Healthy, EffectiveMax: 4, LastPlacedAt: t0},
		{Name: "c", Health: domain.Healthy, EffectiveMax: 4, LastPlacedAt: t0},
	}
	for i := 0; i < 5; i++ {
		if got, ok := Spread(machines, map[string]int{}, t0); !ok || got != "a" {
			t.Fatalf("Spread = %q, %v, want \"a\", true", got, ok)
		}
	}
}

// [§8.2, §7.1-3] healthy 아님·여유 슬롯 0·effectiveMax 0 은 후보가 아니다.
func TestSpread_S8_2_NoCandidate(t *testing.T) {
	machines := []domain.Machine{
		{Name: "unhealthy", Health: domain.Unhealthy, EffectiveMax: 8},
		{Name: "failed", Health: domain.Failed, EffectiveMax: 8},
		{Name: "full", Health: domain.Healthy, EffectiveMax: 2},
		{Name: "zero", Health: domain.Healthy, EffectiveMax: 0},
	}
	occupied := map[string]int{"full": 2}
	if got, ok := Spread(machines, occupied, t0); ok {
		t.Fatalf("Spread = %q, %v, want \"\", false", got, ok)
	}
	if got, ok := Spread(nil, nil, t0); ok {
		t.Fatalf("Spread(nil) = %q, %v, want \"\", false", got, ok)
	}
	// 초과 점유(정리 실패 등)도 여유 슬롯 없음으로 본다.
	if got, ok := Spread(machines, map[string]int{"full": 5}, t0); ok {
		t.Fatalf("Spread = %q, %v, want \"\", false", got, ok)
	}
}

// [§7.2-3] running = Creating + Starting + Running + Draining. Dying 은 제외.
func TestRunning_S7_2_3(t *testing.T) {
	units := []domain.Unit{
		{ID: "1", ScaleSet: "build", State: domain.StateCreating},
		{ID: "2", ScaleSet: "build", State: domain.StateStarting},
		{ID: "3", ScaleSet: "build", State: domain.StateRunning},
		{ID: "4", ScaleSet: "build", State: domain.StateDraining},
		{ID: "5", ScaleSet: "build", State: domain.StateDying, Parts: domain.Parts{Runner: true}},
		{ID: "6", ScaleSet: "other", State: domain.StateRunning},
	}
	if got := Running(units, "build"); got != 4 {
		t.Fatalf("Running = %d, want 4", got)
	}
	if got := Running(units, "other"); got != 1 {
		t.Fatalf("Running = %d, want 1", got)
	}
	if got := Running(units, "none"); got != 0 {
		t.Fatalf("Running = %d, want 0", got)
	}
}

// [§8.3] occupied = running + Dying(부품 잔존) + Foreign unit. 머신별 slot 수.
func TestOccupied_S8_3(t *testing.T) {
	units := []domain.Unit{
		{ID: "1", Machine: "m1", ScaleSet: "build", State: domain.StateRunning},
		{ID: "2", Machine: "m1", ScaleSet: "build", State: domain.StateCreating},
		// Dying 이지만 부품이 남아 있으면 slot 을 계속 점유한다.
		{ID: "3", Machine: "m1", State: domain.StateDying, Parts: domain.Parts{Volumes: [3]bool{true, false, false}}},
		// Dying 이고 부품이 모두 정리되었으면 점유하지 않는다.
		{ID: "4", Machine: "m1", State: domain.StateDying},
		// Foreign unit 은 예산을 몰라도 머신 slot 1개로 센다.
		{ID: "5", Machine: "m2", ScaleSet: "gone", State: domain.StateStarting, Foreign: true},
		{ID: "6", Machine: "m2", ScaleSet: "build", State: domain.StateDraining},
	}
	got := Occupied(units)
	want := map[string]int{"m1": 3, "m2": 2}
	if len(got) != len(want) {
		t.Fatalf("Occupied = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("Occupied = %v, want %v", got, want)
		}
	}
}
