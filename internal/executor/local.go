package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// streamStderrMax 는 장기 스트림의 stderr 보관 상한이다. events 스트림은 며칠씩
// 살아 있을 수 있어 무제한으로 쌓으면 안 된다. 종료 사유를 로그에 남기는 것이
// 목적이므로 앞부분만 있으면 충분하다.
const streamStderrMax = 8 << 10

// pipeDrainDelay 는 자식이 끝난 뒤 stdout/stderr 파이프가 닫히기를 기다리는
// 상한이다. 파이프는 자식의 fd 를 물려받은 손자가 살아 있는 동안 열려 있으므로,
// 자식이 죽어도 EOF 가 오지 않을 수 있다(`sudo -n podman info` 가 이 모양이다).
// 상한이 없으면 Run 과 스트림이 그 손자의 수명만큼 매달려, 기동 타임아웃
// 2분(§7.2-4)이 지나도 startUnit goroutine 이 풀리지 않고 events
// 재시작(§7.1-8)도 영영 일어나지 않는다.
//
// 타이머는 ctx 취소뿐 아니라 자식의 정상 종료에서도 시작한다(os/exec 계약).
// 손자가 없으면 자식 종료와 함께 파이프가 닫혀 발동하지 않지만, 발동했다면
// 출력이 잘렸을 수 있다. 그래서 Run 은 그것을 값이 아니라 오류로 알린다.
const pipeDrainDelay = 2 * time.Second

// local 은 gh-ars 가 실행 중인 머신에서 SSH 없이 직접 명령을 돌린다.
// `host` 를 주지 않은 machines[] 항목이 이것을 쓴다. [§2, §10.1]
type local struct{}

// NewLocal 은 local Executor 를 만든다. 유지할 연결이 없어 상태가 없다. [DESIGN §4.1]
func NewLocal() Executor { return local{} }

