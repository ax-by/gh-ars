package domain

import (
	"reflect"
	"testing"
)

const uid = UnitID("01K4Z9V6H8QW3T5R7Y9B2C4D6E")

// [§4.3] GitHub runner 이름은 <scaleSet>-<machine>-<unit> 이며 GitHub 등록 ↔ 컨테이너 대조 키다.
func TestRunnerName_S4_3(t *testing.T) {
	got := RunnerName("linux-x64", "box1", uid)
	want := "linux-x64-box1-" + string(uid)
	if got != want {
		t.Fatalf("RunnerName = %q, want %q", got, want)
	}
	if len(got) > MaxRunnerNameLen {
		t.Fatalf("RunnerName 길이 %d > %d", len(got), MaxRunnerNameLen)
	}
	// R14 의 경계: len(scaleSet)+len(machine) == 36 이면 64자에 정확히 맞는다.
	ss, m := "0123456789012345678901234567890123", "45" // 34 + 2 = 36
	if n := len(RunnerName(ss, m, uid)); n != MaxRunnerNameLen {
		t.Fatalf("경계 이름 길이 = %d, want %d", n, MaxRunnerNameLen)
	}
}

// [§4.3] 컨테이너·볼륨·slice 이름 규칙.
func TestPartNames_S4_3(t *testing.T) {
	if got, want := ContainerName(uid, RoleRunner), "gh-ars-"+string(uid)+"-runner"; got != want {
		t.Fatalf("ContainerName(runner) = %q, want %q", got, want)
	}
	if got, want := ContainerName(uid, RoleSidecar), "gh-ars-"+string(uid)+"-sidecar"; got != want {
		t.Fatalf("ContainerName(sidecar) = %q, want %q", got, want)
	}
	wantVols := []string{
		"gh-ars-" + string(uid) + "-work",
		"gh-ars-" + string(uid) + "-sock",
		"gh-ars-" + string(uid) + "-externals",
	}
	for i, vk := range VolumeKinds {
		if got := VolumeName(uid, vk); got != wantVols[i] {
			t.Fatalf("VolumeName(%s) = %q, want %q", vk, got, wantVols[i])
		}
	}
	if got, want := VolumeNames(uid), wantVols; !reflect.DeepEqual(got, want) {
		t.Fatalf("VolumeNames = %v, want %v", got, want)
	}
	if got, want := SliceName(uid), "gh-ars-"+string(uid)+".slice"; got != want {
		t.Fatalf("SliceName = %q, want %q", got, want)
	}
}

// [§4.2] 라벨 세트. 볼륨에도 같은 라벨을 붙인다.
func TestLabels_S4_2(t *testing.T) {
	u := Unit{ID: uid, ScaleSet: "linux-x64", Machine: "box1", Mode: ModeSidecar}
	want := map[string]string{
		"gh-ars.unit":     string(uid),
		"gh-ars.role":     "sidecar",
		"gh-ars.scaleSet": "linux-x64",
		"gh-ars.mode":     "sidecar",
		"gh-ars.machine":  "box1",
	}
	if got := Labels(u, RoleSidecar); !reflect.DeepEqual(got, want) {
		t.Fatalf("Labels = %v, want %v", got, want)
	}
	want["gh-ars.role"] = "runner"
	if got := Labels(u, RoleRunner); !reflect.DeepEqual(got, want) {
		t.Fatalf("Labels(runner) = %v, want %v", got, want)
	}
	// 볼륨 라벨에는 role 이 없다 (볼륨은 unit 단위 부품이다).
	delete(want, "gh-ars.role")
	if got := VolumeLabels(u); !reflect.DeepEqual(got, want) {
		t.Fatalf("VolumeLabels = %v, want %v", got, want)
	}
	if UnitLabelFilter != "gh-ars.unit" {
		t.Fatalf("UnitLabelFilter = %q", UnitLabelFilter)
	}
}

