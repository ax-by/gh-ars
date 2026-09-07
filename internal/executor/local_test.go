package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// helperFlag 는 테스트 바이너리를 자식 프로세스로 재실행할 때의 표식이다.
// `sh -c` / `cmd /c` 같은 OS 별 셸 대신 자기 자신을 실행해, 실제 프로세스를
// 쓰는 테스트가 개발 머신(Windows/Mac)과 실행 대상(Linux)에서 똑같이 돈다.
const helperFlag = "-gh-ars-exec-helper"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == helperFlag {
		os.Exit(helperMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// helperMain 은 자식 프로세스 모드의 동작이다. TestMain 에서 분기하므로
// 테스트 프레임워크의 출력이 stdout 에 섞이지 않는다.
func helperMain(args []string) int {
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "both": // stdout 과 stderr 를 분리해서 낸다
		fmt.Fprint(os.Stdout, "out")
		fmt.Fprint(os.Stderr, "err")
		return 0
	case "cat": // stdin 을 stdout 으로 흘린다 (`docker cp -` 의 tar 스트림 대역)
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			return 1
		}
		return 0
	case "fail": // fail <code> <stderr>
		code, err := strconv.Atoi(args[1])
		if err != nil {
			return 2
		}
		fmt.Fprint(os.Stderr, args[2])
		return code
	case "lines": // lines <n>: n 줄을 내고 정상 종료 (events 스트림 대역)
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return 2
		}
		for i := 0; i < n; i++ {
			fmt.Printf("line%d\n", i)
		}
		return 0
	case "sleep": // sleep <duration>: 죽이기 전까지 안 끝나는 명령
		d, err := time.ParseDuration(args[1])
		if err != nil {
			return 2
		}
		time.Sleep(d)
		return 0
	case "spawn-stdin": // spawn-stdin <duration> <readyFile>
		// stdin 만 물려받은 손자를 남기고 즉시 정상 종료한다. 손자가 읽기 끝을
		// 쥐고 있으면 부모 쪽 쓰기가 EPIPE 로 풀리지 않아, stdin 복사 goroutine 이
		// 파이프 버퍼가 찬 지점에서 막힌다. stdout/stderr 는 물려주지 않아
		// 출력 파이프 상한과 섞이지 않는다.
		if len(args) < 3 {
			return 2
		}
		exe, err := os.Executable()
		if err != nil {
			return 2
		}
		grandchild := exec.Command(exe, helperFlag, "sleep", args[1])
		grandchild.Stdin = os.Stdin
		if err := grandchild.Start(); err != nil {
			return 2
		}
		if err := os.WriteFile(args[2], []byte("ready"), 0o600); err != nil {
			return 2
		}
		return 0
	case "spawn", "spawn-exit": // spawn[-exit] <duration> <readyFile>
		// stdout/stderr 를 물려받은 손자를 남긴다. `sudo -n podman info` 처럼
		// 자식이 또 자식을 만드는 경우의 재현이다. ctx 취소는 직계 자식만
		// 죽이므로 손자가 파이프를 계속 물고 있다.
		// spawn 은 자신도 오래 살고(취소 경로), spawn-exit 은 즉시 정상 종료한다
		// (취소 없이 파이프만 열려 있는 경로).
		if len(args) < 3 {
			return 2
		}
		d, err := time.ParseDuration(args[1])
		if err != nil {
			return 2
		}
		exe, err := os.Executable()
		if err != nil {
			return 2
		}
		grandchild := exec.Command(exe, helperFlag, "sleep", args[1])
		grandchild.Stdout = os.Stdout
		grandchild.Stderr = os.Stderr
		if err := grandchild.Start(); err != nil {
			return 2
		}
		// 손자가 파이프를 물었음을 알린다. 테스트는 이 신호를 보고 나서
		// 취소하거나 닫아야 실제 시나리오를 검증한다.
		if err := os.WriteFile(args[2], []byte("ready"), 0o600); err != nil {
			return 2
		}
		if args[0] == "spawn-exit" {
			return 0
		}
		fmt.Println("spawned")
		time.Sleep(d)
		return 0
	}
	return 2
}

func helperArgv(t *testing.T, args ...string) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return append([]string{exe, helperFlag}, args...)
}

