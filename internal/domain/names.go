package domain

import "strings"

// 이름 접두. 컨테이너·볼륨·slice 가 공유한다. [§4.3]
const namePrefix = "gh-ars-"

// 라벨 키. 설정 파일 없이 reconcile 할 수 있도록 unit 의 소속을 전부 담는다. [§4.2]
const (
	LabelUnit     = "gh-ars.unit"
	LabelRole     = "gh-ars.role"
	LabelScaleSet = "gh-ars.scaleSet"
	LabelMode     = "gh-ars.mode"
	LabelMachine  = "gh-ars.machine"
)

// UnitLabelFilter 는 gh-ars 가 만든 부품만 고르는 ps/volume ls 필터 키다. [§4.2]
const UnitLabelFilter = LabelUnit

// MaxRunnerNameLen 은 GitHub runner 이름의 상한이다. len(scaleSet)+len(machine) ≤ 36 의 근거. [§4.3, R14]
const MaxRunnerNameLen = 64

// UnitIDLen 은 ULID 길이다. [§4.1]
const UnitIDLen = 26

// isUnitID 는 ULID(26자, Crockford base32 대문자) 형식만 unit id 로 인정한다. [§4.1]
// systemd 는 slice 이름의 대시를 계층 구분자로 해석해 상위 계층 슬라이스
// (gh-ars.slice, gh.slice)를 함께 만든다. 형식 검증이 없으면 이런 이름이
// 고아 부품으로 잘못 판정되어 stop/revert 대상이 된다 (PLAN Phase 0-2 발견사항). [§8.3]
func isUnitID(s string) bool {
	if len(s) != UnitIDLen {
		return false
	}
	// 첫 글자는 48비트 타임스탬프의 최상위 자리라 '7'을 넘을 수 없다(넘으면 오버플로).
	if s[0] > '7' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'A' && c <= 'Z' && c != 'I' && c != 'L' && c != 'O' && c != 'U':
		default:
			return false
		}
	}
	return true
}

// RunnerName 은 GitHub 등록 ↔ 머신 컨테이너 1:1 대조 키다. [§4.3]
func RunnerName(scaleSet, machine string, id UnitID) string {
	return scaleSet + "-" + machine + "-" + string(id)
}

// ContainerName 은 runner/sidecar 컨테이너 이름이다. events 에서 unit·role 식별에 쓴다. [§4.2, §4.3]
func ContainerName(id UnitID, role Role) string {
	return namePrefix + string(id) + "-" + string(role)
}

// VolumeName 은 sidecar 모드 볼륨 이름이다. [§4.3]
func VolumeName(id UnitID, kind VolumeKind) string {
	return namePrefix + string(id) + "-" + string(kind)
}

// VolumeNames 는 unit 의 볼륨 3개를 VolumeKinds 순서로 돌려준다. [§4.1]
func VolumeNames(id UnitID) []string {
	names := make([]string, 0, len(VolumeKinds))
	for _, k := range VolumeKinds {
		names = append(names, VolumeName(id, k))
	}
	return names
}

// SliceName 은 unit 의 systemd slice 이름이다. [§4.2, §9.3]
func SliceName(id UnitID) string {
	return namePrefix + string(id) + ".slice"
}

// Labels 는 컨테이너에 붙이는 라벨 세트다. [§4.2]
func Labels(u Unit, role Role) map[string]string {
	l := VolumeLabels(u)
	l[LabelRole] = string(role)
	return l
}

// VolumeLabels 는 볼륨에 붙이는 라벨 세트다. 볼륨은 unit 단위 부품이라 role 이 없다. [§4.2]
func VolumeLabels(u Unit) map[string]string {
	return map[string]string{
		LabelUnit:     string(u.ID),
		LabelScaleSet: u.ScaleSet,
		LabelMode:     string(u.Mode),
		LabelMachine:  u.Machine,
	}
}

// ParseContainerName 은 컨테이너 이름에서 unit 과 role 을 되찾는다.
// docker events 가 붙이는 선행 슬래시를 허용한다. [§4.2]
func ParseContainerName(name string) (UnitID, Role, bool) {
	rest, ok := strings.CutPrefix(strings.TrimPrefix(name, "/"), namePrefix)
	if !ok {
		return "", "", false
	}
	for _, role := range []Role{RoleRunner, RoleSidecar} {
		id, ok := strings.CutSuffix(rest, "-"+string(role))
		if ok && isUnitID(id) {
			return UnitID(id), role, true
		}
	}
	return "", "", false
}

// ParseVolumeName 은 볼륨 이름에서 unit 과 종류를 되찾는다. 고아 정리에 쓴다. [§8.3]
func ParseVolumeName(name string) (UnitID, VolumeKind, bool) {
	rest, ok := strings.CutPrefix(name, namePrefix)
	if !ok {
		return "", "", false
	}
	for _, kind := range VolumeKinds {
		id, ok := strings.CutSuffix(rest, "-"+string(kind))
		if ok && isUnitID(id) {
			return UnitID(id), kind, true
		}
	}
	return "", "", false
}

// ParseSliceName 은 slice 이름에서 unit 을 되찾는다. 고아 정리에 쓴다. [§8.3]
func ParseSliceName(name string) (UnitID, bool) {
	rest, ok := strings.CutPrefix(name, namePrefix)
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(rest, ".slice")
	if !ok || !isUnitID(id) {
		return "", false
	}
	return UnitID(id), true
}
