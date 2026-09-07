// Package plan 은 용량·배치·reconcile 판정을 담는 순수 함수 모음이다. [§8]
// I/O 를 하지 않는다. `runtime.Container` 는 타입으로만 참조한다 (DESIGN §2).
package plan

import (
	"math"

	"gh-ars/internal/domain"
	"gh-ars/internal/resource"
)

// PhysicalMax 는 머신 예산으로 몇 개의 unit 을 올릴 수 있는지다. [§8.1]
//
//	physicalMax(m) = floor(min(m.cpu / unit.cpu, m.mem / unit.mem))
func PhysicalMax(machine, unit resource.Budget) int {
	if unit.CPU <= 0 || unit.MemoryBytes <= 0 || machine.CPU <= 0 || machine.MemoryBytes <= 0 {
		return 0
	}
	byCPU := floorDiv(machine.CPU, unit.CPU)
	byMem := int(machine.MemoryBytes / unit.MemoryBytes)
	return min(byCPU, byMem)
}

// float64Eps 는 float64 의 machine epsilon 이다. 몫이 정수에서 몇 ulp 떨어졌는지 재는 데 쓴다.
const float64Eps = 2.220446049250313e-16

// floorDiv 는 floor(a/b) 다. cpu 는 float64 라 0.3/0.1 이 2.9999999999999996 으로
// 계산되므로, 나눗셈 자체의 표현 오차(몇 ulp) 안에서만 위 정수로 올린다.
// 진짜로 정수 미만인 몫(1.9999999995/1)은 그대로 내림한다. [§8.1, R25]
func floorDiv(a, b float64) int {
	q := a / b
	f := math.Floor(q)
	if next := f + 1; next-q <= next*4*float64Eps {
		f = next
	}
	if f > math.MaxInt32 { // 비현실적인 예산 조합에서 int 오버플로 방지
		return math.MaxInt32
	}
	return int(f)
}

// EffectiveMax 는 machines[].maxRunners 를 반영한 머신 상한이다.
// 정수는 하향만 가능하다(상향 시도는 호출 측이 경고 후 cap). [§8.1, R22]
func EffectiveMax(physical int, maxRunners *int) int {
	e := physical
	if maxRunners != nil && *maxRunners < e {
		e = *maxRunners
	}
	return max(e, 0)
}

// Capacity 는 scale set 이 동시에 굴릴 수 있는 runner 수다.
// healthy 머신만 합산하고 ss.MaxRunners 로 cap 한다. [§8.1, R23]
func Capacity(ss domain.ScaleSet, machines []domain.Machine) int {
	sum := 0
	for _, m := range machines {
		if m.ScaleSet != ss.Name || m.Health != domain.Healthy {
			continue
		}
		sum += max(m.EffectiveMax, 0)
	}
	if ss.MaxRunners < sum {
		return max(ss.MaxRunners, 0)
	}
	return sum
}

// Desired 는 지금 있어야 할 runner 수다. [§7.2-3]
//
//	desired = min(capacity, max(minRunners, TotalAssignedJobs))
func Desired(capacity, minRunners, assigned int) int {
	want := max(minRunners, assigned)
	return max(min(capacity, want), 0)
}
