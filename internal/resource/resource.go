// Package resource 는 cpu·memory 예산의 파싱과 실행기 값 변환을 담당한다. [§6.0, §8.1, §9.3]
// I/O 를 하지 않는 순수 패키지다 (DESIGN §2).
package resource

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Budget 은 unit 하나 또는 머신 하나의 cpu·memory 예산이다. [§8.1]
// sidecar 모드에서는 runner+sidecar 두 컨테이너가 이 예산 하나를 공유한다. [§9.3]
type Budget struct {
	CPU         float64 // 코어 수. 0.5 허용. > 0
	MemoryBytes int64   // 바이트. > 0
}

// MinCPU 는 cpu 의 하한이다(= CPUQuota 1%). 그 아래는 systemd 의 CPUQuotaPerSecUSec
// 반올림에 묻혀 slice 에 적용되지 않고, §8.1 의 floor(machine.cpu/unit.cpu) 가
// 비현실적인 capacity 를 낸다. 하한 위에서는 값을 클램프하지 않는다. [R25]
const MinCPU = 0.01

// memory 접미사. Ki/Mi/Gi 만 허용한다 (1024 진법). [R25]
var memSuffixes = []struct {
	suffix string
	mult   int64
}{
	{"Ki", 1 << 10},
	{"Mi", 1 << 20},
	{"Gi", 1 << 30},
}

// ParseCPU 는 YAML 이 준 숫자(int/float) 또는 문자열("2", "0.5")을 코어 수로 정규화한다.
// 음수·0·숫자 아님은 오류다. [R25]
func ParseCPU(v any) (float64, error) {
	var f float64
	switch t := v.(type) {
	case int:
		f = float64(t)
	case int32:
		f = float64(t)
	case int64:
		f = float64(t)
	case uint64:
		f = float64(t)
	case float32:
		f = float64(t)
	case float64:
		f = t
	case string:
		s := strings.TrimSpace(t)
		parsed, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, fmt.Errorf("cpu %q: 숫자가 아님", t)
		}
		f = parsed
	default:
		return 0, fmt.Errorf("cpu: 숫자 또는 문자열이어야 함 (%T)", v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("cpu: 유한한 숫자여야 함")
	}
	if f <= 0 {
		return 0, fmt.Errorf("cpu: 0보다 커야 함 (%v)", f)
	}
	if f < MinCPU {
		return 0, fmt.Errorf("cpu: %v 이상이어야 함 (%v). 그 아래는 CPUQuota 1%% 미만이라 slice 에 적용되지 않는다", MinCPU, f)
	}
	return f, nil
}

// ParseMemory 는 "512Mi", "4Gi", "1024Ki" 를 바이트로 바꾼다.
// 접미사가 없거나 Ki/Mi/Gi 가 아니면 오류다. [R25]
func ParseMemory(s string) (int64, error) {
	t := strings.TrimSpace(s)
	for _, su := range memSuffixes {
		num, ok := strings.CutSuffix(t, su.suffix)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("memory %q: %s 앞은 정수여야 함", s, su.suffix)
		}
		if n <= 0 {
			return 0, fmt.Errorf("memory %q: 0보다 커야 함", s)
		}
		if n > math.MaxInt64/su.mult {
			return 0, fmt.Errorf("memory %q: 너무 큼", s)
		}
		return n * su.mult, nil
	}
	return 0, fmt.Errorf("memory %q: Ki/Mi/Gi 단위여야 함", s)
}

// ParseBudget 은 설정의 resources 블록 한 쌍을 Budget 으로 만든다. [R25]
func ParseBudget(cpu any, memory string) (Budget, error) {
	c, err := ParseCPU(cpu)
	if err != nil {
		return Budget{}, err
	}
	m, err := ParseMemory(memory)
	if err != nil {
		return Budget{}, err
	}
	return Budget{CPU: c, MemoryBytes: m}, nil
}

// DockerFlags 는 none 모드 컨테이너에 붙일 예산 플래그다. [§9.3 표]
func (b Budget) DockerFlags() []string {
	return []string{
		"--cpus=" + strconv.FormatFloat(b.CPU, 'f', -1, 64),
		"--memory=" + strconv.FormatInt(b.MemoryBytes, 10),
	}
}

// SystemdProps 는 sidecar 모드 slice 에 set-property 로 넣을 값이다. [§9.3 표]
// cpuQuota 는 cpu×100 을 그대로 쓴다(0.125 → "12.5%"). 클램프하지 않는다.
// memoryMax 는 바이트를 1024로 나누어떨어지는 가장 큰 단위(K/M/G)로 표기한다.
// systemd 의 K/M/G 는 1024 진법이므로 Ki/Mi/Gi 와 값이 같다.
func (b Budget) SystemdProps() (cpuQuota, memoryMax string) {
	return formatCPUQuota(b.CPU) + "%", formatMemoryMax(b.MemoryBytes)
}

// formatCPUQuota 는 cpu×100 을 십진 문자열로 만든다. 값을 자르거나 클램프하지 않는다. [§9.3]
// 유효숫자 15자리로 포맷해 이진 부동소수 오차(0.55×100 = 55.00000000000001)만 걷어낸다.
// float64 의 유효숫자가 약 15.95자리이므로 사용자가 쓴 값은 그대로 복원된다.
func formatCPUQuota(cpu float64) string {
	s := strconv.FormatFloat(cpu*100, 'g', 15, 64)
	if strings.ContainsAny(s, "eE") { // 지수 표기는 systemd 가 받지 않는다
		return strconv.FormatFloat(cpu*100, 'f', -1, 64)
	}
	return s
}

func formatMemoryMax(bytes int64) string {
	units := []struct {
		suffix string
		mult   int64
	}{
		{"G", 1 << 30},
		{"M", 1 << 20},
		{"K", 1 << 10},
	}
	for _, u := range units {
		if bytes >= u.mult && bytes%u.mult == 0 {
			return strconv.FormatInt(bytes/u.mult, 10) + u.suffix
		}
	}
	return strconv.FormatInt(bytes, 10)
}
