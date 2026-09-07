// Package github 는 actions/scaleset v0.4.0 위의 얇은 래퍼다. 라이브러리가 (nil, nil) 이나
// sentinel 오류로 표현하는 "없음" 을 호출자(controller, cmd)가 쓰기 쉬운 형태로 바꾸고,
// scale set 조회 경로(§7.1-5, §11)를 한 곳에 둔다. [DESIGN §4.4]
package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"gh-ars/internal/config"
)

// RunnerRef 는 GitHub 에 등록된 runner 의 식별자다. [DESIGN §4.4]
type RunnerRef struct {
	ID   int64
	Name string
}

// ErrScaleSetNotFound 는 DeleteScaleSet 이 그룹 안에서 이름을 찾지 못했을 때 돌려준다. [§11]
var ErrScaleSetNotFound = errors.New("scale set not found")

// workFolder 는 JIT 설정의 작업 디렉터리다. 공식 runner 이미지 기준. [DESIGN §4.4]
const workFolder = "/home/runner/_work"

// Client 는 controller 와 cmd 가 쓰는 GitHub 작업 집합이다. [DESIGN §4.4]
type Client interface {
	// EnsureScaleSet: GetRunnerGroupByName → GetRunnerScaleSet(groupID, name) → 없으면
	// CreateRunnerScaleSet. 그룹 이동은 없다. [§7.1-5]
	EnsureScaleSet(ctx context.Context, name, runnerGroup string) (id int, err error)
	// DeleteScaleSet: 같은 조회 경로 → DeleteRunnerScaleSet(id). 없으면 ErrScaleSetNotFound. [§11]
	DeleteScaleSet(ctx context.Context, name, runnerGroup string) error
	// GenerateJIT: 이름과 workFolder 로 JIT 설정을 만든다. encoded 는 로그에 남기지 않는다. [§7.2-3, §7.2-4]
	GenerateJIT(ctx context.Context, scaleSetID int, runnerName string) (encoded string, runner RunnerRef, err error)
	// GetRunner: GetRunnerByName. 라이브러리의 (nil, nil) 은 found=false. [§8.3]
	GetRunner(ctx context.Context, runnerName string) (ref RunnerRef, found bool, err error)
	// RemoveRunner: RunnerNotFoundError 는 nil(ephemeral runner 가 스스로 해제한 경우).
	// 축소(§7.2-3)에서는 거절을 IsBusy 로 해석한다. [§8.3]
	RemoveRunner(ctx context.Context, runnerID int64) error
	// NewSession: *scaleset.MessageSessionClient 를 Session 으로 돌려준다. listener.Client 에는 Close 가 없고
	// Listener.Run 도 세션을 닫지 않으므로(v0.4.0 listener.go) Controller 가 세션을 보유하고 ctx 취소 시,
	// 그리고 listener 재시작 전에 Close 를 호출한다. [§7.1-9, §7.3, DESIGN §4.4]
	NewSession(ctx context.Context, scaleSetID int, owner string) (Session, error)
}

// Session 은 listener 가 요구하는 메시지 클라이언트에 세션 정리를 더한 것이다. [DESIGN §4.4]
type Session interface {
	listener.Client
	Close(ctx context.Context) error
}

