package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gh-ars/internal/domain"
	"gh-ars/internal/executor"
)

// NewDocker 는 docker CLI 위의 Runtime 이다. sudo 는 §10.2 판단 규칙 결과이며 docker 는
// 규칙 2 에 따라 항상 false 를 받는다(docker 그룹 멤버 또는 root). [DESIGN §4.2]
func NewDocker(ex executor.Executor, sudo bool) Runtime {
	return &cli{kind: domain.RuntimeDocker, bin: "docker", sudo: sudo, ex: ex, f: dockerFlavor{}}
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
func (dockerFlavor) psFormat() string {
	fields := []string{"{{.Names}}", "{{.State}}", "{{.CreatedAt}}"}
	for _, k := range psLabelKeys {
		fields = append(fields, `{{.Label "`+k+`"}}`)
	}
	return strings.Join(fields, psSepTemplate)
}

func (dockerFlavor) parseContainer(line []byte) (Container, error) {
	fields := strings.Split(string(line), psSep)
	if len(fields) != 3+len(psLabelKeys) {
		return Container{}, fmt.Errorf("필드 %d개(기대 %d): %s", len(fields), 3+len(psLabelKeys), line)
	}
	if fields[0] == "" {
		return Container{}, fmt.Errorf("Names 없음: %s", line)
	}
	created, err := time.Parse(dockerPSCreatedLayout, fields[2])
	if err != nil {
		return Container{}, fmt.Errorf("CreatedAt %q: %w", fields[2], err)
	}
	labels := map[string]string{}
	for i, k := range psLabelKeys {
		if v := fields[3+i]; v != "" {
			labels[k] = v
		}
	}
	return Container{Name: fields[0], State: fields[1], Created: created, Labels: labels}, nil
}

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

func (dockerFlavor) parseEvent(line []byte) (Event, bool) {
	var e dockerEvent
	if err := json.Unmarshal(line, &e); err != nil || e.Type != "container" {
		return Event{}, false
	}
	name := e.Actor.Attributes["name"]
	if name == "" {
		return Event{}, false
	}
	ev := Event{Name: name, Action: e.Action, At: time.Unix(0, e.TimeNano)}
	if s, ok := e.Actor.Attributes["exitCode"]; ok {
		if code, err := strconv.Atoi(s); err == nil {
			ev.ExitCode = code
		}
	}
	return ev, true
}

// dockerInfo 는 `docker info --format '{{json .}}'` 중 쓰는 필드다. 데몬에 못 닿으면
// 클라이언트 정보만 채우고 ServerErrors 에 이유를 담는다. [§7.1-3, §9.2]
type dockerInfo struct {
	NCPU          int      `json:"NCPU"`
	MemTotal      int64    `json:"MemTotal"`
	CgroupDriver  string   `json:"CgroupDriver"`
	CgroupVersion string   `json:"CgroupVersion"`
	ServerErrors  []string `json:"ServerErrors"`
}

func (dockerFlavor) parseInfo(out []byte) (Info, error) {
	var i dockerInfo
	if err := json.Unmarshal(out, &i); err != nil {
		return Info{}, fmt.Errorf("runtime: docker info 파싱: %w", err)
	}
	if len(i.ServerErrors) > 0 {
		return Info{}, errors.New("runtime: docker info: " + strings.Join(i.ServerErrors, "; "))
	}
	return Info{
		CPUs:          float64(i.NCPU),
		MemoryBytes:   i.MemTotal,
		CgroupDriver:  i.CgroupDriver,
		CgroupVersion: i.CgroupVersion,
	}, nil
}

// isNotFound 는 docker rm / volume rm 의 "이미 없음" 응답이다. [§8.3]
// docker 는 종료 코드로 구분하지 않으므로(둘 다 1) 메시지로 판별한다.
func (dockerFlavor) isNotFound(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "no such container") || strings.Contains(s, "no such volume")
}
