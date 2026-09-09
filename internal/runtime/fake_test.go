package runtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"gh-ars/internal/executor"
)

// fakeExec 는 argv 조립과 출력 파싱만 검증하기 위한 Executor 대역이다. 실제 docker 는 쓰지 않는다.
// results 를 순서대로 소비하고, 바닥나면 종료 코드 0·빈 출력이다. [DESIGN §11]
type fakeExec struct {
	t       *testing.T
	cmds    []executor.Cmd
	stdins  []string
	results []executor.Result
	runErr  error
	stream  string
	strmErr error
	closed  int
}

func (f *fakeExec) Run(_ context.Context, c executor.Cmd) (executor.Result, error) {
	f.cmds = append(f.cmds, c)
	stdin := ""
	if c.Stdin != nil {
		b, err := io.ReadAll(c.Stdin)
		if err != nil {
			f.t.Fatalf("fake stdin read: %v", err)
		}
		stdin = string(b)
	}
	f.stdins = append(f.stdins, stdin)
	if f.runErr != nil {
		return executor.Result{}, f.runErr
	}
	if len(f.results) == 0 {
		return executor.Result{}, nil
	}
	r := f.results[0]
	f.results = f.results[1:]
	return r, nil
}

type fakeStream struct {
	io.Reader
	closed *int
}

func (s *fakeStream) Close() error { *s.closed++; return nil }

func (f *fakeExec) Stream(_ context.Context, c executor.Cmd) (io.ReadCloser, error) {
	f.cmds = append(f.cmds, c)
	if f.strmErr != nil {
		return nil, f.strmErr
	}
	return &fakeStream{Reader: strings.NewReader(f.stream), closed: &f.closed}, nil
}

func (f *fakeExec) Close() error { return nil }

func (f *fakeExec) last() executor.Cmd {
	if len(f.cmds) == 0 {
		f.t.Fatal("실행된 명령이 없다")
	}
	return f.cmds[len(f.cmds)-1]
}

var errBoom = errors.New("boom")

// testLogger 는 테스트에서 로그를 버린다(경고 경로가 nil 로거로 깨지지 않는지도 함께 본다).
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