// api 는 *scaleset.Client 중 래퍼가 쓰는 메서드다. 테스트는 이 경계에서 대역을 넣는다. [DESIGN §11]
type api interface {
	GetRunnerGroupByName(ctx context.Context, runnerGroup string) (*scaleset.RunnerGroup, error)
	GetRunnerScaleSet(ctx context.Context, runnerGroupID int, name string) (*scaleset.RunnerScaleSet, error)
	CreateRunnerScaleSet(ctx context.Context, s *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	DeleteRunnerScaleSet(ctx context.Context, id int) error
	GenerateJitRunnerConfig(ctx context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunnerByName(ctx context.Context, runnerName string) (*scaleset.RunnerReference, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
	MessageSessionClient(ctx context.Context, scaleSetID int, owner string, options ...scaleset.HTTPOption) (*scaleset.MessageSessionClient, error)
}

type client struct {
	api api
	log *slog.Logger
}

// New 는 인증 방식(PAT / App)에 맞는 scaleset 클라이언트를 만든다. 네트워크 호출은 하지 않는다;
// 인증·scope 확인(§7.1-2)은 첫 호출(EnsureScaleSet 의 그룹 조회)에서 드러난다. [DESIGN §4.4]
func New(cfg config.GitHub, log *slog.Logger) (Client, error) {
	info := scaleset.SystemInfo{System: "gh-ars", Subsystem: "controller"}
	var (
		c   *scaleset.Client
		err error
	)
	switch {
	case cfg.Auth.App != nil:
		c, err = scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: cfg.URL,
			GitHubAppAuth: scaleset.GitHubAppAuth{
				ClientID:       cfg.Auth.App.ClientID,
				InstallationID: cfg.Auth.App.InstallationID,
				PrivateKey:     cfg.Auth.App.PrivateKey,
			},
			SystemInfo: info,
		})
	case cfg.Auth.Token != "":
		c, err = scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     cfg.URL,
			PersonalAccessToken: cfg.Auth.Token,
			SystemInfo:          info,
		})
	default:
		// config R3 가 막지만 방어한다. 오류 문자열에 secret 은 없다.
		return nil, errors.New("github auth: neither token nor app configured")
	}
	if err != nil {
		return nil, fmt.Errorf("github client: %w", err)
	}
	return newClient(c, log), nil
}

func newClient(a api, log *slog.Logger) Client {
	if log == nil {
		log = slog.Default()
	}
	return &client{api: a, log: log}
}

// normalizeGroup 는 R7 의 `Default` 를 클라이언트 상수 "default" 로 맞춘다. 비교는 대소문자 무시.
// 그 밖의 이름은 그대로 넘긴다. [R7, §6.2]
func normalizeGroup(runnerGroup string) string {
	if strings.EqualFold(runnerGroup, config.DefaultRunnerGroup) {
		return scaleset.DefaultRunnerGroup
	}
	return runnerGroup
}

// lookup 은 §7.1-5 와 §11 이 공유하는 조회 경로다: 그룹 → 그룹 안에서 이름. 없으면 (groupID, nil, nil).
func (c *client) lookup(ctx context.Context, name, runnerGroup string) (int, *scaleset.RunnerScaleSet, error) {
	g, err := c.api.GetRunnerGroupByName(ctx, normalizeGroup(runnerGroup))
	if err != nil {
		return 0, nil, fmt.Errorf("runner group %q: %w", runnerGroup, err)
	}
	if g == nil {
		return 0, nil, fmt.Errorf("runner group %q: not found", runnerGroup)
	}
	s, err := c.api.GetRunnerScaleSet(ctx, g.ID, name)
	if err != nil {
		return g.ID, nil, fmt.Errorf("scale set %q in group %q: %w", name, runnerGroup, err)
	}
	return g.ID, s, nil
}

// EnsureScaleSet implements Client. [§7.1-5]
func (c *client) EnsureScaleSet(ctx context.Context, name, runnerGroup string) (int, error) {
	groupID, s, err := c.lookup(ctx, name, runnerGroup)
	if err != nil {
		return 0, err
	}
	if s != nil {
		c.log.Info("scale set found", "scaleSet", name, "id", s.ID, "runnerGroup", runnerGroup)
		return s.ID, nil
	}
	// runs-on 라벨은 scale set 이름 하나뿐이다(§4.2). DisableUpdate: runner 버전은 이미지가
	// 고정하므로(R13) 자동 갱신은 ephemeral 컨테이너에서 의미가 없다. v0.4.0 의 RunnerSetting 에는
	// Ephemeral 필드가 없다(scale set runner 는 항상 ephemeral). [PLAN "남은 결정"]
	created, err := c.api.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
		Name:          name,
		RunnerGroupID: groupID,
		Labels:        []scaleset.Label{{Name: name, Type: "System"}},
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	})
	if err != nil {
		return 0, fmt.Errorf("create scale set %q: %w", name, err)
	}
	c.log.Info("scale set created", "scaleSet", name, "id", created.ID, "runnerGroup", runnerGroup)
	return created.ID, nil
}

