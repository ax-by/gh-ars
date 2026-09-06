// Package domain 은 unit / machine / scale set 모델과 이름·라벨 규칙을 담는다. [§4]
// I/O 를 하지 않는 순수 패키지다 (DESIGN §2).
package domain

import (
	"time"

	"gh-ars/internal/resource"
)

// UnitID 는 run 1회(= JIT config 1개 = job 1개)마다 발급하는 ULID(26자)다. [§4.1]
type UnitID string

// Mode 는 job 실행 방식이다. [§6.0]
type Mode string

const (
	ModeNone    Mode = "none"
	ModeSidecar Mode = "sidecar"
)

// RuntimeKind 는 머신의 컨테이너 runtime 이다. [§6.0]
type RuntimeKind string

const (
	RuntimeDocker RuntimeKind = "docker"
	RuntimePodman RuntimeKind = "podman"
)

// Role 은 unit 안에서 컨테이너가 맡는 역할이다. 라벨 gh-ars.role 의 값. [§4.2]
type Role string

const (
	RoleRunner  Role = "runner"
	RoleSidecar Role = "sidecar"
)

// VolumeKind 는 sidecar 모드 unit 이 쓰는 볼륨 3개의 종류다. [§4.1, §9.1]
type VolumeKind string

const (
	VolumeWork      VolumeKind = "work"
	VolumeSock      VolumeKind = "sock"
	VolumeExternals VolumeKind = "externals"
)

// VolumeKinds 는 Parts.Volumes 인덱스 순서와 같다.
var VolumeKinds = [3]VolumeKind{VolumeWork, VolumeSock, VolumeExternals}

// UnitState 는 unit 생명주기 상태다. 전이는 DESIGN §3.2. [§7.2, §8.3]
type UnitState string

const (
	StateCreating UnitState = "Creating"
	StateStarting UnitState = "Starting"
	StateRunning  UnitState = "Running"
	StateDraining UnitState = "Draining"
	StateDying    UnitState = "Dying"
	StateRemoved  UnitState = "Removed"
)

// Health 는 머신 상태다. Failed 는 재접속 대상에서도 제외된다. [§7.1-3, §7.1-8]
type Health string

const (
	Healthy   Health = "Healthy"
	Unhealthy Health = "Unhealthy"
	Failed    Health = "Failed"
)

// Parts 는 unit 을 이루는 부품의 존재 여부다. 짝이 맞는지 판정하는 근거. [§8.3]
type Parts struct {
	Runner  bool
	Sidecar bool
	Volumes [3]bool // VolumeKinds 순서: work, sock, externals
	Slice   bool
}

// Unit 은 runner 하나의 실행 인스턴스다. [§4.1]
type Unit struct {
	ID         UnitID
	ScaleSet   string
	Machine    string // 소속 머신 (권위: 도달한 머신) [§8.3]
	Mode       Mode
	State      UnitState
	CreatedAt  time.Time // grace·기동 타임아웃 기준 [§8.3]
	Adopted    bool      // 재시작 입양 여부 (로그용)
	Foreign    bool      // 라벨의 scale set 이 YAML 에 없음 → 머신 slot 1개로 계산 [§8.3]
	RunnerName string    // <scaleSet>-<machine>-<unit>. GetRunnerByName 키 [§4.3]
	Busy       bool      // JobStarted 수신. 축소 후보 제외용 최적화 [§7.2-3]
	Parts      Parts
}

// Machine 은 runner 를 띄우는 정적 머신 하나다. [§6.0, §7.1]
type Machine struct {
	Name         string
	ScaleSet     string
	Runtime      RuntimeKind
	Local        bool
	Sudo         bool // podman/systemctl 명령에 sudo -n 접두 여부 [§10.2]
	Health       Health
	PhysicalMax  int       // floor(min(cpu/cpu, mem/mem)) [§8.1]
	EffectiveMax int       // min(PhysicalMax, maxRunners) [§8.1]
	LastPlacedAt time.Time // spread tie-break ② [§8.2]
}

// ScaleSet 은 설정의 scaleSets[] 항목 하나에 대응한다. [§6.0]
type ScaleSet struct {
	Name         string
	GitHubID     int
	RunnerGroup  string
	MinRunners   int
	MaxRunners   int
	Unit         resource.Budget // unit 1개의 예산. sidecar 면 runner+sidecar 합산 [§8.1]
	Mode         Mode
	RunnerImage  string
	SidecarImage string // mode=sidecar 일 때만
	Machines     []string
}
