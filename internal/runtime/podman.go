package runtime

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gh-ars/internal/domain"
	"gh-ars/internal/executor"
)

// NewPodman 은 podman CLI 위의 Runtime 이다. sudo 는 §10.2 규칙 3(경로 고정)의 결과이며,
// docker 와 달리 true 일 수 있다(rootful podman 을 passwordless sudo 로 쓰는 머신). [DESIGN §4.2]
func NewPodman(ex executor.Executor, sudo bool, log *slog.Logger) Runtime {
	return newCLI(domain.RuntimePodman, "podman", ex, sudo, podmanFlavor{}, log)
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
// 종료 코드가 최상위 필드다(Actor.Attributes 를 거치지 않는다). Status 값 "died"(libpod
// events.Exited)가 컨테이너 종료를 뜻하며, 공통 어휘 "die"로 정규화한다 — controller 는
// Action=="die" 로만 분기한다(internal/controller/loop.go).
//
// 시각 필드는 버전마다 다르게 온다. podman 6.1.1 실제 출력은 유닉스 초 `time` 과 나노초
// `timeNano` 정수 둘 다이고, 문서·구버전은 RFC3339 문자열을 보인다. 정수를 time.Time 으로
// 받으려 하면 unmarshal 이 통째로 실패하고, parseEvent 가 그 줄을 버려 **모든 이벤트가 사라진다**
// (die 가 영영 오지 않는다). 그래서 정수·문자열 양쪽을 받는다. [§4.2, §7.2-5]
type podmanEvent struct {
	Type              string     `json:"Type"`
	Status            string     `json:"Status"`
	Name              string     `json:"Name"`
	TimeNano          int64      `json:"timeNano"`
	Time              podmanTime `json:"time"` // 대소문자 무시 매칭이라 구버전의 "Time" 도 여기로 온다
	ContainerExitCode *int       `json:"ContainerExitCode"`
}

// podmanTime 은 숫자(유닉스 초)와 RFC3339 문자열을 모두 받는 시각이다. 읽지 못하면 영값으로
// 두고 줄은 살린다: 이벤트의 쓸모는 이름·상태이고 시각은 로그용이다. [§4.2]
type podmanTime struct{ time.Time }

func (t *podmanTime) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return nil // 시각만 포기한다
		}
		if parsed, err := time.Parse(time.RFC3339Nano, str); err == nil {
			t.Time = parsed
		}
		return nil
	}
	var sec float64
	if err := json.Unmarshal(b, &sec); err != nil {
		return nil
	}
	t.Time = time.Unix(int64(sec), 0)
	return nil
}

// podmanStatusDied 는 libpod events.Exited 의 JSON 값이다("died").
const podmanStatusDied = "died"

func (podmanFlavor) parseEvent(line []byte) (Event, bool, error) {
	var e podmanEvent
	if err := json.Unmarshal(line, &e); err != nil {
		return Event{}, false, fmt.Errorf("runtime: podman event 파싱: %w", err)
	}
	if e.Type != "container" {
		// 구독 argv 에 `--filter type=container` 가 있으므로(cli.go) 다른 type 이 오는 것은
		// 데몬이 필터를 무시했거나 스키마가 바뀐 것이다: 이름 없는 줄과 같은 등급의 이상 신호다.
		return Event{}, false, fmt.Errorf("runtime: podman event type %q(container 아님): %s", e.Type, line)
	}
	if e.Name == "" {
		// 구독에 `--filter type=container` 가 걸려 있으므로 이름 없는 container 줄은 스키마가
		// 어긋난 것이다(예: 필드명이 Names 로 바뀐 버전). 조용히 버리면 시각 필드 때와 똑같이
		// 모든 die 가 사라진다. [§4.2, §7.2-5]
		return Event{}, false, fmt.Errorf("runtime: podman event 이름 없음: %s", line)
	}
	action := e.Status
	if action == podmanStatusDied {
		action = ActionDie
	}
	at := e.Time.Time
	if e.TimeNano != 0 {
		at = time.Unix(0, e.TimeNano)
	}
	ev := Event{Name: e.Name, Action: action, At: at}
	if e.ContainerExitCode != nil {
		ev.ExitCode = *e.ContainerExitCode
	}
	return ev, true, nil
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
	if i.Host.CPUs <= 0 || i.Host.MemTotal <= 0 {
		// docker 쪽과 같은 이유: 예산 0 은 R21 에서 영구 Failed 로 번진다. [§7.1-3, R21]
		return Info{}, fmt.Errorf("runtime: podman info 에 예산이 없다(cpus=%d, memTotal=%d)", i.Host.CPUs, i.Host.MemTotal)
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
