package resource

import (
	"math"
	"testing"
)

// [R25] cpu 파싱: YAML 숫자(int/float)와 문자열을 모두 받고, 음수·0·비숫자는 거부한다.
func TestParseCPU_R25(t *testing.T) {
	ok := []struct {
		name string
		in   any
		want float64
	}{
		{"int", 2, 2},
		{"int64", int64(4), 4},
		{"float", 0.5, 0.5},
		{"string int", "2", 2},
		{"string float", "0.5", 0.5},
		{"string spaces", " 1.5 ", 1.5},
		{"lower bound", 0.01, 0.01},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCPU(tc.in)
			if err != nil {
				t.Fatalf("ParseCPU(%#v) = err %v, want %v", tc.in, err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("ParseCPU(%#v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}

	bad := []struct {
		name string
		in   any
	}{
		{"zero", 0},
		{"zero float", 0.0},
		{"negative", -1},
		{"negative float", -0.5},
		{"string zero", "0"},
		{"string negative", "-2"},
		{"below MinCPU", 0.009},
		{"string below MinCPU", "0.001"},
		{"not a number", "two"},
		{"empty", ""},
		{"bool", true},
		{"nil", nil},
		{"NaN", math.NaN()},
		{"Inf", math.Inf(1)},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ParseCPU(tc.in); err == nil {
				t.Fatalf("ParseCPU(%#v) = %v, want error", tc.in, got)
			}
		})
	}
}

// [R25] memory 파싱: Ki/Mi/Gi (1024 진법)만 허용한다.
func TestParseMemory_R25(t *testing.T) {
	ok := []struct {
		in   string
		want int64
	}{
		{"1024Ki", 1024 * 1024},
		{"512Mi", 512 * 1024 * 1024},
		{"4Gi", 4 * 1024 * 1024 * 1024},
		{" 2Gi ", 2 * 1024 * 1024 * 1024},
	}
	for _, tc := range ok {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseMemory(tc.in)
			if err != nil {
				t.Fatalf("ParseMemory(%q) = err %v, want %d", tc.in, err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("ParseMemory(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}

	bad := []string{
		"1024",  // 접미 없음
		"512M",  // Ki/Mi/Gi 가 아님
		"4G",    //
		"4GB",   //
		"1Ti",   // MVP 단위 밖
		"0Gi",   // 0
		"-1Gi",  // 음수
		"1.5Gi", // 소수
		"Gi",    // 숫자 없음
		"",      //
		"4 Gi",  // 내부 공백
		"4gi",   // 대소문자
	}
	for _, in := range bad {
		t.Run("bad/"+in, func(t *testing.T) {
			if got, err := ParseMemory(in); err == nil {
				t.Fatalf("ParseMemory(%q) = %d, want error", in, got)
			}
		})
	}
}

func TestParseBudget_R25(t *testing.T) {
	b, err := ParseBudget("0.5", "512Mi")
	if err != nil {
		t.Fatalf("ParseBudget: %v", err)
	}
	if b.CPU != 0.5 || b.MemoryBytes != 512*1024*1024 {
		t.Fatalf("ParseBudget = %+v", b)
	}
	if _, err := ParseBudget("x", "512Mi"); err == nil {
		t.Fatal("ParseBudget(bad cpu) = nil error")
	}
	if _, err := ParseBudget(1, "512"); err == nil {
		t.Fatal("ParseBudget(bad memory) = nil error")
	}
}

// [§9.3 표] none 모드는 컨테이너 플래그로 예산을 적용한다.
func TestBudget_DockerFlags_S9_3(t *testing.T) {
	tests := []struct {
		name string
		b    Budget
		want []string
	}{
		{"cpu 2 / 4Gi", Budget{CPU: 2, MemoryBytes: 4 * 1024 * 1024 * 1024}, []string{"--cpus=2", "--memory=4294967296"}},
		{"cpu 0.5 / 512Mi", Budget{CPU: 0.5, MemoryBytes: 512 * 1024 * 1024}, []string{"--cpus=0.5", "--memory=536870912"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.b.DockerFlags()
			if len(got) != len(tc.want) {
				t.Fatalf("DockerFlags = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("DockerFlags = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// [§9.3 표] sidecar 모드는 slice 속성으로 예산을 적용한다.
func TestBudget_SystemdProps_S9_3(t *testing.T) {
	tests := []struct {
		name    string
		b       Budget
		wantCPU string
		wantMem string
	}{
		{"cpu 2 / 4Gi", Budget{CPU: 2, MemoryBytes: 4 * 1024 * 1024 * 1024}, "200%", "4G"},
		{"cpu 0.5 / 512Mi", Budget{CPU: 0.5, MemoryBytes: 512 * 1024 * 1024}, "50%", "512M"},
		{"cpu 1 / 1024Ki", Budget{CPU: 1, MemoryBytes: 1024 * 1024}, "100%", "1M"},
		{"non-round memory", Budget{CPU: 1, MemoryBytes: 1500}, "100%", "1500"},
		{"Ki only", Budget{CPU: 1, MemoryBytes: 3 * 1024}, "100%", "3K"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cpu, mem := tc.b.SystemdProps()
			if cpu != tc.wantCPU || mem != tc.wantMem {
				t.Fatalf("SystemdProps = (%q, %q), want (%q, %q)", cpu, mem, tc.wantCPU, tc.wantMem)
			}
		})
	}
}

// [§9.3, R25] CPUQuota 는 cpu×100 그대로다. 클램프하지 않고 소수도 보존한다.
// (하한 미만은 ParseCPU 가 R25 오류로 떨어뜨리므로 여기서 처리하지 않는다.)
func TestBudget_SystemdProps_FractionalCPU_S9_3(t *testing.T) {
	tests := []struct {
		cpu  float64
		want string
	}{
		{2, "200%"},
		{0.5, "50%"},
		{1.25, "125%"},
		{0.125, "12.5%"},
		{0.55, "55%"}, // 이진 부동소수 오차가 새어 나오지 않아야 한다
		{0.01, "1%"},           // R25 하한
		{0.010004, "1.0004%"},  // 소수 3자리 아래도 자르지 않는다
		{0.333333, "33.3333%"}, //
		{128, "12800%"},        // 큰 값도 지수 표기가 아니어야 한다
	}
	for _, tc := range tests {
		if got, _ := (Budget{CPU: tc.cpu, MemoryBytes: 1 << 20}).SystemdProps(); got != tc.want {
			t.Fatalf("SystemdProps(cpu=%v).cpuQuota = %q, want %q", tc.cpu, got, tc.want)
		}
	}
}