// Run 은 명령을 끝까지 실행한다. 종료 코드는 오류가 아니라 값이다(Executor 참조).
func (local) Run(ctx context.Context, c Cmd) (Result, error) {
	argv, err := resolveArgv(c)
	if err != nil {
		return Result{}, err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = pipeDrainDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	closeStdin, closeStdinWriter, err := setStdin(cmd, c.Stdin)
	if err != nil {
		return Result{}, fmt.Errorf("executor: %s: %w", argv[0], err)
	}
	if err := cmd.Start(); err != nil {
		closeStdin()
		closeStdinWriter()
		return Result{}, fmt.Errorf("executor: %s: %w", argv[0], err)
	}
	closeStdin() // 자식이 fd 복사본을 가졌으니 부모 쪽은 닫는다
	runErr := cmd.Wait()
	closeStdinWriter() // 자식이 다 읽지 않았어도 복사 goroutine 을 푼다
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if runErr == nil {
		return res, nil
	}
	// ctx 취소로 죽은 경우는 명령의 종료 코드가 아니다. 오류로 알린다.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, fmt.Errorf("executor: %s: %w", argv[0], ctxErr)
	}
	// 파이프가 상한 안에 닫히지 않아 강제로 닫혔다. 종료 코드가 0이어도 출력이
	// 잘렸을 수 있으므로 값으로 쓰지 못한다.
	if errors.Is(runErr, exec.ErrWaitDelay) {
		return res, fmt.Errorf("executor: %s: 출력 파이프가 %v 안에 닫히지 않았다(자식이 남아 물고 있다): %w",
			argv[0], pipeDrainDelay, runErr)
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	// 프로세스를 시작하지 못했다(바이너리 없음, 권한 없음 등).
	return res, fmt.Errorf("executor: %s: %w", argv[0], runErr)
}

// Stream 은 명령의 stdout 을 열어 둔 채 돌려준다. 프로세스가 끝나면 Read 가
// io.EOF(정상) 또는 종료 사유를 담은 오류(비정상)를 내고, 호출자는 그것을
// 스트림 종료 통지로 보고 백오프 재시작한다. [§7.1-8]
//
// 회수는 아래 goroutine 이 무조건 하므로 ctx 취소만으로도 좀비는 남지 않는다.
// 그래도 호출자는 다 쓰면 Close 한다: ctx 와 무관하게 스트림을 끝내고, 파이프와
// goroutine 정리가 끝날 때까지 기다리는 유일한 방법이다.
func (local) Stream(ctx context.Context, c Cmd) (io.ReadCloser, error) {
	argv, err := resolveArgv(c)
	if err != nil {
		return nil, err
	}
	// Close 로 프로세스를 확실히 죽이기 위해 ctx 를 파생한다.
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = pipeDrainDelay
	stderr := &capBuffer{}
	cmd.Stderr = stderr

	// StdoutPipe 를 쓰지 않는 이유: 그 경우 파이프를 닫는 주체가 Wait 이라
	// "다 읽은 뒤에 Wait" 순서를 지켜야 하는데, 손자가 stdout 을 물고 있으면
	// EOF 가 오지 않아 Read 가 영영 끝나지 않고 Wait 도 시작되지 못한다.
	// 그러면 WaitDelay 가 발동할 기회 자체가 없다. io.Pipe 를 주면 exec 가
	// 복사 goroutine 을 세우고, Wait 을 곧바로 시작해 둘 수 있어 상한이 산다.
	outR, outW := io.Pipe()
	cmd.Stdout = outW
	closeStdin, closeStdinWriter, err := setStdin(cmd, c.Stdin)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("executor: %s: %w", argv[0], err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		closeStdin()
		closeStdinWriter()
		_ = outW.Close()
		_ = outR.Close()
		return nil, fmt.Errorf("executor: %s: %w", argv[0], err)
	}
	closeStdin() // 자식이 fd 복사본을 가졌으니 부모 쪽은 닫는다
	s := &stream{argv: argv, pr: outR, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		// Read 와 나란히 돈다. 자식이 끝나면 exec 가 복사 goroutine 을 기다리고,
		// 손자가 파이프를 물고 있으면 WaitDelay 뒤 강제로 닫아 여기를 풀어준다.
		// 그 결과를 파이프에 실어 보내 Read 가 종료를 알아채게 한다.
		err := cmd.Wait()
		closeStdinWriter() // stdin 복사 goroutine 을 푼다
		_ = outW.CloseWithError(exitError(argv, err, stderr))
	}()
	return s, nil
}

// Close 는 local 에서 할 일이 없다. 유지하는 연결이 없고, Stream 이 띄운
// 프로세스는 각 ReadCloser 가 소유한다.
func (local) Close() error { return nil }

// setStdin 은 stdin 복사를 exec 가 아니라 우리가 소유하게 만든다. 반환한 함수는
// Start 뒤에 불러 부모 쪽 읽기 끝을 닫는다.
//
// c.Stdin 을 exec 에 그대로 주면 exec 가 복사 goroutine 을 세우고 Wait 이 그것을
// 기다리는데, 그 goroutine 이 Read 에서 막혀 있으면 WaitDelay 가 목적지 파이프를
// 닫아도 풀리지 않아 Run 과 스트림 Close 가 상한을 넘겨 매달린다. *os.File 을 주면
// exec 는 fd 를 그대로 넘기고 복사를 하지 않으므로, 막히는 쪽은 우리 goroutine 뿐이고
// Run/Close 는 상한을 지킨다.
//
// closeParent 는 Start 뒤에 불러 부모 쪽 읽기 끝을 닫고, closeWriter 는 명령이
// 끝난 뒤에 불러 쓰기 끝을 닫는다. 쓰기 끝을 닫아야 자식이 stdin 을 다 읽지
// 않고 끝났을 때(또는 손자가 물고만 있을 때) 파이프 버퍼에서 막힌 복사
// goroutine 이 풀린다. 그러지 않으면 goroutine 과 fd 가 그대로 남는다.
//
// 호출자는 즉시 반환하는 Reader 를 준다. §7.2-4 의 stdin 은 메모리에서 만든
// tar 스트림이라 이 조건을 만족한다.
func setStdin(cmd *exec.Cmd, r io.Reader) (closeParent, closeWriter func(), err error) {
	noop := func() {}
	if r == nil {
		return noop, noop, nil
	}
	if f, ok := r.(*os.File); ok {
		cmd.Stdin = f // 이미 fd 다. exec 가 복사 goroutine 을 만들지 않는다
		return noop, noop, nil
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdin = pr
	go func() {
		_, _ = io.Copy(pw, r)
		_ = pw.Close() // 다 보냈다. 자식에게 EOF
	}()
	return func() { _ = pr.Close() }, func() { _ = pw.Close() }, nil
}

// exitError 는 프로세스 종료 사유를 스트림 독자에게 줄 오류로 바꾼다.
// 정상 종료면 nil 이고, 그때 Read 는 io.EOF 를 받는다.
func exitError(argv []string, err error, stderr *capBuffer) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &cmdError{argv: argv, exitCode: exitErr.ExitCode(), stderr: stderr.String()}
	}
	// 종료 코드가 없는 실패(파이프 드레인 만료 등)도 argv 와 stderr 를 함께 남긴다.
	if s := strings.TrimSpace(stderr.String()); s != "" {
		return fmt.Errorf("executor: %s: %w: %s", strings.Join(argv, " "), err, s)
	}
	return fmt.Errorf("executor: %s: %w", strings.Join(argv, " "), err)
}

// stream 은 장기 스트림의 읽기 끝이다. 프로세스 수명을 함께 소유한다.
type stream struct {
	argv   []string
	pr     *io.PipeReader
	cancel context.CancelFunc
	done   chan struct{} // 프로세스 회수 완료
}

// Read 는 스트림이 끝나면 io.EOF(정상) 또는 종료 사유(비정상)를 낸다.
// 종료 통지는 Wait 을 도는 goroutine 이 파이프에 실어 보낸다.
func (s *stream) Read(p []byte) (int, error) { return s.pr.Read(p) }

// Close 는 프로세스를 죽이고 회수한다. 우리가 죽인 것이므로 그 종료 사유는
// 오류가 아니다. 여러 번 불러도 안전하다.
func (s *stream) Close() error {
	s.cancel()
	// 복사 goroutine 이 쓰기에서 막혀 있으면 풀어준다.
	_ = s.pr.Close()
	<-s.done
	return nil
}

// capBuffer 는 상한까지만 모으는 쓰기 버퍼다. exec 가 별도 goroutine 에서 쓰고
// 종료 사유를 만들 때 다른 goroutine 에서 읽으므로 잠근다.
type capBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := streamStderrMax - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
