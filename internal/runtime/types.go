// Package runtime 은 docker/podman CLI 를 공통 모델로 정규화한다. [§5, §7, §9]
//
// 구현은 Executor 위에서 CLI 를 호출한다(원격 에이전트 없음). docker 와 podman 은
// argv 조립과 출력 파싱만 다르므로 공통 골격은 cli.go 에, 차이는 docker.go /
// podman.go 에 둔다. [DESIGN §4.2]
package runtime

import (
	"context"
	"io"
	"time"

	"gh-ars/internal/domain"
)

// Info 는 `info` 의 정규화 결과다. 머신 예산 자동 탐지와 sidecar preflight 에 쓴다. [§7.1-3, §9.2]
type Info struct {
	CPUs          float64
	MemoryBytes   int64
	Rootless      bool   // podman 은 Host.Security.Rootless, docker 는 SecurityOptions 의 "name=rootless" [§9.2]
	CgroupDriver  string // "systemd" 기대
	CgroupVersion string // "2" 기대. podman 의 "v2" 는 "2" 로 정규화
}

// Container 는 `ps -a` 한 줄이다. 이름으로 unit·role 을 식별하고,
// 라벨은 입양 시 scale set/mode 복원에 쓴다. [§4.2, §8.3]
type Container struct {
	Name    string // gh-ars-<unit>-runner|sidecar
	Labels  map[string]string
	State   string // running | exited | created ...
	Created time.Time
}

// ActionDie 는 컨테이너 종료를 뜻하는 공통 어휘다. docker 는 이 값을 그대로 내고 podman 은
// "died" 를 이것으로 정규화한다(podman.go). controller 는 이 값으로만 분기한다. [§4.2, §7.2-5]
const ActionDie = "die"

// Event 는 events 스트림 한 건이다. unit/role 은 호출자가 Name 에서 파싱한다. [§4.2]
type Event struct {
	Name     string // 컨테이너 이름 → unit/role 파싱 [§4.2]
	Action   string // "die" | "start" | ...
	ExitCode int    // die 에서만 의미
	At       time.Time
}

// Mount 는 명명 볼륨 → 컨테이너 경로 마운트다. [§9.1]
type Mount struct {
	Volume        string
	ContainerPath string
}

// CreateSpec 은 `create` 인자다. Env 에 JIT 값은 절대 넣지 않는다. [§7.2-4, §9.1]
type CreateSpec struct {
	Name, Image  string
	Labels       map[string]string
	Entrypoint   []string // 원소 1개. docker/podman 의 --entrypoint 는 실행 파일 하나만 받는다
	Cmd          []string
	Env          map[string]string // DOCKER_HOST 등. JIT 값은 절대 넣지 않음
	Volumes      []Mount
	CgroupParent string  // sidecar 모드: gh-ars-<unit>.slice
	CPUs         float64 // none 모드만 (0 이면 미지정)
	MemoryBytes  int64   // none 모드만 (0 이면 미지정)
	Privileged   bool    // sidecar 컨테이너만
}

// Runtime 은 한 머신의 컨테이너 runtime 이다. [§5, DESIGN §4.2]
type Runtime interface {
	Kind() domain.RuntimeKind
	Info(ctx context.Context) (Info, error)
	Pull(ctx context.Context, image string) error
	// List 는 항상 `ps -a` 다. labelFilter 는 "gh-ars.unit" 또는 "gh-ars.unit=<id>". [§4.2, §8.3]
	List(ctx context.Context, labelFilter string) ([]Container, error)
	Create(ctx context.Context, spec CreateSpec) error
	// CopyIn 은 `cp - <container>:<destDir>` 에 tar 스트림을 stdin 으로 넘긴다. [§7.2-4]
	CopyIn(ctx context.Context, container string, tar io.Reader, destDir string) error
	Start(ctx context.Context, container string) error
	// Remove 는 이미 없는 컨테이너를 성공으로 취급한다(정리 단계 멱등). [§8.3]
	Remove(ctx context.Context, container string, force bool) error
	VolumeCreate(ctx context.Context, name string, labels map[string]string) error
	VolumeList(ctx context.Context, labelFilter string) ([]string, error)
	// VolumeRemove 는 이미 없는 볼륨을 성공으로 취급한다. [§8.3]
	VolumeRemove(ctx context.Context, name string) error
	// Events 는 스트림이 끝나면(정상·비정상 모두) error 채널로 통지하고 두 채널을 닫는다.
	// 호출자는 백오프로 재시작한다. [§7.1-8]
	Events(ctx context.Context, labelFilter string) (<-chan Event, <-chan error)
}
