package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"gh-ars/internal/domain"
	"gh-ars/internal/executor"
)

// NewDocker 는 docker CLI 위의 Runtime 이다. sudo 는 §10.2 판단 규칙 결과이며 docker 는
// 규칙 2 에 따라 항상 false 를 받는다(docker 그룹 멤버 또는 root). [DESIGN §4.2]
func NewDocker(ex executor.Executor, sudo bool, log *slog.Logger) Runtime {
	return newCLI(domain.RuntimeDocker, "docker", ex, sudo, dockerFlavor{}, log)
}

type dockerFlavor struct{}

// dockerPSCreatedLayout 은 `docker ps` 의 CreatedAt 형식이다. 예: "2026-09-07 16:03:42 +0900 KST".
const dockerPSCreatedLayout = "2006-01-02 15:04:05 -0700 MST"

// psLabelKeys 는 ps 에서 뽑는 라벨이다. 입양 시 scale set/mode/machine 복원에 쓰는 gh-ars.*
// 키만 필요하다. [§4.2, §8.3]
var psLabelKeys = []string{domain.LabelUnit, domain.LabelRole, domain.LabelScaleSet, domain.LabelMode, domain.LabelMachine}

// ps 템플릿 필드 구분자. 템플릿에는 이스케이프 형태 `\t` 를 넣는다(docker/podman 이 탭으로
// 바꾼다. 원격 셸을 거치는 argv 에 제어 문자를 싣지 않기 위함). 출력은 실제 탭으로 나온다.
// gh-ars.* 라벨 값(ULID, 설정 이름)에 탭은 없다.
const (
	psSepTemplate = `\t`
	psSep         = "\t"
)

// psFormat 은 라벨을 `{{.Label "key"}}` 로 하나씩 뽑는다. `{{json .}}` 의 Labels 는 "k=v,k=v"
// 를 이스케이프 없이 이어붙인 문자열이라, 이미지가 물려준 라벨 값에 ",gh-ars.mode=none"
// 같은 조각이 들어 있으면 실제 라벨을 덮어쓸 수 있다. 키별 템플릿은 이 모호함이 없다. [§4.2]
func (dockerFlavor) psFormat() string { return commonPSFormat() }

func (dockerFlavor) parseContainer(line []byte) (Container, error) {
	return parsePSLine(line, dockerPSCreatedLayout)
}

// eventsFormat: docker 는 `{{json .}}` 를 쓴다(podman 은 `json` 리터럴, podman.go 참조). [§5]
func (dockerFlavor) eventsFormat() string { return jsonFormat }

// dockerEvent 는 `docker events --format '{{json .}}'` 한 줄이다. 컨테이너 이름과 die 의
// exitCode 는 Actor.Attributes 에 있다. [§4.2]
type dockerEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	TimeNano int64 `json:"timeNano"`
}

func (dockerFlavor) parseEvent(line []byte) (Event, bool, error) {
	var e dockerEvent
	if err := json.Unmarshal(line, &e); err != nil {
		return Event{}, false, fmt.Errorf("runtime: docker event 파싱: %w", err)
	}
	if e.Type != "container" {
		// 구독 argv 에 `--filter type=container` 가 있다(cli.go). 그래도 다른 type 이 온다면
		// 필터가 무시됐거나 스키마가 바뀐 것이므로 이상 신호로 센다.
		return Event{}, false, fmt.Errorf("runtime: docker event type %q(container 아님): %s", e.Type, line)
	}
	name := e.Actor.Attributes["name"]
	if name == "" {
		// 구독이 이미 type=container 로 좁혀져 있으므로 이름 없는 줄은 스키마가 어긋난 것이다.
		// 조용히 버리면 die 가 전부 사라져도 아무 신호가 없다. [§4.2, §7.2-5]
		return Event{}, false, fmt.Errorf("runtime: docker event 이름 없음: %s", line)
	}
	ev := Event{Name: name, Action: e.Action, At: time.Unix(0, e.TimeNano)}
	s, has := e.Actor.Attributes["exitCode"]
	if !has && e.Action == ActionDie {
		// podman 쪽과 같은 규칙: 전달하되 0(정상 종료)으로 단정하지 않는다. [§7.2-5, DESIGN §4.2]
		return ev, true, fmt.Errorf("runtime: docker die 이벤트에 exitCode 속성이 없다: %s", line)
	}
	if has {
		code, err := strconv.Atoi(s)
		if err != nil {
			// 이벤트는 전달한다(die 를 버리면 정리가 늦어진다). 다만 조용히 0(정상 종료)으로
			// 보고하지는 않는다: 스키마 어긋남으로 세어 신호를 남긴다. [§7.2-5, DESIGN §4.2]
			return ev, true, fmt.Errorf("runtime: docker event exitCode %q 를 읽지 못했다: %s", s, line)
		}
		ev.ExitCode = code
	}
	return ev, true, nil
}

// dockerInfo 는 `docker info --format '{{json .}}'` 중 쓰는 필드다. 데몬에 못 닿으면
// 클라이언트 정보만 채우고 ServerErrors 에 이유를 담는다. [§7.1-3, §9.2]
type dockerInfo struct {
	NCPU          int      `json:"NCPU"`
	MemTotal      int64    `json:"MemTotal"`
	CgroupDriver  string   `json:"CgroupDriver"`
	CgroupVersion string   `json:"CgroupVersion"`
	ServerErrors  []string `json:"ServerErrors"`
	// SecurityOptions 는 rootless 여부를 담는다: rootless 데몬은 "name=rootless" 항목을 낸다.
	// docker 에는 podman 의 Host.Security.Rootless 같은 전용 필드가 없다. [§9.2]
	SecurityOptions []string `json:"SecurityOptions"`
}

// dockerRootlessOption 은 `docker info` 의 SecurityOptions 에서 rootless 데몬을 가리키는 항목이다.
const dockerRootlessOption = "name=rootless"

func (dockerFlavor) parseInfo(out []byte) (Info, error) {
	var i dockerInfo
	if err := json.Unmarshal(out, &i); err != nil {
		return Info{}, fmt.Errorf("runtime: docker info 파싱: %w", err)
	}
	if len(i.ServerErrors) > 0 {
		return Info{}, errors.New("runtime: docker info: " + strings.Join(i.ServerErrors, "; "))
	}
	if i.NCPU <= 0 || i.MemTotal <= 0 {
		// 스키마가 어긋나 0 이 나가면 R21 이 physicalMax 0 으로 읽어 그 머신을 **영구 Failed**
		// (재접속 대상에서도 제외)로 만든다. 예산을 못 얻은 info 는 쓸모가 없으므로 오류로
		// 올려 unhealthy(재시도 가능)로 두는 편이 맞다. [§7.1-3, R21]
		return Info{}, fmt.Errorf("runtime: docker info 에 예산이 없다(NCPU=%d, MemTotal=%d)", i.NCPU, i.MemTotal)
	}
	rootless := false
	for _, opt := range i.SecurityOptions {
		if strings.Contains(opt, dockerRootlessOption) {
			rootless = true
		}
	}
	return Info{
		CPUs:          float64(i.NCPU),
		MemoryBytes:   i.MemTotal,
		Rootless:      rootless,
		CgroupDriver:  i.CgroupDriver,
		CgroupVersion: i.CgroupVersion,
	}, nil
}

// isNotFound 는 docker rm / volume rm 의 "이미 없음" 응답이다. [§8.3]
// docker 는 종료 코드로 구분하지 않으므로(둘 다 1) 메시지로 판별한다.
func (dockerFlavor) isNotFound(stderr string) bool { return containsNotFoundMsg(stderr) }
