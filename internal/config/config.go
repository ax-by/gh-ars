// Package config 는 설정 파일을 strict 디코드하고 `${env:}`/`${file:}` 를 치환한 뒤
// 기본값을 채우고 정적 규칙(R2~R15, R17~R20, R23 필수성, R25)을 검증한다. [§6, DESIGN §8]
// 동적 규칙(R16, R21, R22, R23 cap, R24)은 preflight 이후 소속 패키지가 판정한다.
//
// 파이프라인: 파일 읽기 → strict 디코드(KnownFields) → secret 필드 참조 형식 검사(R4)
// → 필드 값 치환(R5) → 기본값 → 정적 검증. 원문 텍스트 치환은 하지 않는다.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"

	"gh-ars/internal/domain"
	"gh-ars/internal/resource"
)

// 코드 상수. 릴리스마다 갱신한다. [§2, §9.1]
const (
	DefaultRunnerImage    = "ghcr.io/actions/actions-runner:2.337.0"
	DefaultRunnerGroup    = "Default"
	DefaultSSHPort        = 22
	DefaultKnownHostsFile = "~/.ssh/known_hosts"
)

// DefaultSidecarImage 는 sidecar 이미지를 생략했을 때 연결 머신의 공통 runtime 별 기본값이다. [§9.1]
var DefaultSidecarImage = map[domain.RuntimeKind]string{
	domain.RuntimeDocker: "docker:29.7.2-dind",
	domain.RuntimePodman: "quay.io/podman/stable:v5.8.4",
}

// Env 는 config 가 하는 I/O 전부다. 유닛 테스트는 가짜를 주입한다.
type Env struct {
	LookupEnv func(name string) (string, bool)
	ReadFile  func(path string) ([]byte, error)
	HomeDir   func() (string, error)
}

// OSEnv 는 실제 OS 를 쓰는 Env 다.
func OSEnv() Env {
	return Env{LookupEnv: os.LookupEnv, ReadFile: os.ReadFile, HomeDir: os.UserHomeDir}
}

// Config 는 검증·치환·기본값·병합을 마친 설정이다. 소비자는 raw YAML 모양을 몰라도 된다.
type Config struct {
	GitHub    GitHub
	ScaleSets []ScaleSet
	Machines  []Machine
	// Warnings 는 시작을 막지 않는 지적(R20 insecureSkipHostKeyVerify)이다. 호출자가 로그로 남긴다.
	Warnings []string
}

// ScaleSet 은 이름으로 scale set 항목을 찾는다. `scaleset delete` 가 runnerGroup 을 읽을 때 쓴다. [§11]
func (c *Config) ScaleSet(name string) (*ScaleSet, bool) {
	for i := range c.ScaleSets {
		if c.ScaleSets[i].Name == name {
			return &c.ScaleSets[i], true
		}
	}
	return nil, false
}

// Scope 는 github.url 에서 판별한 대상 종류다. [§3.1, R6]
type Scope string

const (
	ScopeOrg  Scope = "org"
	ScopeRepo Scope = "repo"
)

type GitHub struct {
	URL   string // 원문. scaleset 클라이언트에 그대로 넘긴다
	Scope Scope
	Owner string // org 또는 repo 소유자
	Repo  string // repo scope 일 때만
	Auth  Auth
}

// Auth 는 PAT 또는 GitHub App 중 정확히 하나다. [R3]
type Auth struct {
	Token string // 치환된 실제 값. 로그에 남기지 않는다
	App   *App
}

type App struct {
	ClientID       string
	InstallationID int64
	PrivateKey     string // PEM 본문. 로그에 남기지 않는다
}

type ScaleSet struct {
	Name         string
	RunnerGroup  string
	MinRunners   int
	MaxRunners   int
	Unit         resource.Budget // unit 1개 예산. sidecar 면 runner+sidecar 합산 [§8.1]
	Mode         domain.Mode
	RunnerImage  string
	SidecarImage string   // mode=sidecar 일 때 확정(명시값 또는 runtime 별 기본). none 이면 명시값 그대로
	Machines     []string // 연결 머신 이름, YAML 순서
}

type Machine struct {
	Name       string
	ScaleSet   string
	Local      bool               // host 없음 → local executor [§2]
	Runtime    domain.RuntimeKind // machineDefaults.runtime 상속 완료
	SSH        *SSH               // Local 이면 nil, 아니면 machineDefaults 병합 완료
	MaxRunners *int               // nil 이면 physicalMax [R22]
	Resources  *resource.Budget   // nil 이면 runtime info 로 자동 탐지 [R21]
}

// SSH 는 executor.SSHConfig 의 재료다. KeyFile·KnownHostsFile 은 `~` 확장을 마친 경로다. [§10.1]
type SSH struct {
	Host                      string
	Port                      int
	User                      string
	KeyFile                   string
	KeyPassphrase             string // 치환된 실제 값
	KnownHostsFile            string
	Fingerprint               string
	InsecureSkipHostKeyVerify bool
}

// Load 는 파일을 읽어 OS env 로 Parse 한다.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("설정 파일 읽기: %w", err)
	}
	return Parse(data, OSEnv())
}

// Parse 는 YAML 바이트를 Config 로 만든다. 검증 오류는 전부 모아 errors.Join 으로 돌려준다.
func Parse(data []byte, env Env) (*Config, error) {
	p := &parser{env: env}
	raw, err := decodeStrict(data)
	if err != nil {
		return nil, err
	}
	p.substitute(reflect.ValueOf(raw), "", false)
	cfg := p.resolve(raw)
	cfg.Warnings = p.warnings
	if err := p.errs.join(); err != nil {
		return nil, err
	}
	return cfg, nil
}

type parser struct {
	env      Env
	errs     errs
	warnings []string
}

// decodeStrict 는 알 수 없는 키를 오류로 본다. [R2]
func decodeStrict(data []byte) (*rawConfig, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var raw rawConfig
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return &raw, nil // 빈 파일. 필수 필드 누락으로 이어진다
		}
		rule := "decode"
		if isUnknownField(err) {
			rule = "R2"
		}
		return nil, &RuleError{Rule: rule, Msg: err.Error()}
	}
	// 문서는 하나여야 한다. 두 번째 문서를 조용히 버리면 그 안의 미지 키가 strict decode 를 피한다. [R2]
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		msg := "설정 파일에 YAML 문서가 2개 이상 있음 (`---` 구분자)"
		if err != nil {
			msg = err.Error()
		}
		return nil, &RuleError{Rule: "R2", Msg: msg}
	}
	return &raw, nil
}

// isUnknownField 는 yaml.v3 의 KnownFields 위반 메시지("field x not found in type ...")를 가려낸다.
// 라이브러리가 sentinel 을 제공하지 않아 메시지로 판별한다.
func isUnknownField(err error) bool {
	return strings.Contains(err.Error(), "not found in type")
}
