package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"gh-ars/internal/domain"
	"gh-ars/internal/executor"
)

// ExitError 는 CLI 가 비0 으로 끝났음을 argv 와 stderr 와 함께 알린다.
// argv 에 secret 은 없다 — JIT 는 stdin 으로만 간다. [§7.2-4]
type ExitError struct {
	Argv     []string
	ExitCode int
	Stderr   string
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("runtime: %s: 종료 코드 %d", strings.Join(e.Argv, " "), e.ExitCode)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += ": " + s
	}
	return msg
}

// flavor 는 docker 와 podman 이 갈리는 지점이다: 출력 파싱과 "이미 없음" 판별. [DESIGN §4.2]
type flavor interface {
	// psFormat 은 `ps --format` 템플릿이다. 라벨은 gh-ars.* 키를 하나씩 뽑는다(docker.go 참조).
	psFormat() string
	parseContainer(line []byte) (Container, error)
	// parseEvent 는 관심 없는 줄(다른 type, 깨진 JSON)에 ok=false 를 돌려준다.
	parseEvent(line []byte) (ev Event, ok bool)
	parseInfo(out []byte) (Info, error)
	isNotFound(stderr string) bool
}

// cli 는 docker/podman 공통 골격이다. argv 는 두 CLI 가 같고 파싱만 flavor 로 위임한다.
type cli struct {
	kind domain.RuntimeKind
	bin  string
	sudo bool // §10.2 판단 규칙 결과. 모든 명령에 그대로 전파한다
	ex   executor.Executor
	f    flavor
}

// jsonFormat 은 events/info 출력 형식이다. ps 는 flavor.psFormat 을 쓴다. [DESIGN §4.2]
const jsonFormat = "{{json .}}"

func (c *cli) Kind() domain.RuntimeKind { return c.kind }

func (c *cli) cmd(stdin io.Reader, args ...string) executor.Cmd {
	argv := make([]string, 0, 1+len(args))
	argv = append(argv, c.bin)
	argv = append(argv, args...)
	return executor.Cmd{Argv: argv, Stdin: stdin, Sudo: c.sudo}
}

// run 은 명령을 실행하고 비0 종료를 ExitError 로 바꾼다. Executor 가 값으로 돌려주는
// 종료 코드를 여기서 오류로 올리는 이유는, runtime 호출자 중 종료 코드로 분기하는 곳이
// "이미 없음"(§8.3) 하나뿐이고 그것은 Remove/VolumeRemove 가 흡수하기 때문이다.
func (c *cli) run(ctx context.Context, stdin io.Reader, args ...string) (executor.Result, error) {
	cmd := c.cmd(stdin, args...)
	res, err := c.ex.Run(ctx, cmd)
	if err != nil {
		return res, fmt.Errorf("runtime: %s: %w", strings.Join(cmd.Argv, " "), err)
	}
	if res.ExitCode != 0 {
		return res, &ExitError{Argv: cmd.Argv, ExitCode: res.ExitCode, Stderr: string(res.Stderr)}
	}
	return res, nil
}

func (c *cli) Info(ctx context.Context) (Info, error) {
	res, err := c.run(ctx, nil, "info", "--format", jsonFormat)
	if err != nil {
		return Info{}, err
	}
	return c.f.parseInfo(res.Stdout)
}

func (c *cli) Pull(ctx context.Context, image string) error {
	_, err := c.run(ctx, nil, "pull", image)
	return err
}

// List 는 항상 `ps -a` 다. 종료된 컨테이너도 §8.3 판정 대상이다.
func (c *cli) List(ctx context.Context, labelFilter string) ([]Container, error) {
	res, err := c.run(ctx, nil, "ps", "-a", "--filter", "label="+labelFilter, "--format", c.f.psFormat())
	if err != nil {
		return nil, err
	}
	var out []Container
	for _, line := range splitLines(res.Stdout) {
		ctr, err := c.f.parseContainer(line)
		if err != nil {
			return nil, fmt.Errorf("runtime: %s ps 출력 파싱: %w", c.bin, err)
		}
		out = append(out, ctr)
	}
	return out, nil
}

// Create 는 CreateSpec 을 argv 로 편다. 라벨·env 는 키 정렬로 결정적 순서를 만든다. [§7.2-4, §9.1]
func (c *cli) Create(ctx context.Context, spec CreateSpec) error {
	args, err := createArgs(spec)
	if err != nil {
		return err
	}
	_, err = c.run(ctx, nil, args...)
	return err
}