// DeleteScaleSet implements Client. [§11]
func (c *client) DeleteScaleSet(ctx context.Context, name, runnerGroup string) error {
	_, s, err := c.lookup(ctx, name, runnerGroup)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("scale set %q in group %q: %w", name, runnerGroup, ErrScaleSetNotFound)
	}
	if err := c.api.DeleteRunnerScaleSet(ctx, s.ID); err != nil {
		return fmt.Errorf("delete scale set %q (id %d): %w", name, s.ID, err)
	}
	c.log.Info("scale set deleted", "scaleSet", name, "id", s.ID)
	return nil
}

// GenerateJIT implements Client. encoded 값은 로그·오류 어디에도 넣지 않는다. [§7.2-3, §7.2-4]
func (c *client) GenerateJIT(ctx context.Context, scaleSetID int, runnerName string) (string, RunnerRef, error) {
	cfg, err := c.api.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{
		Name:       runnerName,
		WorkFolder: workFolder,
	}, scaleSetID)
	if err != nil {
		return "", RunnerRef{}, fmt.Errorf("generate jit config for %q: %w", runnerName, err)
	}
	if cfg == nil || cfg.Runner == nil {
		return "", RunnerRef{}, fmt.Errorf("generate jit config for %q: response has no runner reference", runnerName)
	}
	if cfg.EncodedJITConfig == "" {
		return "", RunnerRef{}, fmt.Errorf("generate jit config for %q: response has empty encoded config", runnerName)
	}
	ref := RunnerRef{ID: int64(cfg.Runner.ID), Name: cfg.Runner.Name}
	c.log.Debug("jit config generated", "runner", ref.Name, "runnerID", ref.ID, "jitLen", len(cfg.EncodedJITConfig))
	return cfg.EncodedJITConfig, ref, nil
}

// GetRunner implements Client. [§8.3]
func (c *client) GetRunner(ctx context.Context, runnerName string) (RunnerRef, bool, error) {
	r, err := c.api.GetRunnerByName(ctx, runnerName)
	if err != nil {
		return RunnerRef{}, false, fmt.Errorf("get runner %q: %w", runnerName, err)
	}
	if r == nil {
		return RunnerRef{}, false, nil
	}
	return RunnerRef{ID: int64(r.ID), Name: r.Name}, true, nil
}

// RemoveRunner implements Client. [§8.3]
func (c *client) RemoveRunner(ctx context.Context, runnerID int64) error {
	err := c.api.RemoveRunner(ctx, runnerID)
	if err == nil {
		return nil
	}
	if errors.Is(err, scaleset.RunnerNotFoundError) {
		c.log.Debug("runner already removed", "runnerID", runnerID)
		return nil
	}
	return fmt.Errorf("remove runner %d: %w", runnerID, err)
}

// NewSession implements Client. [§7.1-9, §7.3]
func (c *client) NewSession(ctx context.Context, scaleSetID int, owner string) (Session, error) {
	s, err := c.api.MessageSessionClient(ctx, scaleSetID, owner)
	if err != nil {
		return nil, fmt.Errorf("message session for scale set %d: %w", scaleSetID, err)
	}
	return s, nil
}

// statusRe 는 라이브러리 newRequestResponseError 가 오류 문자열에 넣는 `status="<code> <text>"` 다.
// 라이브러리는 응답 상태를 타입으로 노출하지 않으므로 문자열에서 읽는다(v0.4.0 errors.go).
var statusRe = regexp.MustCompile(`status="(\d{3})[ "]`)

// IsBusy 는 RemoveRunner 오류가 "job 진행 중" 을 뜻하면 true 다: JobStillRunningError 이거나
// 응답 상태가 4xx(404 제외). RunnerNotFoundError 는 이미 없음이므로 false. [§7.2-3, DESIGN §4.4]
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, scaleset.JobStillRunningError) {
		return true
	}
	if errors.Is(err, scaleset.RunnerNotFoundError) {
		return false
	}
	m := statusRe.FindStringSubmatch(err.Error())
	if m == nil {
		return false
	}
	code, _ := strconv.Atoi(m[1])
	return code >= 400 && code < 500 && code != 404
}