// [§4.2] events 스트림은 컨테이너 이름으로 unit 과 role 을 식별한다.
func TestParseContainerName_S4_2(t *testing.T) {
	tests := []struct {
		in       string
		wantID   UnitID
		wantRole Role
		wantOK   bool
	}{
		{"gh-ars-" + string(uid) + "-runner", uid, RoleRunner, true},
		{"gh-ars-" + string(uid) + "-sidecar", uid, RoleSidecar, true},
		{"/gh-ars-" + string(uid) + "-runner", uid, RoleRunner, true}, // docker events 의 선행 슬래시
		{"gh-ars-" + string(uid) + "-work", "", "", false},            // 볼륨 이름
		{"gh-ars-" + string(uid), "", "", false},
		{"gh-ars--runner", "", "", false},
		{"other-" + string(uid) + "-runner", "", "", false},
		{"gh-ars-01K4Z9V6H8-runner", "", "", false},                 // ULID 길이 아님
		{"gh-ars-01K4Z9V6H8QW3T5R7Y9B2C4D6I-runner", "", "", false}, // Crockford 제외 문자(I)
		{"gh-ars-01k4z9v6h8qw3t5r7y9b2c4d6e-runner", "", "", false}, // 소문자
		{"gh-ars-80000000000000000000000000-runner", "", "", false}, // ULID 타임스탬프 오버플로
		{"gh-ars-7ZZZZZZZZZZZZZZZZZZZZZZZZZ-runner", "7ZZZZZZZZZZZZZZZZZZZZZZZZZ", RoleRunner, true}, // 최대 ULID
		{"", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			id, role, ok := ParseContainerName(tc.in)
			if ok != tc.wantOK || id != tc.wantID || role != tc.wantRole {
				t.Fatalf("ParseContainerName(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.in, id, role, ok, tc.wantID, tc.wantRole, tc.wantOK)
			}
		})
	}
}

// [§4.3] 머신 내부 고아 정리를 위해 볼륨·slice 이름에서도 unit 을 되찾는다. [§8.3]
func TestParseVolumeAndSliceName_S4_3(t *testing.T) {
	for i, vk := range VolumeKinds {
		id, kind, ok := ParseVolumeName(VolumeName(uid, vk))
		if !ok || id != uid || kind != VolumeKinds[i] {
			t.Fatalf("ParseVolumeName(%s) = (%q, %q, %v)", vk, id, kind, ok)
		}
	}
	if _, _, ok := ParseVolumeName("gh-ars-" + string(uid) + "-runner"); ok {
		t.Fatal("ParseVolumeName(container name) = ok, want !ok")
	}
	if _, _, ok := ParseVolumeName("gh-ars-shortid-work"); ok {
		t.Fatal("ParseVolumeName(ULID 아님) = ok, want !ok")
	}
	id, ok := ParseSliceName(SliceName(uid))
	if !ok || id != uid {
		t.Fatalf("ParseSliceName = (%q, %v)", id, ok)
	}
	// systemd 는 대시를 계층 구분자로 해석해 상위 계층 슬라이스를 함께 만든다.
	// 이들은 unit 이 아니므로 고아로 판정되면 안 된다 (PLAN Phase 0-2 발견사항). [§8.3, §9.3]
	notUnits := []string{
		"gh-ars-" + string(uid),   // .slice 접미 없음
		"gh-ars.slice",            // 상위 계층
		"gh.slice",                // 상위 계층
		"gh-ars-.slice",           // 빈 id
		"gh-ars-01K4Z9V6H8.slice", // ULID 길이 아님
		"gh-ars-01K4Z9V6H8QW3T5R7Y9B2C4D6U.slice", // Crockford 제외 문자(U)
		"user.slice",
		"system.slice",
	}
	for _, name := range notUnits {
		if id, ok := ParseSliceName(name); ok {
			t.Fatalf("ParseSliceName(%q) = (%q, true), want !ok", name, id)
		}
	}
}