// waitReady 는 helper 가 손자를 띄우고 남긴 신호 파일을 기다린다. 테스트
// goroutine 밖에서도 부르므로 t.Fatal 대신 결과를 돌려준다(FailNow 는 부른
// goroutine 만 끝내서, 실패가 엉뚱한 타임아웃으로 보고된다).
func waitReady(path string) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// [§5] 명령 실행: stdout 과 stderr 를 분리해 받는다.
func TestLocalRun_S5_StdoutStderr(t *testing.T) {
	res, err := NewLocal().Run(t.Context(), Cmd{Argv: helperArgv(t, "both")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := string(res.Stdout); got != "out" {
		t.Fatalf("Stdout = %q, want %q", got, "out")
	}
	if got := string(res.Stderr); got != "err" {
		t.Fatalf("Stderr = %q, want %q", got, "err")
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// [DESIGN §4.1] 비0 종료는 오류가 아니라 값이다. 이 계약을 쓰는 호출자가
// preflight 의 podman 경로 고정(§10.2 규칙 3)과 정리 단계의 "이미 없음"(§8.3)이다.
func TestLocalRun_S5_NonZeroExitIsValue(t *testing.T) {
	res, err := NewLocal().Run(t.Context(), Cmd{Argv: helperArgv(t, "fail", "3", "boom")})
	if err != nil {
		t.Fatalf("Run err = %v, want nil (종료 코드는 값이다)", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", res.ExitCode)
	}
	if got := string(res.Stderr); got != "boom" {
		t.Fatalf("Stderr = %q, want %q", got, "boom")
	}
}

// [§7.2-4] stdin 전달. JIT config 는 tar 스트림으로만 넘어가므로 이 경로가
// 끊기면 runner 가 등록되지 못한다.
func TestLocalRun_S7_2_4_Stdin(t *testing.T) {
	payload := strings.Repeat("jit-tar-stream\n", 1000)
	res, err := NewLocal().Run(t.Context(), Cmd{
		Argv:  helperArgv(t, "cat"),
		Stdin: strings.NewReader(payload),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := string(res.Stdout); got != payload {
		t.Fatalf("stdin 이 그대로 전달되지 않았다: %d 바이트 받음, %d 기대", len(got), len(payload))
	}
}

// [§5] 프로세스를 시작하지 못한 것은 종료 코드가 아니라 오류다.
func TestLocalRun_S5_StartFailure(t *testing.T) {
	_, err := NewLocal().Run(t.Context(), Cmd{Argv: []string{"gh-ars-no-such-binary-xyz"}})
	if err == nil {
		t.Fatal("Run err = nil, want 오류 (바이너리 없음)")
	}
	if !strings.Contains(err.Error(), "gh-ars-no-such-binary-xyz") {
		t.Fatalf("오류 메시지에 argv[0] 이 없다: %v", err)
	}
}

// [§5] ctx 취소로 죽은 것도 명령의 종료 코드가 아니라 오류다. 기동 타임아웃
// 2분(§7.2-4)과 SSH 접속 타임아웃 10s(§10.1)가 이 경로를 탄다.
func TestLocalRun_S5_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := NewLocal().Run(ctx, Cmd{Argv: helperArgv(t, "sleep", "30s")})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
}

// [§7.2-4] ctx 취소는 직계 자식만 죽인다. 손자가 stdout 파이프를 물고 있으면
// 그것만으로 Run 이 데드라인을 넘겨 매달린다(`sudo -n podman` 이 이 모양이다).
// 기동 타임아웃 2분이 실제로 goroutine 을 풀어주려면 파이프 대기에 상한이 있어야 한다.
func TestLocalRun_S7_2_4_CancelWithOrphanHoldingPipe(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancel(t.Context())
	// 손자가 실제로 파이프를 물기 전에 취소하면 아무것도 검증하지 못한다.
	readyOK := make(chan bool, 1)
	go func() {
		readyOK <- waitReady(ready)
		cancel()
	}()
	done := make(chan error, 1)
	go func() {
		_, err := NewLocal().Run(ctx, Cmd{Argv: helperArgv(t, "spawn", "30s", ready)})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ctx 취소 후에도 Run 이 반환하지 않았다 (손자가 파이프를 물고 있다)")
	}
	if !<-readyOK {
		t.Fatal("손자가 파이프를 물기 전에 취소됐다 — 시나리오를 검증하지 못했다")
	}
}

// [DESIGN §4.1] 취소가 없어도 상한은 돈다(os/exec 계약: 타이머는 자식의 정상
// 종료에서도 시작한다). 그때 출력이 잘렸을 수 있으므로 종료 코드 0을 성공 값으로
// 돌려주면 안 된다 — `docker ps` 결과가 조용히 잘리면 §8.3 판정이 틀어진다.
func TestLocalRun_S5_NormalExitWithOrphanHoldingPipe(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	done := make(chan error, 1)
	go func() {
		_, err := NewLocal().Run(t.Context(), Cmd{Argv: helperArgv(t, "spawn-exit", "30s", ready)})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, exec.ErrWaitDelay) {
			t.Fatalf("Run err = %v, want exec.ErrWaitDelay (출력이 잘렸을 수 있다)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("정상 종료했는데도 Run 이 반환하지 않았다 (손자가 파이프를 물고 있다)")
	}
}

// [§7.1-8] 스트림도 같다. 재접속마다 Close 가 손자에 막히면 백오프 재시작이 멈춘다.
func TestLocalStream_S7_1_8_CloseWithOrphanHoldingPipe(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	rc, err := NewLocal().Stream(t.Context(), Cmd{Argv: helperArgv(t, "spawn", "30s", ready)})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !waitReady(ready) {
		t.Fatal("손자 기동 신호를 기다리다 시간이 다 됐다")
	}
	done := make(chan error, 1)
	go func() { done <- rc.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close 가 반환하지 않았다 (손자가 파이프를 물고 있다)")
	}
}

// [§5] 빈 명령은 실행하지 않는다. Run 과 Stream 둘 다.
func TestLocal_S5_EmptyArgv(t *testing.T) {
	ex := NewLocal()
	if _, err := ex.Run(t.Context(), Cmd{}); !errors.Is(err, errEmptyArgv) {
		t.Fatalf("Run err = %v, want errEmptyArgv", err)
	}
	if _, err := ex.Stream(t.Context(), Cmd{}); !errors.Is(err, errEmptyArgv) {
		t.Fatalf("Stream err = %v, want errEmptyArgv", err)
	}
}

// [§7.1-8] 정상 종료한 스트림은 내용을 다 흘린 뒤 EOF 로 끝난다.
// 호출자는 이것을 스트림 종료 통지로 보고 백오프 재시작한다.
func TestLocalStream_S7_1_8_EOFOnNormalExit(t *testing.T) {
	rc, err := NewLocal().Stream(t.Context(), Cmd{Argv: helperArgv(t, "lines", "3")})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll = %v, want nil (정상 종료는 EOF)", err)
	}
	if want := "line0\nline1\nline2\n"; string(out) != want {
		t.Fatalf("스트림 내용 = %q, want %q", out, want)
	}
	// EOF 까지 읽어 파이프가 이미 닫힌 뒤의 Close 도 오류가 아니다.
	if err := rc.Close(); err != nil {
		t.Fatalf("EOF 이후 Close = %v, want nil", err)
	}
}

// [§7.1-8] 비정상 종료는 EOF 대신 사유를 알린다. 실패한 명령의 argv 와 stderr 를
// 함께 남길 수 있어야 한다.
func TestLocalStream_S7_1_8_ErrorOnAbnormalExit(t *testing.T) {
	rc, err := NewLocal().Stream(t.Context(), Cmd{Argv: helperArgv(t, "fail", "7", "events failed")})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("ReadAll err = nil, want 종료 사유")
	} else {
		for _, want := range []string{"7", "events failed", helperFlag} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("오류 메시지 = %q, %q 가 없다", err, want)
			}
		}
	}
}

// [§7.1-8] Close 는 끝나지 않는 스트림의 프로세스를 죽이고 회수한다.
// 재접속 때마다 events 프로세스가 쌓이면 안 된다.
func TestLocalStream_S7_1_8_CloseKillsProcess(t *testing.T) {
	rc, err := NewLocal().Stream(t.Context(), Cmd{Argv: helperArgv(t, "sleep", "60s")})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- rc.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close = %v, want nil (우리가 죽인 것은 오류가 아니다)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close 가 프로세스 종료를 기다리며 걸렸다")
	}
	// 두 번째 Close 도 안전해야 한다(프로세스 회수는 한 번만).
	if err := rc.Close(); err != nil {
		t.Fatalf("2회차 Close = %v, want nil", err)
	}
}

// [DESIGN §4.1] local 은 유지하는 연결이 없다.
func TestLocalClose(t *testing.T) {
	if err := NewLocal().Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
}

// capBuffer 는 상한을 넘겨 받아도 상한까지만 모은다. events 스트림은 며칠씩
// 살아 있을 수 있어 stderr 를 무제한으로 쌓으면 안 된다.
func TestCapBuffer_Limit(t *testing.T) {
	var b capBuffer
	chunk := strings.Repeat("x", 1024)
	total := 0
	for i := 0; i < 20; i++ {
		n, err := b.Write([]byte(chunk))
		if err != nil || n != len(chunk) {
			t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(chunk))
		}
		total += n
	}
	if total <= streamStderrMax {
		t.Fatalf("테스트가 상한을 넘기지 못했다: %d <= %d", total, streamStderrMax)
	}
	if got := len(b.String()); got != streamStderrMax {
		t.Fatalf("보관 길이 = %d, want %d", got, streamStderrMax)
	}
}

// [§7.1-8] 자식이 정상 종료해도 손자가 stdout 을 물고 있으면 EOF 가 오지 않는다.
// 그때 Read 가 무한정 막히면 스트림 종료를 감지하지 못해 백오프 재시작이 영영
// 일어나지 않고, 그 머신의 events 가 멈춘 채로 남는다.
func TestLocalStream_S7_1_8_NormalExitWithOrphanHoldingStdout(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	rc, err := NewLocal().Stream(t.Context(), Cmd{Argv: helperArgv(t, "spawn-exit", "30s", ready)})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer rc.Close()
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(rc)
		done <- err
	}()
	select {
	case err := <-done:
		// 종료 통지가 돌아오기만 하면 호출자는 재시작할 수 있다. 다만 종료 코드가
		// 없는 실패(드레인 만료)도 어느 명령이 끊겼는지 로그로 남아야 한다.
		if err != nil && !strings.Contains(err.Error(), helperFlag) {
			t.Fatalf("종료 사유에 argv 가 없다: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("자식이 끝났는데 Read 가 반환하지 않았다 (손자가 stdout 을 물고 있다)")
	}
}

// stalledReader 는 영영 반환하지 않는 stdin 이다.
type stalledReader struct{ ch chan struct{} }

func (r stalledReader) Read(p []byte) (int, error) { <-r.ch; return 0, io.EOF }

// [DESIGN §4.1] stdin 이 막혀 있어도 Run 과 스트림 Close 는 상한을 지켜야 한다.
// exec 에 Reader 를 그대로 넘기면 Wait 이 그 복사 goroutine 을 기다리는데, 그
// goroutine 은 Read 에 막혀 있어 WaitDelay 로도 풀리지 않는다. 그러면 파이프
// 상한이 stdin 경로에서만 조용히 무효가 된다.
func TestLocal_S5_StalledStdinDoesNotBlock(t *testing.T) {
	t.Run("Run/ctx 취소", func(t *testing.T) {
		block := make(chan struct{})
		defer close(block)
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
		done := make(chan error, 1)
		go func() {
			_, err := NewLocal().Run(ctx, Cmd{Argv: helperArgv(t, "sleep", "30s"), Stdin: stalledReader{block}})
			done <- err
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("ctx 취소 후에도 Run 이 반환하지 않았다 (stdin 이 막혀 있다)")
		}
	})

	t.Run("Run/정상 종료", func(t *testing.T) {
		block := make(chan struct{})
		defer close(block)
		done := make(chan error, 1)
		go func() {
			// stdin 을 읽지 않고 끝나는 명령이다.
			_, err := NewLocal().Run(t.Context(), Cmd{Argv: helperArgv(t, "both"), Stdin: stalledReader{block}})
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run = %v, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("명령이 끝났는데 Run 이 반환하지 않았다 (stdin 이 막혀 있다)")
		}
	})

	t.Run("Stream/Close", func(t *testing.T) {
		block := make(chan struct{})
		defer close(block)
		rc, err := NewLocal().Stream(t.Context(), Cmd{Argv: helperArgv(t, "sleep", "30s"), Stdin: stalledReader{block}})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- rc.Close() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Close = %v, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Close 가 반환하지 않았다 (stdin 이 막혀 있다)")
		}
	})
}

// [DESIGN §4.1] 자식이 stdin 을 다 읽지 않고 끝나면 파이프 버퍼(보통 64KB)를
// 넘긴 나머지가 복사 goroutine 을 쓰기에서 막는다. 명령이 끝난 뒤 쓰기 끝을
// 닫지 않으면 그 goroutine 과 fd 가 그대로 남는다.
func TestLocal_S5_UnreadStdinDoesNotLeak(t *testing.T) {
	// 파이프 버퍼보다 확실히 큰 입력. 자식은 stdin 을 읽지 않고 끝나지만,
	// stdin 을 물려받은 손자가 읽기 끝을 쥐고 있어 쓰기가 EPIPE 로 풀리지 않는다.
	big := bytes.Repeat([]byte("x"), 1<<20)
	before := runtime.NumGoroutine()
	for i := 0; i < 3; i++ {
		ready := filepath.Join(t.TempDir(), "ready")
		if _, err := NewLocal().Run(t.Context(), Cmd{
			Argv:  helperArgv(t, "spawn-stdin", "10s", ready),
			Stdin: bytes.NewReader(big),
		}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !waitReady(ready) {
			t.Fatal("손자 기동 신호를 기다리다 시간이 다 됐다")
		}
	}
	// 복사 goroutine 이 풀릴 시간을 준다. 손자(10s)보다 먼저 끝나야 의미가 있다.
	deadline := time.Now().Add(5 * time.Second)
	last := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		last = runtime.NumGoroutine()
		if last <= before+1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine 이 남았다: 시작 %d, 마지막 관측 %d (stdin 복사가 안 풀렸다)", before, last)
}
