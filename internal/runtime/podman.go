package runtime

import (
	"encoding/json"
	"fmt"
	"time"

	"gh-ars/internal/domain"
	"gh-ars/internal/executor"
)

// NewPodman 은 podman CLI 위의 Runtime 이다. sudo 는 §10.2 규칙 3(경로 고정)의 결과이며,
// docker 와 달리 true 일 수 있다(rootful podman 을 passwordless sudo 로 쓰는 머신). [DESIGN §4.2]
func NewPodman(ex executor.Executor, sudo bool) Runtime {
	return &cli{kind: domain.RuntimePodman, bin: "podman", sudo: sudo, ex: ex, f: podmanFlavor{}}
}

type podmanFlavor struct{}

// podmanPSCreatedLayout 은 `podman ps` 의 CreatedAt 형식이다. podman 의 `.CreatedAt` 템플릿
// 필드는 커스텀 포맷 없이 `time.Time.String()` 을 그대로 쓴다(docker 처럼 초 단위로 자르지
// 않고 나노초까지 나온다). 예: "2026-09-07 16:03:42.123456789 +0900 KST".
const podmanPSCreatedLayout = "2006-01-02 15:04:05.999999999 -0700 MST"

// psFormat/parseContainer: podman ps 는 docker 와 같은 템플릿 함수(.Names, .State,
// .CreatedAt, .Label)를 지원한다. CreatedAt 레이아웃만 다르다. [DESIGN §4.2]
func (podmanFlavor) psFormat() string { return commonPSFormat() }

func (podmanFlavor) parseContainer(line []byte) (Container, error) {
	return parsePSLine(line, podmanPSCreatedLayout)
}

// eventsFormat: podman 은 `json` 리터럴을 쓴다(docker 의 `{{json .}}` 와 달리 podman 자체
// JSON Lines 출력이다). [§5]
func (podmanFlavor) eventsFormat() string { return "json" }

// podmanEvent 는 `podman events --format json` 한 줄이다. docker 와 달리 컨테이너 이름과
// 종료 코드가 최상위 필드다(Actor.Attributes 를 거치지 않는다). Time 은 RFC3339Nano 문자열
// 이다(docker 의 timeNano 정수와 다르다). Status 값 "died"(libpod events.Exited)가 컨테이너
// 종료를 뜻하며, 공통 어휘 "die"로 정규화한다 — controller 는 Action=="die" 로만 분기한다
// (internal/controller/loop.go). [§4.2, TESTPLAN (runtime/podman)]
type podmanEvent struct {
	Type              string    `json:"Type"`
	Status            string    `json:"Status"`
	Name              string    `json:"Name"`
	Time              time.Time `json:"Time"`
	ContainerExitCode *int      `json:"ContainerExitCode"`
}

// podmanStatusDied 는 libpod events.Exited 의 JSON 값이다("died").
const podmanStatusDied = "died"

func (podmanFlavor) parseEvent(line []byte) (Event, bool) {
	var e podmanEvent
	if err := json.Unmarshal(line, &e); err != nil || e.Type != "container" || e.Name == "" {
		return Event{}, false
	}
	action := e.Status
	if action == podmanStatusDied {
		action = "die"
	}
	ev := Event{Name: e.Name, Action: action, At: e.Time}
	if e.ContainerExitCode != nil {
		ev.ExitCode = *e.ContainerExitCode
	}
	return ev, true
}

// podmanInfo 는 `podman info --format '{{json .}}'` 중 쓰는 필드다. docker 와 달리 Host 아래
// 중첩되고, Rootless 는 Host 최상위가 아니라 Host.Security.Rootless 에 있다(§10.2 규칙 3의
// preflight 예시 `{{.Host.Security.Rootless}}`와 동일 경로). [§9.2]
type podmanInfo struct {
	Host struct {
		CgroupManager string `json:"cgroupManager"`
		CgroupVersion string `json:"cgroupVersion"`
		CPUs          int    `json:"cpus"`
		MemTotal      int64  `json:"memTotal"`
		Security      struct {
			Rootless bool `json:"rootless"`
		} `json:"security"`
	} `json:"host"`
}

func (podmanFlavor) parseInfo(out []byte) (Info, error) {
	var i podmanInfo
	if err := json.Unmarshal(out, &i); err != nil {
		return Info{}, fmt.Errorf("runtime: podman info 파싱: %w", err)
	}
	// [§7.1-3, DESIGN §4.2] podman 의 cgroup 버전은 "v2"로 오며 docker 의 "2"에 맞춰 정규화한다.
	cgVersion := i.Host.CgroupVersion
	if cgVersion == "v2" {
		cgVersion = "2"
	}
	return Info{
		CPUs:          float64(i.Host.CPUs),
		MemoryBytes:   i.Host.MemTotal,
		Rootless:      i.Host.Security.Rootless,
		CgroupDriver:  i.Host.CgroupManager,
		CgroupVersion: cgVersion,
	}, nil
}

// isNotFound 는 podman rm / volume rm 의 "이미 없음" 응답이다. docker 와 같은 부분 문자열
// ("no such container"/"no such volume")을 포함한다. [§8.3]
func (podmanFlavor) isNotFound(stderr string) bool { return containsNotFoundMsg(stderr) }