func createArgs(spec CreateSpec) ([]string, error) {
	if spec.Name == "" || spec.Image == "" {
		return nil, errors.New("runtime: CreateSpec 에 Name/Image 가 없다")
	}
	if len(spec.Entrypoint) != 1 {
		return nil, fmt.Errorf("runtime: Entrypoint 는 실행 파일 하나여야 한다: %q", spec.Entrypoint)
	}
	// [§7.2-4] JIT 값은 컨테이너 env 에 절대 넣지 않는다. 래퍼가 파일에서 읽어 export 한다.
	if _, bad := spec.Env[jitEnvVar]; bad {
		return nil, fmt.Errorf("runtime: Env 에 %s 를 넣을 수 없다", jitEnvVar)
	}
	args := []string{"create", "--name", spec.Name}
	for _, k := range sortedKeys(spec.Labels) {
		args = append(args, "--label", k+"="+spec.Labels[k])
	}
	// [§9.3 표] none 모드 예산. 0 이면 미지정(sidecar 모드는 slice 가 예산을 쥔다).
	if spec.CPUs > 0 {
		args = append(args, "--cpus="+strconv.FormatFloat(spec.CPUs, 'f', -1, 64))
	}
	if spec.MemoryBytes > 0 {
		args = append(args, "--memory="+strconv.FormatInt(spec.MemoryBytes, 10))
	}
	if spec.CgroupParent != "" {
		args = append(args, "--cgroup-parent="+spec.CgroupParent)
	}
	if spec.Privileged {
		args = append(args, "--privileged")
	}
	for _, m := range spec.Volumes {
		args = append(args, "-v", m.Volume+":"+m.ContainerPath)
	}
	for _, k := range sortedKeys(spec.Env) {
		args = append(args, "-e", k+"="+spec.Env[k])
	}
	args = append(args, "--entrypoint", spec.Entrypoint[0], spec.Image)
	return append(args, spec.Cmd...), nil
}

// CopyIn 은 tar 스트림을 stdin 으로 넘긴다. 값은 argv 에 나타나지 않는다. [§7.2-4]
func (c *cli) CopyIn(ctx context.Context, container string, tar io.Reader, destDir string) error {
	if tar == nil {
		return errors.New("runtime: CopyIn 에 tar 스트림이 없다")
	}
	_, err := c.run(ctx, tar, "cp", "-", container+":"+destDir)
	return err
}

func (c *cli) Start(ctx context.Context, container string) error {
	_, err := c.run(ctx, nil, "start", container)
	return err
}

// Remove 는 이미 없는 컨테이너를 성공으로 본다. 정리는 매 tick 재시도되므로 멱등이어야 한다. [§8.3]
func (c *cli) Remove(ctx context.Context, container string, force bool) error {
	args := []string{"rm"}
	if force {
		args = append(args, "-f")
	}
	_, err := c.run(ctx, nil, append(args, container)...)
	return c.ignoreNotFound(err)
}

func (c *cli) VolumeCreate(ctx context.Context, name string, labels map[string]string) error {
	args := []string{"volume", "create"}
	for _, k := range sortedKeys(labels) {
		args = append(args, "--label", k+"="+labels[k])
	}
	_, err := c.run(ctx, nil, append(args, name)...)
	return err
}

func (c *cli) VolumeList(ctx context.Context, labelFilter string) ([]string, error) {
	res, err := c.run(ctx, nil, "volume", "ls", "--filter", "label="+labelFilter, "--format", "{{.Name}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range splitLines(res.Stdout) {
		names = append(names, string(line))
	}
	return names, nil
}

// VolumeRemove 는 이미 없는 볼륨을 성공으로 본다. [§8.3]
func (c *cli) VolumeRemove(ctx context.Context, name string) error {
	_, err := c.run(ctx, nil, "volume", "rm", name)
	return c.ignoreNotFound(err)
}

func (c *cli) ignoreNotFound(err error) error {
	var ee *ExitError
	if errors.As(err, &ee) && c.f.isNotFound(ee.Stderr) {
		return nil
	}
	return err
}

// eventsLineMax 는 events 한 줄 상한이다. 라벨 Attributes 가 커도 이 안에 든다.
const eventsLineMax = 1 << 20

// Events 는 events 스트림을 열고 줄 단위로 정규화한다. 스트림이 끝나면(EOF·단절·취소)
// error 채널로 한 번 통지하고 두 채널을 닫는다. 호출자는 백오프로 재시작한다. [§7.1-8]
// 깨진 줄은 건너뛴다 — 한 줄 때문에 스트림을 끊으면 재시작 사이에 die 를 놓친다.
func (c *cli) Events(ctx context.Context, labelFilter string) (<-chan Event, <-chan error) {
	evCh := make(chan Event)
	errCh := make(chan error, 1)
	cmd := c.cmd(nil, "events", "--filter", "label="+labelFilter, "--filter", "type=container", "--format", jsonFormat)
	rc, err := c.ex.Stream(ctx, cmd)
	if err != nil {
		errCh <- fmt.Errorf("runtime: %s: %w", strings.Join(cmd.Argv, " "), err)
		close(errCh)
		close(evCh)
		return evCh, errCh
	}
	go func() {
		defer close(errCh)
		defer close(evCh)
		defer rc.Close()
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 0, 64*1024), eventsLineMax)
		for sc.Scan() {
			ev, ok := c.f.parseEvent(sc.Bytes())
			if !ok {
				continue
			}
			select {
			case evCh <- ev:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}
		err := sc.Err()
		if err == nil {
			err = io.EOF
		}
		errCh <- fmt.Errorf("runtime: %s 스트림 종료: %w", strings.Join(cmd.Argv, " "), err)
	}()
	return evCh, errCh
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// splitLines 는 빈 줄을 버린 줄 목록이다. 한 줄에 항목 하나. 탭 구분 ps 줄의 끝 빈 필드를
// 지키려고 줄 끝의 CR 만 떼고 공백은 자르지 않는다.
func splitLines(b []byte) [][]byte {
	var out [][]byte
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) != "" {
			out = append(out, []byte(line))
		}
	}
	return out
}
