package plan

import (
	"sort"
	"time"

	"gh-ars/internal/domain"
)

// runningStates 는 job 을 받을 수 있거나 곧 받을 상태다. desired 계산의 running. [§7.2-3]
func isRunningState(s domain.UnitState) bool {
	switch s {
	case domain.StateCreating, domain.StateStarting, domain.StateRunning, domain.StateDraining:
		return true
	}
	return false
}

// hasParts 는 머신에 아직 부품이 남아 있는지다. [§8.3]
func hasParts(p domain.Parts) bool {
	return p.Runner || p.Sidecar || p.Slice || p.Volumes[0] || p.Volumes[1] || p.Volumes[2]
}

// Running 은 scale set 의 running 집계다: Creating + Starting + Running + Draining.
// Dying 은 포함하지 않는다. [§7.2-3, DESIGN §3.2]
func Running(units []domain.Unit, scaleSet string) int {
	n := 0
	for _, u := range units {
		if u.ScaleSet == scaleSet && isRunningState(u.State) {
			n++
		}
	}
	return n
}

// Occupied 는 머신별로 점유된 slot 수다: running + Dying(부품 잔존) + Foreign unit.
// Foreign unit 은 예산을 알 수 없으므로 머신 slot 1개로 센다. [§8.3, DESIGN §3.2]
func Occupied(units []domain.Unit) map[string]int {
	occ := make(map[string]int)
	for _, u := range units {
		if isRunningState(u.State) || (u.State == domain.StateDying && hasParts(u.Parts)) {
			occ[u.Machine]++
		}
	}
	return occ
}

// Spread 는 다음 unit 을 올릴 머신을 고른다. 후보가 없으면 ok=false. [§8.2]
//
// 후보: healthy 이고 여유 슬롯 > 0. 기준: occupied/effectiveMax 사용률 최저.
// 여유 슬롯과 사용률 모두 occupied(= running + Dying 부품 잔존 + Foreign)를 쓴다.
// tie-break: ① 여유 슬롯 절대값 큰 쪽 → ② LastPlacedAt 오래된 쪽 → ③ 슬라이스(YAML) 순서.
//
// now 는 판정 기준 시각이다. 현재 규칙은 LastPlacedAt 의 상대 순서만 쓰므로 읽지 않는다.
func Spread(machines []domain.Machine, occupied map[string]int, now time.Time) (string, bool) {
	best := -1
	for i, m := range machines {
		if m.Health != domain.Healthy || m.EffectiveMax <= 0 {
			continue
		}
		if freeSlots(m, occupied) <= 0 {
			continue
		}
		if best < 0 || betterCandidate(m, machines[best], occupied) {
			best = i
		}
	}
	if best < 0 {
		return "", false
	}
	return machines[best].Name, true
}

func freeSlots(m domain.Machine, occupied map[string]int) int {
	return m.EffectiveMax - occupied[m.Name]
}

// betterCandidate 는 a 가 b 보다 앞선 후보인지다. 동률이면 false 를 돌려
// 먼저 나온 쪽(= YAML 순서)이 이기게 한다. [§8.2]
func betterCandidate(a, b domain.Machine, occupied map[string]int) bool {
	// 사용률 비교: occupied/effectiveMax. 부동소수를 쓰지 않으려고 교차 곱한다.
	au := int64(occupied[a.Name]) * int64(b.EffectiveMax)
	bu := int64(occupied[b.Name]) * int64(a.EffectiveMax)
	if au != bu {
		return au < bu
	}
	if fa, fb := freeSlots(a, occupied), freeSlots(b, occupied); fa != fb {
		return fa > fb
	}
	return a.LastPlacedAt.Before(b.LastPlacedAt)
}

// ScaleDown 은 축소 후보를 remove 개까지 고른다.
// 후보는 Running 이고 busy 표시가 없는 unit, 오래된 순.
// Creating/Starting/Draining/Dying 은 건드리지 않는다. [§7.2-3]
func ScaleDown(units []domain.Unit, remove int) []domain.UnitID {
	if remove <= 0 {
		return nil
	}
	cand := make([]domain.Unit, 0, len(units))
	for _, u := range units {
		if u.State == domain.StateRunning && !u.Busy {
			cand = append(cand, u)
		}
	}
	// CreatedAt 동률은 unit id 로 갈라 결정적으로 만든다.
	sort.SliceStable(cand, func(i, j int) bool {
		if !cand[i].CreatedAt.Equal(cand[j].CreatedAt) {
			return cand[i].CreatedAt.Before(cand[j].CreatedAt)
		}
		return cand[i].ID < cand[j].ID
	})
	if remove > len(cand) {
		remove = len(cand)
	}
	ids := make([]domain.UnitID, 0, remove)
	for _, u := range cand[:remove] {
		ids = append(ids, u.ID)
	}
	return ids
}
