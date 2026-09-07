// Package executor 는 머신에서 명령을 실행하는 통로다. local 은 gh-ars 가 도는
// 머신에서 직접 실행하고, ssh 는 원격에서 실행한다. 요구사항은 셋이다:
// 명령 실행, stdin 파이프 전달(§7.2-4 의 `docker cp -` tar 스트림), 장기 스트림(events). [§5, §10]
package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// sudo 접두. -n 은 비대화형이라 암호가 필요하면 프롬프트 없이 실패한다.
// gh-ars 는 root 가 아닐 때 root 가 필요한 명령에만 이것을 붙인다. [§10.2]
const (
	sudoBin  = "sudo"
	sudoFlag = "-n"
)

// Cmd 는 실행할 명령 하나다. [DESIGN §4.1]
type Cmd struct {
	Argv []string
	// Stdin 은 nil 이면 없다. `docker cp -` 의 tar 스트림 용도. [§7.2-4]
	Stdin io.Reader
	// Sudo 는 true 면 `sudo -n` 접두를 붙인다. 붙일지 말지는 preflight 가
	// 판단하고(§10.2 판단 규칙), Executor 는 이 플래그를 그대로 따른다.
	Sudo bool
}

// Result 는 명령이 종료 코드를 남기고 끝났을 때의 결과다. [DESIGN §4.1]
type Result struct {
	Stdout, Stderr []byte
	ExitCode       int
}

// Executor 는 한 머신에 대한 명령 실행 통로다. [§5, DESIGN §4.1]
type Executor interface {
	// Run 은 명령이 끝날 때까지 기다린다. 프로세스가 실행되어 종료 코드를
	// 남겼으면 오류가 아니라 Result.ExitCode 로 돌려주고, 프로세스를 시작하지
	// 못했거나 ctx 가 취소된 경우만 오류다. 이 계약은 DESIGN §4.1 이 정한다.
	// 종료 코드로 분기하는 호출자가 있기 때문이다: preflight 의 podman 경로
	// 고정(§10.2 규칙 3), 정리 단계의 "이미 없음"(§8.3).
	Run(ctx context.Context, c Cmd) (Result, error)
	// Stream 은 장기 스트림(events)을 연다. 반환 ReadCloser 가 닫히면
	// (원격 종료·단절) 호출자가 백오프로 재시작한다. [§7.1-8]
	Stream(ctx context.Context, c Cmd) (io.ReadCloser, error)
	Close() error
}

// errEmptyArgv 는 호출자의 실수다. 빈 명령은 실행하지 않는다.
var errEmptyArgv = errors.New("executor: 빈 Argv")

// resolveArgv 는 Cmd.Sudo 를 반영한 실제 실행 argv 를 만든다. local 과 ssh 가
// 같은 규칙을 쓰도록 여기에 둔다. 원본 Cmd.Argv 는 수정하지 않는다. [§10.2]
func resolveArgv(c Cmd) ([]string, error) {
	if len(c.Argv) == 0 {
		return nil, errEmptyArgv
	}
	if !c.Sudo {
		return c.Argv, nil
	}
	argv := make([]string, 0, 2+len(c.Argv))
	argv = append(argv, sudoBin, sudoFlag)
	return append(argv, c.Argv...), nil
}

// cmdError 는 명령이 비정상 종료했음을 argv 와 stderr 와 함께 알린다.
// JIT config 는 stdin 으로만 전달되므로 argv 에 secret 이 없다. [§7.2-4]
type cmdError struct {
	argv     []string
	exitCode int
	stderr   string
}

func (e *cmdError) Error() string {
	msg := fmt.Sprintf("executor: %s: 종료 코드 %d", strings.Join(e.argv, " "), e.exitCode)
	if s := strings.TrimSpace(e.stderr); s != "" {
		msg += ": " + s
	}
	return msg
}
